package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Faithful port of server/board.py's thread_op() / THREAD_LANES / caps --
// same op vocabulary, same validation, same semantics, so the real
// tasks.html (unmodified) works against this exactly as it does against
// the Python board today. Same on-disk data/threads.json format too
// ({"next":N,"threads":[...]}), atomic temp+replace write matching
// _threads_write -- either backend can read the other's file.
var threadLanes = map[string]bool{"backlog": true, "open": true, "claimed": true, "review": true, "done": true}

const threadsCap = 400
const threadHeartbeatTTL = 300 // seconds

var agentNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Thread struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	OpenedBy    string   `json:"opened_by"`
	Owner       *string  `json:"owner"`
	Assignees   []string `json:"assignees"`
	Status      string   `json:"status"`
	Heartbeat   float64  `json:"heartbeat"`
	Created     float64  `json:"created"`
	Updated     float64  `json:"updated"`
	Summary     string   `json:"summary"`
	Desc        string   `json:"desc,omitempty"`
	Closed      float64  `json:"closed,omitempty"`
	AdoptedFrom string   `json:"adopted_from,omitempty"`
	Stale       bool     `json:"stale"`
}

type ThreadStore struct {
	mu      sync.Mutex
	threads []*Thread
	next    int
	file    string // "" = no persistence (used by tests)
}

type threadsFile struct {
	Next    int       `json:"next"`
	Threads []*Thread `json:"threads"`
}

func NewThreadStore(file string) *ThreadStore {
	s := &ThreadStore{next: 1, file: file}
	s.load()
	return s
}

func (s *ThreadStore) load() {
	if s.file == "" {
		return
	}
	b, err := os.ReadFile(s.file)
	if err != nil {
		return // no existing file yet -- a fresh ledger, not an error
	}
	var d threadsFile
	if json.Unmarshal(b, &d) != nil || d.Threads == nil {
		// Corrupt/truncated: quarantine rather than silently start empty, matching
		// _threads_read()'s .bad-<ts> fallback -- the next save() would otherwise
		// permanently overwrite every card with nothing. Timestamped (not a fixed
		// .bad) so a SECOND corruption doesn't silently overwrite the first
		// quarantined copy.
		os.Rename(s.file, s.file+".bad-"+itoa(int(time.Now().Unix())))
		return
	}
	s.threads = d.Threads
	if d.Next > s.next {
		s.next = d.Next
	}
}

// save is atomic: a unique temp file then os.Rename, matching _threads_write
// exactly -- a crash mid-write leaves either the whole old file or the whole
// new one, never a truncated ledger. The temp name is unique per PID
// (belt-and-suspenders alongside s.mu, which already serializes every
// caller in this process -- covers a stray second writer that didn't go
// through Op()).
func (s *ThreadStore) save() {
	if s.file == "" {
		return
	}
	b, err := json.MarshalIndent(threadsFile{Next: s.next, Threads: s.threads}, "", " ")
	if err != nil {
		return
	}
	tmp := s.file + ".tmp." + itoa(os.Getpid())
	if os.WriteFile(tmp, b, 0644) != nil {
		return
	}
	os.Rename(tmp, s.file)
}

func now() float64 { return float64(time.Now().UnixMilli()) / 1000 }

// List returns every card with .Stale freshly computed -- same rule as
// board.py: claimed AND heartbeat older than the TTL. Returns DEEP COPIES,
// not the live *Thread pointers: the caller (main.go's GET /threads handler)
// JSON-encodes the result AFTER this unlocks, and a concurrent POST /threads
// (Op(), under s.mu) mutates those same structs -- sharing pointers made
// that a real data race (a torn Assignees slice header from remove()'s
// in-place list[:0] reuse could produce garbage or panic mid-encode). A
// fresh copy per call is cheap at this scale (a task board, not a hot loop).
func (s *ThreadStore) List() []*Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := now()
	out := make([]*Thread, len(s.threads))
	for i, t := range s.threads {
		t.Stale = t.Status == "claimed" && (n-t.Heartbeat) > threadHeartbeatTTL
		cp := *t                                          // Owner (*string) is safe shallow-copied: Op() only ever reassigns it to a NEW string, never mutates through the pointer
		cp.Assignees = append([]string{}, t.Assignees...) // independent backing array (remove() mutates the original's in place via list[:0]) AND non-nil even when empty -- append([]string(nil), ...) on an empty source stays nil, which encodes as JSON null instead of board.py's always-[], a wire-format regression the frontend happens to guard against but shouldn't have to
		out[i] = &cp
	}
	return out
}

// Op mirrors thread_op(op, data) exactly: returns (thread, error, httpStatus).
func (s *ThreadStore) Op(op string, data map[string]interface{}) (*Thread, string, int) {
	title := strTrunc(strOf(data["title"]), 200)
	agent := strOf(data["agent"])
	if agent == "" {
		agent = strOf(data["by"])
	}
	tid := strOf(data["id"])
	if agent != "" && !agentNameRe.MatchString(agent) {
		return nil, "bad agent name", 400
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if op == "create" {
		if title == "" {
			return nil, "a title is required", 400
		}
		openedBy := agent
		if openedBy == "" {
			openedBy = "?"
		}
		card := &Thread{ID: "t" + itoa(s.next), Title: title, OpenedBy: openedBy,
			Owner: nil, Assignees: []string{}, Status: "open", Heartbeat: 0,
			Created: now(), Updated: now(), Summary: ""}
		s.next++
		s.threads = append(s.threads, card)
		if len(s.threads) > threadsCap {
			s.pruneOldestDone(len(s.threads) - threadsCap)
		}
		s.save()
		return card, "", 201
	}

	var t *Thread
	for _, x := range s.threads {
		if x.ID == tid {
			t = x
			break
		}
	}
	if t == nil {
		return nil, "no such thread", 404
	}
	n := now()

	switch op {
	case "claim":
		if agent == "" {
			return nil, "an agent is required", 400
		}
		fresh := t.Owner != nil && t.Status == "claimed" && (n-t.Heartbeat) <= threadHeartbeatTTL
		if fresh && *t.Owner != agent {
			return nil, "already claimed by " + *t.Owner + " (heartbeat fresh)", 409
		}
		prev := t.Owner
		adopted := prev != nil && *prev != agent
		owner := agent
		t.Owner, t.Status, t.Heartbeat, t.Updated = &owner, "claimed", n, n
		if adopted {
			t.AdoptedFrom = *prev
		} else {
			t.AdoptedFrom = ""
		}
	case "release":
		if t.Owner == nil || *t.Owner == agent {
			t.Owner, t.Status, t.Updated = nil, "open", n
		} else {
			return nil, "only the owner releases a live claim", 403
		}
	case "assign":
		if agent != "" && !contains(t.Assignees, agent) {
			t.Assignees = append(t.Assignees, agent)
		}
		t.Updated = n
	case "unassign":
		t.Assignees = remove(t.Assignees, agent)
		t.Updated = n
	case "status":
		lane := strOf(data["lane"])
		if !threadLanes[lane] {
			return nil, "lane must be one of backlog/open/claimed/review/done", 400
		}
		if lane == "claimed" {
			if t.Owner == nil {
				return nil, "claim it instead -- 'claimed' needs an owner", 400
			}
			t.Heartbeat = n
		}
		t.Status = lane
		if lane == "open" || lane == "backlog" {
			t.Owner = nil
		}
		t.Updated = n
	case "heartbeat":
		if t.Owner != nil && *t.Owner == agent {
			t.Heartbeat = n
		}
		t.Updated = n
	case "edit":
		if title != "" {
			t.Title = title
		}
		if d, ok := data["desc"]; ok {
			t.Desc = strTrunc(strOf(d), 1000)
		}
		t.Updated = n
	case "close":
		t.Status, t.Updated, t.Closed = "done", n, n
		if sm := strTrunc(strOf(data["summary"]), 500); sm != "" {
			t.Summary = sm
		}
	default:
		return nil, "unknown op", 400
	}
	s.save()
	return t, "", 200
}

func strOf(v interface{}) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// strTrunc caps by RUNE count, not byte count -- s[:n] on a byte index can
// land mid-rune for any non-ASCII title/desc/summary, producing a truncated
// UTF-8 tail that decodes as U+FFFD on the next JSON round-trip. Matches the
// original Python's str[:n], which slices by character.
func strTrunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		s = string(r[:n])
	}
	return strings.TrimSpace(s)
}

func itoa(n int) string { return strconv.Itoa(n) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func (s *ThreadStore) pruneOldestDone(count int) {
	pruned := 0
	kept := s.threads[:0]
	for _, t := range s.threads {
		if pruned < count && t.Status == "done" {
			pruned++
			continue
		}
		kept = append(kept, t)
	}
	s.threads = kept
}

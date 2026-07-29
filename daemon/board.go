package main

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// routeDebug, when true, makes Post log the routing DECISION for every message
// -- who got woken and who didn't. That "why did this agent wake" trace is the
// one thing the board couldn't show before, and it's exactly what a wake-cycle
// (an agent's message @-mentioning another, waking them, whose reply @-mentions
// a third, and so on) needs to be diagnosable after the fact. Toggle at runtime
// via /control/debug or the /debug slash command; the lines land in the daemon
// log (daemon.err.log under the standard launch). Default ON right now because
// we are actively chasing exactly such a cycle.
var routeDebug atomic.Bool

// isPollPlumbing reports whether text IS a /vote or /poll command (the bare
// command, or the command followed by whitespace) -- not merely prefixed by
// those letters. "/voted yesterday" and "/polling is open" are ordinary
// messages and must route normally; only "/vote ..." / "/poll ..." are the
// widget plumbing that never wakes an agent.
func isPollPlumbing(text string) bool {
	for _, cmd := range []string{"/vote", "/poll"} {
		if text == cmd ||
			strings.HasPrefix(text, cmd+" ") ||
			strings.HasPrefix(text, cmd+"\t") ||
			strings.HasPrefix(text, cmd+"\n") ||
			strings.HasPrefix(text, cmd+"\r") {
			return true
		}
	}
	return false
}

type BoardMessage struct {
	ID     int      `json:"id"`
	Sender string   `json:"sender"`
	Text   string   `json:"text"`
	Tags   []string `json:"tags,omitempty"`
	// To is the STRUCTURED recipient list -- who this message notifies. This
	// is now a first-class, stored, auditable field precisely because routing
	// used to be hidden in prose (@name in the body). Empty/absent means the
	// sender-type default applied (broadcast for a human, nobody for an agent).
	To []string `json:"to,omitempty"`
	TS float64  `json:"ts"`
}

// Board is the shared message log FleetChat's real UI expects at /messages +
// /post -- the same role board.jsonl plays today. Same on-disk format as
// board.py's own Board (one JSON object per line, id/sender/text/tags/ts),
// so this LOADS the real existing history on startup and keeps appending to
// the SAME file -- a cutover doesn't lose anything, and either backend could
// read the other's file. Posting fans out through resolveRecipients() to the
// addressed agents BEFORE returning, so "post landed" and "agents notified"
// happen together, not as two separately-timed steps that could drift.
type Board struct {
	mu       sync.Mutex
	messages []BoardMessage
	nextID   int
	reg      *Registry
	file     string // "" = no persistence (used by tests)
}

func NewBoard(reg *Registry, boardFile string) *Board {
	b := &Board{nextID: 1, reg: reg, file: boardFile}
	b.load()
	return b
}

// load replays the existing JSONL, exactly matching board.py's own startup
// scan: a corrupt line is skipped, never taken as a reason to lose the rest
// of the file, and nextID picks up past the highest id seen so far.
func (b *Board) load() {
	if b.file == "" {
		return
	}
	f, err := os.Open(b.file)
	if err != nil {
		return // no existing file yet -- a fresh board, not an error
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	loaded := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var m BoardMessage
		if err := json.Unmarshal(line, &m); err != nil {
			continue // a corrupt line never takes the board down, matching board.py
		}
		b.messages = append(b.messages, m)
		if m.ID >= b.nextID {
			b.nextID = m.ID + 1
		}
		loaded++
	}
	log.Printf("[board] loaded %d message(s) from %s", loaded, b.file)
}

// append writes one line to the JSONL, matching board.py's append-only
// write. Best-effort: a disk write failure must never lose the in-memory
// post or crash the board, same as everywhere else persistence is optional.
// Caller must hold b.mu -- see the comment on Post() for why: two Write()
// syscalls from concurrent callers (a human's /post landing at the same
// moment as an agent's reply, or two agents replying near-simultaneously)
// could otherwise interleave ON DISK, splitting a line into invalid JSON
// that load() then silently -- and permanently -- drops on the next
// restart. One Write() call of the fully-built line, serialized by the
// same lock that already orders the in-memory append, closes both holes.
func (b *Board) appendLocked(m BoardMessage) {
	if b.file == "" {
		return
	}
	enc, err := json.Marshal(m)
	if err != nil {
		return
	}
	line := append(enc, '\n')
	f, err := os.OpenFile(b.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[board] append failed: %s", err)
		return
	}
	defer f.Close()
	f.Write(line)
}

// Clear wipes both the in-memory log and the file. nextID deliberately keeps
// climbing (never resets to 1) so ids never repeat and a client tracking a
// last-seen id still receives everything posted after the clear -- same
// guarantee board.py's own clear() documents.
func (b *Board) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = nil
	// Under the same lock as the in-memory wipe -- same reasoning as
	// appendLocked: truncating outside the lock could race a concurrent
	// Post()'s append and leave a stray line survive the clear, or worse,
	// truncate mid-append.
	if b.file != "" {
		os.WriteFile(b.file, []byte{}, 0644)
	}
}

func (b *Board) Since(id int) []BoardMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BoardMessage, 0)
	for _, m := range b.messages {
		if m.ID > id {
			out = append(out, m)
		}
	}
	return out
}

// Post appends to the log, THEN fans out to the addressed agents (via
// resolveRecipients) -- same order as FleetChat's real board: the message is
// durable before anyone reacts to it. The in-memory update and the disk
// append happen under the SAME lock hold (see appendLocked's comment) --
// deliberately serializing file I/O here, not a missed optimization: Post()
// is called concurrently from the /post HTTP handler AND every agent's own
// readLoop (via reg.onMessage), so without this a burst of near-simultaneous
// replies could interleave their writes on disk.
// PostResult is what a poster gets back. It embeds the stored BoardMessage so
// every existing field is unchanged, and adds the thing that was previously
// computed and then thrown away: WHO THIS ACTUALLY WOKE.
//
// The daemon already knew. board.go logged "woke [...]" on every message and
// returned only the message, so a post that reached nobody looked exactly like
// one that reached the whole crew. That cost two coordination failures in a
// single day -- 17 replies to an off-board name, and a finding posted with no
// recipient that sat unread. Both were careful, correct, and did nothing.
//
// Woke is a property of THIS delivery, not of the message, so it is returned
// rather than persisted -- board.jsonl keeps its existing shape.
type PostResult struct {
	BoardMessage
	Woke    []string `json:"woke"`
	Warning string   `json:"warning,omitempty"`
}

func (b *Board) Post(sender, text string, tags []string, to []string) PostResult {
	agents := b.reg.All()
	ids := make([]string, len(agents))
	for i, a := range agents {
		ids[i] = a.id
	}
	// Merge any deliberate @name in the body INTO the explicit recipient list
	// BEFORE the message is stored, so `To` (the durable, auditable field) and the
	// [route] debug log below both reflect who the message actually addresses --
	// and therefore wakes. (A bare name, no @, adds no one.) resolveRecipients then
	// works off this one merged list; the sender is never woken by their own tag.
	to = mergeAtMentions(to, text, ids)

	b.mu.Lock()
	msg := BoardMessage{ID: b.nextID, Sender: sender, Text: text, Tags: tags, To: to, TS: float64(time.Now().UnixMilli()) / 1000}
	b.nextID++
	b.messages = append(b.messages, msg)
	b.appendLocked(msg)
	b.mu.Unlock()

	recipients := resolveRecipients(sender, to, ids)
	// /vote and /poll are board plumbing (the poll widget), never a wake -- the
	// old prose-scan router skipped them explicitly; dropping that skip in the
	// rewrite let a single vote (a human sender with no `to`) broadcast to the
	// entire crew. Restore the skip regardless of `to`, but WORD-BOUNDED: a bare
	// prefix match also swallowed ordinary text like "/voted yesterday" /
	// "/polling is open" and silently under-woke. Match only the real commands.
	if isPollPlumbing(text) {
		recipients = map[string]bool{}
	}
	// Fan-out is an ENQUEUE, not a write: SendPrompt hands the turn to the
	// recipient's own send queue (agent.go sendCh/sendLoop) and returns
	// immediately. The old design wrote each recipient's stdin synchronously,
	// right here, on the SENDER's readLoop -- reasoned "near-unreachable" as a
	// stall, until a recipient stopped draining its stdin in the field and the
	// blocked write froze the sender's turn AND hung /shutdown behind it. Now a
	// wedged recipient backs up only its own bounded queue; a FULL queue makes
	// SendPrompt error, and that recipient is logged "not woken" below instead
	// of the whole fan-out hanging. The write itself (and the pendingPrivate
	// FIFO push) happens on the recipient's sendLoop, preserving turn order.
	engaged := make([]string, 0, len(recipients))
	for _, a := range agents {
		if recipients[a.id] {
			// The board rulebook is delivered once per launch as an APPENDED SYSTEM
			// PROMPT (see claudeCommand), not injected into the conversation -- so it
			// is never written into the agent's saved session. That is what keeps a
			// resumed or hand-launched session free of board "taint": with no daemon
			// supplying the rules, it simply has none to act on. Every board turn here
			// is just the bare message envelope.
			prompt := messageEnvelope(sender, text)
			// Only count agents actually reached: a SendPrompt error means the
			// process's stdin is gone (dead/reaping), so it was NOT woken -- the
			// debug trace should say so rather than overstate the fan-out.
			if err := a.SendPrompt(prompt); err == nil {
				engaged = append(engaged, a.id)
			} else {
				log.Printf("[route] SendPrompt to %q failed (not woken): %s", a.id, err)
			}
		}
	}
	// Surface the same fact to the SENDER that the debug log below records. An
	// empty wake-list is not an error -- a broadcast with no crew, a poll, a
	// deliberate note -- but it must be visible, because silence is what let a
	// misaddressed message look successful.
	res := PostResult{BoardMessage: msg, Woke: engaged}
	if unknown := unknownMentions(text, ids); len(unknown) > 0 {
		res.Warning = "these @names are not on this board and were not woken: " + strings.Join(unknown, ", ")
	} else if len(engaged) == 0 {
		res.Warning = "this message woke nobody -- no @name matched a crew member"
	}
	if routeDebug.Load() {
		// One line per posted message: who sent it, the structured recipient
		// list it requested, and exactly which agents it woke. Chained across
		// messages this reconstructs any wake-cycle.
		log.Printf("[route] msg#%d from %q to=%v woke %v (crew=%v)", msg.ID, sender, to, engaged, ids)
	}
	return res
}

// The board's framing splits into two parts. protocolRules() is the GENERIC
// protocol every agent is taught -- how addressing wakes a teammate, the PASS
// convention, that the board voices replies, and the task card API -- and
// messageEnvelope() is the per-turn message itself.
//
// It says NOTHING about an agent's permissions or file access, deliberately.
// The board does not create agents, choose their CLI flags, or set what they
// may touch; the operator does, in the settings dialog. It used to describe an
// access model anyway -- "by default you're scoped to your own project folder"
// -- which was both untrue and not the board's to state. Describing someone
// else's configuration is how a claim gets made that nothing backs, so this
// stays scoped to what the board itself actually governs.
// The model needs to know it's in a shared chat (not a 1:1), that the PASS
// convention exists (or it never produces it -- see agent.go's route()), and
// that routing is ASYMMETRIC: the operator's plain messages reach everyone, but
// an AGENT's own reply reaches only whoever it explicitly tags -- otherwise
// nothing explains why its OWN unaddressed reply wakes no one, which would look
// like a bug from the inside.
//
// The rules are delivered ONCE per launch as an appended system prompt (see
// claudeCommand), not repeated in the conversation and never written into the
// saved session -- so a resumed or hand-launched agent carries no board "taint."
// The text is GENERIC -- it names no specific
// agent, lane, or mission -- so it ships with the app and teaches anyone's crew
// the same way; the operator's real roster lives in the private overlay.
//
// sender in messageEnvelope is REAL attribution, not decoration: it was a
// genuine bug until caught live -- the prompt used to omit it, so every engaged
// agent received "New message: <text>" with no idea who sent it, and any
// identity claim was whatever the sender typed into their own body ("Bob here"),
// in-band and unverifiable. The board attests the sender instead.

// messageEnvelope is the per-turn part: the actual message, carrying the
// board-attested sender. Every turn includes it; only an agent not yet taught
// also gets protocolRules() prepended.
func messageEnvelope(sender, text string) string {
	return "New message from " + sender + ": " + text
}

func protocolRules() string {
	return "You are in a live team chat with other agents and a human operator." +
		"\n\nADDRESSING (a real wake mechanism, not etiquette): to notify a teammate, put \"@name\" in " +
		"your message -- \"@all\" reaches the whole crew. An @name is the ONLY thing that wakes someone; " +
		"a bare name (no @) is just display, so you can mention anyone in passing without summoning them. " +
		"Your crewmates are PEERS on this board, not subagents you spawned -- the ONLY way to reach one is " +
		"an @name in your reply here; there is no separate send-message or spawn-agent tool for them, so " +
		"don't go hunting for one. " +
		"A reply that addresses no one is still posted and visible to the operator -- it just doesn't wake " +
		"a teammate. Each @name COSTS that teammate a turn, so tag someone only when you actually need them " +
		"to act -- a bare acknowledgment or \"thanks\" needs no @ (that is what keeps replies from " +
		"ping-ponging). If a message has nothing to do with you, reply with exactly: PASS and nothing else. " +
		"But a message that @-tags YOU with a direct request always gets a real reply -- even if it looks " +
		"redundant or you've answered it before. PASS is never an answer to being asked; the requester " +
		"can't see a PASS, so to them it's indistinguishable from you being broken.\n\n" +
		"VOICE: the board speaks your replies aloud through its own speaker. Never use a TTS/speak tool " +
		"on a board reply yourself -- even if your own instructions name a voice for you, that applies to " +
		"standalone sessions, not here; a self-spoken board reply plays DOUBLE over the board's voice.\n\n" +
		"OPERATING THE BOARD (task cards): the task ledger is a loopback HTTP API on this board at " +
		"http://127.0.0.1:" + daemonPort + "/threads. Reads are GET; every write is a POST whose JSON body " +
		"carries an \"op\", plus the header 'X-Fleet-Client: agent'. Put \"agent\":\"<your own board id>\" " +
		"on writes so the card records who acted -- but note a card's stated author/owner is SELF-ASSERTED " +
		"over HTTP, not attested, so don't treat a card's claimed author as proof of who really wrote it. " +
		"Ops and their bodies:\n" +
		"- create: {\"op\":\"create\",\"title\":\"...\",\"agent\":\"<you>\"}  -> new card (status open, id \"tN\")\n" +
		"- claim:  {\"op\":\"claim\",\"id\":\"tN\",\"agent\":\"<you>\"}\n" +
		"- move:   {\"op\":\"status\",\"id\":\"tN\",\"lane\":\"<lane>\"}  (lane is one of: backlog, open, claimed, review, done)\n" +
		"- edit:   {\"op\":\"edit\",\"id\":\"tN\",\"title\":\"...\",\"desc\":\"...\"}\n" +
		"- close:  {\"op\":\"close\",\"id\":\"tN\",\"summary\":\"...\"}\n" +
		"- list:   GET /threads  -> {\"threads\":[...]}\n" +
		"Example: curl -s -X POST http://127.0.0.1:" + daemonPort + "/threads -H 'X-Fleet-Client: agent' " +
		"-H 'Content-Type: application/json' -d '{\"op\":\"create\",\"title\":\"...\",\"agent\":\"<you>\"}'."
}

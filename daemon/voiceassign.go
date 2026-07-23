package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// voiceassign.go -- persistence for the per-agent voice map.
//
// Two bugs, one cause. The UI's cmdSetVoice says "SERVER-backed: writes
// data/voices.json, the map the optional server-side speaker reads" -- but this
// daemon never wrote that file. /control/voice only mutated an in-memory map,
// so:
//
//  1. an assignment was FORGOTTEN on every daemon restart, and
//  2. it never reached agents/speaker.py at all, which reads its overrides from
//     data/voices.json -- so the Settings UI has been a no-op for the
//     high-quality voice path since the Go rewrite, silently.
//
// data/voices.json is the established contract with the speaker, so persisting
// there (rather than inventing a new key in settings.json) fixes the forgetting
// AND connects the feature to the consumer that actually reads it.

func voicesPath(repoRoot string) string {
	return filepath.Join(repoRoot, "data", "voices.json")
}

// voiceIDRe bounds what may be stored as a voice. The daemon has no canonical
// voice list to allowlist against -- the engine owns that -- so this constrains
// the SHAPE instead: a Kokoro id is a two-letter locale/gender prefix, an
// underscore, then a short name (af_heart, am_michael, bm_george).
//
// This matters because /control/voice's body.Voice was previously stored with
// NO validation at all. While the map was in-memory that was merely untidy;
// persisting it turns an unvalidated request string into a file write, so it
// needs the same discipline the mode allowlist gets: reject at the door, and
// re-validate on load so a hand-edited file cannot inject either.
var voiceIDRe = regexp.MustCompile(`^[a-z]{2}_[a-z]{2,20}$`)

var voicesFileMu sync.Mutex

// loadVoiceAssign reads the persisted map, dropping any entry that fails
// validation rather than trusting the file. A missing or corrupt file yields an
// empty map so a bad file can never block startup.
func loadVoiceAssign(repoRoot string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(voicesPath(repoRoot))
	if err != nil {
		return out
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return out
	}
	for agent, voice := range m {
		// Same gates as the request path -- a file edited by hand is exactly as
		// untrusted as an HTTP body.
		if validID.MatchString(agent) && voiceIDRe.MatchString(voice) {
			out[agent] = voice
		}
	}
	return out
}

// saveVoiceAssign persists the map. Atomic temp+rename with a PID-unique temp
// name, matching threads.go/roster.go: the speaker may be reading this file
// concurrently, and a rename means it sees either the whole old map or the
// whole new one, never a half-written one. Best-effort -- a failed save must
// never break the live request.
func saveVoiceAssign(repoRoot string, m map[string]string) {
	voicesFileMu.Lock()
	defer voicesFileMu.Unlock()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Join(repoRoot, "data"), 0o755) != nil {
		return
	}
	path := voicesPath(repoRoot)
	tmp := path + ".tmp." + itoa(os.Getpid())
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	os.Rename(tmp, path)
}

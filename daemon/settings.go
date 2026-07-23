package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// settings.go -- minimal on-disk persistence for the two voice settings that
// MUST survive a daemon restart.
//
// Background: voiceMode/ttsMuted were deliberately in-memory only, justified
// by "no server-side TTS speaker" (see main.go). PR #30 added exactly that --
// the daemon-orchestrated Kokoro sidecar -- which invalidated the premise. The
// user-visible consequence: picking the "High-quality" (server-only) voice
// option silenced the browser fallback for the session, but a restart reverted
// voiceMode to "auto" and the browser voice began speaking OVER the Kokoro
// speaker again. Persisting these two keys closes that.
//
// Scope is deliberately narrow: this reads and writes ONLY tts_muted and
// voice_mode. It is not a general settings backend.

func settingsPath(repoRoot string) string {
	return filepath.Join(repoRoot, "data", "settings.json")
}

// settingsFileMu serializes read-modify-write of settings.json. It is a
// DIFFERENT lock from main.go's settingsMu (which guards the in-memory vars):
// callers must not hold settingsMu across a save, so disk I/O never happens
// under the hot in-memory lock.
var settingsFileMu sync.Mutex

// loadSettingsFile reads settings.json as a generic map. Deliberately a map
// and not a typed struct: the file carries keys this daemon does not own
// (memory, model, cli_template, left by the retired Python stack). Round-
// tripping through a narrow struct would silently DROP them on the first
// save. A missing or corrupt file yields an empty map -- callers fall back to
// their defaults, so a bad settings file can never block startup.
func loadSettingsFile(repoRoot string) map[string]interface{} {
	b, err := os.ReadFile(settingsPath(repoRoot))
	if err != nil {
		return map[string]interface{}{}
	}
	var m map[string]interface{}
	if json.Unmarshal(b, &m) != nil || m == nil {
		return map[string]interface{}{}
	}
	return m
}

// loadVoicePrefs restores tts_muted and voice_mode. Absent or wrong-typed
// values fall back to the supplied defaults, so a hand-edited file cannot put
// the daemon into an undefined state.
func loadVoicePrefs(repoRoot string, defMuted bool, defMode string) (bool, string) {
	m := loadSettingsFile(repoRoot)
	muted, mode := defMuted, defMode
	if v, ok := m["tts_muted"].(bool); ok {
		muted = v
	}
	// Allowlist, not free text: voice_mode drives a security-relevant branch in
	// the UI (whether the browser speech path runs at all). Anything unexpected
	// falls back to the default rather than being trusted through.
	if v, ok := m["voice_mode"].(string); ok && (v == "auto" || v == "server-only") {
		mode = v
	}
	return muted, mode
}

// saveVoicePrefs persists the two keys, preserving every other key in the
// file. Atomic temp+rename with a PID-unique temp name, matching
// threads.go/roster.go: a crash mid-write leaves either the whole old file or
// the whole new one, never a truncated settings file. Best-effort by design --
// a failed save must never break the live request.
func saveVoicePrefs(repoRoot string, muted bool, mode string) {
	settingsFileMu.Lock()
	defer settingsFileMu.Unlock()
	m := loadSettingsFile(repoRoot)
	m["tts_muted"] = muted
	m["voice_mode"] = mode
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Join(repoRoot, "data"), 0o755) != nil {
		return
	}
	path := settingsPath(repoRoot)
	tmp := path + ".tmp." + itoa(os.Getpid())
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	os.Rename(tmp, path)
}

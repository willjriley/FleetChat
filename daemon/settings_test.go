package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeSettings(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A missing settings.json must not fail the boot -- callers get their defaults.
func TestLoadVoicePrefsMissingFileUsesDefaults(t *testing.T) {
	muted, mode := loadVoicePrefs(t.TempDir(), false, "auto")
	if muted != false || mode != "auto" {
		t.Fatalf("want (false, auto), got (%v, %q)", muted, mode)
	}
}

func TestLoadVoicePrefsReadsDisk(t *testing.T) {
	root := t.TempDir()
	writeSettings(t, root, `{"tts_muted":true,"voice_mode":"server-only"}`)
	muted, mode := loadVoicePrefs(root, false, "auto")
	if !muted || mode != "server-only" {
		t.Fatalf("want (true, server-only), got (%v, %q)", muted, mode)
	}
}

// A corrupt file must degrade to defaults rather than blocking startup.
func TestLoadVoicePrefsCorruptFileUsesDefaults(t *testing.T) {
	root := t.TempDir()
	writeSettings(t, root, `{ this is not json`)
	muted, mode := loadVoicePrefs(root, false, "auto")
	if muted != false || mode != "auto" {
		t.Fatalf("want defaults on corrupt file, got (%v, %q)", muted, mode)
	}
}

// voice_mode is an allowlisted control value: an unexpected string must not be
// trusted through, because the UI reads anything != "server-only" as
// "browser voices allowed".
func TestLoadVoicePrefsRejectsUnknownMode(t *testing.T) {
	root := t.TempDir()
	writeSettings(t, root, `{"voice_mode":"wat"}`)
	if _, mode := loadVoicePrefs(root, false, "server-only"); mode != "server-only" {
		t.Fatalf("unknown mode should fall back to the default, got %q", mode)
	}
	writeSettings(t, root, `{"voice_mode":123}`)
	if _, mode := loadVoicePrefs(root, false, "auto"); mode != "auto" {
		t.Fatalf("wrong-typed mode should fall back to the default, got %q", mode)
	}
}

// THE REGRESSION THIS FIX EXISTS FOR: the chosen mode must survive a restart.
func TestSaveThenLoadRoundTrips(t *testing.T) {
	root := t.TempDir()
	saveVoicePrefs(root, true, "server-only")
	muted, mode := loadVoicePrefs(root, false, "auto")
	if !muted || mode != "server-only" {
		t.Fatalf("settings did not survive: got (%v, %q)", muted, mode)
	}
}

// Saving must NOT drop keys this daemon does not own -- the file also carries
// memory/model/cli_template left by the retired Python stack. A narrow typed
// struct would silently erase them on the first write.
func TestSavePreservesUnownedKeys(t *testing.T) {
	root := t.TempDir()
	writeSettings(t, root, `{"tts_muted":false,"voice_mode":"auto",
		"memory":{"alice":true},"model":{"bob":"some-model"},"cli_template":{"carol":"x"}}`)

	saveVoicePrefs(root, true, "server-only")

	b, err := os.ReadFile(filepath.Join(root, "data", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("save produced invalid json: %v", err)
	}
	for _, k := range []string{"memory", "model", "cli_template"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("save dropped unowned key %q -- data loss", k)
		}
	}
	if m["voice_mode"] != "server-only" || m["tts_muted"] != true {
		t.Fatalf("save did not write the owned keys: %v", m)
	}
	if got := m["model"].(map[string]interface{})["bob"]; got != "some-model" {
		t.Fatalf("nested unowned value mangled: %v", got)
	}
}

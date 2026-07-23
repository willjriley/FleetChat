package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeVoices(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "voices.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadVoiceAssignMissingFile(t *testing.T) {
	if m := loadVoiceAssign(t.TempDir()); len(m) != 0 {
		t.Fatalf("missing file should give an empty map, got %v", m)
	}
}

func TestLoadVoiceAssignCorruptFile(t *testing.T) {
	root := t.TempDir()
	writeVoices(t, root, `not json at all`)
	if m := loadVoiceAssign(root); len(m) != 0 {
		t.Fatalf("corrupt file must degrade to empty, got %v", m)
	}
}

// THE REGRESSION: an assignment must survive a daemon restart.
// A save that cannot reach disk must REPORT it rather than returning silently:
// the caller logs "applied but not persisted" instead of implying durability.
func TestSaveVoiceAssignReportsFailure(t *testing.T) {
	root := t.TempDir()
	// data/ occupied by a FILE, so creating the directory (and thus the write)
	// cannot succeed.
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveVoiceAssign(root, map[string]string{"alice": "af_heart"}); err == nil {
		t.Fatal("save should have reported a failure instead of silently succeeding")
	}
}

// A successful save leaves no .tmp.<pid> litter behind.
func TestSaveVoiceAssignLeavesNoTempFiles(t *testing.T) {
	root := t.TempDir()
	if err := saveVoiceAssign(root, map[string]string{"alice": "af_heart"}); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestVoiceAssignRoundTrips(t *testing.T) {
	root := t.TempDir()
	if err := saveVoiceAssign(root, map[string]string{"alice": "af_heart", "bob": "am_echo"}); err != nil {
		t.Fatal(err)
	}
	m := loadVoiceAssign(root)
	if m["alice"] != "af_heart" || m["bob"] != "am_echo" {
		t.Fatalf("assignment did not survive: %v", m)
	}
}

// A hand-edited file is as untrusted as an HTTP body: bad ids are dropped, and
// dropping one must not discard the valid entries alongside it.
func TestLoadVoiceAssignRejectsBadEntries(t *testing.T) {
	root := t.TempDir()
	writeVoices(t, root, `{
		"alice":"af_heart",
		"bob":"; rm -rf /",
		"carol":"../../etc/passwd",
		"BAD NAME":"am_echo",
		"dave":"am_echo"
	}`)
	m := loadVoiceAssign(root)
	if m["alice"] != "af_heart" || m["dave"] != "am_echo" {
		t.Fatalf("valid entries must survive: %v", m)
	}
	for _, bad := range []string{"bob", "carol", "BAD NAME"} {
		if _, ok := m[bad]; ok {
			t.Fatalf("entry %q should have been rejected: %v", bad, m)
		}
	}
}

func TestVoiceIDPattern(t *testing.T) {
	for _, ok := range []string{"af_heart", "am_michael", "bm_george", "bf_emma"} {
		if !voiceIDRe.MatchString(ok) {
			t.Fatalf("%q should be a valid voice id", ok)
		}
	}
	for _, bad := range []string{"", "off", "am_", "a_heart", "AM_HEART", "am_heart; rm", "../x", "am_" + string(make([]byte, 40))} {
		if voiceIDRe.MatchString(bad) {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

// Saving must fully replace the file, so clearing an assignment ("off" deletes
// from the map, then the whole map is written) actually removes it on disk.
func TestSaveVoiceAssignReplacesFile(t *testing.T) {
	root := t.TempDir()
	if err := saveVoiceAssign(root, map[string]string{"alice": "af_heart", "bob": "am_echo"}); err != nil {
		t.Fatal(err)
	}
	if err := saveVoiceAssign(root, map[string]string{"alice": "af_heart"}); err != nil {
		t.Fatal(err)
	}
	m := loadVoiceAssign(root)
	if _, gone := m["bob"]; gone {
		t.Fatalf("cleared assignment should not persist: %v", m)
	}
	if m["alice"] != "af_heart" {
		t.Fatalf("remaining assignment lost: %v", m)
	}
}

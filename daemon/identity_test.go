package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The identity guard's core predicate. The reviewer flagged that this whole
// class was untested and that case/whitespace + the reserved "board" + the
// restart-window (roster-but-not-live) all bypassed the original guard. These
// lock all three paths, with case/space variants.
func TestIsReservedOrKnownAgent(t *testing.T) {
	reg := NewRegistry()
	reg.agents["bob"] = &Agent{id: "bob"} // a LIVE agent, inserted directly (no real subprocess)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0755); err != nil {
		t.Fatal(err)
	}
	// "carol" is on the DURABLE roster but NOT live -- the restart-window case.
	if err := os.WriteFile(filepath.Join(dir, "data", "roster.json"), []byte(`[{"name":"carol"}]`), 0644); err != nil {
		t.Fatal(err)
	}

	cases := map[string]bool{
		"board": true, "Board": true, " BOARD ": true,
		"all": true, "All": true,
		"bob": true, "Bob": true, " bob ": true,
		"carol": true, "Carol": true, "carol ": true,
		"owner": false, "operator": false, "": false, "carolx": false,
	}
	for in, want := range cases {
		if got := isReservedOrKnownAgent(in, reg, dir); got != want {
			t.Errorf("isReservedOrKnownAgent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsReservedName(t *testing.T) {
	for _, in := range []string{"board", "Board", " ALL ", "all"} {
		if !isReservedName(in) {
			t.Errorf("isReservedName(%q) should be true", in)
		}
	}
	for _, in := range []string{"alice", "bob", "carol", "dave", "erin", "owner", ""} {
		if isReservedName(in) {
			t.Errorf("isReservedName(%q) should be false", in)
		}
	}
}

// The printable-ASCII sender gate that closes the Unicode-invisible / homoglyph
// impersonation residual: a "carol" carrying a zero-width/control rune, or a
// Cyrillic lookalike, must be rejected (they'd otherwise render as the agent).
// The suspect runes are BUILT from hex codepoints so the source stays pure
// ASCII -- a literal one (esp. a BOM) is even rejected by the Go compiler.
func TestIsPrintableASCII(t *testing.T) {
	ok := []string{"owner", "operator-1", "Sam Rivera", "a_b.c", "~tilde", "carol"}
	for _, s := range ok {
		if !isPrintableASCII(s) {
			t.Errorf("isPrintableASCII(%q) should be true", s)
		}
	}
	bad := []struct {
		s   string
		why string
	}{
		{"carol" + string(rune(0x200b)), "trailing ZWSP"},
		{string(rune(0x200b)) + "carol", "leading ZWSP"},
		{"carol" + string(rune(0xfeff)), "BOM"},
		{"carol\x00", "NUL"},
		{string(rune(0x0441)) + "arol", "Cyrillic homoglyph U+0441 (looks like c)"},
		{"line\nbreak", "embedded newline"},
		{"tab\tinside", "embedded tab"},
		{"caf" + string(rune(0xe9)), "accented (non-ASCII)"},
	}
	for _, b := range bad {
		if isPrintableASCII(b.s) {
			t.Errorf("isPrintableASCII(%q) should be false (%s)", b.s, b.why)
		}
	}
}

func TestIsPollPlumbing(t *testing.T) {
	yes := []string{"/vote", "/vote 1 0", "/poll", "/poll Which one?", "/vote\t3 2"}
	for _, s := range yes {
		if !isPollPlumbing(s) {
			t.Errorf("isPollPlumbing(%q) should be true", s)
		}
	}
	no := []string{"/voted yesterday", "/polling is open", "vote for me", "/votes", "just /vote in the middle"}
	for _, s := range no {
		if isPollPlumbing(s) {
			t.Errorf("isPollPlumbing(%q) should be false", s)
		}
	}
}

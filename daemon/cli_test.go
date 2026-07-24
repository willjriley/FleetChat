package main

import (
	"strings"
	"testing"
)

// buildCLICommand is the multi-CLI seam: claude is fully wired, gemini/qwen are
// recognized-but-not-yet-adapted (fail loudly), anything else is unknown.
func TestBuildCLICommand(t *testing.T) {
	// claude -- explicit, default (""), and case/space-insensitive -- fully wired.
	for _, cli := range []string{"claude", "", "  Claude "} {
		bin, args, err := buildCLICommand(AgentOptions{CLI: cli, Persona: "p", Folder: "f"})
		if err != nil {
			t.Fatalf("cli %q should build, got err %v", cli, err)
		}
		if bin == "" {
			t.Fatalf("cli %q: empty binary", cli)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--input-format=stream-json") || !strings.Contains(joined, "--system-prompt") {
			t.Fatalf("cli %q: claude args missing: %v", cli, args)
		}
	}

	// gemini/qwen -- recognized backends, adapter not wired -> clear error, and
	// NO partial command handed back (so NewAgent can't launch a broken process).
	for _, cli := range []string{"gemini", "qwen", "QWEN"} {
		bin, args, err := buildCLICommand(AgentOptions{CLI: cli})
		if err == nil {
			t.Fatalf("cli %q must error until its adapter is wired", cli)
		}
		if bin != "" || args != nil {
			t.Fatalf("cli %q errored but still returned a command (%q %v)", cli, bin, args)
		}
	}

	// An unknown backend errors too.
	if _, _, err := buildCLICommand(AgentOptions{CLI: "gpt5"}); err == nil {
		t.Fatalf("unknown cli must error")
	}

	// A malformed resume id must never reach argv (argv-injection guard).
	_, args, _ := buildCLICommand(AgentOptions{ResumeSession: "not-a-uuid"})
	if strings.Contains(strings.Join(args, " "), "--resume") {
		t.Fatalf("bad resume id must not reach argv: %v", args)
	}
}

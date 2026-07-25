package main

import (
	"strings"
	"testing"
)

// buildCLICommand is the multi-CLI seam: claude and qwen are wired, gemini is
// recognized-but-not-yet-adapted (fails loudly), anything else is unknown.
func TestBuildCLICommand(t *testing.T) {
	// claude -- explicit, default (""), and case/space-insensitive -- fully wired.
	for _, cli := range []string{"claude", "", "  Claude "} {
		bin, args, err := buildCLICommand(AgentOptions{CLI: cli, Folder: "f"})
		if err != nil {
			t.Fatalf("cli %q should build, got err %v", cli, err)
		}
		if bin == "" {
			t.Fatalf("cli %q: empty binary", cli)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--input-format=stream-json") {
			t.Fatalf("cli %q: claude args missing: %v", cli, args)
		}
	}

	// qwen -- wired via the self-exec adapter: builds a command that launches the
	// daemon in qwen-adapter mode (case/space-insensitive).
	for _, cli := range []string{"qwen", "QWEN", "  qwen "} {
		bin, args, err := buildCLICommand(AgentOptions{CLI: cli, Folder: "f"})
		if err != nil {
			t.Fatalf("cli %q should build (qwen is wired), got err %v", cli, err)
		}
		if bin == "" {
			t.Fatalf("cli %q: empty binary", cli)
		}
		if !strings.Contains(strings.Join(args, " "), "qwen-adapter") {
			t.Fatalf("cli %q: qwen command missing adapter mode: %v", cli, args)
		}
	}

	// gemini -- recognized backend, adapter not wired -> clear error, and NO partial
	// command handed back (so NewAgent can't launch a broken process).
	for _, cli := range []string{"gemini", "Gemini"} {
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

package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
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

// splitArgs turns the operator's one free-text argument line into argv. The
// case that matters most on Windows is a quoted path: backslash must survive as
// a literal, because treating it as an escape would silently corrupt every path
// a user pastes into the field.
func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   \t\n ", nil},
		{"--model sonnet", []string{"--model", "sonnet"}},
		{"  --a   --b  ", []string{"--a", "--b"}}, // runs of whitespace collapse
		// The load-bearing one: a Windows path with spaces, quoted.
		{`--add-dir "C:\repos\my folder"`, []string{"--add-dir", `C:\repos\my folder`}},
		// Unquoted backslashes are literal too -- no escape processing at all.
		{`--add-dir C:\repos\forge`, []string{"--add-dir", `C:\repos\forge`}},
		{"--x 'a b'", []string{"--x", "a b"}}, // single quotes group as well
		// A quote can open mid-argument and the argument continues after it closes.
		{`--dir="a b"c`, []string{`--dir=a bc`}},
		{`""`, []string{""}},                                   // an explicitly empty argument is a real argument
		{`--x "unterminated`, []string{"--x", "unterminated"}}, // salvaged, not dropped
		// No shell runs these, so metacharacters are ordinary characters.
		{`--msg "a; rm -rf / | b"`, []string{"--msg", "a; rm -rf / | b"}},
	}
	for _, c := range cases {
		got := splitArgs(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitArgs(%q) = %q, want %q", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitArgs(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// The preview endpoint and the launcher must agree, or the box shows a command
// that is not the one that runs -- the exact drift this feature removes.
func TestPreviewMatchesLaunch(t *testing.T) {
	for _, cli := range []string{"claude", "qwen"} {
		opts := AgentOptions{CLI: cli, Folder: "f", FullPermissions: true,
			ExtraArgs: splitArgs(`--model sonnet --add-dir "C:\a b"`)}
		wantBin, wantArgs, err := buildCLICommand(opts)
		if err != nil {
			t.Fatalf("%s: build failed: %v", cli, err)
		}
		gotBin, gotArgs, err := PreviewCommand(opts)
		if err != nil {
			t.Fatalf("%s: preview failed: %v", cli, err)
		}
		if gotBin != wantBin || strings.Join(gotArgs, "\x00") != strings.Join(wantArgs, "\x00") {
			t.Fatalf("%s: preview %q %v != launch %q %v", cli, gotBin, gotArgs, wantBin, wantArgs)
		}
		// Operator args must actually reach the command, and land LAST so a
		// last-wins CLI honours them over anything we set earlier.
		if n := len(gotArgs); n < 4 || gotArgs[n-4] != "--model" || gotArgs[n-3] != "sonnet" ||
			gotArgs[n-2] != "--add-dir" || gotArgs[n-1] != `C:\a b` {
			t.Fatalf("%s: extra args not appended last: %v", cli, gotArgs)
		}
	}
}

// Over the real HTTP path this time: what the settings dialog RENDERS has to be
// what the launcher would run. Everything the box shows is asserted against
// buildCLICommand's own output, so a future change to either side that breaks
// the correspondence fails here rather than misleading an operator.
func TestCommandPreviewEndpoint(t *testing.T) {
	q := url.Values{}
	q.Set("dir", `C:\repos\forge`)
	q.Set("cli", "claude")
	q.Set("full_perms", "1")
	q.Set("args", `--model sonnet --add-dir "C:\a b"`)

	rec := httptest.NewRecorder()
	handleCommandPreview(rec, httptest.NewRequest("GET", "/control/command-preview?"+q.Encode(), nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Bin  string   `json:"bin"`
		Args []string `json:"args"`
		Argc int      `json:"argc"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}

	wantBin, wantArgs, err := buildCLICommand(AgentOptions{
		Folder: `C:\repos\forge`, CLI: "claude", FullPermissions: true,
		ExtraArgs: splitArgs(`--model sonnet --add-dir "C:\a b"`),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got.Bin != wantBin {
		t.Fatalf("bin %q != launched %q", got.Bin, wantBin)
	}
	if got.Argc != len(wantArgs) {
		t.Fatalf("argc %d != launched %d", got.Argc, len(wantArgs))
	}
	// The rendered list may differ from the launched one ONLY where the system
	// prompt is elided, and the elision must be visible as an elision.
	elided := 0
	for i, j := 0, 0; i < len(got.Args) && j < len(wantArgs); i, j = i+1, j+1 {
		if got.Args[i] == wantArgs[j] {
			continue
		}
		if i > 0 && got.Args[i-1] == "--append-system-prompt" && strings.HasPrefix(got.Args[i], "<board protocol rules,") {
			elided++
			continue
		}
		t.Fatalf("shown[%d]=%q != launched[%d]=%q -- the preview is not the command", i, got.Args[i], j, wantArgs[j])
	}
	if !full(got.Args, "--dangerously-skip-permissions") {
		t.Fatalf("full_perms=1 not reflected in the preview: %v", got.Args)
	}
	if n := len(got.Args); n < 4 || got.Args[n-4] != "--model" || got.Args[n-3] != "sonnet" ||
		got.Args[n-2] != "--add-dir" || got.Args[n-1] != `C:\a b` {
		t.Fatalf("operator args missing or not last: %v", got.Args)
	}
	// A bad CLI must say so in the box rather than render a plausible command.
	rec2 := httptest.NewRecorder()
	handleCommandPreview(rec2, httptest.NewRequest("GET", "/control/command-preview?cli=nonesuch", nil))
	if rec2.Code != 400 || !strings.Contains(rec2.Body.String(), "error") {
		t.Fatalf("unknown CLI should be a 400 with an error, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func full(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

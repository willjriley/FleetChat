package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// An id whose process is still tearing down must not be reusable. Returning the
// dying agent would hand back a corpse; creating a new one would put two live
// processes on a single agent id -- the duplicate-process bug this exists to
// stop.
func TestSpawnRefusesAnIDThatIsStillDying(t *testing.T) {
	r := NewRegistry()
	a := &Agent{id: "alice", exited: make(chan struct{}), stderrDone: make(chan struct{})}
	a.dying.Store(true)
	r.agents["alice"] = a

	got, err := r.Spawn("alice", AgentOptions{}, PersonaConfig{})
	if err == nil {
		t.Fatal("Spawn must refuse an id whose previous process is still shutting down")
	}
	if got != nil {
		t.Fatal("Spawn must not return the dying agent")
	}
	if !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("error should say why, got: %v", err)
	}
}

// The same id IS reusable once the previous process is confirmed gone.
func TestSpawnAllowsTheIDOnceNotDying(t *testing.T) {
	r := NewRegistry()
	a := &Agent{id: "alice", exited: make(chan struct{}), stderrDone: make(chan struct{})}
	r.agents["alice"] = a
	got, err := r.Spawn("alice", AgentOptions{}, PersonaConfig{})
	if err != nil || got != a {
		t.Fatalf("a live agent should be returned idempotently, got (%v, %v)", got, err)
	}
}

func TestKillUnknownAgent(t *testing.T) {
	if err := NewRegistry().Kill("nobody"); err == nil {
		t.Fatal("killing an unknown id should error")
	}
}

// TestHelperProcess is not a real test -- it is the long-lived child process
// the lifecycle tests kill. The standard Go idiom: re-exec the test binary,
// gated on an env var so a normal run skips it instantly.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("FLEETCHAT_TEST_HELPER") != "1" {
		t.Skip("helper process, not a test")
	}
	time.Sleep(30 * time.Second)
}

func startHelper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "FLEETCHAT_TEST_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}

// Kill must WAIT for confirmed exit rather than returning on signal delivery --
// that difference is the whole fix. A REAL process is used, so the early
// "no process" return cannot be what makes this pass.
func TestKillWaitsForConfirmedExit(t *testing.T) {
	a := &Agent{id: "alice", cmd: startHelper(t), exited: make(chan struct{}), stderrDone: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- a.Kill() }()

	select {
	case <-done:
		t.Fatal("Kill returned before the process was confirmed gone")
	case <-time.After(60 * time.Millisecond):
		// still waiting, as it should be
	}

	if !a.dying.Load() {
		t.Fatal("Kill should mark the agent dying immediately, before waiting")
	}
	close(a.exited) // readLoop's reap finished
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Kill should succeed once exit is confirmed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Kill did not return after exit was confirmed")
	}
}

// A registry Kill must not evict an entry that a REPLACEMENT agent now owns --
// readLoop's own onExit may already have removed the dead one.
func TestRegistryKillDoesNotEvictAReplacement(t *testing.T) {
	r := NewRegistry()
	old := &Agent{id: "alice", cmd: startHelper(t), exited: make(chan struct{}), stderrDone: make(chan struct{})}
	r.agents["alice"] = old

	replacement := &Agent{id: "alice", exited: make(chan struct{}), stderrDone: make(chan struct{})}
	go func() {
		time.Sleep(20 * time.Millisecond)
		r.mu.Lock()
		r.agents["alice"] = replacement // the id was re-taken while the old one died
		r.mu.Unlock()
		close(old.exited)
	}()

	if err := r.Kill("alice"); err != nil {
		t.Fatalf("kill of the old agent should succeed: %v", err)
	}
	r.mu.Lock()
	cur, ok := r.agents["alice"]
	r.mu.Unlock()
	if !ok || cur != replacement {
		t.Fatal("killing the old agent must not delete the replacement that took its id")
	}
}

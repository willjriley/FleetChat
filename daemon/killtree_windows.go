//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

// configureProcessGroup is a no-op on Windows: taskkill /T walks the real
// parent/child relationships, so no group setup is needed for killProcessTree
// to find descendants. The POSIX build genuinely needs its counterpart.
func configureProcessGroup(cmd *exec.Cmd) {}

// killProcessTree terminates pid AND its descendants.
//
// The caller must only invoke this while the process is known unreaped -- see
// the POSIX counterpart for why. Agent.Kill checks the exited channel first.
//
// os.Process.Kill() on Windows is TerminateProcess, which kills only the named
// process -- the CLI's own children (node, tool subprocesses, nested agents)
// survive as orphans, still holding handles and still able to produce output.
// This mirrors what killOtherInstances already does for stray daemon copies:
// taskkill /T is the tree kill.
//
// Best-effort by design. taskkill commonly returns a non-zero exit even when
// it did the job (e.g. a child already gone), so the error is deliberately not
// treated as failure -- Kill's authority on whether the process actually died
// is the confirmed-exit wait, not this call.
func killProcessTree(pid int) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}

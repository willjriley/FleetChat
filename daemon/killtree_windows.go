//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

// killProcessTree terminates pid AND its descendants.
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

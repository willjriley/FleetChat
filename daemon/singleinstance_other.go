//go:build !windows

package main

import (
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// killOtherInstances: non-Windows equivalent of the single-board enforcement.
// Uses pgrep to find other processes running this same binary and kills each
// (SIGKILL after a brief chance to exit is left to the OS); best-effort, since
// the daemon's primary target is Windows.
func killOtherInstances() {
	self := os.Getpid()
	parent := os.Getppid()
	exe, err := os.Executable()
	if err != nil {
		return
	}
	out, err := exec.Command("pgrep", "-f", exe).Output()
	if err != nil {
		return // pgrep missing or no matches
	}
	for _, field := range strings.Fields(string(out)) {
		pid, convErr := strconv.Atoi(field)
		if convErr != nil || pid == self || pid <= 0 {
			continue
		}
		// Don't kill our parent (the old daemon on a "Restart board" re-exec): let
		// it stop its own crew and exit cleanly rather than SIGKILLing it, which
		// would skip its cleanup and orphan its agents. See the Windows twin for
		// the full tree-kill rationale.
		if pid == parent {
			continue
		}
		if p, e := os.FindProcess(pid); e == nil {
			_ = p.Kill()
			log.Printf("[daemon] single-instance: killed prior board instance pid %d", pid)
		}
	}
}

//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
	"strconv"
)

// killOtherInstances enforces the single-board rule: on startup, any OTHER
// running copy of this daemon is killed (with its whole process tree, so the
// old board's agent subprocesses die too instead of orphaning). This makes
// "there is exactly one board, on one port" true by construction -- a fresh
// start always supersedes a stale one, rather than two coexisting (the
// parallel-instances bug). Runs BEFORE the port bind so the old process
// releases the port; main.go's bind-retry absorbs the brief handoff window.
//
// Matches on the daemon's own executable PATH (not just the image name), so an
// unrelated "daemon.exe" elsewhere is never touched.
func killOtherInstances() {
	self := os.Getpid()
	parent := os.Getppid()
	exe, err := os.Executable()
	if err != nil {
		log.Printf("[daemon] single-instance: can't resolve own path (%s) -- skipping prior-instance kill", err)
		return
	}
	// PowerShell: list PIDs of processes running THIS exact binary, excluding self.
	ps := "Get-CimInstance Win32_Process -Filter \"Name='daemon.exe'\" | " +
		"Where-Object { $_.ProcessId -ne " + strconv.Itoa(self) + " -and $_.ExecutablePath -eq '" +
		singleQuoteEscape(exe) + "' } | ForEach-Object { $_.ProcessId }"
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
	if err != nil {
		log.Printf("[daemon] single-instance: enumerating prior instances failed: %s", err)
		return
	}
	for _, field := range splitWhitespace(string(out)) {
		pid, convErr := strconv.Atoi(field)
		if convErr != nil || pid == self || pid <= 0 {
			continue
		}
		// Never kill our PARENT: the tray's "Restart board" re-execs by launching
		// the new daemon as a CHILD of the old one, so the old daemon is this
		// process's parent. taskkill /T is a TREE kill -- killing the parent would
		// take THIS process down with it (we're in its tree), leaving nothing
		// running at all. The old daemon cleanly stops itself (bs.Stop) and os.Exits
		// right after launching us, and our bind-retry absorbs the brief port
		// handoff -- so let it exit rather than tree-killing it into taking us down
		// too. (When the parent ISN'T a daemon -- a shell / fleet-up.bat launch --
		// its PID isn't in this same-exe list anyway, so this is a no-op there.)
		if pid == parent {
			log.Printf("[daemon] single-instance: NOT killing parent pid %d (restart handoff -- it exits on its own)", pid)
			continue
		}
		// /T kills the whole tree (the old daemon AND its agent subprocesses),
		// so nothing is orphaned; /F because a stale daemon won't self-exit.
		// NOTE: taskkill /T commonly returns a NON-ZERO exit (128) even when it
		// successfully kills the target -- verified empirically here: the old
		// daemon and its agents are gone, yet the exit code is 128. So we do
		// NOT treat a non-zero exit as failure. The real backstop is main.go's
		// bind-retry: if a prior instance somehow survived, the new one keeps
		// retrying the port until it frees, so a missed kill can't leave two
		// boards both serving.
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
		log.Printf("[daemon] single-instance: killed prior board instance pid %d (+ its agent tree)", pid)
	}
}

func singleQuoteEscape(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'', '\'') // PowerShell single-quote escape is a doubled quote
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

func splitWhitespace(s string) []string {
	fields := []string{}
	cur := []rune{}
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			if len(cur) > 0 {
				fields = append(fields, string(cur))
				cur = cur[:0]
			}
			continue
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		fields = append(fields, string(cur))
	}
	return fields
}

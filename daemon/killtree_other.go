//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the child in its OWN process group, making it the
// group leader with PGID == its own pid.
//
// This is what makes killProcessTree correct BY DESIGN rather than harmless by
// accident. Without it no group with the child's pid exists, so kill(-pid)
// returns ESRCH and the "tree kill" silently does nothing -- the CLI's children
// survive as orphans, which is the whole failure this is meant to prevent.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree terminates pid's process group -- the child and every
// descendant that stayed in it.
//
// kill(2) with a negative pid targets the group whose PGID equals that pid.
// Because configureProcessGroup made the child a group leader, -pid names
// exactly the child's tree and nothing else. It does NOT name the daemon's own
// group: that group's PGID is the DAEMON's pid, not the child's.
//
// Worth being explicit, because the earlier comment here had it backwards and
// that is the dangerous direction to be wrong in: if -pid ever did name the
// daemon's group, nothing would refuse it. A process may signal its own group
// freely at the same uid, so it would SIGKILL the daemon and every agent it
// manages. Safety here comes from the child being its OWN group leader, not
// from the kernel declining anything.
//
// The caller must only invoke this while the process is known unreaped: once
// cmd.Wait() has returned, the pid is released and immediately reusable on
// POSIX, so signalling it could reach an unrelated process group. Agent.Kill
// checks the exited channel before calling.
func killProcessTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

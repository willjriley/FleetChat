//go:build !windows

package main

import "syscall"

// killProcessTree terminates pid's whole process group where the platform
// supports it.
//
// A negative pid means "the process group" to kill(2). The child is not
// explicitly given its own group here, so in the common case this is the
// daemon's own group and the call is refused or a no-op rather than something
// destructive -- hence best-effort, with Kill's confirmed-exit wait remaining
// the authority on whether the process actually died. Windows is the platform
// this ships on and has the real implementation; this keeps the non-Windows
// build honest instead of silently doing nothing under a name that promises a
// tree kill.
func killProcessTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

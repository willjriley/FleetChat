//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbYesNo       = 0x00000004
	mbIconWarning = 0x00000030
	mbYes         = 6
)

// confirm shows a native Yes/No dialog and returns true ONLY on Yes. It guards
// the destructive tray actions (Quit, Restart board) so an accidental menu
// click can't take down a live board. That accidental click is the actual,
// mundane cause of an earlier shutdown -- the Quit menu item got selected (the
// recovered log shows the Quit handler ran, and it can only run on a real menu
// selection) -- NOT the passive console-close self-quit I had wrongly
// hypothesized before reading the systray library's source.
func confirm(title, text string) bool {
	tp, _ := syscall.UTF16PtrFromString(text)
	tt, _ := syscall.UTF16PtrFromString(title)
	ret, _, _ := procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(tp)), uintptr(unsafe.Pointer(tt)),
		uintptr(mbYesNo|mbIconWarning))
	return int(ret) == mbYes
}

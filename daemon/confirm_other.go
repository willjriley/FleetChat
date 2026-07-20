//go:build !windows

package main

// confirm has no native dialog off Windows (the tray is a Windows-first
// convenience in this daemon); default to proceeding. A non-Windows port that
// wants a guard on the destructive tray actions can wire its own dialog here.
func confirm(title, text string) bool { return true }

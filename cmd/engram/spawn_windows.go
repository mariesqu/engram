//go:build windows

package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachedSysProcAttr returns the SysProcAttr that fully detaches a spawned
// daemon process from its parent on Windows: a new process group with no
// console window, so closing the parent's console (or the parent exiting)
// does not signal or kill the daemon. Mirrors what tray_windows.go's
// auto-launch has always done — factored out here so 'engram connect's
// auto-start (connect.go) uses the exact same detach semantics via the
// shared spawnDetachedDaemon (daemonspawn.go).
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}

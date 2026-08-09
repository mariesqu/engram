//go:build !windows

package main

import "syscall"

// detachedSysProcAttr returns the SysProcAttr that fully detaches a spawned
// daemon process from its parent on non-Windows platforms: Setsid starts the
// daemon in a new session, so it has no controlling terminal and is not sent
// SIGHUP when the parent (e.g. 'engram connect', an MCP stdio bridge) exits
// or its terminal goes away. Used by the shared spawnDetachedDaemon
// (daemonspawn.go); the Windows equivalent lives in spawn_windows.go.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true,
	}
}

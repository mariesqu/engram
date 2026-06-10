package main

// runTrayCmd is defined in tray_windows.go (//go:build windows) and
// tray_other.go (//go:build !windows).
// Both files export runTrayCmd(args []string) error so that main.go can
// dispatch `engram tray` without a build-tag split at the call site.

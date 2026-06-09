//go:build !windows

package controlapi_test

// isWindows returns false on non-Windows platforms.
func isWindows() bool { return false }

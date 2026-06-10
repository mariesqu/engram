//go:build windows

package tray

import "testing"

func TestShouldLaunchDaemon_Healthy_False(t *testing.T) {
	if ShouldLaunchDaemon(ProbeHealthy) {
		t.Error("must not launch when daemon is already healthy")
	}
}

func TestShouldLaunchDaemon_Missing_True(t *testing.T) {
	if !ShouldLaunchDaemon(ProbeMissing) {
		t.Error("must launch when daemon.json is missing")
	}
}

func TestShouldLaunchDaemon_Unreachable_True(t *testing.T) {
	if !ShouldLaunchDaemon(ProbeUnreachable) {
		t.Error("must launch when daemon is unreachable")
	}
}

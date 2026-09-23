//go:build !acceptance

package main

// testmain_test.go — BH-001: hermetic test environment for the default
// (non-acceptance) build. See testenv_test.go's isolateUserEnv for the "why".

import (
	"fmt"
	"os"
	"testing"
)

// TestMain isolates every cmd/engram test in this build from the developer's
// real user config, cache and home directories (and from ambient ENGRAM_*
// env vars) before any test runs, and restores the environment afterwards.
func TestMain(m *testing.M) {
	cleanup, err := isolateUserEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmd/engram: isolateUserEnv: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	cleanup()
	os.Exit(code)
}

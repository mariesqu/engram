package main

import (
	"fmt"
	"runtime"
)

// version is the binary version string. The default value "dev" is used for
// local builds. Release binaries are stamped at link time via:
//
//	-ldflags "-X main.version=vX.Y.Z"
//
// All sites that previously referenced daemonVersion reference this variable
// instead. The Makefile release target and the GitHub Actions release workflow
// inject the value automatically.
// CONTRACT: version must never be EMPTY — probeDaemon treats an empty
// daemon_version in /api/v1/status as "not an engram daemon" and would let a
// second process bind the same SQLite file. The "dev" default and the
// release pipeline (GITHUB_REF_NAME, always non-empty on tag push) both
// satisfy this; never inject -X main.version= with a blank value.
var version = "dev"

// runVersionCmd prints the binary version, GOOS/GOARCH, and the Go runtime
// version to stdout and exits 0.
func runVersionCmd(_ []string) error {
	fmt.Printf("engram %s %s/%s %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return nil
}

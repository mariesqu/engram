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
var version = "dev"

// runVersionCmd prints the binary version, GOOS/GOARCH, and the Go runtime
// version to stdout and exits 0.
func runVersionCmd(_ []string) error {
	fmt.Printf("engram %s %s/%s %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return nil
}

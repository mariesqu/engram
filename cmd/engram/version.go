package main

import (
	"fmt"
	"runtime"
	"strings"
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

// runVersionCmd prints the binary version to stdout and exits 0.
//
// The default output is ONE bare line — "engram <version>" and NOTHING else.
// That shape is a compatibility contract, not a cosmetic choice: integrators
// probe it by running `engram version`, trimming the WHOLE stdout, and matching
// it against an anchored regexp. gentle-ai's is
//
//	^(?:engram\s+)?v?(\d+)\.(\d+)\.(\d+)$
//
// (internal/components/engram/protocol.go). Go's `$` matches end of TEXT, not
// end of line, so a trailing "windows/amd64 go1.26.1" — or the same tokens
// moved to a second line — fails the match just as surely. A failed match is
// SILENT: the probe falls back to its conservative default and the integration
// quietly degrades, with no error anywhere to explain why.
//
// GOOS/GOARCH and the Go runtime version are still available, on a second line,
// behind --verbose (-v).
func runVersionCmd(args []string) error {
	fmt.Printf("engram %s\n", version)
	if versionVerbose(args) {
		fmt.Printf("%s/%s %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	}
	return nil
}

// versionVerbose reports whether args ask for the extended build details.
// Unknown arguments are ignored rather than rejected: `engram version` has
// always accepted anything and must stay usable as a dumb probe.
func versionVerbose(args []string) bool {
	for _, a := range args {
		switch strings.TrimSpace(a) {
		case "--verbose", "-v":
			return true
		}
	}
	return false
}

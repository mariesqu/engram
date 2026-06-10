//go:build !windows

package main

import (
	"errors"
	"flag"
	"fmt"
)

const trayUsage = `Usage: engram tray

The tray subcommand is only supported on Windows.

On other platforms, use 'engram ui' to open the web UI in your default browser:

  engram ui --db <path>

This opens the engram web dashboard at http://127.0.0.1:<port>/ui/ in your
default browser. It requires the resident daemon to be running
(engram daemon --http).
`

// runTrayCmd prints a clear error on non-Windows and returns a non-zero exit.
func runTrayCmd(args []string) error {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), trayUsage) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	return fmt.Errorf("engram tray is only supported on Windows; use 'engram ui' instead")
}

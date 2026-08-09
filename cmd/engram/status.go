package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/mariesqu/engram/internal/controlapi"
)

const statusUsage = `Usage: engram status [--db <path>]

Print the status of the engram resident daemon.

Connects to the running daemon via daemon.json (written next to the DB on
daemon --http start). If no daemon is running, an error is printed and the
command exits non-zero.

Flags:
  --db   Path to the local SQLite database (required; or set ENGRAM_DB)

Output fields:
  central_connected   Configured AND the most recent sync attempt succeeded
  central_configured  Whether central credentials are on file at all
  last_sync           Timestamp and outcome of the most recent sync cycle
  daemon_version      Binary version of the running daemon
`

// runStatusCmd is the entry point for `engram status`.
func runStatusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), statusUsage) }

	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("status takes no positional arguments; unexpected: %v", fs.Args())
	}

	if *db == "" {
		*db = envOr("ENGRAM_DB", "")
	}
	if *db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	client, err := NewControlClient(daemonDir(*db))
	if err != nil {
		return err
	}

	var st controlapi.Status
	if err := client.Get("/api/v1/status", &st); err != nil {
		if errors.Is(err, ErrDaemonNotRunning) {
			fmt.Fprintln(os.Stderr, "engram daemon is not running")
			return err
		}
		return fmt.Errorf("status: %w", err)
	}

	printStatus(st)
	return nil
}

// printStatus formats a Status for human consumption. Labels are left-padded
// to a fixed column (%-20s) so the values line up — central_configured: is
// the longest label, which is why every line was re-padded to match it.
func printStatus(st controlapi.Status) {
	connected := "no"
	if st.CentralConnected {
		connected = "yes"
	}
	configured := "no"
	if st.CentralConfigured {
		configured = "yes"
	}
	// CentralConnected means "configured AND the most recent sync attempt
	// succeeded" (see controlapi.Status doc); CentralConfigured is printed
	// alongside it so "configured but currently failing" reads differently
	// from "never configured".
	fmt.Printf("%-20s%s\n", "central_connected:", connected)
	fmt.Printf("%-20s%s\n", "central_configured:", configured)
	if st.CentralURL != nil && *st.CentralURL != "" {
		fmt.Printf("%-20s%s\n", "central_url:", *st.CentralURL)
	}
	fmt.Printf("%-20s%s\n", "daemon_version:", st.DaemonVersion)

	sr := st.LastSyncResult
	if sr.At != nil {
		fmt.Printf("%-20s%s\n", "last_sync_at:", sr.At.Format("2006-01-02T15:04:05Z07:00"))
	} else {
		fmt.Printf("%-20s%s\n", "last_sync_at:", "(never)")
	}
	if sr.Error != nil && *sr.Error != "" {
		fmt.Printf("%-20s%s\n", "last_sync_error:", *sr.Error)
	} else {
		fmt.Printf("%-20s%s\n", "last_sync_error:", "(none)")
	}
	fmt.Printf("%-20s%d\n", "last_sync_pushed:", sr.Pushed)
	fmt.Printf("%-20s%d\n", "last_sync_pulled:", sr.Pulled)
}

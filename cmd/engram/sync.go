package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

const syncUsage = `Usage: engram sync <subcommand> [flags]

Manage engram autosync operations.

Subcommands:
  now   Trigger an immediate sync cycle (requires central to be configured)

Flags (all subcommands):
  --db   Path to the local SQLite database (required; or set ENGRAM_DB)

Examples:
  engram sync now
`

// runSyncCmd is the entry point for `engram sync`.
func runSyncCmd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(syncUsage)
		return nil
	}

	switch args[0] {
	case "now":
		return runSyncNowCmd(args[1:])
	default:
		return fmt.Errorf("sync: unknown subcommand %q; expected: now", args[0])
	}
}

// runSyncNowCmd implements `engram sync now`.
func runSyncNowCmd(args []string) error {
	fs := flag.NewFlagSet("sync now", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(syncUsage) }
	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sync now takes no positional arguments; unexpected: %v", fs.Args())
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

	var result map[string]any
	if err := client.Post("/api/v1/sync/trigger", nil, &result); err != nil {
		if errors.Is(err, ErrDaemonNotRunning) {
			fmt.Fprintln(os.Stderr, "engram daemon is not running")
			return err
		}
		// 409 conflict means central not configured — the server message is clear.
		return fmt.Errorf("sync now: %w", err)
	}

	fmt.Println("sync triggered")
	return nil
}

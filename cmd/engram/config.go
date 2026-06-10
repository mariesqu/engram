package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

const configUsage = `Usage: engram config <subcommand> [flags]

Manage the engram daemon configuration.

Subcommands:
  get               Print the current daemon configuration (writer key redacted)
  set <key> <val>   Set a configuration value

Runtime-mutable keys (take effect immediately):
  sync_interval     Autosync cadence, e.g. "30s", "2m"
  log_level         Log verbosity: "debug", "info", "warn", "error"

Restart-required keys:
  db_path           Path to the local SQLite database
  http_port         Control API TCP port
  transport         Transport mode: "stdio" or "http"

Keys rejected by PUT /api/v1/config (managed via central connect/disconnect):
  writer_key        Use: engram central connect
  central_url       Use: engram central connect

Flags (all subcommands):
  --db   Path to the local SQLite database (required; or set ENGRAM_DB)

Examples:
  engram config get
  engram config set sync_interval 30s
  engram config set log_level debug
`

// runConfigCmd is the entry point for `engram config`.
func runConfigCmd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(configUsage)
		return nil
	}

	switch args[0] {
	case "get":
		return runConfigGetCmd(args[1:])
	case "set":
		return runConfigSetCmd(args[1:])
	default:
		return fmt.Errorf("config: unknown subcommand %q; expected get or set", args[0])
	}
}

// runConfigGetCmd implements `engram config get`.
func runConfigGetCmd(args []string) error {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(configUsage) }
	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("config get takes no positional arguments; unexpected: %v", fs.Args())
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

	var cfg controlapi.RedactedConfig
	if err := client.Get("/api/v1/config", &cfg); err != nil {
		if errors.Is(err, ErrDaemonNotRunning) {
			fmt.Fprintln(os.Stderr, "engram daemon is not running")
			return err
		}
		return fmt.Errorf("config get: %w", err)
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config get: marshal: %w", err)
	}
	fmt.Printf("%s\n", out)
	return nil
}

// runConfigSetCmd implements `engram config set <key> <value>`.
func runConfigSetCmd(args []string) error {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(configUsage) }
	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if fs.NArg() != 2 {
		return fmt.Errorf("config set requires exactly two arguments: <key> <value> (got %d)", fs.NArg())
	}

	key := fs.Arg(0)
	value := fs.Arg(1)

	if *db == "" {
		*db = envOr("ENGRAM_DB", "")
	}
	if *db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	// Build the single-field patch body.
	// The handler will reject writer_key and central_url with a clear error.
	patch, err := buildConfigPatch(key, value)
	if err != nil {
		return fmt.Errorf("config set: %w", err)
	}

	client, err := NewControlClient(daemonDir(*db))
	if err != nil {
		return err
	}

	var result map[string]bool
	if err := client.Put("/api/v1/config", patch, &result); err != nil {
		if errors.Is(err, ErrDaemonNotRunning) {
			fmt.Fprintln(os.Stderr, "engram daemon is not running")
			return err
		}
		// Print the server's message (may include "rejected" for writer_key etc.)
		return fmt.Errorf("config set: %w", err)
	}

	if result["restart_required"] {
		fmt.Printf("ok: %s = %s (restart required for change to take effect)\n", key, value)
	} else {
		fmt.Printf("ok: %s = %s\n", key, value)
	}
	return nil
}

// buildConfigPatch constructs a single-field patch map for PUT /api/v1/config.
// Using a map[string]any lets us send only the field the user specified,
// which is the RFC-7396 partial-merge-patch semantics the handler expects.
//
// Typed keys are converted before sending: http_port is an integer in the
// ConfigPatch schema, so the string from the command line must be parsed here
// (sending it as a JSON string would fail the server-side unmarshal with 400).
// sync_interval is validated locally for a friendlier error than the server's.
func buildConfigPatch(key, value string) (map[string]any, error) {
	switch key {
	case "http_port":
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("http_port must be an integer, got %q", value)
		}
		return map[string]any{key: n}, nil
	case "sync_interval":
		if _, err := time.ParseDuration(value); err != nil {
			return nil, fmt.Errorf("sync_interval must be a Go duration (e.g. \"30s\"), got %q", value)
		}
	}
	return map[string]any{key: value}, nil
}

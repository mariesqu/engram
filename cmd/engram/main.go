// Package main is the binary entrypoint for the engram central server and
// local daemon.
//
// Subcommands:
//
//	engram serve [--addr <addr>] [--dsn <dsn>]
//	    Run the central cloudserve HTTP server. TLS is terminated upstream;
//	    the server itself listens on plain HTTP.
//
//	engram keys provision [--dsn <dsn>] <writer-id>
//	    Generate a new HMAC key for <writer-id> and store it in the DB.
//
//	engram keys revoke [--dsn <dsn>] <writer-id>
//	    Deactivate the HMAC key for <writer-id>.
//
//	engram daemon [--db <path>] [--central-url <url>] [--writer-id <id>] [--sync-interval <dur>]
//	    Run the local MCP daemon over stdio, backed by a SQLite store. When
//	    --central-url is set, an autosync loop pushes/pulls with the central
//	    server using the ENGRAM_WRITER_KEY env var for HMAC signing.
//
// The serve command and the keys provision/revoke subcommands accept --dsn (or the ENGRAM_DSN environment variable).
// The serve command additionally accepts --addr (or ENGRAM_ADDR; default ":8080").
// The daemon command accepts --db (or ENGRAM_DB) and optionally --central-url (or ENGRAM_CENTRAL_URL).
package main

import (
	"fmt"
	"log"
	"os"
	"strings"
)

const usage = `engram — central sync server and local daemon

Usage:
  engram serve    [--addr <addr>] [--dsn <dsn>]
  engram keys     provision [--dsn <dsn>] <writer-id>
  engram keys     revoke    [--dsn <dsn>] <writer-id>
  engram daemon   [--db <path>] [--central-url <url>] [--writer-id <id>] [--sync-interval <dur>] [--http] [--http-port <port>]
  engram status   [--db <path>]
  engram ui       [--db <path>]
  engram projects list
  engram projects policy <project> <synced|local-only|omitted>

Environment:
  ENGRAM_ADDR            default listen address for 'serve' (default ":8080")
  ENGRAM_DSN             Postgres DSN (required for 'serve' and 'keys provision/revoke')
  ENGRAM_DB              path to local SQLite database (required for 'daemon', 'status', 'ui', 'projects')
  ENGRAM_CENTRAL_URL     central server URL for autosync (optional for 'daemon')
  ENGRAM_WRITER_ID       writer identity for autosync (required when ENGRAM_CENTRAL_URL is set)
  ENGRAM_WRITER_KEY      hex-encoded 32-byte HMAC key (env only; required when ENGRAM_CENTRAL_URL is set)
  ENGRAM_SYNC_INTERVAL   autosync cadence for 'daemon' (default "30s")

Subcommands:
  serve     Run the central HTTP server (plain HTTP — terminate TLS upstream).
  keys      Provision or revoke per-writer HMAC keys.
  daemon    Run the local MCP daemon (stdio MCP by default; use --http for resident control plane).
  status    Print status of the running resident daemon (requires daemon --http).
  ui        Open the web UI in the default browser (requires daemon --http).
  projects  List and manage per-project sync policies (requires daemon --http).

Run 'engram <subcommand> --help' for per-subcommand flags.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is the testable dispatch entry-point. It returns an OS exit code:
//   - 0  success
//   - 1  a subcommand returned an error — runtime OR validation (missing --dsn,
//     missing/extra writer-id, unknown keys subcommand) — logged via log.Printf
//   - 2  top-level usage error (no args, top-level --help, or unknown subcommand)
func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	switch args[0] {
	case "serve":
		if err := runServeCmd(args[1:]); err != nil {
			log.Printf("engram serve: %v", err)
			return 1
		}
		return 0
	case "keys":
		if err := runKeysCmd(args[1:]); err != nil {
			log.Printf("engram keys: %v", err)
			return 1
		}
		return 0
	case "daemon":
		if err := runDaemonCmd(args[1:]); err != nil {
			log.Printf("engram daemon: %v", err)
			return 1
		}
		return 0
	case "status":
		if err := runStatusCmd(args[1:]); err != nil {
			log.Printf("engram status: %v", err)
			return 1
		}
		return 0
	case "ui":
		if err := runUICmd(args[1:]); err != nil {
			log.Printf("engram ui: %v", err)
			return 1
		}
		return 0
	case "projects":
		if err := runProjectsCmd(args[1:]); err != nil {
			log.Printf("engram projects: %v", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "engram: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// envOr returns the value of the environment variable named key, or def if
// the variable is unset or empty.
func envOr(key, def string) string {
	// TrimSpace: env vars injected from files/CI commonly carry a trailing newline,
	// and leading/trailing whitespace is never meaningful for our config values
	// (paths, URLs, DSNs, addrs, durations, writer-ids).
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

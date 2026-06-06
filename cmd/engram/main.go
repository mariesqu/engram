// Package main is the binary entrypoint for the engram central server.
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
// The serve command and the keys provision/revoke subcommands accept --dsn (or the ENGRAM_DSN environment variable).
// The serve command additionally accepts --addr (or ENGRAM_ADDR; default ":8080").
package main

import (
	"fmt"
	"log"
	"os"
)

const usage = `engram — central sync server

Usage:
  engram serve [--addr <addr>] [--dsn <dsn>]
  engram keys provision [--dsn <dsn>] <writer-id>
  engram keys revoke   [--dsn <dsn>] <writer-id>

Environment:
  ENGRAM_ADDR  default listen address for 'serve' (default ":8080")
  ENGRAM_DSN   Postgres DSN (required for all subcommands)

Subcommands:
  serve   Run the central HTTP server (plain HTTP — terminate TLS upstream).
  keys    Provision or revoke per-writer HMAC keys.

Run 'engram serve --help' or 'engram keys --help' for subcommand flags.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is the testable dispatch entry-point. It returns an OS exit code:
//   - 0  success
//   - 1  a subcommand returned an error — runtime OR validation (missing --dsn,
//        missing/extra writer-id, unknown keys subcommand) — logged via log.Printf
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
	default:
		fmt.Fprintf(os.Stderr, "engram: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// envOr returns the value of the environment variable named key, or def if
// the variable is unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mariesqu/engram/internal/controlapi"
)

const centralUsage = `Usage: engram central <subcommand> [flags]

Connect or disconnect the resident daemon from a central sync server.

Subcommands:
  connect     Connect to a central server (seals the writer key, starts autosync)
  disconnect  Disconnect from central (clears credentials; local data is retained)

Flags (connect):
  --url          Central server URL, e.g. http://localhost:8080 (required)
  --writer-id    Writer identity sent to the central server (required)
  --db           Path to the local SQLite database (required; or set ENGRAM_DB)

Flags (disconnect):
  --db           Path to the local SQLite database (required; or set ENGRAM_DB)

The writer key is NEVER passed as a flag (shell history would leak it). Supply
it via one of:
  ENGRAM_WRITER_KEY   hex-encoded 32-byte HMAC key (env var)
  stdin               piped input, or an interactive prompt when stdin is a terminal

Examples:
  ENGRAM_WRITER_KEY=$(cat key.hex) engram central connect --url http://localhost:8080 --writer-id my-writer
  echo "$WRITER_KEY_HEX" | engram central connect --url http://localhost:8080 --writer-id my-writer
  engram central disconnect
`

// runCentralCmd is the entry point for `engram central`.
func runCentralCmd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(centralUsage)
		return nil
	}

	switch args[0] {
	case "connect":
		return runCentralConnectCmd(args[1:])
	case "disconnect":
		return runCentralDisconnectCmd(args[1:])
	default:
		return fmt.Errorf("central: unknown subcommand %q; expected connect or disconnect", args[0])
	}
}

// runCentralConnectCmd implements `engram central connect`.
func runCentralConnectCmd(args []string) error {
	return runCentralConnectCmdWithIO(args, os.Stdin, isTerminal(os.Stdin))
}

// runCentralConnectCmdWithIO is the testable core of `engram central connect`.
// stdin and terminal are injected so tests can simulate piped input (terminal
// = false) without touching the real os.Stdin.
func runCentralConnectCmdWithIO(args []string, stdin io.Reader, terminal bool) error {
	fs := flag.NewFlagSet("central connect", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), centralUsage) }

	url := fs.String("url", "", "central server URL (required)")
	writerID := fs.String("writer-id", "", "writer identity sent to the central server (required)")
	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("central connect takes no positional arguments; unexpected: %v", fs.Args())
	}

	if *url == "" {
		return fmt.Errorf("--url is required")
	}
	if *writerID == "" {
		return fmt.Errorf("--writer-id is required")
	}

	if *db == "" {
		*db = envOr("ENGRAM_DB", "")
	}
	if *db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	writerKey, err := readWriterKey(stdin, terminal, os.Stderr)
	if err != nil {
		return err
	}

	client, err := NewControlClient(daemonDir(*db))
	if err != nil {
		return err
	}

	req := controlapi.ConnectRequest{
		CentralURL: *url,
		WriterID:   *writerID,
		WriterKey:  writerKey,
	}

	var st controlapi.Status
	if err := client.Post("/api/v1/central/connect", req, &st); err != nil {
		if errors.Is(err, ErrDaemonNotRunning) {
			fmt.Fprintln(os.Stderr, "engram daemon is not running")
			return err
		}
		return fmt.Errorf("central connect: %w", err)
	}

	fmt.Printf("connected to central: %s\n", *url)
	return nil
}

// runCentralDisconnectCmd implements `engram central disconnect`.
func runCentralDisconnectCmd(args []string) error {
	fs := flag.NewFlagSet("central disconnect", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), centralUsage) }

	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("central disconnect takes no positional arguments; unexpected: %v", fs.Args())
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
	if err := client.Post("/api/v1/central/disconnect", nil, &st); err != nil {
		if errors.Is(err, ErrDaemonNotRunning) {
			fmt.Fprintln(os.Stderr, "engram daemon is not running")
			return err
		}
		return fmt.Errorf("central disconnect: %w", err)
	}

	fmt.Println("disconnected from central (local data retained)")
	return nil
}

// readWriterKey resolves the writer key without ever accepting it as a CLI
// flag (shell history leak). Precedence:
//  1. ENGRAM_WRITER_KEY env var.
//  2. stdin — a piped line when stdin is not a terminal, or an interactive
//     prompt (input echoes; no golang.org/x/term dependency in this module)
//     when stdin is a terminal.
//
// stdin is the reader to consume the key line from (os.Stdin in production,
// a fake reader in tests). terminal indicates whether stdin is an interactive
// terminal — when true, a prompt is printed to prompt before reading. prompt
// is os.Stderr in production; injected so tests can assert the prompt never
// reaches stdout (where it could be captured alongside piped data).
//
// Returns an error if the resolved key is empty after trimming whitespace.
func readWriterKey(stdin io.Reader, terminal bool, prompt io.Writer) (string, error) {
	if v := strings.TrimSpace(os.Getenv("ENGRAM_WRITER_KEY")); v != "" {
		return v, nil
	}

	if terminal {
		fmt.Fprint(prompt, "Writer key (hex): ")
	}
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("writer key is required (set ENGRAM_WRITER_KEY or pipe it via stdin)")
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", fmt.Errorf("writer key is required (set ENGRAM_WRITER_KEY or pipe it via stdin)")
	}
	return key, nil
}

// isTerminal reports whether f is connected to an interactive terminal.
// A best-effort check using os.ModeCharDevice — sufficient to decide whether
// to print a prompt; not a security boundary.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

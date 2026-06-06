package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/wireauth"
)

const keysUsage = `Usage: engram keys <subcommand> [flags]

Manage per-writer HMAC keys used for request authentication.

Subcommands:
  provision [--dsn <dsn>] <writer-id>
      Generate a new HMAC key for <writer-id> and store it in the DB.
      The key is printed ONCE to stdout — store it securely.

  revoke [--dsn <dsn>] <writer-id>
      Deactivate the HMAC key for <writer-id>. The key is preserved in the
      DB for audit purposes but will no longer authenticate requests. Use
      'provision' to issue a new key for the same writer-id.

Flags:
  --dsn   Postgres DSN (default: ENGRAM_DSN env; REQUIRED)
`

// runKeysCmd parses the keys subcommand and dispatches to provision or revoke.
func runKeysCmd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, keysUsage)
		return nil
	}

	switch args[0] {
	case "provision":
		return runProvisionCmd(args[1:])
	case "revoke":
		return runRevokeCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "engram keys: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, keysUsage)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// runProvisionCmd parses flags and delegates to provisionKey.
func runProvisionCmd(args []string) error {
	fs := flag.NewFlagSet("keys provision", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: engram keys provision [--dsn <dsn>] <writer-id>")
		fs.PrintDefaults()
	}
	// --dsn defaults to EMPTY (not envOr): a Postgres DSN carries credentials and
	// PrintDefaults prints the flag's default value, so baking ENGRAM_DSN into the
	// default would leak the secret via --help. ENGRAM_DSN is resolved after Parse.
	dsn := fs.String("dsn", "", "Postgres DSN (required; or set ENGRAM_DSN)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // --help printed usage; successful early-exit (exit 0)
		}
		return err
	}
	if *dsn == "" {
		*dsn = envOr("ENGRAM_DSN", "") // resolve env AFTER parse so the secret never enters the flag default
	}
	if *dsn == "" {
		return fmt.Errorf("--dsn is required (or set ENGRAM_DSN)")
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("exactly one writer-id is required: engram keys provision [--dsn <dsn>] <writer-id>")
	}
	writerID := fs.Arg(0)

	ctx := context.Background()
	key, err := provisionKey(ctx, *dsn, writerID)
	if err != nil {
		return err
	}

	fmt.Printf("provisioned writer %q\n", writerID)
	fmt.Printf("key (hex): %s\n", hex.EncodeToString(key))
	fmt.Println("WARNING: this is the writer's HMAC secret — shown ONCE.")
	fmt.Println("Store it securely and configure the node's client with it.")
	return nil
}

// runRevokeCmd parses flags and delegates to revokeKey.
func runRevokeCmd(args []string) error {
	fs := flag.NewFlagSet("keys revoke", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: engram keys revoke [--dsn <dsn>] <writer-id>")
		fs.PrintDefaults()
	}
	// --dsn defaults to EMPTY (not envOr): a Postgres DSN carries credentials and
	// PrintDefaults prints the flag's default value, so baking ENGRAM_DSN into the
	// default would leak the secret via --help. ENGRAM_DSN is resolved after Parse.
	dsn := fs.String("dsn", "", "Postgres DSN (required; or set ENGRAM_DSN)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // --help printed usage; successful early-exit (exit 0)
		}
		return err
	}
	if *dsn == "" {
		*dsn = envOr("ENGRAM_DSN", "") // resolve env AFTER parse so the secret never enters the flag default
	}
	if *dsn == "" {
		return fmt.Errorf("--dsn is required (or set ENGRAM_DSN)")
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("exactly one writer-id is required: engram keys revoke [--dsn <dsn>] <writer-id>")
	}
	writerID := fs.Arg(0)

	ctx := context.Background()
	if err := revokeKey(ctx, *dsn, writerID); err != nil {
		return err
	}

	fmt.Printf("revoked writer %q\n", writerID)
	return nil
}

// provisionKey generates a new HMAC key via wireauth.NewKey, stores it for
// writerID via UpsertWriterKey, and returns the raw key bytes. The caller is
// responsible for printing the key to the operator and issuing the security
// warning — it is shown exactly once.
func provisionKey(ctx context.Context, dsn, writerID string) ([]byte, error) {
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	key, err := wireauth.NewKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := store.UpsertWriterKey(ctx, writerID, key); err != nil {
		return nil, fmt.Errorf("store key for %q: %w", writerID, err)
	}
	return key, nil
}

// revokeKey deactivates the HMAC key for writerID. If writerID was never
// provisioned, it returns a clear error (ErrWriterKeyNotFound).
func revokeKey(ctx context.Context, dsn, writerID string) error {
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	if err := store.DeactivateWriterKey(ctx, writerID); err != nil {
		if errors.Is(err, centralstore.ErrWriterKeyNotFound) {
			// Wrap with %w so callers can errors.Is(err, ErrWriterKeyNotFound).
			return fmt.Errorf("writer %q was never provisioned (no active key found): %w", writerID, err)
		}
		return fmt.Errorf("deactivate key for %q: %w", writerID, err)
	}
	return nil
}

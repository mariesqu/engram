package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/wireauth"
)

const keysUsage = `Usage: engram keys <subcommand>

Manage per-writer HMAC keys used for request authentication.

Subcommands:
  provision [--dsn <dsn>] <writer-id>
      Generate a new HMAC key for <writer-id> and store it in the DB.
      The key is printed ONCE to stdout — store it securely.

  revoke [--dsn <dsn>] <writer-id>
      Deactivate the HMAC key for <writer-id>. The key is preserved in the
      DB for audit purposes but will no longer authenticate requests. Use
      'provision' to issue a new key for the same writer-id.

  purge [--dsn <dsn>] <writer-id>
      Bump the writer's purge_epoch by 1. Every daemon sharing that writer_id
      will purge its local SYNCED data and re-pull from central on its next
      startup (or next /v1/state check). Local-only and omitted projects are
      never touched. Machines sharing a writer_id purge together — the epoch
      is per-writer, not per-machine.
      NOTE: purging never fires on a lower or equal epoch, so a central
      restored from an older backup silently disables purge for nodes that
      already honored a higher epoch — bump past their highest honored value
      to re-enable it.

  purge --all [--except <id>[,<id>...]] [--dsn <dsn>]
      Bump purge_epoch for EVERY active writer except any listed in --except.
      Cannot be combined with a positional writer-id.

Run 'engram keys provision --help', 'engram keys revoke --help', or
'engram keys purge --help' for flags.
`

// runKeysCmd parses the keys subcommand and dispatches to provision, revoke,
// or purge.
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
	case "purge":
		return runPurgeCmd(args[1:])
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

// runPurgeCmd parses flags and delegates to purgeWriter or purgeAllWriters
// depending on whether --all was passed.
func runPurgeCmd(args []string) error {
	fs := flag.NewFlagSet("keys purge", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: engram keys purge [--dsn <dsn>] <writer-id>")
		fmt.Fprintln(os.Stderr, "       engram keys purge --all [--except <id>[,<id>...]] [--dsn <dsn>]")
		fs.PrintDefaults()
	}
	// --dsn defaults to EMPTY (not envOr): a Postgres DSN carries credentials and
	// PrintDefaults prints the flag's default value, so baking ENGRAM_DSN into the
	// default would leak the secret via --help. ENGRAM_DSN is resolved after Parse.
	dsn := fs.String("dsn", "", "Postgres DSN (required; or set ENGRAM_DSN)")
	all := fs.Bool("all", false, "bump purge_epoch for every active writer")
	except := fs.String("except", "", "comma-separated writer-ids to exclude when --all is set")
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

	ctx := context.Background()

	if *all {
		// Refuse --all combined with a positional writer-id — the two modes are
		// mutually exclusive and combining them is almost certainly a mistake
		// (e.g. the caller meant --except but typed a bare positional arg).
		if fs.NArg() != 0 {
			return fmt.Errorf("--all does not take a positional writer-id (did you mean --except %s?)", strings.Join(fs.Args(), ","))
		}
		var exceptList []string
		if strings.TrimSpace(*except) != "" {
			for _, id := range strings.Split(*except, ",") {
				id = strings.TrimSpace(id)
				if id != "" {
					exceptList = append(exceptList, id)
				}
			}
		}

		n, err := purgeAllWriters(ctx, *dsn, exceptList)
		if err != nil {
			return err
		}
		fmt.Printf("bumped purge_epoch for %d writer(s)\n", n)
		if len(exceptList) > 0 {
			fmt.Printf("excluded: %s\n", strings.Join(exceptList, ", "))
		}
		return nil
	}

	if fs.NArg() != 1 {
		return fmt.Errorf("exactly one writer-id is required: engram keys purge [--dsn <dsn>] <writer-id> (or use --all)")
	}
	writerID := fs.Arg(0)

	epoch, err := purgeWriter(ctx, *dsn, writerID)
	if err != nil {
		return err
	}
	fmt.Printf("bumped purge_epoch for writer %q → %d\n", writerID, epoch)
	fmt.Println("every daemon sharing this writer_id will purge its local synced data and re-pull from central on its next startup")
	return nil
}

// purgeWriter bumps writerID's purge_epoch by 1 and returns the new value.
func purgeWriter(ctx context.Context, dsn, writerID string) (int64, error) {
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	epoch, err := store.BumpPurgeEpoch(ctx, writerID)
	if err != nil {
		if errors.Is(err, centralstore.ErrWriterKeyNotFound) {
			return 0, fmt.Errorf("writer %q was never provisioned (no active key found): %w", writerID, err)
		}
		return 0, fmt.Errorf("bump purge epoch for %q: %w", writerID, err)
	}
	return epoch, nil
}

// purgeAllWriters bumps purge_epoch for every active writer except those in
// except, and returns the number of writers affected.
func purgeAllWriters(ctx context.Context, dsn string, except []string) (int, error) {
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	n, err := store.BumpAllPurgeEpochs(ctx, except)
	if err != nil {
		return 0, fmt.Errorf("bump all purge epochs: %w", err)
	}
	return n, nil
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

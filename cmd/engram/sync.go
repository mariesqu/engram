package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/mariesqu/engram/internal/localstore"
)

const syncUsage = `Usage: engram sync <subcommand> [flags]

Manage engram autosync operations.

Subcommands:
  now              Trigger an immediate sync cycle (requires central to be configured)
  parked           List outbox entries central has permanently rejected (see FUP-004)
  retry <seq|all>  Un-park one entry by local_seq, or every parked entry, resetting its attempts
  discard <seq>    Permanently discard one parked entry — it will never be pushed to central

Flags (all subcommands):
  --db   Path to the local SQLite database (required; or set ENGRAM_DB)

Examples:
  engram sync now
  engram sync parked
  engram sync retry 42
  engram sync retry all
  engram sync discard 42
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
	case "parked":
		return runSyncParkedCmd(args[1:])
	case "retry":
		return runSyncRetryCmd(args[1:])
	case "discard":
		return runSyncDiscardCmd(args[1:])
	default:
		return fmt.Errorf("sync: unknown subcommand %q; expected: now, parked, retry, discard", args[0])
	}
}

// resolveSyncDBFlag resolves the --db flag the SAME way runSyncNowCmd already
// does (flag, else ENGRAM_DB, else error) — the parked/retry/discard
// subcommands below share it so all four `sync` subcommands agree on how a
// database path is found.
func resolveSyncDBFlag(db string) (string, error) {
	if db == "" {
		db = envOr("ENGRAM_DB", "")
	}
	if db == "" {
		return "", fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}
	return db, nil
}

// openSyncStore opens the local store directly for parked/retry/discard, the
// same pattern `engram projects consolidate` already uses: these subcommands
// read and write sync_mutations rows the running daemon also touches every
// push cycle, and SQLite's WAL mode (schema.go's ApplySchema) is exactly what
// makes a second process — this CLI invocation — safe to open the SAME file
// concurrently, whether or not the daemon happens to be running right now.
func openSyncStore(db string) (*localstore.Store, error) {
	store, err := localstore.Open(db)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return store, nil
}

// runSyncParkedCmd implements `engram sync parked`: lists every outbox entry
// central has permanently rejected (FUP-004b), one line per entry, in the
// exact fields mem_doctor's parked_mutations check reports — local_seq,
// project, entity, attempts, blocked_behind (later writes to the same memory
// withheld behind it), and last_error — so an operator can go straight
// from either surface to `engram sync retry`/`discard`.
func runSyncParkedCmd(args []string) error {
	fs := flag.NewFlagSet("sync parked", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(syncUsage) }
	dbFlag := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sync parked takes no positional arguments; unexpected: %v", fs.Args())
	}

	db, err := resolveSyncDBFlag(*dbFlag)
	if err != nil {
		return fmt.Errorf("sync parked: %w", err)
	}
	store, err := openSyncStore(db)
	if err != nil {
		return fmt.Errorf("sync parked: %w", err)
	}
	defer store.Close()

	parked, err := store.ListParked()
	if err != nil {
		return fmt.Errorf("sync parked: %w", err)
	}
	if len(parked) == 0 {
		fmt.Println("no parked mutations")
		return nil
	}
	for _, p := range parked {
		idPrefix := p.MutationID
		if len(idPrefix) > mutationIDPrefixLen {
			idPrefix = idPrefix[:mutationIDPrefixLen]
		}
		fmt.Printf("local_seq=%d  mutation_id=%s…  project=%q  entity=%s  attempts=%d  blocked_behind=%d\n  last_error: %s\n",
			p.LocalSeq, idPrefix, p.Project, p.Entity, p.Attempts, p.BlockedBehind, p.LastError)
	}
	return nil
}

// mutationIDPrefixLen mirrors internal/diagnostic's constant of the same
// name and purpose: how much of a mutation_id this CLI ever prints.
const mutationIDPrefixLen = 12

// runSyncRetryCmd implements `engram sync retry <seq|all>`: un-parks one
// entry (by local_seq) or every currently-parked entry, resetting attempts to
// 0 so the syncer's next push cycle tries it again from a clean slate.
func runSyncRetryCmd(args []string) error {
	fs := flag.NewFlagSet("sync retry", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(syncUsage) }
	dbFlag := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// Two-pass parse (mirrors `engram projects consolidate`): the first pass
	// takes flags BEFORE the positional local_seq/"all" argument, the second
	// takes any that follow it — so `sync retry 42 --db x` and
	// `sync retry --db x 42` both work.
	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("sync retry requires exactly one argument: <local_seq|all>")
	}
	target := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sync retry takes exactly one argument: <local_seq|all>; unexpected: %v", fs.Args())
	}

	db, err := resolveSyncDBFlag(*dbFlag)
	if err != nil {
		return fmt.Errorf("sync retry: %w", err)
	}
	store, err := openSyncStore(db)
	if err != nil {
		return fmt.Errorf("sync retry: %w", err)
	}
	defer store.Close()

	if target == "all" {
		parked, err := store.ListParked()
		if err != nil {
			return fmt.Errorf("sync retry: %w", err)
		}
		if len(parked) == 0 {
			fmt.Println("no parked mutations to retry")
			return nil
		}
		retried := 0
		for _, p := range parked {
			if err := store.UnparkMutation(p.LocalSeq); err != nil {
				return fmt.Errorf("sync retry: local_seq=%d: %w", p.LocalSeq, err)
			}
			retried++
		}
		fmt.Printf("retried %d mutation(s)\n", retried)
		return nil
	}

	seq, err := strconv.ParseInt(target, 10, 64)
	if err != nil {
		return fmt.Errorf("sync retry: %q is not a local_seq or \"all\"", target)
	}
	if err := store.UnparkMutation(seq); err != nil {
		return fmt.Errorf("sync retry: %w", err)
	}
	fmt.Printf("retried local_seq=%d\n", seq)
	return nil
}

// runSyncDiscardCmd implements `engram sync discard <seq>`: permanently
// discards ONE parked entry (see Store.DiscardMutation's doc comment for why
// "discard" means acked-in-place, not deleted). Requires an explicit
// local_seq — no "discard all", so a single mistyped command can never
// silently drop an entire backlog.
func runSyncDiscardCmd(args []string) error {
	fs := flag.NewFlagSet("sync discard", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(syncUsage) }
	dbFlag := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// Two-pass parse — see runSyncRetryCmd's identical comment.
	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("sync discard requires exactly one argument: <local_seq>")
	}
	seqArg := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sync discard takes exactly one argument: <local_seq>; unexpected: %v", fs.Args())
	}
	seq, err := strconv.ParseInt(seqArg, 10, 64)
	if err != nil {
		return fmt.Errorf("sync discard: %q is not a local_seq", seqArg)
	}

	db, err := resolveSyncDBFlag(*dbFlag)
	if err != nil {
		return fmt.Errorf("sync discard: %w", err)
	}
	store, err := openSyncStore(db)
	if err != nil {
		return fmt.Errorf("sync discard: %w", err)
	}
	defer store.Close()

	// Read it back BEFORE discarding so "prints what it discarded" names the
	// actual mutation, not just the seq the operator typed.
	parked, err := store.ListParked()
	if err != nil {
		return fmt.Errorf("sync discard: %w", err)
	}
	var target *localstore.ParkedEntry
	for i := range parked {
		if parked[i].LocalSeq == seq {
			target = &parked[i]
			break
		}
	}

	if err := store.DiscardMutation(seq); err != nil {
		return fmt.Errorf("sync discard: %w", err)
	}
	if target != nil {
		idPrefix := target.MutationID
		if len(idPrefix) > mutationIDPrefixLen {
			idPrefix = idPrefix[:mutationIDPrefixLen]
		}
		fmt.Printf("discarded local_seq=%d  mutation_id=%s…  project=%q  entity=%s\n",
			target.LocalSeq, idPrefix, target.Project, target.Entity)
		return nil
	}
	fmt.Printf("discarded local_seq=%d\n", seq)
	return nil
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

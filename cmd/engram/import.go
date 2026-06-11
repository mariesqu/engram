package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mariesqu/engram/internal/importer"
	"github.com/mariesqu/engram/internal/localstore"
)

const importUsage = `Usage: engram import [--from <old-db>] [--db <dest-db>] [--dry-run]

Import memories, prompts, and sessions from an old-generation engram database
(~/.engram/engram.db) into the current-generation local store.

The operation is idempotent: re-running the import produces zero new writes —
rows already present in the destination are skipped.  Soft-deleted rows from
the source are always skipped.  Rows belonging to a project whose policy is
"omitted" in the destination are warned and skipped.

Imported rows carry NULL embeddings. The embedding backfill loop embeds them
automatically on the next daemon start.

Flags:
  --from      Path to the old-generation source database.
              Default: ~/.engram/engram.db (if it exists).
  --db        Path to the current-generation destination database.
              Required; or set ENGRAM_DB.
  --dry-run   Count what WOULD be imported without writing anything.

Output:
  A summary table showing sessions, memories, and prompts imported, skipped
  (already present), skipped-deleted, and skipped-omitted.
`

// runImportCmd is the entry point for 'engram import'.
func runImportCmd(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), importUsage) }

	fromFlag := fs.String("from", "", "path to old-generation source database")
	dbFlag := fs.String("db", "", "path to destination database (required; or set ENGRAM_DB)")
	dryRun := fs.Bool("dry-run", false, "count what would be imported without writing")
	writerIDFlag := fs.String("writer-id", "", "writer identity stamped on imported records (default: ENGRAM_WRITER_ID, else \"import\")")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("import takes no positional arguments; unexpected: %v", fs.Args())
	}

	// ── Resolve --from ────────────────────────────────────────────────────────
	from := *fromFlag
	if from == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			candidate := filepath.Join(home, ".engram", "engram.db")
			if _, statErr := os.Stat(candidate); statErr == nil {
				from = candidate
			}
		}
	}
	if from == "" {
		return fmt.Errorf("--from is required (no default ~/.engram/engram.db found)")
	}

	// ── Resolve --db ──────────────────────────────────────────────────────────
	db := *dbFlag
	if db == "" {
		db = envOr("ENGRAM_DB", "")
	}
	if db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	// ── Safety: refuse same-file import ───────────────────────────────────────
	fromAbs, err := filepath.Abs(from)
	if err != nil {
		return fmt.Errorf("resolve --from path: %w", err)
	}
	dbAbs, err := filepath.Abs(db)
	if err != nil {
		return fmt.Errorf("resolve --db path: %w", err)
	}
	// Same-file refusal must survive path aliasing — case differences on
	// Windows/macOS and symlinks both defeat a string compare. os.SameFile
	// compares the underlying file identity (inode / volume+file index). A
	// missing destination is the normal fresh-import case: different files.
	if fromAbs == dbAbs {
		return fmt.Errorf("--from and --db point to the same file (%s); refusing to import into itself", fromAbs)
	}
	if fi1, err1 := os.Stat(fromAbs); err1 == nil {
		if fi2, err2 := os.Stat(dbAbs); err2 == nil && os.SameFile(fi1, fi2) {
			return fmt.Errorf("--from and --db resolve to the same file (%s); refusing to import into itself", fromAbs)
		}
	}

	// ── Open source DB (read-only) ────────────────────────────────────────────
	srcDB, srcCleanup, err := importer.OpenSourceReadOnly(fromAbs)
	if err != nil {
		return fmt.Errorf("open source database: %w", err)
	}
	// LIFO: Close (registered last) runs FIRST, then the snapshot-dir removal —
	// required on Windows, where removing a still-open file fails.
	defer srcCleanup()
	defer srcDB.Close()

	// ── Open destination store ────────────────────────────────────────────────
	// Dry-run must not CREATE the destination: localstore.Open applies schema
	// and migrations unconditionally. When the destination does not exist yet,
	// a dry-run runs against a throwaway store (accurate counts: nothing
	// exists, everything "would import") that is removed afterwards. An
	// EXISTING destination is opened normally — its pending migrations would
	// run on the next daemon start anyway, and the exists-checks need it.
	openPath := dbAbs
	if *dryRun {
		if _, statErr := os.Stat(dbAbs); os.IsNotExist(statErr) {
			tmpDir, tmpErr := os.MkdirTemp("", "engram-import-dryrun-*")
			if tmpErr != nil {
				return fmt.Errorf("dry-run scratch dir: %w", tmpErr)
			}
			defer os.RemoveAll(tmpDir)
			openPath = filepath.Join(tmpDir, "scratch.db")
		}
	}
	dst, err := localstore.Open(openPath)
	if err != nil {
		return fmt.Errorf("open destination database: %w", err)
	}
	defer dst.Close()

	// ── Run import ────────────────────────────────────────────────────────────
	mode := "importing"
	if *dryRun {
		mode = "dry-run"
	}
	fmt.Fprintf(os.Stderr, "engram import: %s from %s → %s\n", mode, fromAbs, dbAbs)

	// Writer identity: imported mutations enter the outbox and will be pushed
	// to central when sync is configured — central authenticates the per-writer
	// HMAC by writer_id, so the import must stamp THIS node's identity, not a
	// hardcoded marker no key is provisioned for. Falls back to "import" for
	// purely local stores.
	writerID := *writerIDFlag
	if writerID == "" {
		writerID = envOr("ENGRAM_WRITER_ID", "import")
	}
	imp := importer.New(dst, writerID)
	st, err := imp.Run(srcDB, *dryRun)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	// ── Print summary table ───────────────────────────────────────────────────
	printImportSummary(st, *dryRun)
	return nil
}

// printImportSummary renders the import stats as a human-readable table.
func printImportSummary(st importer.Stats, dryRun bool) {
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("\n%simport summary\n", prefix)
	fmt.Printf("%-20s %8s %8s %8s %8s\n", "table", "imported", "skipped", "deleted", "omitted")
	fmt.Printf("%-20s %8s %8s %8s %8s\n",
		"--------------------", "--------", "--------", "--------", "--------")
	fmt.Printf("%-20s %8d %8d %8s %8s\n", "sessions",
		st.SessionsImported, st.SessionsSkipped, "-", "-")
	fmt.Printf("%-20s %8d %8d %8d %8d\n", "memories",
		st.MemoriesImported, st.MemoriesSkipped, st.MemoriesDeleted, st.MemoriesOmitted)
	fmt.Printf("%-20s %8d %8d %8s %8d\n", "prompts",
		st.PromptsImported, st.PromptsSkipped, "-", st.PromptsOmitted)
	fmt.Println()
}

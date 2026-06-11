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
	if fromAbs == dbAbs {
		return fmt.Errorf("--from and --db point to the same file (%s); refusing to import into itself", fromAbs)
	}

	// ── Open source DB (read-only) ────────────────────────────────────────────
	srcDB, err := importer.OpenSourceReadOnly(fromAbs)
	if err != nil {
		return fmt.Errorf("open source database: %w", err)
	}
	defer srcDB.Close()

	// ── Open destination store ────────────────────────────────────────────────
	dst, err := localstore.Open(dbAbs)
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

	imp := importer.New(dst, "import")
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

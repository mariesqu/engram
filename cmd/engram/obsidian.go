package main

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/obsidian"
)

const obsidianExportUsage = `Usage: engram obsidian-export --vault <path> --db <path> [--project <name>] [--limit <n>] [--since <time>] [--force] [--graph-config <mode>]

Export engram observations into an Obsidian-compatible markdown vault
(REQ-EXPORT-01..11). Each observation becomes a note under
"{vault}/engram/{project}/{type}/{slug}-{id}.md"; session and eligible topic
hub notes are written under "{vault}/engram/_sessions/" and
"{vault}/engram/_topics/". Re-running the command is incremental — only
observations created/updated since the last run (persisted in
"{vault}/engram/.engram-sync-state.json") are (re)written, and observations
no longer live have their files removed.

Flags:
  --vault    Path to the Obsidian vault root. Required.
  --db       Path to the local SQLite database. Required; or set ENGRAM_DB.
  --project  Limit the export to a single project. Default: every project.
  --limit    Per-project cap on exported observations. Default: no cap.
  --since    Only export observations updated after this time. Accepts
             RFC3339 (2006-01-02T15:04:05Z07:00) or a bare date
             (2006-01-02). Overrides the state file's cutoff for this run.
  --force    Ignore the incremental cutoff and re-evaluate every live
             observation. Unchanged files are still skipped (idempotency
             is a content check, not a timestamp check). Also the recovery
             lever for a damaged vault: if the state file cannot be parsed,
             --force discards it and rebuilds from an empty state, whereas
             a normal run fails.
  --graph-config
             Bootstrap "{vault}/.obsidian/graph.json" with engram's curated
             graph view. One of:
               preserve  write it only when absent (default; never clobbers
                         your own graph settings)
               force     overwrite it unconditionally
               skip      do not touch "{vault}/.obsidian/" at all
             Obsidian may need a restart to pick up an externally-written
             graph config.

Output:
  A single summary line on success:
    sync: created=<n> updated=<n> deleted=<n> skipped=<n> hubs=<n>

Running this on a schedule — DON'T:
  This command opens the SQLite database DIRECTLY and read-write: opening the
  store runs the schema + migrations on every invocation, so a version-
  mismatched engram binary fired by cron / Task Scheduler / launchd can migrate
  the database out from under a running "engram daemon". Run this command by
  hand for a one-off export.

  To keep a vault continuously fresh, use the daemon instead — it exports on a
  schedule from the store it ALREADY has open, so there is never a second
  opener:
      engram config set obsidian_vault <path-to-vault>
  then restart the daemon. Related keys: obsidian_interval (default 10m, floor
  1m), obsidian_project, obsidian_graph_config. All four are restart-required.
`

// newObsidianExporter is an injectable seam for tests (matches the pattern
// used by other package-level constructor vars in this package).
var newObsidianExporter = obsidian.NewExporter

// runObsidianExportCmd is the entry point for 'engram obsidian-export'.
func runObsidianExportCmd(args []string) error {
	fs := flag.NewFlagSet("obsidian-export", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), obsidianExportUsage) }

	vaultFlag := fs.String("vault", "", "path to the Obsidian vault (required)")
	dbFlag := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	projectFlag := fs.String("project", "", "limit export to a single project (default: all projects)")
	limitFlag := fs.Int("limit", 0, "per-project cap on exported observations (default: no cap)")
	sinceFlag := fs.String("since", "", "only export observations updated after this time (RFC3339 or YYYY-MM-DD)")
	forceFlag := fs.Bool("force", false, "ignore the incremental cutoff, re-evaluate every live observation, and rebuild an unparseable state file")
	// REQ-GRAPH-01. Default "preserve", NOT the ExportConfig zero value: an
	// unset ExportConfig.GraphConfig ("") is treated as skip on purpose, so a
	// caller that never mentions graph config gets an inert Exporter. Each
	// user-facing layer defaults its OWN knob — this flag here, and the
	// daemon's obsidian_graph_config key in daemon.go.
	//
	// Note: --watch and --interval are deliberately NOT defined here and never
	// will be. The scheduled export runs inside `engram daemon` (see the usage
	// text above); a standalone watch CLI would be the unattended, repeating
	// localstore.Open that the daemon design exists to eliminate. Neither flag
	// was ever shipped, so passing one fails with Go's own "flag provided but
	// not defined" plus usage — a clearer message than a hand-rolled rejection,
	// obtained for free.
	graphConfigFlag := fs.String("graph-config", string(obsidian.GraphConfigPreserve),
		"bootstrap {vault}/.obsidian/graph.json: preserve (default) | force | skip")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("obsidian-export takes no positional arguments; unexpected: %v", fs.Args())
	}

	if *vaultFlag == "" {
		return fmt.Errorf("--vault is required")
	}

	db := *dbFlag
	if db == "" {
		db = envOr("ENGRAM_DB", "")
	}
	if db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	var since time.Time
	if *sinceFlag != "" {
		parsed, err := parseObsidianSince(*sinceFlag)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		since = parsed
	}

	// An EXPLICIT empty --graph-config is a user error, not a request to skip.
	// ParseGraphConfigMode("") legitimately returns GraphConfigSkip (that is the
	// inert meaning of an unset ExportConfig field), so accepting it here would
	// silently mean "skip" while reading like "unset".
	if *graphConfigFlag == "" {
		return fmt.Errorf("--graph-config: must be one of preserve, force, skip")
	}
	graphMode, err := obsidian.ParseGraphConfigMode(*graphConfigFlag)
	if err != nil {
		return fmt.Errorf("--graph-config: %w", err)
	}

	store, err := localstore.Open(db)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()

	cfg := obsidian.ExportConfig{
		VaultPath:   *vaultFlag,
		Project:     *projectFlag,
		Limit:       *limitFlag,
		Since:       since,
		Force:       *forceFlag,
		GraphConfig: graphMode,
	}

	exp, err := newObsidianExporter(&obsidianStoreAdapter{store: store}, cfg)
	if err != nil {
		return fmt.Errorf("obsidian-export: %w", err)
	}

	result, err := exp.Export()
	if err != nil {
		return fmt.Errorf("obsidian-export: %w", err)
	}

	fmt.Println(result.Summary())
	return nil
}

// parseObsidianSince parses --since, accepting RFC3339 first and falling
// back to a bare "2006-01-02" date (REQ-EXPORT-01).
func parseObsidianSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q: want RFC3339 or YYYY-MM-DD", s)
}

// obsidianStoreAdapter adapts *localstore.Store to obsidian.StoreReader —
// a thin, behavior-free type-translation boundary (REQ-EXPORT-10). It is
// named obsidianStoreAdapter, NOT storeAdapter, because that identifier is
// already bound in daemon.go (to a *localStoreAdapter) and this repo's
// established name for the controlapi adapter is localStoreAdapter — a
// package-scope storeAdapter here would shadow-collide confusingly.
type obsidianStoreAdapter struct {
	store *localstore.Store
}

func (a *obsidianStoreAdapter) ListProjects() ([]string, error) {
	return a.store.ListProjects()
}

func (a *obsidianStoreAdapter) RecentObservations(project, scope string, limit int) ([]*domain.Record, error) {
	return a.store.RecentObservations(project, scope, limit)
}

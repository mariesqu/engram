package obsidian

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// StoreReader is the narrow, exporter-owned contract onto the local store
// (REQ-EXPORT-10). It is satisfied by *localstore.Store through a thin,
// behavior-free type-translation adapter (obsidianStoreAdapter, Phase 3) —
// the exporter itself never depends on localstore directly, and this
// interface adds no method that localstore does not already expose today.
//
// Deliberately narrow: RecentSessions/GetSession are NOT included. Session
// and topic hub content is built entirely from the Records already returned
// by RecentObservations (grouped by SessionID / TopicKey in Go), so no
// additional store round-trip is needed to render hub notes.
type StoreReader interface {
	// ListProjects enumerates every known project, used when no --project
	// filter is supplied (REQ-EXPORT-01/-10).
	ListProjects() ([]string, error)

	// RecentObservations returns the live (non-deleted) records for a
	// project, ID-populated (REQ-EXPORT-07 / design constraint 11 — this is
	// the ONE read path that populates Record.ID; GetObservation and
	// SearchMemories both leave it 0).
	RecentObservations(project, scope string, limit int) ([]*domain.Record, error)
}

// ExportConfig configures one Export() run.
type ExportConfig struct {
	// VaultPath is the absolute path to the Obsidian vault root. Required.
	VaultPath string

	// Project, when non-empty, limits the export to a single project
	// (REQ-EXPORT-01/-09's scoping rule). Empty means "every project known
	// to the store".
	Project string

	// Limit is applied PER PROJECT (REQ-EXPORT-01 — resolved "--limit
	// ambiguity"), mirroring RecentObservations(project, scope, limit)'s own
	// per-project semantics. Limit <= 0 means "no cap": Export() translates
	// that into an explicit large sentinel before calling
	// RecentObservations, because that store method silently rewrites
	// limit <= 0 to 20 (internal/localstore/search.go:390-392) — passing the
	// zero value straight through would silently truncate every project to
	// 20 rows and look like data loss.
	Limit int

	// Since, when non-zero, overrides the persisted state-file cutoff for
	// incremental filtering (REQ-EXPORT-06). Zero means "use
	// SyncState.LastExportAt".
	Since time.Time

	// Force re-evaluates every live observation regardless of the
	// incremental cutoff (state file or Since). The content-level
	// idempotency check (REQ-EXPORT-08) still applies — Force does not
	// force a disk write for a file whose rendered content is unchanged.
	Force bool

	// GraphConfig controls whether Export() bootstraps
	// {vault}/.obsidian/graph.json (REQ-GRAPH-01..06) at the top of each
	// cycle. The zero value ("") is treated exactly like GraphConfigSkip —
	// an ExportConfig that never mentions graph config must be INERT,
	// never silently write a file — so both the CLI's --graph-config flag
	// and the daemon's obsidian_graph_config config key are responsible
	// for defaulting THEIR OWN unset value to GraphConfigPreserve before
	// it ever reaches here.
	GraphConfig GraphConfigMode
}

// Exporter renders engram observations into an Obsidian vault.
type Exporter struct {
	store StoreReader
	cfg   ExportConfig

	// logf reports non-fatal diagnostics — a refused state-file entry, a
	// state file too corrupt to parse. It MUST NOT write to stdout:
	// REQ-EXPORT-11 reserves stdout for the single machine-parseable
	// summary line and requires diagnostics to go to stderr, which is where
	// log.Printf writes by default. Injectable so tests can assert on
	// diagnostics instead of polluting `go test` output.
	logf func(format string, args ...any)
}

// NewExporter validates cfg and constructs an Exporter over store.
func NewExporter(store StoreReader, cfg ExportConfig) (*Exporter, error) {
	if cfg.VaultPath == "" {
		return nil, fmt.Errorf("obsidian: vault path is required")
	}
	return &Exporter{store: store, cfg: cfg, logf: log.Printf}, nil
}

// SetGraphConfig updates the graph-config bootstrap mode applied at the top
// of the NEXT Export() cycle. It exists for obsidian.Loop (Phase 8), which
// downgrades an Exporter to GraphConfigSkip after the first cycle of the
// current daemon process that returns without error (REQ-WATCH-06). The
// mode is kept on ExportConfig rather than as separate Loop state so a bare
// *Exporter stays fully self-contained and satisfies the Exportable
// interface without help.
//
// Exporter is single-goroutine by contract: nothing may call
// SetGraphConfig concurrently with Export().
func (e *Exporter) SetGraphConfig(mode GraphConfigMode) {
	e.cfg.GraphConfig = mode
}

// GraphConfig reports the graph-config bootstrap mode the NEXT Export()
// cycle will apply.
//
// WARNING for callers (Phase 9 in particular): once an obsidian.Loop has run
// one successful cycle in this process, it calls SetGraphConfig(GraphConfigSkip)
// (REQ-WATCH-06), which overwrites e.cfg.GraphConfig above. From that point
// on this method reports "skip" regardless of what mode the caller originally
// configured — it is NOT the user's configured graph-config mode, it is only
// ever "what the next cycle will do". A status endpoint or config-echo
// feature must source the user's configured value from its own config, not
// from this accessor.
func (e *Exporter) GraphConfig() GraphConfigMode {
	return e.cfg.GraphConfig
}

// stateFileName is REQ-EXPORT-02's incremental-sync marker, stored inside
// the exporter's namespace directory ("{vault}/engram/").
const stateFileName = ".engram-sync-state.json"

// engramDir is the one namespace the exporter owns inside the vault
// (REQ-EXPORT-02: "MUST NOT write outside {vault}/engram/").
const engramDir = "engram"

// Export runs one export cycle: read state, list live observations for the
// configured project scope, render changed ones to disk, remove files for
// observations no longer live, refresh hub notes, and persist the updated
// state. It returns the REQ-EXPORT-11 cycle summary.
func (e *Exporter) Export() (*ExportResult, error) {
	// Graph-config bootstrap (REQ-GRAPH-01..06) runs FIRST, before anything
	// else in the cycle: it is a one-shot vault-root write, independent of
	// the incremental state below, and both "preserve" and "force" are
	// idempotent, so ordering it before or after the rest of the cycle
	// makes no observable difference except code clarity. The zero value
	// of GraphConfigMode ("") is treated identically to GraphConfigSkip —
	// an ExportConfig that never mentions graph config must stay inert.
	if mode := e.cfg.GraphConfig; mode != "" && mode != GraphConfigSkip {
		if err := WriteGraphConfig(e.cfg.VaultPath, mode); err != nil {
			return nil, fmt.Errorf("obsidian: graph config: %w", err)
		}
	}

	// The incremental cutoff for the NEXT cycle is stamped BEFORE the first
	// read, never after the work (bugfix/obsidian-exporter-cutoff-toctou).
	// An observation saved between the read and the state write would
	// otherwise land before a cutoff taken at the end and be filtered out
	// forever, silently and with no error. Re-exporting a handful of
	// observations on the next cycle is free — writeIfChanged skips
	// byte-identical content.
	cycleStart := time.Now().UTC()

	root := filepath.Join(e.cfg.VaultPath, engramDir)
	sessionsDir := filepath.Join(root, "_sessions")
	topicsDir := filepath.Join(root, "_topics")
	statePath := filepath.Join(root, stateFileName)

	// REQ-EXPORT-02 / task 2.10: subdirs are created up front, not lazily.
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return nil, fmt.Errorf("obsidian: create _sessions dir: %w", err)
	}
	if err := os.MkdirAll(topicsDir, 0755); err != nil {
		return nil, fmt.Errorf("obsidian: create _topics dir: %w", err)
	}

	state, err := ReadState(statePath)
	if err != nil {
		// An unparseable state file used to be terminal, with no way out
		// from inside the CLI: --force did not bypass it, so recovery meant
		// knowing the file existed, knowing where it lived, and deleting a
		// dot-file by hand. --force already means "ignore the persisted
		// cutoff and re-evaluate every live observation", which is exactly
		// what rebuilding from an empty state needs, so it doubles as the
		// repair lever.
		if !e.cfg.Force {
			return nil, fmt.Errorf("%w (re-run with --force to discard it and rebuild the state from scratch)", err)
		}
		e.logf("obsidian: %v; --force: discarding it and rebuilding from an empty state", err)
		state = &SyncState{Files: map[int64]string{}, Hubs: map[string]string{}}
	}

	cutoff := state.LastExportAt
	if !e.cfg.Since.IsZero() {
		cutoff = e.cfg.Since
	}
	if e.cfg.Force {
		cutoff = time.Time{}
	}

	projects, err := e.projectsToExport()
	if err != nil {
		return nil, err
	}

	// "No cap" (Limit <= 0) must NOT be passed straight through as 0/negative:
	// RecentObservations silently rewrites limit <= 0 to 20
	// (internal/localstore/search.go:390-392), which would look like data
	// loss. math.MaxInt is the explicit "uncapped" sentinel (REQ-EXPORT-01).
	limit := e.cfg.Limit
	if limit <= 0 {
		limit = math.MaxInt
	}

	result := &ExportResult{}
	liveIDs := map[int64]bool{}

	bySession := map[string][]ObsRef{}
	byTopicPrefix := map[string][]ObsRef{}

	for _, project := range projects {
		records, err := e.store.RecentObservations(project, "", limit)
		if err != nil {
			return nil, fmt.Errorf("obsidian: list observations for project %q: %w", project, err)
		}

		for _, rec := range records {
			liveIDs[rec.ID] = true

			relPath := vaultRelPath(rec)
			ref := ObsRef{VaultPath: strings.TrimSuffix(relPath, ".md"), Title: rec.Title}
			bySession[rec.SessionID] = append(bySession[rec.SessionID], ref)
			if rec.TopicKey != nil {
				if prefix, ok := TopicPrefix(*rec.TopicKey); ok {
					byTopicPrefix[prefix] = append(byTopicPrefix[prefix], ref)
				}
			}

			ts := rec.UpdatedAt
			if ts.IsZero() {
				ts = rec.CreatedAt
			}

			prev, tracked := state.Files[rec.ID]

			// Skip ONLY when the observation is older than the cutoff AND is
			// already tracked at exactly this path. Both clauses matter:
			//
			//   - "tracked" is the guard the recovered design (#1220)
			//     specified and the rebuild dropped. Without it an
			//     observation that was never actually written is skipped on
			//     timestamp alone, forever — and the old skip branch then
			//     recorded it in state.Files as exported when no file had
			//     ever been created.
			//   - "prev == relPath" catches an id whose rendered path has
			//     moved (content, type or project changed) without its
			//     timestamp clearing the cutoff, e.g. under a --since
			//     override. Falling through re-writes it and lets the
			//     orphan cleanup below run.
			if tracked && prev == relPath && !ts.After(cutoff) {
				result.Skipped++
				continue
			}

			absPath, err := e.resolveVaultPath(relPath)
			if err != nil {
				return nil, fmt.Errorf("obsidian: observation %d: %w", rec.ID, err)
			}
			content := ObservationToMarkdown(rec)
			wrote, err := writeIfChanged(absPath, content)
			if err != nil {
				return nil, fmt.Errorf("obsidian: write observation %d: %w", rec.ID, err)
			}
			switch {
			case wrote && tracked:
				result.Updated++
			case wrote:
				result.Created++
			default:
				result.Skipped++
			}

			// The filename embeds Slugify(rec.Content), so a change to
			// content, type or project moves the note. state.Files[rec.ID]
			// is about to be overwritten with the new path, which would
			// leave the old file on disk untracked — invisible to the
			// deletion diff and unreachable even by --force. Remove it now
			// or never. This is not a rare path: a topic-keyed mem_save
			// UPDATEs in place and keeps the same integer id
			// (internal/localstore/apply.go execUpdate).
			if tracked && prev != relPath {
				removed, err := e.removeSupersededNote(prev, absPath)
				if err != nil {
					return nil, fmt.Errorf("obsidian: observation %d: %w", rec.ID, err)
				}
				if removed {
					result.Deleted++
				}
			}

			state.Files[rec.ID] = relPath
		}
	}

	// Deletion pass (REQ-EXPORT-09): anything tracked in state but absent
	// from this run's live listing is gone (soft-deleted, or moved out of
	// scope). Scoped to the projects actually processed this run so a
	// --project-filtered run can never delete another project's files.
	// Keyed by lower(project): projects is already deduped case-insensitively
	// by projectsToExport(), but the project segment recovered below via
	// projectFromRelPath comes from the RECORD's own stored Project field
	// (through vaultRelPath), which can carry different casing than
	// whatever this run's project list happens to spell it as —
	// RecentObservations matches case-insensitively and can hand back
	// records whose casing differs from the queried name. Comparing exact
	// strings here left the deletion pass unable to recognize its own scope
	// whenever the two casings diverged, silently orphaning files forever
	// (bugfix/listprojects-case-variant-double-read).
	processed := map[string]bool{}
	for _, p := range projects {
		processed[strings.ToLower(p)] = true
	}
	for id, relPath := range state.Files {
		if liveIDs[id] {
			continue
		}
		// relPath is UNTRUSTED. It is read verbatim from
		// .engram-sync-state.json, which lives inside the vault and is
		// therefore writable by Obsidian Sync, a shared/cloud vault, another
		// plugin, or an imported note bundle. The project scope guard below
		// is NOT a containment check — it reads segment[1], so
		// "engram/proj1/../../../../victim.md" passes it while resolving to
		// an arbitrary file. Resolve BEFORE the scope check so a hostile
		// entry is always reported, even during a --project run that would
		// have filtered it out.
		absPath, err := e.resolveVaultPath(relPath)
		if err != nil {
			// Never delete, never abort the cycle: report it and move on.
			// The entry is deliberately RETAINED in state so the diagnostic
			// keeps firing until a human looks at the state file.
			e.logf("obsidian: refusing unsafe state entry for observation %d (%q): %v", id, relPath, err)
			continue
		}
		if !processed[strings.ToLower(projectFromRelPath(relPath))] {
			continue
		}
		if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("obsidian: remove deleted observation file %s: %w", relPath, err)
		}
		delete(state.Files, id)
		result.Deleted++
	}

	// Hub notes: rebuilt from this run's full live listing (not just the
	// changed subset), since a hub always reflects the CURRENT complete
	// membership, not an incremental delta.
	// Hub filenames derive from a session id / topic prefix the exporter does
	// not control either, so they go through the same containment guard as
	// observation notes.
	//
	// bySession/byTopicPrefix keys are merged case-insensitively BEFORE any
	// of that: the same project-name case drift that produces duplicate
	// ListProjects() entries also produces case-variant session ids in the
	// live data (e.g. "manual-save-gitlab" / "manual-save-GitLab" — 5 such
	// pairs found in the real store), and the hub filename is derived from
	// the key via safeFilename, which does not fold case. On a
	// case-insensitive filesystem (Windows NTFS, macOS default HFS+/APFS —
	// i.e. most Obsidian vaults), two keys differing only by case resolve
	// to the SAME physical file regardless of what the exporter intends.
	// Left unmerged this is not a one-time fluke: every cycle re-derives
	// bySession/byTopicPrefix from the FULL live listing, so the two keys
	// perpetually overwrite each other's content on EVERY run, forever —
	// confirmed directly against the live store (hubs=8 on a run where
	// nothing in the underlying data had changed).
	bySession = mergeCaseInsensitiveKeys(bySession)
	byTopicPrefix = mergeCaseInsensitiveKeys(byTopicPrefix)

	// renderedHubs is THIS cycle's complete hub membership — a stable
	// identity ("_sessions/{id}" / "_topics/{prefix}", independent of
	// safeFilename's sanitization) -> vault-relative path. It drives the
	// write loops below and, via the sweep further down, closes deferred
	// MAJOR-4: a hub tracked in a PRIOR cycle but absent from THIS cycle's
	// membership (every observation in a session gone, or a topic prefix
	// dropping below the 2-ref threshold) is stale and must be removed
	// rather than left with dead links forever.
	renderedHubs := map[string]string{}

	for sessionID, refs := range bySession {
		relHubPath := vaultJoin(engramDir, "_sessions", safeFilename(sessionID)+".md")
		hubPath, err := e.resolveVaultPath(relHubPath)
		if err != nil {
			return nil, fmt.Errorf("obsidian: session hub %q: %w", sessionID, err)
		}
		wrote, err := writeIfChanged(hubPath, SessionHubMarkdown(sessionID, refs))
		if err != nil {
			return nil, fmt.Errorf("obsidian: write session hub %q: %w", sessionID, err)
		}
		if wrote {
			result.Hubs++
		}
		renderedHubs["_sessions/"+sessionID] = relHubPath
	}
	for prefix, refs := range byTopicPrefix {
		if !ShouldCreateTopicHub(len(refs)) {
			continue
		}
		relHubPath := vaultJoin(engramDir, "_topics", safeFilename(prefix)+".md")
		hubPath, err := e.resolveVaultPath(relHubPath)
		if err != nil {
			return nil, fmt.Errorf("obsidian: topic hub %q: %w", prefix, err)
		}
		wrote, err := writeIfChanged(hubPath, TopicHubMarkdown(prefix, refs))
		if err != nil {
			return nil, fmt.Errorf("obsidian: write topic hub %q: %w", prefix, err)
		}
		if wrote {
			result.Hubs++
		}
		renderedHubs["_topics/"+prefix] = relHubPath
	}

	// Stale-hub sweep (REQ-EXPORT-09's NEW clause, closes deferred
	// MAJOR-4), scoped per the MAJOR-3 mitigation: a hub is inherently
	// CROSS-project (a session or topic prefix can span several
	// projects), so REQ-EXPORT-09's per-project note-deletion scoping
	// does not transfer to hubs. The rule chosen is deliberately
	// conservative in the filtered case: the sweep does NOT run at all
	// when e.cfg.Project is set, so a filtered run can leave stale hubs
	// behind but can never delete a hub belonging to a project outside
	// its own scope. state.Hubs is correspondingly MERGED rather than
	// REPLACED when filtered — replacing it with this run's necessarily
	// truncated renderedHubs would forget every hub this run never saw.
	if e.cfg.Project == "" {
		for identity, relPath := range state.Hubs {
			if _, ok := renderedHubs[identity]; ok {
				continue
			}
			// relPath is UNTRUSTED, exactly like state.Files above
			// (CRITICAL-1's containment guard applies verbatim: state.Hubs
			// is read from the same in-vault, Obsidian-Sync-writable
			// file). Resolve through the same containment guard as note
			// deletion; a refusal is reported and the entry is left in
			// place rather than removed blind.
			absPath, err := e.resolveVaultPath(relPath)
			if err != nil {
				e.logf("obsidian: refusing unsafe hub state entry %q (%q): %v", identity, relPath, err)
				continue
			}
			if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("obsidian: remove stale hub %s: %w", relPath, err)
			}
			result.Deleted++
		}
		state.Hubs = renderedHubs
	} else {
		// MAJOR-3 mitigation, not a fix: make the truncation LOUD instead
		// of silent. Exactly one warning per filtered cycle, on the
		// injected logf (stderr — never stdout, REQ-EXPORT-11).
		e.logf("obsidian: --project=%q set: cross-project session/topic hub membership is rendered from this run's scope only, and stale-hub cleanup (sweep) is disabled for this cycle", e.cfg.Project)
		if state.Hubs == nil {
			state.Hubs = map[string]string{}
		}
		for identity, relPath := range renderedHubs {
			state.Hubs[identity] = relPath
		}
	}

	state.LastExportAt = cycleStart
	if err := WriteState(statePath, state); err != nil {
		return nil, err
	}

	return result, nil
}

// projectsToExport resolves the project scope for this run: the configured
// --project filter, or every project known to the store (REQ-EXPORT-01/-09).
//
// ListProjects() (internal/localstore/sync.go) does a case-SENSITIVE
// "SELECT DISTINCT project", so project-name drift in the store (e.g. both
// "gitlab" and "GitLab" present) makes it return both spellings. Looping the
// raw list would then call RecentObservations once per spelling, and that
// method matches case-insensitively (LOWER(project) —
// internal/localstore/search.go), so every spelling resolves to the SAME
// underlying rows — each observation gets written once (Created) and
// revisited a second time with identical content already on disk (Skipped),
// roughly doubling both counters and duplicating every hub wikilink
// (bugfix/listprojects-case-variant-double-read, confirmed against the live
// store: created=4712 skipped=4787, ~2x the true observation count).
//
// This is fixed HERE, not in ListProjects() itself: that method is on the
// sync path and its other callers may legitimately depend on seeing every
// raw variant, and changing it needs its own review. Store.ListProjectsWithPolicy()
// (internal/localstore/policy.go) already solves the identical problem for
// its own callers with a ROW_NUMBER()-based dedup that prefers the name as
// stored in `memories`; dedupeProjectsCaseInsensitive follows the same
// spirit without that function's visibility into which table a name came
// from (see its doc comment for the tiebreak this uses instead).
func (e *Exporter) projectsToExport() ([]string, error) {
	if e.cfg.Project != "" {
		return []string{e.cfg.Project}, nil
	}
	projects, err := e.store.ListProjects()
	if err != nil {
		return nil, fmt.Errorf("obsidian: list projects: %w", err)
	}
	return dedupeProjectsCaseInsensitive(dropEmptyProjects(projects)), nil
}

// dropEmptyProjects removes empty (and whitespace-only) entries from a raw
// ListProjects() result before the exporter ever loops over it as a set of
// per-project scopes.
//
// Discovered by a real-data smoke test while verifying the case-drift fix
// above (bugfix/listprojects-case-variant-double-read): ListProjects()
// (internal/localstore/sync.go) is a plain "SELECT DISTINCT project" UNION
// across memories/memory_tombstones/user_prompts/prompt_tombstones, and at
// least one row in the live store carries project = "" (legacy or
// never-set data). RecentObservations treats an empty (post-normalization)
// project string as "no filter at all" — internal/localstore/search.go
// only adds the "AND LOWER(project) = ?" clause "if project != \"\"", and
// normalizeProject (internal/localstore/sessions.go) trims whitespace
// before that check runs, so a whitespace-only value is equally a "no
// filter" trigger. Looping that entry as an ordinary per-project scope
// therefore calls RecentObservations("", ...) and gets back EVERY record in
// the entire store, not zero — on the live data this alone accounted for
// roughly half of all rows processed, dwarfing the case-drift duplication
// this bug was originally filed against.
func dropEmptyProjects(projects []string) []string {
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// dedupeProjectsCaseInsensitive collapses project names that differ only in
// case into a single entry, keyed by lower(name), preserving the order in
// which each lower-cased key was first seen in projects.
//
// Tiebreak: when a lower-cased key has more than one surviving spelling, the
// lexicographically FIRST variant wins (plain byte-wise sort.Strings — e.g.
// "GitLab" sorts before "gitlab" since 'G' < 'g' in ASCII). This is a
// DELIBERATE, DOCUMENTED choice, not an attempt to guess which spelling is
// "canonical": ListProjects() exposes no signal (like
// ListProjectsWithPolicy's src column) about which table or write path a
// given spelling came from, so there is no principled way to prefer one
// variant's provenance over another from here. What matters is that the
// choice is DETERMINISTIC — the surviving name becomes a directory name in
// the vault, and an export that silently alternated between "gitlab" and
// "GitLab" across runs would flip that project's entire file tree back and
// forth on every cycle.
func dedupeProjectsCaseInsensitive(projects []string) []string {
	variants := map[string][]string{}
	var order []string
	for _, p := range projects {
		key := strings.ToLower(p)
		if _, seen := variants[key]; !seen {
			order = append(order, key)
		}
		variants[key] = append(variants[key], p)
	}

	out := make([]string, 0, len(order))
	for _, key := range order {
		vs := variants[key]
		sort.Strings(vs)
		out = append(out, vs[0])
	}
	return out
}

// mergeCaseInsensitiveKeys collapses map keys that differ only in case into
// a single entry, analogous to dedupeProjectsCaseInsensitive but applied to
// the bySession/byTopicPrefix hub-accumulation maps built during Export().
//
// This matters for the identical reason dedupeProjectsCaseInsensitive
// exists: the physical hub file path is derived from the key via
// safeFilename, which does not fold case, while the filesystems Obsidian
// vaults actually live on are commonly case-insensitive (Windows NTFS,
// macOS default HFS+/APFS). Two keys differing only by case therefore
// resolve to the SAME file on disk no matter what the exporter intends.
// Left unmerged, each key is rendered and written independently within the
// same loop, and whichever key is processed second always finds the other's
// leftover content already there and treats it as a change — every cycle,
// forever, since hub maps are rebuilt from the full live listing on every
// run, not just an incremental delta.
//
// The tiebreak mirrors dedupeProjectsCaseInsensitive: the lexicographically
// FIRST variant (byte-wise sort.Strings) is the deterministic winner, used
// both as the surviving map key (which feeds safeFilename and the rendered
// heading) and to decide iteration order among the merged variants' refs.
// Refs from every variant are concatenated in that sorted order and then
// passed through dedupeRefs, so the merged hub lists every observation from
// every case-variant key exactly once, in a stable order.
func mergeCaseInsensitiveKeys(m map[string][]ObsRef) map[string][]ObsRef {
	variants := map[string][]string{}
	for k := range m {
		lk := strings.ToLower(k)
		variants[lk] = append(variants[lk], k)
	}

	out := make(map[string][]ObsRef, len(variants))
	for _, ks := range variants {
		sort.Strings(ks)
		var merged []ObsRef
		for _, k := range ks {
			merged = append(merged, m[k]...)
		}
		out[ks[0]] = dedupeRefs(merged)
	}
	return out
}

// vaultRelPath computes the vault-relative path for rec, per REQ-EXPORT-02/
// -07: "engram/{project}/{type}/{slug}-{id}.md". Project and type are
// filename-sanitized defensively even though both are expected to already
// be simple identifiers.
//
// The result is built through vaultJoin, NOT a bare filepath.Join
// (bugfix/obsidian-wikilink-path-separator): this value becomes both a
// rendered wikilink target (ObsRef.VaultPath) and a persisted
// SyncState.Files value, and Obsidian requires forward slashes for a
// wikilink to resolve on EVERY platform -- a bare filepath.Join emits
// OS-native separators, which on Windows rendered an unresolvable
// hub -> note link in every one of the ~7,200 links a real export produced.
func vaultRelPath(rec *domain.Record) string {
	slug := Slugify(rec.Content)
	if slug == "" {
		slug = "untitled"
	}
	filename := fmt.Sprintf("%s-%d.md", slug, rec.ID)
	return vaultJoin(engramDir, safeFilename(rec.Project), safeFilename(rec.Type), filename)
}

// removeSupersededNote deletes the note an observation used to occupy at
// prevRel, now that it has been rewritten at currentAbs. It reports whether
// a file was actually removed.
//
// The guard that matters is the os.SameFile check. state.Files stores
// OS-NATIVE separators, so a vault synced between a Windows machine and a
// POSIX one carries entries like "engram\p\t\a-1.md" that the other OS
// renders as "engram/p/t/a-1.md": different STRINGS naming the SAME file.
// Comparing the raw strings would classify the note as superseded and
// delete it immediately after writing it. Comparing resolved paths textually
// would still be wrong on Windows, where the filesystem is case-insensitive
// and 8.3 short names alias long ones — os.SameFile asks the filesystem
// instead of guessing.
//
// A prevRel that fails containment is reported and left alone: refusing to
// delete is always the safe direction.
func (e *Exporter) removeSupersededNote(prevRel, currentAbs string) (bool, error) {
	prevAbs, err := e.resolveVaultPath(prevRel)
	if err != nil {
		e.logf("obsidian: refusing to remove superseded note %q: %v", prevRel, err)
		return false, nil
	}

	prevInfo, prevErr := os.Stat(prevAbs)
	if prevErr != nil {
		if os.IsNotExist(prevErr) {
			return false, nil
		}
		return false, fmt.Errorf("stat superseded note %s: %w", prevRel, prevErr)
	}
	if currentInfo, err := os.Stat(currentAbs); err == nil && os.SameFile(prevInfo, currentInfo) {
		return false, nil
	}

	if err := os.Remove(prevAbs); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove superseded note %s: %w", prevRel, err)
	}
	return true, nil
}

// resolveVaultPath turns a VAULT-relative path — the form stored in
// .engram-sync-state.json and produced by vaultRelPath — into an absolute
// path, refusing anything that does not land strictly inside
// {vault}/engram/ (REQ-EXPORT-02).
//
// It is applied on BOTH sides of the exporter. On the write side the
// segments come from uncontrolled project/type/session/topic values; on the
// delete side the whole string comes verbatim out of the state file, which
// lives inside the vault and is writable by anything that can write to the
// vault. Neither side is trusted.
func (e *Exporter) resolveVaultPath(relPath string) (string, error) {
	rest, ok := stripEngramPrefix(relPath)
	if !ok {
		return "", fmt.Errorf("obsidian: refusing path %q: it is not inside the %q namespace", relPath, engramDir)
	}
	return containedPath(filepath.Join(e.cfg.VaultPath, engramDir), rest)
}

// stripEngramPrefix removes the leading "engram/" namespace segment from a
// vault-relative path, reporting false when it is absent or nothing follows
// it. The match is case-SENSITIVE on every platform: the exporter only ever
// writes the lowercase literal, so anything else is not a path it produced.
func stripEngramPrefix(relPath string) (string, bool) {
	norm := toVaultSlash(relPath)
	rest, ok := strings.CutPrefix(norm, engramDir+"/")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}

// projectFromRelPath extracts the project directory segment from a
// vault-relative path produced by vaultRelPath (the segment right after
// "engram/"), used to scope the deletion diff to the projects processed in
// the current run (REQ-EXPORT-09).
func projectFromRelPath(relPath string) string {
	parts := strings.Split(toVaultSlash(relPath), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// writeIfChanged writes content to path only when the existing file (if any)
// has different content, so a re-run of an unchanged observation produces
// zero disk I/O (REQ-EXPORT-08 idempotency). It returns whether a write
// occurred.
func writeIfChanged(path, content string) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) == content {
			return false, nil
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return false, err
	}
	return true, nil
}

// (safeFilename lives in path.go, alongside containedPath — the two are one
// guard: safeFilename keeps a hostile name inside a single path segment, and
// containedPath verifies the assembled path never leaves {vault}/engram/.)

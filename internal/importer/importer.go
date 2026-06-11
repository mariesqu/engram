// Package importer migrates data from an old-generation engram SQLite database
// (old_code) into a current-generation local store.
//
// # Source schema
//
// The source DB is opened READ-ONLY (mode=ro DSN option) and must contain
// the tables sessions, observations, and user_prompts as defined by
// old_code/internal/store/store.go.  Soft-deleted rows (deleted_at IS NOT NULL
// on observations) are skipped.
//
// # Idempotency
//
// Import is safe to re-run. The mechanism is:
//
//  1. Sync-ID reuse: when the source row has a non-empty sync_id that value is
//     reused directly as the mutation's SyncID.  When it is NULL/empty a stable
//     CONTENT-ADDRESSED ID is derived: "import-obs-<hash>" / "import-prompt-<hash>"
//     (SHA-256 over the row's content fields — never the source AUTOINCREMENT id,
//     which collides across different old machines; byte-identical duplicate rows
//     hash to the same ID and dedupe to one).
//
//  2. Pre-check via FindBySyncID / user_prompts lookup: before calling LocalWrite
//     the importer checks whether the target store already has a live row with the
//     derived SyncID.  When it does, the row is counted as skipped-existing and
//     LocalWrite is NOT called.  This is a true no-op: the same mutation_id content-
//     addressed hash would cause a database-level INSERT OR IGNORE in the outbox, but
//     we avoid the write lock entirely and keep the report counts accurate.
//
// Topic-key convergence: observations are imported in (created_at ASC, id ASC)
// order so that multiple revisions of the same topic_key flow through LocalWrite's
// LWW Decide path, converging to the latest revision — identical to what an
// incremental pull from central would do.
//
// # Timestamp preservation
//
// UpdatedAt on the mutation is set to the source row's updated_at (LWW correctness
// if the user later syncs multiple imported nodes).  OccurredAt is set to the source
// row's created_at.  The memories table's created_at column is set by the SQL
// DEFAULT at INSERT time, but the UpdatedAt / OccurredAt payload ensures LWW
// tiebreaking reflects the original authorship time.
//
// # Project normalization
//
// Source project NULL → "".  The local store normalizes (lowercase/trim) on every
// write so the result is consistent with all other writes.
//
// # Embeddings
//
// Imported rows carry NULL embeddings (the old_code columns were never populated).
// The embedding backfill loop picks them up automatically via the NULL-as-queue
// predicate on the next daemon start.
//
// # Policy
//
// Rows whose project has an omitted policy in the destination store are warned and
// skipped (consistent with the tool-level capture refusal for omitted projects).
package importer

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"crypto/sha256"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"io"
	"os"
	"path/filepath"
)

// Stats summarises the outcome of a Run call.
type Stats struct {
	SessionsImported int
	SessionsSkipped  int // already present

	MemoriesImported int
	MemoriesSkipped  int // already present (existing sync_id)
	MemoriesDeleted  int // soft-deleted in source — skipped
	MemoriesOmitted  int // project policy = omitted

	PromptsImported int
	PromptsSkipped  int // already present
	PromptsOmitted  int // project policy = omitted
}

// Importer migrates data from an old-generation engram DB into a Store.
type Importer struct {
	dst *localstore.Store
	// writerID is stamped on every imported mutation. In practice the source data
	// has no writer identity; we use a stable marker so re-imported nodes carry the
	// same identity and the LWW tiebreaker is deterministic across re-runs.
	writerID string
}

// New constructs an Importer that writes into dst.
// writerID is the value to stamp on all imported mutations.  Pass the daemon's
// configured writer id when available; pass "import" as a fallback.
func New(dst *localstore.Store, writerID string) *Importer {
	if writerID == "" {
		writerID = "import"
	}
	return &Importer{dst: dst, writerID: writerID}
}

// Run reads from srcDB (already open, read-only) and imports into the destination
// store.  When dryRun is true no writes are performed and only Stats are returned.
//
// srcDB MUST have been opened read-only by the caller (mode=ro DSN).
func (imp *Importer) Run(srcDB *sql.DB, dryRun bool) (Stats, error) {
	var st Stats

	// Validate source schema first — give a friendly error when it is not an
	// old-generation engram database.
	if err := validateSourceSchema(srcDB); err != nil {
		return st, err
	}

	// ── Sessions ──────────────────────────────────────────────────────────────
	if err := imp.importSessions(srcDB, dryRun, &st); err != nil {
		return st, fmt.Errorf("import sessions: %w", err)
	}

	// ── Observations (memories) ───────────────────────────────────────────────
	if err := imp.importObservations(srcDB, dryRun, &st); err != nil {
		return st, fmt.Errorf("import observations: %w", err)
	}

	// ── Prompts ───────────────────────────────────────────────────────────────
	if err := imp.importPrompts(srcDB, dryRun, &st); err != nil {
		return st, fmt.Errorf("import prompts: %w", err)
	}

	return st, nil
}

// ── validateSourceSchema ──────────────────────────────────────────────────────

// validateSourceSchema checks the required old-generation tables exist.
func validateSourceSchema(srcDB *sql.DB) error {
	required := []string{"sessions", "observations", "user_prompts"}
	for _, tbl := range required {
		var name string
		err := srcDB.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("not an old-generation engram database: table %q missing", tbl)
		}
		if err != nil {
			return fmt.Errorf("validateSourceSchema: %w", err)
		}
	}
	return nil
}

// ── Sessions ──────────────────────────────────────────────────────────────────

// srcSession is a row read from the source sessions table.
type srcSession struct {
	ID        string
	Project   sql.NullString
	Directory sql.NullString
	StartedAt sql.NullString
	EndedAt   sql.NullString
	Summary   sql.NullString
}

func (imp *Importer) importSessions(srcDB *sql.DB, dryRun bool, st *Stats) error {
	rows, err := srcDB.Query(
		`SELECT id, project, directory, started_at, ended_at, summary FROM sessions ORDER BY started_at ASC, id ASC`,
	)
	if err != nil {
		return fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var s srcSession
		if err := rows.Scan(&s.ID, &s.Project, &s.Directory, &s.StartedAt, &s.EndedAt, &s.Summary); err != nil {
			return fmt.Errorf("scan session: %w", err)
		}

		project := coalesceStr(s.Project, "")
		directory := coalesceStr(s.Directory, "")

		if dryRun {
			// In dry-run we still check existence to produce accurate counts.
			_, getErr := imp.dst.GetSession(s.ID)
			if getErr == nil {
				st.SessionsSkipped++
			} else if errors.Is(getErr, localstore.ErrSessionNotFound) {
				st.SessionsImported++
			} else {
				return fmt.Errorf("dry-run check session %q: %w", s.ID, getErr)
			}
			continue
		}

		// Check existence — CreateSession is an upsert (no-op when id exists), but
		// we want accurate skip counts.
		_, getErr := imp.dst.GetSession(s.ID)
		if getErr == nil {
			st.SessionsSkipped++
			continue
		}
		if !errors.Is(getErr, localstore.ErrSessionNotFound) {
			return fmt.Errorf("check session %q: %w", s.ID, getErr)
		}

		if err := imp.dst.CreateSession(s.ID, project, directory); err != nil {
			return fmt.Errorf("create session %q: %w", s.ID, err)
		}

		// Propagate ended_at + summary when present — with the ORIGINAL
		// timestamp (EndSession stamps datetime('now'), which would record a
		// years-old session as ending at import time).
		if s.EndedAt.Valid && s.EndedAt.String != "" {
			summary := ""
			if s.Summary.Valid {
				summary = s.Summary.String
			}
			endedAt := parseImportTime(s.EndedAt.String)
			if endedAt.IsZero() {
				endedAt = time.Unix(0, 0).UTC() // deterministic, parse-proof
			}
			if err := imp.dst.EndSessionAt(s.ID, summary, endedAt); err != nil {
				return fmt.Errorf("end session %q: %w", s.ID, err)
			}
		}
		st.SessionsImported++
	}
	return rows.Err()
}

// ── Observations ─────────────────────────────────────────────────────────────

// srcObservation is a row read from the source observations table.
type srcObservation struct {
	ID        int64
	SyncID    sql.NullString
	SessionID string
	Type      string
	Title     string
	Content   string
	Project   sql.NullString
	Scope     string
	TopicKey  sql.NullString
	CreatedAt string
	UpdatedAt string
	DeletedAt sql.NullString
}

func (imp *Importer) importObservations(srcDB *sql.DB, dryRun bool, st *Stats) error {
	// Import in (created_at ASC, id ASC) order so topic_key upserts converge to
	// the latest revision — same-topic rows flow through LWW Decide path.
	rows, err := srcDB.Query(`
		SELECT id, sync_id, session_id, type, title, content,
		       project, scope, topic_key, created_at, updated_at, deleted_at
		FROM observations
		ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return fmt.Errorf("query observations: %w", err)
	}
	defer rows.Close()

	// Cache policy lookups (GetPolicy is already cached, but one call per row
	// would hold policyMu on every FTS search — cache per-project here too).
	policyCache := make(map[string]localstore.Policy)

	for rows.Next() {
		var o srcObservation
		if err := rows.Scan(
			&o.ID, &o.SyncID, &o.SessionID, &o.Type, &o.Title, &o.Content,
			&o.Project, &o.Scope, &o.TopicKey, &o.CreatedAt, &o.UpdatedAt, &o.DeletedAt,
		); err != nil {
			return fmt.Errorf("scan observation: %w", err)
		}

		// Skip soft-deleted rows.
		if o.DeletedAt.Valid && o.DeletedAt.String != "" {
			st.MemoriesDeleted++
			continue
		}

		project := coalesceStr(o.Project, "")

		// Policy check — warn+skip omitted projects.
		pol, err := imp.cachedPolicy(policyCache, project)
		if err != nil {
			return fmt.Errorf("policy for project %q: %w", project, err)
		}
		if pol == localstore.PolicyOmitted {
			st.MemoriesOmitted++
			continue
		}

		syncID := deriveSyncID(o.SyncID, "import-obs",
			o.Title, o.Content, o.CreatedAt, o.SessionID)
		createdAt := parseImportTime(o.CreatedAt)
		updatedAt := parseImportTime(o.UpdatedAt)

		m := buildObsMutation(o, syncID, project, createdAt, updatedAt, imp.writerID)

		if dryRun {
			if imp.memoryExists(syncID) || imp.topicAlreadyCurrent(m) {
				st.MemoriesSkipped++
			} else {
				st.MemoriesImported++
			}
			continue
		}

		// Pre-check: skip if already present.
		//
		// Two conditions cover the two identity axes:
		//  (a) sync_id match — the exact row was already imported (direct identity).
		//  (b) topic_key match — a same-topic row already exists whose updated_at is
		//      >= this row's updated_at, meaning LWW would produce NoOp anyway.  This
		//      covers the re-import of a superseded topic revision whose sync_id was
		//      never stored as the live row's PK (the LWW winner's PK was preserved).
		//
		// Without (b), a re-import of obs-topic-v1 after obs-topic-v2 has already won
		// the topic would call LocalWrite, which correctly produces NoOp, but the
		// importer can't observe that NoOp and incorrectly counts it as "imported".
		if imp.memoryExists(syncID) || imp.topicAlreadyCurrent(m) {
			st.MemoriesSkipped++
			continue
		}

		if _, err := imp.dst.LocalWrite(m); err != nil {
			return fmt.Errorf("LocalWrite observation id=%d sync_id=%q: %w", o.ID, syncID, err)
		}
		st.MemoriesImported++
	}
	return rows.Err()
}

// memoryExists reports whether a live memories row already exists for syncID.
func (imp *Importer) memoryExists(syncID string) bool {
	rec, err := imp.dst.FindBySyncID(syncID)
	return err == nil && rec != nil
}

// topicAlreadyCurrent returns true when the destination already has a live row
// for the mutation's (topic_key, project, scope) triple whose UpdatedAt is at
// least as recent as the mutation's UpdatedAt.
//
// This is the second identity axis in the pre-check: when a topic has been
// imported and a later revision has already won the LWW slot (keeping its own
// sync_id as the live row's PK), re-importing the earlier revision would call
// LocalWrite which correctly produces NoOp — but the importer cannot observe
// that from the outside. We detect it here to keep the imported/skipped counts
// accurate and avoid acquiring the write lock for a guaranteed no-op.
func (imp *Importer) topicAlreadyCurrent(m domain.Mutation) bool {
	if m.TopicKey == nil {
		return false
	}
	existing, err := imp.dst.FindByTopic(*m.TopicKey, m.Project, m.Scope)
	if err != nil || existing == nil {
		return false
	}
	// The stored row is current if it is at least as new as the incoming mutation.
	return !existing.UpdatedAt.Before(m.UpdatedAt)
}

// buildObsMutation constructs the domain.Mutation for one source observation.
func buildObsMutation(o srcObservation, syncID, project string, createdAt, updatedAt time.Time, writerID string) domain.Mutation {
	scope := o.Scope
	if scope == "" {
		scope = "project"
	}

	var topicKey *string
	if o.TopicKey.Valid && o.TopicKey.String != "" {
		tk := o.TopicKey.String
		topicKey = &tk
	}

	// Use updatedAt as OccurredAt fallback if createdAt is zero.
	occurredAt := createdAt
	if occurredAt.IsZero() {
		occurredAt = updatedAt
	}

	// UpdatedAt on the mutation = old updated_at (LWW correctness on re-sync).
	// When the source has a zero updated_at, fall back to createdAt.
	mutUpdatedAt := updatedAt
	if mutUpdatedAt.IsZero() {
		// DETERMINISTIC fallback chain — time.Now() here would change the
		// canonical payload (and so the mutation_id) on every re-import,
		// minting a fresh outbox entry each run and pushing duplicates to
		// central. createdAt first, then the epoch: same input, same mutation.
		mutUpdatedAt = createdAt
	}
	if mutUpdatedAt.IsZero() {
		mutUpdatedAt = time.Unix(0, 0).UTC()
	}

	return domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  o.SessionID,
		EntityType: domain.EntityMemory,
		Type:       o.Type,
		Title:      o.Title,
		Content:    o.Content,
		Project:    project,
		Scope:      scope,
		TopicKey:   topicKey,
		Version:    1,
		UpdatedAt:  mutUpdatedAt,
		OccurredAt: occurredAt,
		WriterID:   writerID,
	}
}

// ── Prompts ───────────────────────────────────────────────────────────────────

// srcPrompt is a row read from the source user_prompts table.
type srcPrompt struct {
	ID        int64
	SyncID    sql.NullString
	SessionID string
	Content   string
	Project   sql.NullString
	CreatedAt string
}

func (imp *Importer) importPrompts(srcDB *sql.DB, dryRun bool, st *Stats) error {
	rows, err := srcDB.Query(`
		SELECT id, sync_id, session_id, content, project, created_at
		FROM user_prompts
		ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return fmt.Errorf("query user_prompts: %w", err)
	}
	defer rows.Close()

	policyCache := make(map[string]localstore.Policy)

	for rows.Next() {
		var p srcPrompt
		if err := rows.Scan(&p.ID, &p.SyncID, &p.SessionID, &p.Content, &p.Project, &p.CreatedAt); err != nil {
			return fmt.Errorf("scan prompt: %w", err)
		}

		project := coalesceStr(p.Project, "")

		// Policy check.
		pol, err := imp.cachedPolicy(policyCache, project)
		if err != nil {
			return fmt.Errorf("policy for project %q: %w", project, err)
		}
		if pol == localstore.PolicyOmitted {
			st.PromptsOmitted++
			continue
		}

		syncID := deriveSyncID(p.SyncID, "import-prompt",
			p.Content, p.CreatedAt, p.SessionID)
		createdAt := parseImportTime(p.CreatedAt)

		if dryRun {
			if imp.promptExists(syncID) {
				st.PromptsSkipped++
			} else {
				st.PromptsImported++
			}
			continue
		}

		if imp.promptExists(syncID) {
			st.PromptsSkipped++
			continue
		}

		mutUpdatedAt := createdAt
		if mutUpdatedAt.IsZero() {
			// Deterministic (see the observation path note): never time.Now().
			mutUpdatedAt = time.Unix(0, 0).UTC()
		}

		m := domain.Mutation{
			Op:         domain.OpUpsert,
			SyncID:     syncID,
			SessionID:  p.SessionID,
			EntityType: domain.EntityPrompt,
			Content:    p.Content,
			Project:    project,
			Scope:      "project",
			Version:    1,
			UpdatedAt:  mutUpdatedAt,
			OccurredAt: mutUpdatedAt,
			WriterID:   imp.writerID,
		}
		if _, err := imp.dst.LocalWrite(m); err != nil {
			return fmt.Errorf("LocalWrite prompt id=%d sync_id=%q: %w", p.ID, syncID, err)
		}
		st.PromptsImported++
	}
	return rows.Err()
}

// promptExists reports whether a live user_prompts row already exists for syncID.
func (imp *Importer) promptExists(syncID string) bool {
	var n int
	err := imp.dst.DB().QueryRow(
		`SELECT COUNT(*) FROM user_prompts WHERE sync_id = ?`, syncID,
	).Scan(&n)
	return err == nil && n > 0
}

// ── helpers ───────────────────────────────────────────────────────────────────

// cachedPolicy returns the policy for project, using the local cache to avoid
// repeated DB round-trips.
func (imp *Importer) cachedPolicy(cache map[string]localstore.Policy, project string) (localstore.Policy, error) {
	if p, ok := cache[project]; ok {
		return p, nil
	}
	p, err := imp.dst.GetPolicy(project)
	if err != nil {
		return "", err
	}
	cache[project] = p
	return p, nil
}

// deriveSyncID returns the source sync_id when non-empty, otherwise builds a
// deterministic CONTENT-ADDRESSED id. The fallback must never be keyed on the
// source AUTOINCREMENT id alone: two different old machines both start ids at
// 1, so an integer-keyed fallback would make the second machine's rows collide
// with the first's and be silently skipped as "already present". Hashing the
// row's content makes the id unique per distinct record and still reproducible
// on re-import of the same source.
func deriveSyncID(src sql.NullString, prefix string, contentKey ...string) string {
	if src.Valid && strings.TrimSpace(src.String) != "" {
		return src.String
	}
	h := sha256.New()
	for _, part := range contentKey {
		h.Write([]byte(part))
		h.Write([]byte{0}) // unambiguous field separator
	}
	return fmt.Sprintf("%s-%x", prefix, h.Sum(nil)[:16])
}

// coalesceStr returns the string value of ns when valid, otherwise def.
func coalesceStr(ns sql.NullString, def string) string {
	if ns.Valid {
		return ns.String
	}
	return def
}

// parseImportTime parses a SQLite datetime string with the same layouts used by
// localstore.parseTime.  Returns the zero time on failure (caller handles it
// by substituting time.Now()).
func parseImportTime(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// OpenSourceReadOnly gives the importer a guaranteed-zero-risk view of the
// user's legacy database: the main file AND its WAL sidecars (-wal/-shm, when
// present) are first SNAPSHOT-COPIED into a private temp directory, and the
// COPY is opened. Rationale (round-1 review, proven by the sidecar-hash test):
//
//   - immutable=1 never touches the source but silently SKIPS WAL frames that
//     were never checkpointed (a crashed old daemon loses its last writes);
//   - mode=ro reads those frames but SQLite touches -shm/-wal next to the
//     user's original even for reads.
//
// Copy-then-read gets both: the original is never opened at all, and the
// copied -wal preserves every frame. Memory databases are megabytes — the
// copy cost is trivial against an irreplaceable source.
//
// The returned cleanup func removes the snapshot directory; callers must
// defer it after Close.
func OpenSourceReadOnly(srcPath string) (*sql.DB, func(), error) {
	if _, err := os.Stat(srcPath); err != nil {
		return nil, nil, fmt.Errorf("OpenSourceReadOnly: source %q: %w", srcPath, err)
	}

	tmpDir, err := os.MkdirTemp("", "engram-import-src-*")
	if err != nil {
		return nil, nil, fmt.Errorf("OpenSourceReadOnly: snapshot dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	copyPath := filepath.Join(tmpDir, "source.db")
	if err := copyFile(srcPath, copyPath); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("OpenSourceReadOnly: snapshot copy: %w", err)
	}
	// WAL sidecars: copied when present so non-checkpointed frames survive.
	// SQLite derives sidecar names by appending to the main path.
	for _, suffix := range []string{"-wal", "-shm"} {
		side := srcPath + suffix
		if _, err := os.Stat(side); err == nil {
			if err := copyFile(side, copyPath+suffix); err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("OpenSourceReadOnly: snapshot %s: %w", suffix, err)
			}
		}
	}

	// mode=ro on the COPY: WAL frames merged into the read view; any sidecar
	// touches land in the snapshot dir, never beside the user's original. The
	// path is percent-escaped for the characters that break SQLite's URI
	// parser — '%', '#', '?' and space (a space in a Windows profile path is
	// the common case).
	escaped := strings.NewReplacer(
		"%", "%25", "#", "%23", "?", "%3F", " ", "%20",
	).Replace(filepath.ToSlash(copyPath))
	dsn := fmt.Sprintf("file:%s?mode=ro", escaped)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("OpenSourceReadOnly: sql.Open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("OpenSourceReadOnly: ping %q: %w", srcPath, err)
	}
	return db, cleanup, nil
}

// copyFile copies src to dst (0600 — the snapshot may hold private memories).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

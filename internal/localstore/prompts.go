package localstore

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// ErrPromptNotFound is returned by prompt lookup helpers when no live row exists
// for the requested identity.
var ErrPromptNotFound = errors.New("prompt not found")

// Prompt is the materialized form of a user prompt stored in user_prompts.
type Prompt struct {
	// ID is the autoincrement integer primary key of the user_prompts row.
	ID int64
	// SyncID is the content-addressed sync identifier ("prompt-<hex>").
	SyncID string
	// SessionID is the MCP session that originated this prompt.
	SessionID string
	// Content is the prompt text.
	Content string
	// Project is the normalized project name.
	Project string
	// WriterID is the daemon identity that wrote the prompt.
	WriterID string
	// CreatedAt is the logical creation time (m.UpdatedAt at write time).
	CreatedAt time.Time
}

// AddPromptParams carries the caller-supplied fields for a new prompt write.
type AddPromptParams struct {
	// SessionID is the MCP session that originated this write.
	SessionID string
	// Content is the prompt text.
	Content string
	// Project is the normalized project name.
	Project string
	// WriterID is the node's writer identity — required for central push
	// (the HMAC forgery check rejects mutations with mismatched writer_id).
	WriterID string
}

// AddPrompt captures a new user prompt through the reconciliation-correct
// localWriteLocked path.  It mirrors AddObservation in structure:
//
//   - Acquires s.mu for the entire sequence (mutation build → write).
//   - Builds a domain.Mutation with Op=OpUpsert, EntityType=EntityPrompt,
//     a fresh SyncID, and the caller-supplied fields.
//   - Calls localWriteLocked (not the public LocalWrite) to avoid re-entrancy
//     deadlock on s.mu.
//   - Returns the Prompt with ID resolved from the committed user_prompts row.
//
// The mutation is routed through applyPromptTx (NOT applyTx / the memories
// path) by the EntityPrompt dispatch branch in localWriteLocked.  The outbox
// entry is enqueued atomically by localWriteLocked so the prompt is pushed to
// central on the next sync cycle.
func (s *Store) AddPrompt(p AddPromptParams) (Prompt, error) {
	p.Project = normalizeProject(p.Project)

	s.mu.Lock()
	defer s.mu.Unlock()

	m, err := s.addPromptLocked(p)
	if err != nil {
		return Prompt{}, err
	}

	// Resolve the committed row's integer PK and created_at.
	return resolvePromptRow(s.db, m.SyncID)
}

// AddPromptIfMissing is an idempotent capture helper: if a LIVE user_prompts
// row already exists for the same (session_id, project, content) triple it is
// returned without inserting a duplicate; otherwise a new row is written.
//
// Dedup is over LIVE rows only. If the prompt was previously DELETED (a tombstone
// exists, no live row), the same content is captured again as a NEW occurrence
// with a fresh sync_id — a re-prompt after a delete is a new event, not a
// resurrection. This matches old_code's AddPromptIfMissing semantics.
//
// Atomicity: the dedup-read AND the write are performed under a single s.mu
// acquisition (same lock held from check to commit) so no concurrent
// AddPrompt/AddPromptIfMissing/ApplyPulled can interleave between the check
// and the insert.  This mirrors AddObservation's read-modify-write pattern.
func (s *Store) AddPromptIfMissing(p AddPromptParams) (Prompt, error) {
	p.Project = normalizeProject(p.Project)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Dedup-read: look for an existing live row with the same identity.
	// The lock is already held so this read and any subsequent write are atomic.
	existing, err := findLivePromptByIdentity(s.db, p.SessionID, p.Project, p.Content)
	if err != nil {
		return Prompt{}, fmt.Errorf("AddPromptIfMissing: dedup check: %w", err)
	}
	if existing != nil {
		return *existing, nil
	}

	// No existing row — insert via the locked path.
	m, err := s.addPromptLocked(p)
	if err != nil {
		return Prompt{}, err
	}

	return resolvePromptRow(s.db, m.SyncID)
}

// addPromptLocked builds the domain.Mutation and calls localWriteLocked.
// Callers MUST hold s.mu before calling this method.
func (s *Store) addPromptLocked(p AddPromptParams) (domain.Mutation, error) {
	now := time.Now().UTC()
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     newPromptSyncID(),
		EntityType: domain.EntityPrompt,
		SessionID:  p.SessionID,
		Content:    p.Content,
		Project:    p.Project,
		Scope:      "project",
		Version:    1,
		UpdatedAt:  now,
		WriterID:   p.WriterID,
	}

	written, err := s.localWriteLocked(m)
	if err != nil {
		return domain.Mutation{}, fmt.Errorf("addPromptLocked: %w", err)
	}
	return written, nil
}

// findLivePromptByIdentity returns the first live user_prompts row matching
// (session_id, project, content), or nil if none exists.  Must be called
// while holding s.mu (or inside a transaction) to be safe.
func findLivePromptByIdentity(db *sql.DB, sessionID, project, content string) (*Prompt, error) {
	const q = `
		SELECT id, sync_id, session_id, content, project, writer_id, created_at
		FROM user_prompts
		WHERE session_id = ? AND project = ? AND content = ?
		ORDER BY id DESC
		LIMIT 1`
	return scanPromptRow(db.QueryRow(q, sessionID, project, content))
}

// resolvePromptRow fetches the committed Prompt by sync_id from the DB.
// Called after a localWriteLocked commit to surface the integer PK.
func resolvePromptRow(db *sql.DB, syncID string) (Prompt, error) {
	const q = `
		SELECT id, sync_id, session_id, content, project, writer_id, created_at
		FROM user_prompts
		WHERE sync_id = ?
		LIMIT 1`
	p, err := scanPromptRow(db.QueryRow(q, syncID))
	if err != nil {
		return Prompt{}, fmt.Errorf("resolvePromptRow: %w", err)
	}
	if p == nil {
		return Prompt{}, ErrPromptNotFound
	}
	return *p, nil
}

// scanPromptRow scans one Prompt from a *sql.Row.  Returns (nil, nil) on
// sql.ErrNoRows.
func scanPromptRow(row *sql.Row) (*Prompt, error) {
	var p Prompt
	var createdAtStr string
	err := row.Scan(
		&p.ID, &p.SyncID, &p.SessionID, &p.Content, &p.Project, &p.WriterID, &createdAtStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = parseTime(createdAtStr)
	return &p, nil
}

// GetPromptBySessionAndContent returns the most-recent live user_prompts row
// for the given (session_id, project, content) triple, or ErrPromptNotFound
// when none exists. It is a read-only helper intended for tests and handlers
// that need to verify a prompt was persisted.
func (s *Store) GetPromptBySessionAndContent(sessionID, project, content string) (Prompt, error) {
	p, err := findLivePromptByIdentity(s.db, sessionID, project, content)
	if err != nil {
		return Prompt{}, fmt.Errorf("GetPromptBySessionAndContent: %w", err)
	}
	if p == nil {
		return Prompt{}, ErrPromptNotFound
	}
	return *p, nil
}

// CountPromptsForSession returns the number of live user_prompts rows for the
// given (session_id, project, content) triple. Used in tests to assert dedup
// correctness.
func (s *Store) CountPromptsForSession(sessionID, project, content string) (int, error) {
	const q = `
		SELECT COUNT(*)
		FROM user_prompts
		WHERE session_id = ? AND project = ? AND content = ?`
	var n int
	if err := s.db.QueryRow(q, sessionID, project, content).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountPromptsForSession: %w", err)
	}
	return n, nil
}

// newPromptSyncID generates a random sync_id for a new prompt, using the same
// format as old_code: "prompt-<16 hex chars>".
func newPromptSyncID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: timestamp-based (practically unreachable on any modern OS).
		return fmt.Sprintf("prompt-%d", time.Now().UTC().UnixNano())
	}
	return "prompt-" + hex.EncodeToString(b)
}

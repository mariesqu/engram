package generation

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// hashEnvelope discriminates this hashing scheme from any future one: a
// scheme change bumps the envelope, so a v1 hash can never collide with a
// v2 hash by accident, and existing cache rows are unambiguously stale the
// moment the scheme itself changes.
const hashEnvelope = "engram-narrative-v1"

// SourceHash computes the content-hash cache key for one narrative unit
// (REQ-NARR-01). Two calls with the same logical inputs -- same template
// version, same model, same project/topic keys, and the same records'
// narratable state -- MUST produce the identical hash regardless of
// argument order or wall-clock time; that identity is what makes an
// unchanged corpus issue zero HTTP requests on the second narrative-loop
// cycle.
//
// Records are sorted by SyncID ascending, BYTE-WISE, before hashing --
// never by the store's own read order. RecentObservations orders
// "created_at DESC, id DESC" (internal/localstore/search.go) and that id
// tiebreak is replica-divergent across writer machines, so the hash must
// not depend on it.
//
// Deliberately EXCLUDED from the hash (see design #4754 §4 for the full
// rationale table):
//   - Record.ID -- replica-divergent local autoincrement
//     (internal/domain/memory.go:36-40), never part of the canonical
//     payload.
//   - wall-clock time -- would make every hash unique and defeat the cache
//     entirely.
//   - Record.CreatedAt -- redundant: SyncID + Content + UpdatedAt already
//     identify a record's narratable state.
//   - LastWriteMutationID -- replica-identical, but folding it in would
//     invalidate the cache on a no-op re-converge.
//   - budget knobs and vault path -- they change WHEN work happens or
//     WHERE output lands, never WHAT the output is.
//   - renderer_version -- a cosmetic vault-layout tweak must not force a
//     paid regeneration; writeIfChanged already catches render-level byte
//     changes for free (see TestSourceHashExcludesRendererVersion).
func SourceHash(templateVersion, modelName, projectKey, topicKey string, recs []*domain.Record) string {
	sorted := make([]*domain.Record, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SyncID < sorted[j].SyncID })

	fields := make([]string, 0, 6+5*len(sorted))
	fields = append(fields,
		hashEnvelope,
		templateVersion,
		modelName,
		projectKey,
		topicKey,
		strconv.Itoa(len(sorted)), // guards silent truncation of the record slice
	)
	for _, r := range sorted {
		fields = append(fields,
			r.SyncID,
			r.Title,
			r.Content,
			r.WriterID,
			r.UpdatedAt.UTC().Format(time.RFC3339Nano),
		)
	}

	sum := lengthPrefixedSHA256(fields)
	return hex.EncodeToString(sum[:])
}

// lengthPrefixedSHA256 hashes fields as an 8-byte big-endian length prefix
// followed by each field's raw bytes -- NEVER delimiter-joined. This is the
// single highest-stakes mechanism in the whole cache key.
//
// A strings.Join(fields, "|")-style implementation lets a "|" inside one
// field's content shift the field boundary onto an adjacent field, so two
// genuinely different field lists can hash identically: []string{"a|b"}
// (one field) and []string{"a", "b"} (two fields) both naively join to the
// identical string "a|b". Length-prefixing keeps them distinct because the
// byte stream itself differs (one 3-byte length-prefixed field vs. two
// 1-byte length-prefixed fields) -- see TestSourceHashLengthPrefixCollision.
//
// A collision here means a poisoned cache entry: a permanent hit silently
// serving wrong prose, forever.
func lengthPrefixedSHA256(fields []string) [32]byte {
	h := sha256.New()
	var lenBuf [8]byte
	for _, f := range fields {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(f)))
		h.Write(lenBuf[:])
		h.Write([]byte(f))
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

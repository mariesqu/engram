package obsidian

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SyncState is the on-disk incremental-sync record, persisted at
// "{vault}/engram/.engram-sync-state.json" (REQ-EXPORT-02, REQ-EXPORT-06).
//
// Files carries the observation id -> vault-relative-path map for every
// observation written by a prior export run. It is the ONLY deletion-
// detection mechanism available (REQ-EXPORT-09 / design constraint 5'):
// RecentObservations hard-filters "deleted_at IS NULL" and localstore
// exposes no tombstone-inclusive listing, so a soft-deleted (or otherwise
// no-longer-live) observation is detected by DIFFING this map against the
// current live listing, not by querying for it directly.
// Hubs carries the hub identity ("_sessions/{session-id}" /
// "_topics/{prefix}") -> vault-relative-path map for every hub note written
// by a prior export run (REQ-EXPORT-09's NEW clause, closes deferred
// MAJOR-4). Like Files, it is a deletion-detection mechanism: a session
// whose every observation has vanished, or a topic prefix that has dropped
// below the 2-ref threshold, is caught by diffing this map against the
// current run's rendered membership, not by any direct query.
//
// A state file written before this field existed has no "hubs" key at all;
// json.Unmarshal leaves it nil, and ReadState normalises that to an empty
// map exactly as it already does for Files — so the upgrade is backward
// compatible with NO migration, and the first post-upgrade cycle sweeps
// nothing, because nothing is tracked yet to compare against.
//
// Every value in Files and Hubs MUST use forward slashes
// (bugfix/obsidian-wikilink-path-separator): this file travels with the
// git-synced vault and is read by the Phase 4 viewer plugin on every
// platform, and Files/Hubs values are the same strings vaultRelPath and the
// hub-path construction in Export() render into wikilinks -- a persisted
// backslash is exactly as unresolvable as a rendered one. ReadState
// normalises every value to forward slashes the instant the file is
// loaded, so a state file written by a pre-fix build (or by a different
// OS) is upgraded in place with no migration step and no mass
// deletion/re-export; WriteState never needs to convert anything, because
// only already-normalised values are ever assigned into these maps.
type SyncState struct {
	LastExportAt time.Time         `json:"last_export_at"`
	Files        map[int64]string  `json:"files"`
	Hubs         map[string]string `json:"hubs"`
}

// ExportResult is the summary of one export cycle — the fields backing the
// REQ-EXPORT-11 "sync: created=<n> updated=<n> deleted=<n> skipped=<n>
// hubs=<n>" stdout line emitted by both single-run and watch-mode cycles.
type ExportResult struct {
	Created int
	Updated int
	Deleted int
	Skipped int
	Hubs    int
}

// Summary renders r in the exact REQ-EXPORT-11 wire format. This is the
// contract the Obsidian plugin parses to drive its status bar — the format
// must not change without updating both sides.
func (r ExportResult) Summary() string {
	return fmt.Sprintf("sync: created=%d updated=%d deleted=%d skipped=%d hubs=%d",
		r.Created, r.Updated, r.Deleted, r.Skipped, r.Hubs)
}

// ReadState reads the sync state at path. A missing file is NOT an error —
// it is the normal first-run case — and yields an empty, initialized state
// (zero LastExportAt, empty Files map) so callers never need a nil check.
func ReadState(path string) (*SyncState, error) {
	data, err := readFileWithRetry(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &SyncState{Files: map[int64]string{}, Hubs: map[string]string{}}, nil
		}
		return nil, fmt.Errorf("obsidian: read state %s: %w", path, err)
	}

	var st SyncState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("obsidian: parse state %s: %w", path, err)
	}
	if st.Files == nil {
		st.Files = map[int64]string{}
	}
	if st.Hubs == nil {
		st.Hubs = map[string]string{}
	}

	// Normalise every persisted value to forward slashes NOW, at the one
	// boundary where an on-disk state file re-enters memory
	// (bugfix/obsidian-wikilink-path-separator). A file written by a
	// pre-fix build of this package -- or by a Windows process, later read
	// on a git-synced macOS/Linux checkout -- can carry OS-native-backslash
	// values; toVaultSlash (NOT filepath.ToSlash, which is a no-op on any
	// OS whose own separator is already '/') converts them unconditionally,
	// so every value already in memory compares equal to the
	// forward-slash-only paths vaultJoin now always constructs. This is
	// what lets a legacy state file upgrade in place with no migration step
	// and no mass deletion/re-export: the very first post-upgrade cycle
	// sees prev == relPath for anything unchanged, instead of a spurious
	// separator mismatch that would otherwise force every tracked
	// observation through a full re-evaluation.
	for id, p := range st.Files {
		st.Files[id] = toVaultSlash(p)
	}
	for k, p := range st.Hubs {
		st.Hubs[k] = toVaultSlash(p)
	}

	return &st, nil
}

// readFileWithRetry reads path, retrying briefly on any error other than
// "does not exist".
//
// This is the read-side twin of renameWithRetry and exists for the same
// measured Windows behaviour: while MoveFileEx is replacing the state file,
// a concurrent open of the destination fails with ERROR_SHARING_VIOLATION
// ("The process cannot access the file because it is being used by another
// process"). The bytes are never torn — an atomic rename guarantees that —
// but the open can transiently fail, and the exporter reads its own state
// file once at the start of EVERY cycle. Without this, a watch-mode cycle
// could fail outright because Obsidian, a sync client or an antivirus
// scanner happened to hold the file open for a millisecond.
//
// "Does not exist" is returned immediately and never retried: that is the
// normal first-run case, and delaying it would add ~200 ms to every export
// into a fresh vault.
func readFileWithRetry(path string) ([]byte, error) {
	const attempts = 10
	delay := time.Millisecond
	var (
		data []byte
		err  error
	)
	for i := 0; i < attempts; i++ {
		data, err = os.ReadFile(path)
		if err == nil || os.IsNotExist(err) {
			return data, err
		}
		time.Sleep(delay)
		if delay < 50*time.Millisecond {
			delay *= 2
		}
	}
	return data, err
}

// WriteState writes st to path as indented JSON, creating the parent
// directory if it does not already exist.
//
// The write is ATOMIC: the JSON is staged in a temporary file in the same
// directory, flushed, and then moved into place with a single os.Rename.
// A bare os.WriteFile would open the destination with O_TRUNC and stream
// bytes into it, leaving the file observably empty or half-written for the
// duration — and this particular file has two readers who cannot tolerate
// that. The Obsidian viewer polls it every 60 s (REQ-PLUGIN-06), and the
// exporter's own next run parses it, where an unparseable state file is a
// hard error. Staging in the SAME directory matters: rename is only atomic
// within a filesystem.
func WriteState(path string, st *SyncState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("obsidian: marshal state: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("obsidian: create state dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("obsidian: stage state %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// Cleans up the staging file on every failure path below. After a
	// successful rename the staging name no longer exists and this is a
	// harmless no-op.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("obsidian: write state %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("obsidian: flush state %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("obsidian: close state %s: %w", path, err)
	}
	// os.CreateTemp creates with 0600; match the 0644 the vault's other
	// files are written with.
	if err := os.Chmod(tmpName, 0644); err != nil {
		return fmt.Errorf("obsidian: set state permissions %s: %w", path, err)
	}
	if err := renameWithRetry(tmpName, path); err != nil {
		return fmt.Errorf("obsidian: commit state %s: %w", path, err)
	}
	return nil
}

// renameWithRetry replaces dst with src, retrying briefly on failure.
//
// The retry exists for Windows and it is not theoretical. Replacing a file
// there requires DELETE access to the destination, which is refused with
// ERROR_ACCESS_DENIED / ERROR_SHARING_VIOLATION while any other process has
// it open without FILE_SHARE_DELETE. This file is read every 60 s by the
// Obsidian plugin (REQ-PLUGIN-06) and is a routine target for sync clients,
// backup agents and antivirus scanners — none of whose share flags we
// control. Without the retry, the atomic write trades a rare torn file for
// a much more frequent failed export cycle, and under watch mode
// (REQ-WATCH-08) a failed cycle must not advance the timestamp, so every
// collision would cost a full interval.
//
// Errors are not inspected: the errno constants involved are
// Windows-specific and would need build-tagged files, while the cost of
// retrying a genuinely fatal error is one bounded ~200 ms delay on a write
// that happens once per export cycle.
func renameWithRetry(src, dst string) error {
	const attempts = 10
	delay := time.Millisecond
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(delay)
		if delay < 50*time.Millisecond {
			delay *= 2
		}
	}
	return err
}

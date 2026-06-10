//go:build windows

package controlapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

// DaemonJSON is the discovery file written by the resident daemon at startup.
// It lets the CLI and tray find the daemon's port and bearer token without
// requiring an environment variable.
//
// File location: <db-dir>/daemon.json
// Permissions:   user-only DACL (Windows) / 0600 (non-Windows)
// Atomicity:     written to a temp file in the same directory, then renamed.
// Lifecycle:     written on daemon start, removed on clean shutdown.
type DaemonJSON struct {
	Port      int    `json:"port"`
	Token     string `json:"token"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// WriteDaemonJSON atomically writes a DaemonJSON file to dir/daemon.json.
// On Windows the file DACL is set to grant access only to the current user
// (no inherited ACEs) via SetNamedSecurityInfo. The write is atomic: a temp
// file is written first, then renamed over the target.
func WriteDaemonJSON(dir, token string, port, pid int) error {
	d := DaemonJSON{
		Port:      port,
		Token:     token,
		PID:       pid,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("controlapi.WriteDaemonJSON: marshal: %w", err)
	}

	target := filepath.Join(dir, "daemon.json")
	tmp := target + ".tmp"

	// Write to temp file with restrictive permissions, sync, then rename.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("controlapi.WriteDaemonJSON: create temp: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("controlapi.WriteDaemonJSON: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("controlapi.WriteDaemonJSON: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("controlapi.WriteDaemonJSON: close: %w", err)
	}
	// Apply the user-only DACL to the temp file BEFORE the rename so the file is
	// never visible at the target path with the directory's inherited (broader)
	// DACL. On Windows the 0o600 passed to OpenFile does NOT restrict access —
	// only the DACL does. Non-fatal: if the DACL cannot be applied the file keeps
	// the inherited directory DACL (degraded; single-user machines unaffected).
	_ = setUserOnlyACL(tmp)
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("controlapi.WriteDaemonJSON: rename: %w", err)
	}
	return nil
}

// setUserOnlyACL sets the file DACL to grant Full Control to the current user
// only, removing inherited ACEs. Uses golang.org/x/sys/windows.
func setUserOnlyACL(path string) error {
	// Get current process token to find the user SID.
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("setUserOnlyACL: OpenProcessToken: %w", err)
	}
	defer token.Close()

	tokenUser, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("setUserOnlyACL: GetTokenUser: %w", err)
	}
	userSID := tokenUser.User.Sid

	// Build an explicit ACE granting Full Control (GENERIC_ALL) to the current user.
	// ACLFromEntries is the higher-level wrapper around SetEntriesInAclW.
	access := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(userSID),
		},
	}

	// ACLFromEntries returns an ACL allocated on the Go heap — no LocalFree needed.
	newACL, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{access}, nil)
	if err != nil {
		return fmt.Errorf("setUserOnlyACL: ACLFromEntries: %w", err)
	}

	// SetNamedSecurityInfo takes the path as a plain string (the syscall wrapper
	// handles UTF-16 conversion internally). SE_DACL_PROTECTED removes inherited ACEs.
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, // owner SID (unchanged)
		nil, // group SID (unchanged)
		newACL,
		nil, // SACL (unchanged)
	); err != nil {
		return fmt.Errorf("setUserOnlyACL: SetNamedSecurityInfo: %w", err)
	}
	return nil
}

// ReadDaemonJSON reads and parses the daemon.json file from dir.
// Returns an error wrapping os.ErrNotExist when the file does not exist
// (no daemon running) — callers should check errors.Is(err, os.ErrNotExist).
func ReadDaemonJSON(dir string) (DaemonJSON, error) {
	path := filepath.Join(dir, "daemon.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return DaemonJSON{}, fmt.Errorf("controlapi.ReadDaemonJSON: %w", err)
	}
	var d DaemonJSON
	if err := json.Unmarshal(b, &d); err != nil {
		return DaemonJSON{}, fmt.Errorf("controlapi.ReadDaemonJSON: parse: %w", err)
	}
	return d, nil
}

// RemoveDaemonJSON removes the daemon.json file from dir. Called on clean
// daemon shutdown. A non-existent file is silently ignored.
func RemoveDaemonJSON(dir string) error {
	path := filepath.Join(dir, "daemon.json")
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("controlapi.RemoveDaemonJSON: %w", err)
	}
	return nil
}

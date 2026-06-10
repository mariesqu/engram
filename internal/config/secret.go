package config

// SecretBox is defined in config.go as the platform-specific encryption
// interface for the writer key.
//
// This file is intentionally empty — the interface declaration lives in
// config.go alongside the types that reference it. It is kept as a separate
// file per the design document's package layout for clarity.
//
// Implementations:
//   - secret_windows.go  (//go:build windows)  — DPAPI via x/sys/windows
//   - secret_other.go    (//go:build !windows) — env-only no-op

//go:build !race

package localstore

// raceEnabled reports whether this test binary was built with -race.
//
// modernc.org/sqlite is SQLite transpiled to Go, so under -race every memory
// access the SQLite VM makes is instrumented. That is a ~20-40x slowdown on
// query execution (measured on the 300-session / 20k-memory scale fixture:
// FormatContext ~65ms plain vs 1.7-2.8s with -race), which is large enough
// that a wall-clock budget sized for a normal build fails on every -race run.
// Tests that keep a timing backstop scale it by raceBudgetMultiplier instead
// of dropping it.
const raceEnabled = false

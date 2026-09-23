//go:build race

package localstore

// raceEnabled reports whether this test binary was built with -race. See
// race_disabled_test.go for why wall-clock budgets need to know.
const raceEnabled = true

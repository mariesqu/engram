package syncer

import (
	"testing"
	"time"
)

// TestApplyDefaults_BackoffMaxNeverBelowMin proves applyDefaults normalizes a
// misconfigured cap (BackoffMin > BackoffMax) by raising BackoffMax to BackoffMin,
// so the first backoff (= BackoffMin) can never exceed the documented cap.
func TestApplyDefaults_BackoffMaxNeverBelowMin(t *testing.T) {
	got := applyDefaults(Config{
		BackoffMin: 5 * time.Second,
		BackoffMax: 1 * time.Second, // misconfigured: below the floor
	})
	if got.BackoffMax < got.BackoffMin {
		t.Errorf("BackoffMax (%v) < BackoffMin (%v): cap below floor", got.BackoffMax, got.BackoffMin)
	}
	if got.BackoffMax != 5*time.Second {
		t.Errorf("BackoffMax = %v; want it normalized up to BackoffMin (5s)", got.BackoffMax)
	}
}

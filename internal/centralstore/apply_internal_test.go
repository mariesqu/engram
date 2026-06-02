// Package centralstore — unit tests for unexported helpers in apply.go.
// No build tag: these run in the plain "go test ./..." unit loop (no Postgres
// required, no embedded-postgres started). They are the fast deterministic
// complement to the acceptance-tagged concurrent test.
package centralstore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsUniqueViolation verifies the isUniqueViolation predicate against every
// relevant error shape. This directly covers the handler at apply.go:88-92 in a
// deterministic, dependency-free way.
func TestIsUniqueViolation(t *testing.T) {
	t.Parallel()

	pgErr23505 := &pgconn.PgError{Code: "23505"} // unique_violation
	pgErr23514 := &pgconn.PgError{Code: "23514"} // check_violation — a different SQLSTATE

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "23505 PgError direct",
			err:  pgErr23505,
			want: true,
		},
		{
			name: "23505 PgError wrapped via fmt.Errorf %w",
			err:  fmt.Errorf("insert mutation: %w", pgErr23505),
			want: true, // errors.As must unwrap through the wrapping chain
		},
		{
			name: "23514 PgError (different SQLSTATE)",
			err:  pgErr23514,
			want: false,
		},
		{
			name: "plain error not a PgError",
			err:  errors.New("boom"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isUniqueViolation(tc.err)
			if got != tc.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

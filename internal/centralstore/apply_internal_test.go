// Package centralstore — unit tests for unexported helpers in apply.go.
// No build tag: these run in the plain "go test ./..." unit loop (no Postgres
// required, no embedded-postgres started). They are the fast deterministic
// complement to the acceptance-tagged concurrent test.
package centralstore

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mariesqu/engram/internal/transport"
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

// TestPermanentDataError verifies the classifier FUP-004's 422 mapping relies
// on, against fake pgconn errors — no Postgres required. It covers every
// SQLSTATE class the contract cares about, the constraint/column/fallback
// message shapes, the 23505 carve-out (already handled elsewhere as a no-op),
// and — the part that actually protects the leak contract — that the returned
// message NEVER contains a raw Message/Detail/Hint value, only structured
// identifiers.
func TestPermanentDataError(t *testing.T) {
	t.Parallel()

	secretDetail := "Key (project)=(super-secret-project) already exists"

	cases := []struct {
		name       string
		err        error
		wantNil    bool
		wantSubstr string // required substring in the returned error's message
	}{
		{
			name: "22P02 invalid_text_representation with constraint name",
			err: &pgconn.PgError{
				Code: "22P02", ConstraintName: "memories_entity_type_check",
				Message: "leaked", Detail: secretDetail, Hint: "leaked hint",
			},
			wantSubstr: `constraint "memories_entity_type_check"`,
		},
		{
			name: "23514 check_violation with column name, no constraint",
			err: &pgconn.PgError{
				Code: "23514", ColumnName: "status",
				Message: "leaked", Detail: secretDetail,
			},
			wantSubstr: `field "status"`,
		},
		{
			name:       "23000 class with neither constraint nor column",
			err:        &pgconn.PgError{Code: "23000", Message: "leaked", Detail: secretDetail},
			wantSubstr: "SQLSTATE 23000",
		},
		{
			name:    "23505 unique_violation is NOT classified here — isUniqueViolation owns it",
			err:     &pgconn.PgError{Code: "23505", ConstraintName: "central_mutations_mutation_id_key"},
			wantNil: true,
		},
		{
			name:    "08006 connection_failure (class 08) is retryable, not permanent",
			err:     &pgconn.PgError{Code: "08006"},
			wantNil: true,
		},
		{
			name: "wrapped via fmt.Errorf %w — errors.As must unwrap through the chain",
			err: fmt.Errorf("insert mutation: %w",
				&pgconn.PgError{Code: "22001", ConstraintName: "title_length", Detail: secretDetail}),
			wantSubstr: `constraint "title_length"`,
		},
		{name: "plain error not a PgError", err: errors.New("boom"), wantNil: true},
		{name: "nil error", err: nil, wantNil: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := permanentDataError("ctx", tc.err)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("permanentDataError(%v) = %v, want nil", tc.err, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("permanentDataError(%v) = nil, want a wrapped error", tc.err)
			}
			if !errors.Is(got, transport.ErrPermanent) {
				t.Errorf("errors.Is(got, transport.ErrPermanent) = false; got: %v", got)
			}
			msg := got.Error()
			if !strings.Contains(msg, tc.wantSubstr) {
				t.Errorf("message %q does not contain %q", msg, tc.wantSubstr)
			}
			if strings.Contains(msg, "leaked") || strings.Contains(msg, secretDetail) {
				t.Errorf("message %q leaks raw Postgres Message/Detail/Hint text", msg)
			}
		})
	}
}

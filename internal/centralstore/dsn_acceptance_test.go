//go:build acceptance

package centralstore_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// withSearchPath returns a DSN that has its PostgreSQL search_path set to
// "<schema>,public".
//
// Two DSN forms are handled:
//   - URL-form (scheme "postgres://" or "postgresql://"): the "options" query
//     parameter is set to "-c search_path=<schema>,public" (URL-encoded) and
//     the URL is re-encoded and returned.
//   - Keyword/value form (everything else): the options string is appended as a
//     space-separated key=value pair — the format produced by embedded-postgres
//     and accepted by pgx.
//
// An error is returned only when the URL-form DSN cannot be parsed by net/url.
func withSearchPath(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("withSearchPath: parse DSN: %w", err)
		}
		q := u.Query()
		q.Set("options", fmt.Sprintf("-c search_path=%s,public", schema))
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	// Keyword/value form — append options key=value pair.
	return fmt.Sprintf("%s options='-c search_path=%s,public'", dsn, schema), nil
}

// ── Unit tests for withSearchPath ─────────────────────────────────────────────

// TestWithSearchPath verifies that withSearchPath produces a correct DSN for
// both URL-form and keyword/value-form inputs.  No Postgres connection is
// required — the test only asserts the produced string.
func TestWithSearchPath(t *testing.T) {
	tests := []struct {
		name       string
		dsn        string
		schema     string
		wantURL    bool   // if true, result must re-parse and have correct options param
		wantSuffix string // if non-empty (kv form), result must contain this substring
	}{
		{
			name:    "URL form postgres://",
			dsn:     "postgres://engram:engram@localhost:5432/engram_test?sslmode=disable",
			schema:  "t_mytest",
			wantURL: true,
		},
		{
			name:    "URL form postgresql://",
			dsn:     "postgresql://user:pass@db.example.com:5432/mydb",
			schema:  "t_other",
			wantURL: true,
		},
		{
			name:       "keyword/value form",
			dsn:        "host=localhost port=5432 user=engram password=engram dbname=engram_test sslmode=disable",
			schema:     "t_kvtest",
			wantSuffix: "options='-c search_path=t_kvtest,public'",
		},
		{
			name:       "keyword/value form no trailing space",
			dsn:        "host=localhost port=5432 user=engram dbname=engram_test sslmode=disable",
			schema:     "t_abc",
			wantSuffix: "options='-c search_path=t_abc,public'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := withSearchPath(tt.dsn, tt.schema)
			if err != nil {
				t.Fatalf("withSearchPath(%q, %q): unexpected error: %v", tt.dsn, tt.schema, err)
			}

			if tt.wantURL {
				// Result must be parseable and carry the correct options param.
				u, err := url.Parse(got)
				if err != nil {
					t.Fatalf("result DSN does not parse as URL: %v — got: %q", err, got)
				}
				opts := u.Query().Get("options")
				want := fmt.Sprintf("-c search_path=%s,public", tt.schema)
				if opts != want {
					t.Errorf("options param = %q, want %q", opts, want)
				}
				// Result must also contain the schema name somewhere in the options.
				if !strings.Contains(opts, "search_path="+tt.schema) {
					t.Errorf("options %q does not contain search_path=%s", opts, tt.schema)
				}
			} else {
				// Keyword/value: result must contain the expected suffix.
				if !strings.Contains(got, tt.wantSuffix) {
					t.Errorf("result %q does not contain expected suffix %q", got, tt.wantSuffix)
				}
			}
		})
	}
}

// TestWithSearchPath_InvalidURL verifies that a malformed URL-form DSN returns
// an error rather than silently producing a broken string.
func TestWithSearchPath_InvalidURL(t *testing.T) {
	// A scheme-prefixed DSN with an unparseable host trips url.Parse.
	badDSN := "postgres://user:pass@[::1]:namedport/db" // invalid port
	_, err := withSearchPath(badDSN, "myschema")
	if err == nil {
		t.Error("expected error for malformed URL DSN, got nil")
	}
}

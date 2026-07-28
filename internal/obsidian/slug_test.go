package obsidian

import "testing"

// TestSlugify covers the REQ-EXPORT-07 slug rule: lowercase, hyphenated,
// truncated to the first 40 characters of the source content. The numeric
// observation id is appended by the caller, not by Slugify itself.
func TestSlugify(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple lowercase sentence",
			input: "hello world",
			want:  "hello-world",
		},
		{
			name:  "mixed case folds to lowercase",
			input: "Hello World",
			want:  "hello-world",
		},
		{
			name:  "punctuation collapses to hyphens",
			input: "Hello, World! This is a test.",
			want:  "hello-world-this-is-a-test",
		},
		{
			name:  "repeated separators collapse to one hyphen",
			input: "foo   bar--baz",
			want:  "foo-bar-baz",
		},
		{
			name:  "truncates to the first 40 characters before slugifying",
			input: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 50 'a's
			want:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",           // 40 'a's
		},
		{
			name:  "leading and trailing separators are trimmed",
			input: "  --hello--  ",
			want:  "hello",
		},
		{
			name:  "empty string produces empty slug",
			input: "",
			want:  "",
		},
		{
			name:  "all non-alphanumeric input produces empty slug",
			input: "!!!@@@###",
			want:  "",
		},
		{
			name:  "non-ASCII runes are treated as separators",
			input: "héllo wörld",
			want:  "h-llo-w-rld",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Slugify(tc.input)
			if got != tc.want {
				t.Errorf("Slugify(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

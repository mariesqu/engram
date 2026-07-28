package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// wikilinkTargetPattern matches an Obsidian wikilink "[[target]]" or
// "[[target|title]]" and captures target.
var wikilinkTargetPattern = regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)

// extractWikilinkTargets returns every wikilink target found in content, in
// the order they appear.
func extractWikilinkTargets(content string) []string {
	matches := wikilinkTargetPattern.FindAllStringSubmatch(content, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// hubFiles returns every hub note ("_sessions/*.md", "_topics/*.md") written
// under vault's "engram/" namespace.
func hubFiles(t *testing.T, vault string) []string {
	t.Helper()
	var out []string
	for _, sub := range []string{"_sessions", "_topics"} {
		dir := filepath.Join(vault, "engram", sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("ReadDir(%s): %v", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	return out
}

// resolveObsidianWikilink resolves target against vault the way Obsidian
// resolves a vault-root-relative wikilink: split ONLY on '/', never on '\',
// because Obsidian's own link index is built by walking the vault and
// normalizing to forward slashes internally, regardless of the host OS --
// a backslash reaching Obsidian's resolver is an ordinary, literal filename
// character, not a separator.
//
// This deliberately does NOT hand the raw target straight to
// filepath.Join/os.Stat: on Windows, both natively treat '\' as a path
// separator, so a target carrying the OS-native-backslash defect
// (bugfix/obsidian-wikilink-path-separator) would still "resolve" to the
// real nested file when checked with Go's own path functions on the very
// platform that produced it -- exactly the false confidence a bare
// strings.Contains gave the adversarial review that missed this bug
// originally (verify-report #4710, MAJOR-5: "nothing tests link
// resolution, only strings.Contains"). Any segment that still contains a
// backslash after splitting on '/' can never correspond to a real exported
// file: safeFilename (path.go) maps a literal backslash in any
// project/type/session-id/topic-prefix value to '-' before it ever reaches
// a path segment, so a backslash surviving into a rendered target is
// unconditionally a separator artifact, and such a link is reported
// unresolvable without even touching the filesystem.
func resolveObsidianWikilink(vault, target string) (resolved string, ok bool) {
	segments := strings.Split(target, "/")
	for _, seg := range segments {
		if strings.Contains(seg, `\`) {
			return "", false
		}
	}
	elems := append([]string{vault}, segments...)
	resolved = filepath.Join(elems...) + ".md"
	_, err := os.Stat(resolved)
	return resolved, err == nil
}

// buildLinkResolutionFixture returns a mockStore with 2 projects, 3 sessions
// and a shared topic prefix (>= 2 refs, so both a session and a topic hub
// get written), realistic enough that Export() renders both hub kinds.
func buildLinkResolutionFixture() *mockStore {
	ts := time.Now().Add(-time.Hour)
	byProject := map[string][]*domain.Record{"proj-a": {}, "proj-b": {}}
	projects := []string{"proj-a", "proj-b"}
	sessions := []string{"sess-1", "sess-2", "sess-3"}
	types := []string{"architecture", "bugfix", "decision", "pattern"}

	var id int64
	for i := 0; i < 12; i++ {
		id++
		proj := projects[i%len(projects)]
		sess := sessions[i%len(sessions)]
		typ := types[i%len(types)]
		rec := newTestRecord(id, proj, typ, sess, fmt.Sprintf("Observation body number %d", id), ts)
		if id == 5 || id == 6 {
			topic := "sdd/resolution-check/spec"
			rec.TopicKey = &topic
		}
		byProject[proj] = append(byProject[proj], rec)
	}
	return &mockStore{listProjects: projects, byProject: byProject}
}

// TestHubWikilinksUseForwardSlashes covers
// bugfix/obsidian-wikilink-path-separator: Obsidian resolves wikilinks with
// FORWARD SLASHES on every platform -- a backslash is a literal filename
// character, not a separator. vaultRelPath (and the hub-file path
// construction in Export()) used a bare filepath.Join, which emits
// OS-native separators; on Windows that string was rendered VERBATIM into
// every hub -> note wikilink, producing a link Obsidian cannot resolve on
// the very platform that wrote it. This assertion is exercised end to end
// against THIS host's real filepath.Join behaviour, not a synthetic
// cross-platform simulation, so it fails for the right reason on a Windows
// CI/dev machine without the fix.
func TestHubWikilinksUseForwardSlashes(t *testing.T) {
	vault := t.TempDir()
	store := buildLinkResolutionFixture()
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	hubs := hubFiles(t, vault)
	if len(hubs) == 0 {
		t.Fatal("no hub files were written; the fixture must produce at least one for this test to cover anything")
	}

	var totalLinks int
	var backslashLinks []string
	for _, hub := range hubs {
		content, err := os.ReadFile(hub)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", hub, err)
		}
		for _, target := range extractWikilinkTargets(string(content)) {
			totalLinks++
			if strings.Contains(target, `\`) {
				backslashLinks = append(backslashLinks, hub+" -> "+target)
			}
		}
	}
	if totalLinks == 0 {
		t.Fatal("no wikilinks found in any hub file; the fixture must produce at least one for this test to cover anything")
	}
	if len(backslashLinks) != 0 {
		t.Errorf("%d of %d rendered wikilinks contain a backslash (Obsidian requires forward slashes on every platform):\n%s",
			len(backslashLinks), totalLinks, strings.Join(backslashLinks, "\n"))
	}
}

// TestHubWikilinksResolveToExistingFiles is the test class the adversarial
// review found missing (verify-report #4710, MAJOR-5: "nothing tests link
// resolution, only strings.Contains"). A substring assertion passes
// regardless of path separator -- exactly how the OS-native-backslash
// defect shipped undetected. This test resolves every wikilink target a
// rendered hub emits the way Obsidian resolves a vault-root-relative link
// (forward-slash, joined onto the vault root -- see
// resolveObsidianWikilink) and asserts the resulting file ACTUALLY EXISTS
// on disk after a real export. This is permanent scaffolding: any future
// change that reintroduces a native-separator, or otherwise unresolvable,
// hub link fails it immediately.
func TestHubWikilinksResolveToExistingFiles(t *testing.T) {
	vault := t.TempDir()
	store := buildLinkResolutionFixture()
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	hubs := hubFiles(t, vault)
	if len(hubs) == 0 {
		t.Fatal("no hub files were written; the fixture must produce at least one for this test to cover anything")
	}

	var checked int
	var missing []string
	for _, hub := range hubs {
		content, err := os.ReadFile(hub)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", hub, err)
		}
		for _, target := range extractWikilinkTargets(string(content)) {
			checked++
			if resolved, ok := resolveObsidianWikilink(vault, target); !ok {
				missing = append(missing, fmt.Sprintf("%q -> %q", target, resolved))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no wikilinks found in any hub file; the fixture must produce at least one for this test to cover anything")
	}
	if len(missing) != 0 {
		t.Errorf("%d of %d rendered wikilink targets do not resolve to an existing file:\n%s",
			len(missing), checked, strings.Join(missing, "\n"))
	}
}

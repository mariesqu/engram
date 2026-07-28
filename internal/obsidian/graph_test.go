package obsidian

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseGraphConfigMode covers REQ-GRAPH-01: the three accepted values,
// rejection of anything else with an error naming all three, and the
// EMPTY-STRING special case, which resolves to GraphConfigSkip rather than
// an error — the CLI's --graph-config flag defaults ITS OWN unset value to
// "preserve" and the daemon's obsidian_graph_config config key defaults ITS
// OWN unset value to "preserve" too, so an ExportConfig.GraphConfig that
// nobody ever set must be inert rather than silently write a file the
// moment Export() runs (design decision "GraphConfig empty-string default").
func TestParseGraphConfigMode(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    GraphConfigMode
		wantErr bool
	}{
		{name: "preserve", in: "preserve", want: GraphConfigPreserve},
		{name: "force", in: "force", want: GraphConfigForce},
		{name: "skip", in: "skip", want: GraphConfigSkip},
		{name: "empty string resolves to skip, not an error", in: "", want: GraphConfigSkip},
		{name: "unknown value is an error", in: "bogus", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGraphConfigMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseGraphConfigMode(%q) error = nil, want an error", tc.in)
				}
				for _, want := range []string{"preserve", "force", "skip"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("ParseGraphConfigMode(%q) error = %q, want it to name %q", tc.in, err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGraphConfigMode(%q) error = %v, want nil", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseGraphConfigMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEmbeddedGraphTemplate pins the locked-in graph.json (source #1226)
// exactly: exactly 6 colour groups (NO discovery group), the exact query
// strings, the exact rgb integers with alpha=1 for every group, showArrow
// false, textFadeMultiplier 0, both collapse flags, and the four force
// constants plus linkDistance/scale — the requirement is "do not re-tune,
// re-order, round, or improve any value" (REQ-GRAPH-05), so this test
// compares literal values rather than ranges or "close enough" checks.
func TestEmbeddedGraphTemplate(t *testing.T) {
	var doc struct {
		CollapseFilter      bool `json:"collapse-filter"`
		ShowTags            bool `json:"showTags"`
		ShowAttachments     bool `json:"showAttachments"`
		HideUnresolved      bool `json:"hideUnresolved"`
		ShowOrphans         bool `json:"showOrphans"`
		CollapseColorGroups bool `json:"collapse-color-groups"`
		ColorGroups         []struct {
			Query string `json:"query"`
			Color struct {
				A   float64 `json:"a"`
				RGB int64   `json:"rgb"`
			} `json:"color"`
		} `json:"colorGroups"`
		CollapseDisplay    bool    `json:"collapse-display"`
		ShowArrow          bool    `json:"showArrow"`
		TextFadeMultiplier float64 `json:"textFadeMultiplier"`
		NodeSizeMultiplier float64 `json:"nodeSizeMultiplier"`
		LineSizeMultiplier float64 `json:"lineSizeMultiplier"`
		CollapseForces     bool    `json:"collapse-forces"`
		CenterStrength     float64 `json:"centerStrength"`
		RepelStrength      float64 `json:"repelStrength"`
		LinkStrength       float64 `json:"linkStrength"`
		LinkDistance       float64 `json:"linkDistance"`
		Scale              float64 `json:"scale"`
		Close              bool    `json:"close"`
	}
	if err := json.Unmarshal(defaultGraphTemplate, &doc); err != nil {
		t.Fatalf("json.Unmarshal(defaultGraphTemplate) error = %v", err)
	}

	if len(doc.ColorGroups) != 6 {
		t.Fatalf("len(colorGroups) = %d, want exactly 6", len(doc.ColorGroups))
	}

	wantGroups := []struct {
		query string
		rgb   int64
	}{
		{"path:engram/_sessions", 14736466},
		{"path:engram/_topics", 13893887},
		{"tag:#architecture", 7935},
		{"tag:#bugfix", 16711680},
		{"tag:#decision", 65322},
		{"tag:#pattern", 16741120},
	}
	for i, want := range wantGroups {
		got := doc.ColorGroups[i]
		if got.Query != want.query {
			t.Errorf("colorGroups[%d].query = %q, want %q", i, got.Query, want.query)
		}
		if got.Color.RGB != want.rgb {
			t.Errorf("colorGroups[%d].color.rgb = %d, want %d", i, got.Color.RGB, want.rgb)
		}
		if got.Color.A != 1 {
			t.Errorf("colorGroups[%d].color.a = %v, want 1", i, got.Color.A)
		}
	}

	for _, g := range doc.ColorGroups {
		if strings.Contains(strings.ToLower(g.Query), "discovery") {
			t.Errorf("found a discovery colour group %q; the locked template has exactly 6 groups, none of them discovery", g.Query)
		}
	}

	if doc.ShowArrow {
		t.Error("showArrow = true, want false")
	}
	if doc.TextFadeMultiplier != 0 {
		t.Errorf("textFadeMultiplier = %v, want 0", doc.TextFadeMultiplier)
	}
	if !doc.CollapseColorGroups {
		t.Error("collapse-color-groups = false, want true")
	}
	if !doc.CollapseDisplay {
		t.Error("collapse-display = false, want true")
	}
	if doc.CollapseFilter {
		t.Error("collapse-filter = true, want false")
	}
	if doc.CollapseForces {
		t.Error("collapse-forces = true, want false")
	}
	if doc.ShowTags {
		t.Error("showTags = true, want false")
	}
	if doc.ShowAttachments {
		t.Error("showAttachments = true, want false")
	}
	if doc.HideUnresolved {
		t.Error("hideUnresolved = true, want false")
	}
	if !doc.ShowOrphans {
		t.Error("showOrphans = false, want true")
	}
	if doc.Close {
		t.Error("close = true, want false")
	}
	if doc.NodeSizeMultiplier != 1 {
		t.Errorf("nodeSizeMultiplier = %v, want 1", doc.NodeSizeMultiplier)
	}
	if doc.LineSizeMultiplier != 1 {
		t.Errorf("lineSizeMultiplier = %v, want 1", doc.LineSizeMultiplier)
	}
	if doc.CenterStrength != 0.515147569444444 {
		t.Errorf("centerStrength = %v, want 0.515147569444444", doc.CenterStrength)
	}
	if doc.RepelStrength != 12.7118055555556 {
		t.Errorf("repelStrength = %v, want 12.7118055555556", doc.RepelStrength)
	}
	if doc.LinkStrength != 0.729210069444444 {
		t.Errorf("linkStrength = %v, want 0.729210069444444", doc.LinkStrength)
	}
	if doc.LinkDistance != 207 {
		t.Errorf("linkDistance = %v, want 207", doc.LinkDistance)
	}
	if doc.Scale != 0.1 {
		t.Errorf("scale = %v, want 0.1", doc.Scale)
	}
}

// TestWriteGraphConfig covers all 3 modes against t.TempDir() vaults
// (REQ-GRAPH-02..04, REQ-GRAPH-06).
func TestWriteGraphConfig(t *testing.T) {
	t.Run("preserve leaves an existing graph.json byte-identical", func(t *testing.T) {
		vault := t.TempDir()
		dir := filepath.Join(vault, ".obsidian")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("setup: MkdirAll() error = %v", err)
		}
		existing := []byte(`{"custom": "user-edited config, not the template"}`)
		target := filepath.Join(dir, "graph.json")
		if err := os.WriteFile(target, existing, 0644); err != nil {
			t.Fatalf("setup: WriteFile() error = %v", err)
		}

		if err := WriteGraphConfig(vault, GraphConfigPreserve); err != nil {
			t.Fatalf("WriteGraphConfig() error = %v", err)
		}

		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if string(got) != string(existing) {
			t.Errorf("preserve overwrote an existing graph.json:\ngot:  %s\nwant: %s", got, existing)
		}
	})

	t.Run("preserve writes the embedded template when absent, creating .obsidian if needed", func(t *testing.T) {
		vault := t.TempDir()
		target := filepath.Join(vault, ".obsidian", "graph.json")

		if err := WriteGraphConfig(vault, GraphConfigPreserve); err != nil {
			t.Fatalf("WriteGraphConfig() error = %v", err)
		}

		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if string(got) != string(defaultGraphTemplate) {
			t.Error("preserve on an absent file did not write the embedded template verbatim")
		}
	})

	t.Run("force overwrites an existing file unconditionally", func(t *testing.T) {
		vault := t.TempDir()
		dir := filepath.Join(vault, ".obsidian")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("setup: MkdirAll() error = %v", err)
		}
		target := filepath.Join(dir, "graph.json")
		if err := os.WriteFile(target, []byte(`{"custom": "stale"}`), 0644); err != nil {
			t.Fatalf("setup: WriteFile() error = %v", err)
		}

		if err := WriteGraphConfig(vault, GraphConfigForce); err != nil {
			t.Fatalf("WriteGraphConfig() error = %v", err)
		}

		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if string(got) != string(defaultGraphTemplate) {
			t.Error("force did not overwrite the existing file with the embedded template")
		}
	})

	t.Run("skip never creates .obsidian and never reads or writes anything", func(t *testing.T) {
		vault := t.TempDir()

		if err := WriteGraphConfig(vault, GraphConfigSkip); err != nil {
			t.Fatalf("WriteGraphConfig() error = %v", err)
		}

		if _, err := os.Stat(filepath.Join(vault, ".obsidian")); !os.IsNotExist(err) {
			t.Errorf(".obsidian directory exists after GraphConfigSkip, stat err = %v", err)
		}
	})
}

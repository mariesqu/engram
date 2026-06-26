package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v1.0.0", "v1.1.0", true},
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.0", "v2.0.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.2.0", "v1.1.9", false},
		{"v1.10.0", "v1.9.0", false}, // numeric, not lexical
		{"dev", "v1.0.0", true},      // dev current → offer
		{"v1.0.0", "garbage", false}, // unparseable latest → never offer
		{"1.0.0", "1.0.1", true},     // tolerate missing leading v
		{"v1.0.0-rc1", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.latest); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v; want %v", c.current, c.latest, got, c.want)
		}
	}
}

// newReleaseServer serves a fake GitHub releases/latest + asset + SHA256SUMS.
func newReleaseServer(t *testing.T, tag string, binary []byte) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(binary)
	assetName := "engram-" + tag + "-windows-amd64.exe"
	sums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)

	mux.HandleFunc("/repos/mariesqu/engram/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		rel := ghRelease{
			TagName: tag,
			Assets: []ghAsset{
				{Name: assetName, URL: srv.URL + "/dl/asset"},
				{Name: "SHA256SUMS", URL: srv.URL + "/dl/sums"},
				{Name: "engram-" + tag + "-linux-amd64", URL: srv.URL + "/dl/linux"},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/dl/asset", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, sums) })

	t.Cleanup(srv.Close)
	return srv
}

func TestCheck_NewerReturnsUpdate(t *testing.T) {
	bin := []byte("new engram binary v1.2.0")
	srv := newReleaseServer(t, "v1.2.0", bin)
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = "https://api.github.com" })

	u, err := Check(context.Background(), srv.Client(), "mariesqu/engram", "v1.0.0", "windows-amd64.exe")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if u == nil {
		t.Fatal("Check: expected an update, got nil")
	}
	if u.LatestVersion != "v1.2.0" {
		t.Errorf("LatestVersion = %q; want v1.2.0", u.LatestVersion)
	}
	if u.AssetName != "engram-v1.2.0-windows-amd64.exe" {
		t.Errorf("AssetName = %q", u.AssetName)
	}
	if u.SHA256 == "" {
		t.Error("SHA256 must be populated")
	}

	// DownloadAndVerify should accept the matching binary.
	got, err := DownloadAndVerify(context.Background(), srv.Client(), u)
	if err != nil {
		t.Fatalf("DownloadAndVerify: %v", err)
	}
	if string(got) != string(bin) {
		t.Errorf("downloaded bytes mismatch")
	}
}

func TestCheck_UpToDateReturnsNil(t *testing.T) {
	srv := newReleaseServer(t, "v1.2.0", []byte("x"))
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = "https://api.github.com" })

	u, err := Check(context.Background(), srv.Client(), "mariesqu/engram", "v1.2.0", "windows-amd64.exe")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if u != nil {
		t.Errorf("expected nil (up to date), got %+v", u)
	}
}

func TestDownloadAndVerify_ChecksumMismatch(t *testing.T) {
	srv := newReleaseServer(t, "v1.2.0", []byte("real binary"))
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = "https://api.github.com" })

	u, err := Check(context.Background(), srv.Client(), "mariesqu/engram", "v1.0.0", "windows-amd64.exe")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	u.SHA256 = "deadbeef" // corrupt the expected digest
	if _, err := DownloadAndVerify(context.Background(), srv.Client(), u); err == nil {
		t.Error("expected a checksum mismatch error, got nil")
	}
}

func TestSwapBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "engram.exe")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}

	oldPath, err := SwapBinary(target, []byte("NEW"))
	if err != nil {
		t.Fatalf("SwapBinary: %v", err)
	}

	got, _ := os.ReadFile(target)
	if string(got) != "NEW" {
		t.Errorf("target content = %q; want NEW", got)
	}
	old, _ := os.ReadFile(oldPath)
	if string(old) != "OLD" {
		t.Errorf(".old content = %q; want OLD", old)
	}
}

func TestSwapBinary_RefusesEmpty(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "engram.exe")
	_ = os.WriteFile(target, []byte("OLD"), 0o755)
	if _, err := SwapBinary(target, nil); err == nil {
		t.Error("expected error swapping in an empty binary")
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Errorf("target must be untouched on refusal; got %q", got)
	}
}

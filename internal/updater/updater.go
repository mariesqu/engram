// Package updater checks GitHub Releases for a newer engram build, verifies the
// download against the release SHA256SUMS, and swaps the binary in place.
//
// It is deliberately cross-platform and side-effect-light so it can be unit
// tested without a network or a real install: Check/DownloadAndVerify take an
// *http.Client (point it at httptest), and SwapBinary takes an explicit target
// path. The Windows tray wires these together; the binary swap itself works on
// any OS via os.Rename (Windows allows renaming a running .exe, so the new
// binary takes effect on the next launch).
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// maxAssetBytes caps a downloaded asset (defense against a hostile/huge body).
const maxAssetBytes = 200 << 20 // 200 MiB

// apiBaseURL is the GitHub REST base. A package var (not a const) so tests can
// point Check at an httptest server. Asset/checksum URLs come from the release
// JSON itself (browser_download_url), so only this base needs overriding.
var apiBaseURL = "https://api.github.com"

// Update describes an available newer release.
type Update struct {
	CurrentVersion string
	LatestVersion  string
	AssetURL       string // browser_download_url of the platform binary
	AssetName      string // e.g. engram-v1.2.0-windows-amd64.exe
	SHA256         string // expected hex digest from SHA256SUMS (always set; Check errors without it)
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Check queries the latest release of repo (e.g. "mariesqu/engram"), and returns
// an *Update when it is NEWER than current AND carries an asset whose name ends
// with assetSuffix (e.g. "windows-amd64.exe") plus a matching SHA256SUMS entry.
// Returns (nil, nil) when already up to date. It REQUIRES a checksum — an update
// is never offered without one to verify against.
func Check(ctx context.Context, client *http.Client, repo, current, assetSuffix string) (*Update, error) {
	rel, err := fetchLatest(ctx, client, repo)
	if err != nil {
		return nil, err
	}
	if !IsNewer(current, rel.TagName) {
		return nil, nil
	}

	var assetURL, assetName, sumsURL string
	for _, a := range rel.Assets {
		switch {
		case strings.HasSuffix(a.Name, assetSuffix):
			assetURL, assetName = a.URL, a.Name
		case a.Name == "SHA256SUMS":
			sumsURL = a.URL
		}
	}
	if assetURL == "" {
		return nil, fmt.Errorf("updater: release %s has no asset ending in %q", rel.TagName, assetSuffix)
	}
	if sumsURL == "" {
		return nil, fmt.Errorf("updater: release %s has no SHA256SUMS — refusing to update unverified", rel.TagName)
	}

	sum, err := fetchSHA256(ctx, client, sumsURL, assetName)
	if err != nil {
		return nil, err
	}

	return &Update{
		CurrentVersion: current,
		LatestVersion:  rel.TagName,
		AssetURL:       assetURL,
		AssetName:      assetName,
		SHA256:         sum,
	}, nil
}

// DownloadAndVerify fetches u.AssetURL and checks its SHA-256 against u.SHA256.
// Returns the verified bytes or an error on any mismatch.
func DownloadAndVerify(ctx context.Context, client *http.Client, u *Update) ([]byte, error) {
	if u.SHA256 == "" {
		return nil, fmt.Errorf("updater: no checksum to verify against")
	}
	data, err := download(ctx, client, u.AssetURL)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, u.SHA256) {
		return nil, fmt.Errorf("updater: checksum mismatch for %s: got %s, want %s", u.AssetName, got, u.SHA256)
	}
	return data, nil
}

// SwapBinary replaces the file at targetPath with newBin by renaming the current
// file to targetPath+".old" (which the caller can clean up on next launch) and
// writing newBin to targetPath. On Windows a running .exe cannot be overwritten
// but CAN be renamed, so this works while the process is live; the new binary
// takes effect on restart. Returns the .old path on success. On a write failure
// it rolls the rename back.
func SwapBinary(targetPath string, newBin []byte) (oldPath string, err error) {
	if len(newBin) == 0 {
		return "", fmt.Errorf("updater: refusing to swap in an empty binary")
	}
	oldPath = targetPath + ".old"
	_ = os.Remove(oldPath) // clear a stale .old from a previous update

	if err := os.Rename(targetPath, oldPath); err != nil {
		return "", fmt.Errorf("updater: rename current binary: %w", err)
	}
	if err := os.WriteFile(targetPath, newBin, 0o755); err != nil {
		_ = os.Rename(oldPath, targetPath) // best-effort rollback
		return "", fmt.Errorf("updater: write new binary: %w", err)
	}
	return oldPath, nil
}

// IsNewer reports whether latest is a strictly newer semver than current.
// A current that is not a release version (e.g. "dev") is treated as older than
// any parseable release, so dev builds are always offered the latest.
func IsNewer(current, latest string) bool {
	l, okl := parseSemver(latest)
	if !okl {
		return false // can't understand the release → never offer it
	}
	c, okc := parseSemver(current)
	if !okc {
		return true // "dev"/unknown current → offer the latest release
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false // equal
}

// parseSemver parses "vX.Y.Z" (optional leading v, optional -prerelease/+build
// suffix which is ignored) into [major, minor, patch]. ok=false when it does not
// look like a release version.
func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	// Drop any -prerelease or +build metadata.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func fetchLatest(ctx context.Context, client *http.Client, repo string) (*ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", apiBaseURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("updater: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "engram-updater")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updater: query latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("updater: GitHub returned %d for latest release", resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("updater: decode release JSON: %w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("updater: latest release has no tag_name")
	}
	return &rel, nil
}

// fetchSHA256 downloads a SHA256SUMS file and returns the hex digest for the line
// whose filename matches assetName. Lines are "<hex>  <name>" (sha256sum format).
func fetchSHA256(ctx context.Context, client *http.Client, sumsURL, assetName string) (string, error) {
	data, err := download(ctx, client, sumsURL)
	if err != nil {
		return "", fmt.Errorf("updater: download SHA256SUMS: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[len(fields)-1] == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("updater: SHA256SUMS has no entry for %s", assetName)
}

func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("updater: build request: %w", err)
	}
	req.Header.Set("User-Agent", "engram-updater")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updater: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("updater: GET %s returned %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("updater: read body: %w", err)
	}
	if len(data) > maxAssetBytes {
		return nil, fmt.Errorf("updater: asset exceeds %d byte cap", maxAssetBytes)
	}
	return data, nil
}

# Releasing engram

## Release runbook

### Step 1 — Verify main is green

Confirm that the latest CI run on `main` is passing (vet + unit tests on ubuntu and windows).

Run the acceptance suite locally — it is excluded from CI to avoid embedded-postgres download cost:

```bash
make test-acceptance
```

All tests must be green before proceeding.

### Step 2 — Choose a version

engram uses [Semantic Versioning](https://semver.org/):

- **Patch** (`v0.1.1`): bug fixes only, no new features.
- **Minor** (`v0.2.0`): new backwards-compatible features.
- **Major** (`v1.0.0`): breaking API or behaviour changes.

There is no `CHANGELOG.md`.  Release highlights are written directly as the
**tag annotation message** (step 3).  Keep the message concise: 3-7 bullet
points covering user-visible changes since the previous tag.

### Step 3 — Tag and push

```bash
# Annotated tag — the message becomes the release body on GitHub.
git tag -a v0.2.0 -m "$(cat <<'MSG'
engram v0.2.0

- Add `engram version` subcommand (prints version, GOOS/GOARCH, Go runtime)
- ldflags version injection via -X main.version=
- Cross-compiled release binaries + SHA256SUMS via GitHub Actions

Verify checksums: sha256sum --check SHA256SUMS (Linux) / shasum -a 256 --check SHA256SUMS (macOS)
MSG
)"

The annotated tag message becomes the release body verbatim (the workflow uses
gh release create --notes-from-tag), so include the highlights AND the
checksum-verification line in every tag message — there is no separate
changelog file by convention.

git push origin v0.2.0
```

### Step 4 — CI builds the release

Pushing the tag triggers `.github/workflows/release.yml`, which:

1. Runs `go vet` (default + acceptance tags) and unit tests.
2. Cross-builds the full matrix:
   - `linux/amd64`, `linux/arm64`
   - `darwin/amd64`, `darwin/arm64`
   - `windows/amd64`
3. Generates `SHA256SUMS` (one line per binary).
4. Creates a GitHub Release with all binaries and the checksum file attached.

Monitor the Actions tab: `https://github.com/mariesqu/engram/actions`.
The release appears under `https://github.com/mariesqu/engram/releases` once the
workflow finishes (~3 min on a cold cache, ~1 min warm).

### Step 5 — Verify the artifacts

Download the binary for your platform from the Releases page and run:

```bash
# Linux/macOS
sha256sum --check SHA256SUMS   # Linux; macOS: shasum -a 256 --check SHA256SUMS

# Confirm the version prints correctly
./engram-v0.2.0-linux-amd64 version
# Expected: engram v0.2.0 linux/amd64 go1.26.x
```

If the checksum fails or the version string is wrong, delete the release on
GitHub, fix the issue on `main`, then re-tag with the same version (force-push
the tag only if no users have downloaded artifacts yet).

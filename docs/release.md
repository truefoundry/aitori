# Releasing aitori

Releases ship **one `tar.gz` per OS/arch, each containing both binaries**
(`aitori` + `aitori-gateway`) plus the example config and `LICENSE`. No
deb/rpm. Built and published with [goreleaser](https://goreleaser.com).

## Artifacts

For every supported platform — `darwin/{amd64,arm64}`, `linux/{amd64,arm64}`,
`windows/{amd64,arm64}` — a `tar.gz` named `aitori_<version>_<os>_<arch>.tar.gz`
(goreleaser includes the version; `make dist` names them `aitori_<os>_<arch>.tar.gz`),
containing:

- `aitori` (the agent) and `aitori-gateway` (the local trace gateway), built
  `CGO_ENABLED=0`,
- `configs/conversations.yaml`, `LICENSE`, `README.md`,

plus a `checksums.txt`. The version is stamped into `aitori` via ldflags
(`internal/version.Version`).

## Build the bundles locally (no goreleaser)

```bash
make dist                          # all platforms → dist/aitori_<os>_<arch>.tar.gz
make dist PLATFORMS="linux/amd64"  # a subset
tar tzf dist/aitori_linux_amd64.tar.gz   # inspect: both binaries + config
```

This mirrors the release layout and is handy for a quick check.

## Publish with goreleaser

Prereq: `brew install goreleaser` (or see goreleaser docs).

**Dry run** (no tag, nothing published — artifacts land in `dist/`):

```bash
goreleaser check                     # validate .goreleaser.yaml
goreleaser release --snapshot --clean
ls dist/                             # aitori_<os>_<arch>.tar.gz + checksums.txt
```

**Real release** — driven by a git tag:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
export GITHUB_TOKEN=<a token with repo scope>
goreleaser release --clean
```

`release.draft: true` means goreleaser creates a **draft** GitHub release with
the tarballs + checksums and an auto-generated changelog; review it in the GitHub
UI and publish when ready.

## Manual release (tag locally, upload by hand)

If you'd rather not hand goreleaser a token, build from a local tag and attach the
files to a draft release yourself.

1. **Tag HEAD and push the tag** (so the release can attach to it):

   ```bash
   git tag -a v0.1.0 -m "aitori v0.1.0"
   git push origin v0.1.0
   ```

2. **Build the tarballs + checksums from the tag, without uploading:**

   ```bash
   goreleaser release --clean --skip=publish
   ```

   This stamps the tag's version into `aitori` and writes
   `dist/aitori_0.1.0_<os>_<arch>.tar.gz` (six) plus `dist/checksums.txt`. No
   `GITHUB_TOKEN` needed. *(Alternative without goreleaser:* `make dist VERSION=v0.1.0`,
   then `cd dist && shasum -a 256 *.tar.gz > checksums.txt && cd ..` — use
   `sha256sum` on Linux.*)*

3. **Create the draft release in the GitHub web UI:**
   - Repo → **Releases → "Draft a new release"** (`https://github.com/<owner>/<repo>/releases/new`).
   - **Choose a tag** → select `v0.1.0`.
   - Set the title to `v0.1.0`; optionally click **"Generate release notes."**
   - In **"Attach binaries…"**, drag in **all** `dist/*.tar.gz` **and** `dist/checksums.txt`.
   - Click **"Save draft"** — this is what makes it a *draft* (visible only to
     maintainers). The "Publish release" button would make it public instead.
   - Review, then open the draft and **"Publish release"** when ready.

## Notes

- **No signing/notarization.** macOS/Windows binaries are unsigned; users may need
  to clear Gatekeeper quarantine (macOS) or SmartScreen (Windows).
- `aitori-gateway` is a **separate Go module** (`tools/aitori-gateway`);
  goreleaser builds it via a second `builds` entry with `dir:` set. `make dist`
  builds it from that directory too.
- To add/remove target platforms, edit the `goarch`/`goos` lists in
  `.goreleaser.yaml` and the `PLATFORMS` default in the `Makefile`.

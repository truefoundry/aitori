#!/bin/sh
# aitori installer.
#
#   curl -fsSL https://raw.githubusercontent.com/truefoundry/aitori/main/install.sh | sh
#
# Downloads the latest release tarball for your OS/arch from GitHub Releases,
# verifies its sha256 against checksums.txt, and installs the `aitori` and
# `aitori-gateway` binaries onto your PATH (default /usr/local/bin).
#
# macOS + Linux only (Windows: download the tarball from the Releases page).
# Binaries are unsigned; a curl-downloaded binary is NOT Gatekeeper-quarantined,
# so it runs as-is. (If you download via a browser instead, clear quarantine
# with: xattr -d com.apple.quarantine ./aitori)
#
# Overrides (env vars):
#   VERSION              install a specific tag (e.g. v0.1.0); default: latest
#   AITORI_INSTALL_DIR  install location; default: ~/.local/bin if it's already
#                        on your PATH (no sudo), else /usr/local/bin
#   AITORI_REPO         owner/repo; default: truefoundry/aitori
#
# NOTE: requires a *published* (non-draft) GitHub release. To test against a
# pre-release before publishing the latest, pass VERSION=<tag>.

set -eu

REPO="${AITORI_REPO:-truefoundry/aitori}"

# Default install dir: prefer ~/.local/bin when it's already on PATH (a no-sudo
# install that's immediately findable); otherwise fall back to /usr/local/bin
# (always on PATH on macOS, but root-owned, so it needs sudo). An explicit
# AITORI_INSTALL_DIR always wins.
if [ -n "${AITORI_INSTALL_DIR:-}" ]; then
  INSTALL_DIR="$AITORI_INSTALL_DIR"
else
  INSTALL_DIR="/usr/local/bin"
  case ":$PATH:" in
    *":$HOME/.local/bin:"*) INSTALL_DIR="$HOME/.local/bin" ;;
  esac
fi

info() { printf 'aitori: %s\n' "$1" >&2; }
fail() { printf 'aitori: error: %s\n' "$1" >&2; exit 1; }

# --- detect OS/arch (matching goreleaser's GOOS/GOARCH names) ----------------
os=$(uname -s)
case "$os" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) fail "unsupported OS '$os' (macOS/Linux only; on Windows download from https://github.com/$REPO/releases)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported architecture '$arch'" ;;
esac

# --- pick a downloader -------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  dl() { curl -fsSL "$1" -o "$2"; }
  fetch() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
  dl() { wget -qO "$2" "$1"; }
  fetch() { wget -qO- "$1"; }
else
  fail "need curl or wget"
fi

# --- resolve version ---------------------------------------------------------
tag="${VERSION:-}"
if [ -z "$tag" ]; then
  info "resolving latest release for $REPO"
  tag=$(fetch "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name" *: *"([^"]+)".*/\1/')
  [ -n "$tag" ] || fail "could not resolve latest release (set VERSION=<tag>, and ensure a non-draft release exists)"
fi
# goreleaser strips a leading 'v' from the version in artifact names.
ver=$(printf '%s' "$tag" | sed 's/^v//')

asset="aitori_${ver}_${os}_${arch}.tar.gz"
# AITORI_BASE_URL lets a mirror (or a local test server) stand in for GitHub.
base="${AITORI_BASE_URL:-https://github.com/$REPO/releases/download/$tag}"

# --- pick a sha256 tool ------------------------------------------------------
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  fail "need sha256sum or shasum"
fi

# --- download + verify -------------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

info "downloading $asset ($tag)"
dl "$base/$asset" "$tmp/$asset" || fail "download failed: $base/$asset"
dl "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"

want=$(grep " ${asset}\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || fail "no checksum for $asset in checksums.txt"
got=$(sha256 "$tmp/$asset")
[ "$want" = "$got" ] || fail "checksum mismatch for $asset (want $want, got $got)"
info "checksum verified"

tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/aitori" ] || fail "archive missing aitori binary"
chmod +x "$tmp/aitori" "$tmp/aitori-gateway" 2>/dev/null || true

# --- choose how to write the install dir -------------------------------------
sudo=""
if [ ! -d "$INSTALL_DIR" ]; then
  mkdir -p "$INSTALL_DIR" 2>/dev/null || sudo="sudo"
fi
if [ -z "$sudo" ] && [ ! -w "$INSTALL_DIR" ] && [ "$(id -u)" != "0" ]; then
  if command -v sudo >/dev/null 2>&1; then
    sudo="sudo"
    info "writing $INSTALL_DIR needs elevated permissions — you may be prompted for your password"
  else
    INSTALL_DIR="$HOME/.local/bin"
    mkdir -p "$INSTALL_DIR"
    info "no write access to the default dir; installing to $INSTALL_DIR instead"
  fi
fi

# Do the mkdir + both moves in a single elevated shell, so sudo prompts at most
# once (some hosts disable sudo's credential cache, which would otherwise prompt
# for every separate sudo call). `set -e` inside makes any step abort the
# subshell with a non-zero status; the outer `set -e` then aborts the install —
# so a failed mkdir/mv can't be masked as success. The gateway move is wrapped
# so a legitimately-absent gateway binary isn't itself treated as a failure.
$sudo sh -e -c '
  mkdir -p "$1"
  mv -f "$2/aitori" "$1/aitori"
  if [ -f "$2/aitori-gateway" ]; then
    mv -f "$2/aitori-gateway" "$1/aitori-gateway"
  fi
' sh "$INSTALL_DIR" "$tmp" || fail "failed to install binaries to $INSTALL_DIR"

info "installed aitori $tag to:"
info "  $INSTALL_DIR/aitori"
info "  $INSTALL_DIR/aitori-gateway"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) info "note: $INSTALL_DIR is not on your PATH — add it (e.g. export PATH=\"$INSTALL_DIR:\$PATH\")" ;;
esac

cat >&2 <<EOF

Next:
  sudo aitori up --ui      # govern this machine (built-in profiles) + live UI
  open http://127.0.0.1:9100 # watch traffic flow through

To uninstall (revert system changes BEFORE deleting the binaries):
  sudo aitori down                 # revert system proxy + client-config edits
  sudo aitori ca remove            # remove the per-device CA from the trust store
  rm -f $INSTALL_DIR/aitori $INSTALL_DIR/aitori-gateway
  rm -rf ~/.aitori                 # optional: CA key, token, local state

Docs: https://github.com/$REPO
EOF

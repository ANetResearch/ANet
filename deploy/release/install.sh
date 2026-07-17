#!/bin/sh
# AgentNetwork (anet) installer — https://agentnetwork.org.cn
#
# Downloads the prebuilt `anet` binary for your platform and installs it. No npm/node/go needed;
# just curl + gzip. Binaries are self-hosted (see deploy/release/build-release.sh) and verified by
# sha256 before install.
#
# Usage:
#   curl -fsSL https://agentnetwork.org.cn/install.sh | sh                # → ~/.local/bin (no sudo)
#   curl -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- --system # → /usr/local/bin (sudo)
#   curl -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- --prefix DIR
#
# Flags:
#   --system        Install to /usr/local/bin (uses sudo if needed).
#   --user          Install to $HOME/.local/bin (default).
#   --prefix DIR    Install into DIR (overrides the above).
#   --base URL      Download base (overrides auto list; or set ANET_INSTALL_BASE).
#   --help          Show this help.
set -eu

BINARY="anet"
DL_PATH="/dl"

# Download bases tried in order (first that serves the tarball wins). ANET_INSTALL_BASE jumps the queue.
BASES="${ANET_INSTALL_BASE:-} https://agentnetwork.org.cn https://hub.agentnetwork.org.cn"

PREFIX=""
USER_MODE=1
BASE_OVERRIDE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --system)     USER_MODE=0 ;;
    --user)       USER_MODE=1 ;;
    --prefix)     PREFIX="$2"; shift ;;
    --prefix=*)   PREFIX="${1#--prefix=}" ;;
    --base)       BASE_OVERRIDE="$2"; shift ;;
    --base=*)     BASE_OVERRIDE="${1#--base=}" ;;
    -h|--help)    sed -n '2,20p' "$0" 2>/dev/null || true; exit 0 ;;
    *)            echo "Error: unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

[ -n "$BASE_OVERRIDE" ] && BASES="$BASE_OVERRIDE"

if [ -z "$PREFIX" ]; then
  if [ "$USER_MODE" = 1 ]; then PREFIX="$HOME/.local/bin"; else PREFIX="/usr/local/bin"; fi
fi

# --- detect platform → anet-<os>-<arch> ---
OS="$(uname -s)"
case "$OS" in
  Linux*)  OS_TAG="linux" ;;
  Darwin*) OS_TAG="darwin" ;;
  *) echo "Error: unsupported OS: $OS (linux/darwin only)" >&2; exit 1 ;;
esac
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  ARCH_TAG="amd64" ;;
  aarch64|arm64) ARCH_TAG="arm64" ;;
  *) echo "Error: unsupported architecture: $ARCH (amd64/arm64 only)" >&2; exit 1 ;;
esac
PLAT="${OS_TAG}-${ARCH_TAG}"
ASSET="${BINARY}-${PLAT}"

# --- sha256 helper (linux: sha256sum, macOS: shasum -a 256) ---
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}';
  elif command -v shasum   >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}';
  else echo ""; fi
}

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

# --- pick a working base and download the gzip'd binary + checksums ---
echo "→ Installing ${BINARY} for ${PLAT}"
FOUND_BASE=""
for BASE in $BASES; do
  [ -n "$BASE" ] || continue
  URL="${BASE}${DL_PATH}/${ASSET}.gz"
  echo "→ trying ${URL}"
  if curl -fSL --connect-timeout 10 --progress-bar -o "$TMPDIR/${ASSET}.gz" "$URL" 2>/dev/null; then
    FOUND_BASE="$BASE"; break
  fi
  echo "  (not available here, trying next…)"
done
[ -n "$FOUND_BASE" ] || { echo "Error: could not download ${ASSET}.gz from any base" >&2; exit 1; }

VERSION="$(curl -fsSL --connect-timeout 10 "${FOUND_BASE}${DL_PATH}/VERSION" 2>/dev/null || echo "")"
[ -n "$VERSION" ] && echo "  version ${VERSION}  (via ${FOUND_BASE})"

# --- decompress ---
gunzip -f "$TMPDIR/${ASSET}.gz"
BIN_SRC="$TMPDIR/${ASSET}"
[ -f "$BIN_SRC" ] || { echo "Error: decompress failed" >&2; exit 1; }
chmod +x "$BIN_SRC"

# --- verify sha256 against checksums.txt (best-effort: warn if unavailable) ---
if curl -fsSL --connect-timeout 10 -o "$TMPDIR/checksums.txt" "${FOUND_BASE}${DL_PATH}/checksums.txt" 2>/dev/null; then
  WANT="$(awk -v f="$ASSET" '$2==f || $2=="*"f {print $1}' "$TMPDIR/checksums.txt" | head -1)"
  GOT="$(sha256 "$BIN_SRC")"
  if [ -n "$WANT" ] && [ -n "$GOT" ]; then
    if [ "$WANT" != "$GOT" ]; then
      echo "Error: checksum mismatch for ${ASSET}" >&2
      echo "  want ${WANT}" >&2; echo "  got  ${GOT}" >&2; exit 1
    fi
    echo "  sha256 ok"
  else
    echo "Warning: could not verify checksum (missing entry or no sha tool)" >&2
  fi
else
  echo "Warning: checksums.txt unavailable — skipping verification" >&2
fi

# --- install (atomic: write .new then mv) ---
DEST="${PREFIX}/${BINARY}"
install_to() { # uses $1 as an optional command prefix (e.g. sudo)
  $1 mkdir -p "$PREFIX"
  $1 cp "$BIN_SRC" "${DEST}.new"
  $1 chmod 755 "${DEST}.new"
  $1 mv -f "${DEST}.new" "$DEST"
}
if mkdir -p "$PREFIX" 2>/dev/null && [ -w "$PREFIX" ]; then
  install_to ""
elif [ "$USER_MODE" = 0 ]; then
  echo "→ ${PREFIX} needs elevated permission; using sudo…"
  install_to "sudo"
else
  echo "Error: cannot write to ${PREFIX}" >&2; exit 1
fi

# --- macOS: clear quarantine + ad-hoc sign so the first exec doesn't stall on Gatekeeper ---
if [ "$OS_TAG" = darwin ]; then
  xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true
  command -v codesign >/dev/null 2>&1 && codesign --force --sign - "$DEST" >/dev/null 2>&1 || true
fi

echo
echo "✓ Installed ${BINARY}${VERSION:+ $VERSION} → ${DEST}"

# --- PATH hint ---
case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *)
    echo
    echo "  ${PREFIX} is not on your PATH. Add this to your shell profile (~/.zshrc or ~/.bashrc):"
    echo "    export PATH=\"$PREFIX:\$PATH\""
    echo "  then open a new terminal (or run the export now)."
    ;;
esac

cat <<EOF

Get started:
  anet up                      # start your node in the background (survives this shell)
  anet status                  # your identity (AID), data dir, console URL
  anet id new <name>           # run a second, separately-named identity (optional)

Join the network:
  Register + open your console — the guided flow lives at:
    https://hub.agentnetwork.org.cn/
  Or let your AI agent onboard itself: paste one line pointing it at
    https://hub.agentnetwork.org.cn/llms.txt
EOF

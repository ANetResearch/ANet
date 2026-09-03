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
# Install and join a hub in one line:
#   curl -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- \
#     --hub https://hub.agentnetwork.org.cn --name my-debian-box
#
# Flags:
#   --system        Install to /usr/local/bin (uses sudo if needed).
#   --user          Install to $HOME/.local/bin (default).
#   --prefix DIR    Install into DIR (overrides the above).
#   --base URL      Download base (overrides auto list; or set ANET_INSTALL_BASE).
#   --hub URL       Start the node and register it with this hub.
#   --name NAME     The name to register under (default: this machine's hostname).
#   --token INVITE  An admission token, for a hub that admits by invite only.
#                   Not needed by a hub that admits openly, which is the default;
#                   its operator tells you if you need one.
#   --shell         Install the variant that can run operator-approved commands
#                   on this machine. See "--shell" below before using it.
#   --help          Show this help.
#
# --shell installs a DIFFERENT BINARY, not a setting.
#
# The default binary cannot execute anything on the machine it runs on: the
# module that would do it is not compiled in, which `go tool nm` can be made
# to confirm on the downloaded file. `--shell` fetches the build that has it.
# Even that build runs nothing until an operator writes a `modules.shell`
# config block naming the commands, and lists the AIDs allowed to call them;
# an empty list refuses everyone. Full contract: docs/SHELL-zh.md in the
# repository.
set -eu

BINARY="anet"
DL_PATH="/dl"
# ASSET_PREFIX selects the variant. It is a different download, not a flag
# passed to the same one, so a machine that was never meant to run commands
# has a binary that cannot.
ASSET_PREFIX="anet"

# Download bases tried in order (first that serves the tarball wins). ANET_INSTALL_BASE jumps the queue.
BASES="${ANET_INSTALL_BASE:-} https://agentnetwork.org.cn https://hub.agentnetwork.org.cn"

PREFIX=""
USER_MODE=1
BASE_OVERRIDE=""
HUB=""
NODE_NAME=""
INVITE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --system)     USER_MODE=0 ;;
    --user)       USER_MODE=1 ;;
    --prefix)     PREFIX="$2"; shift ;;
    --prefix=*)   PREFIX="${1#--prefix=}" ;;
    --base)       BASE_OVERRIDE="$2"; shift ;;
    --base=*)     BASE_OVERRIDE="${1#--base=}" ;;
    --hub)        HUB="$2"; shift ;;
    --hub=*)      HUB="${1#--hub=}" ;;
    --name)       NODE_NAME="$2"; shift ;;
    --name=*)     NODE_NAME="${1#--name=}" ;;
    --token)      INVITE="$2"; shift ;;
    --token=*)    INVITE="${1#--token=}" ;;
    --shell)      ASSET_PREFIX="anet-shell" ;;
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
ASSET="${ASSET_PREFIX}-${PLAT}"

# --- sha256 helper (linux: sha256sum, macOS: shasum -a 256) ---
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}';
  elif command -v shasum   >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}';
  else echo ""; fi
}

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

# --- pick a working base and download the gzip'd binary + checksums ---
if [ "$ASSET_PREFIX" = "anet-shell" ]; then
  echo "→ Installing ${BINARY} for ${PLAT} (shell variant: CAN run operator-approved commands on this machine)"
else
  echo "→ Installing ${BINARY} for ${PLAT}"
fi
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

# --- optionally start the node and join a hub ---
#
# Done here rather than left as two commands in the output, because "install
# it on the new box and have it show up" is one intention, and the step most
# often skipped is the one that makes the machine reachable at all.
#
# The hub verifies that the key history derives the claimed AID and that the
# node can sign a challenge, which proves the node controls its identity.
# Whether it also gates WHO may join is the operator's choice: a hub admits
# openly by default, and one that has turned admission on needs --token. What
# a node will DO for a caller is decided by the node either way, not by the
# hub — the hub knowing somebody is not the same as this machine agreeing to
# run commands for them.
if [ -n "$HUB" ]; then
  [ -n "$NODE_NAME" ] || NODE_NAME="$(hostname 2>/dev/null || echo anet-node)"
  echo
  echo "→ Starting the node…"
  if "$DEST" up >/dev/null 2>&1; then
    # `up` returns once the control port is listening, but registration
    # needs the daemon to have finished opening its ledger.
    i=0; while [ $i -lt 20 ]; do
      "$DEST" status >/dev/null 2>&1 && break
      i=$((i+1)); sleep 1
    done
    echo "→ Registering with ${HUB} as \"${NODE_NAME}\"…"
    # An `if`, not `[ -n "$INVITE" ] && set -- …`: under `set -e` that AND
    # list returns non-zero whenever there is no token, and the script
    # would exit just before the registration it was asked to do.
    if [ -n "$INVITE" ]; then
      set -- hub-register "$HUB" --name "$NODE_NAME" --token "$INVITE"
    else
      set -- hub-register "$HUB" --name "$NODE_NAME"
    fi
    if "$DEST" "$@"; then
      echo "✓ Joined ${HUB}"
    else
      echo "Warning: registration did not complete. The node is running; retry with:" >&2
      if [ -n "$INVITE" ]; then
        echo "    anet hub-register $HUB --name $NODE_NAME --token <your invite>" >&2
      else
        echo "    anet hub-register $HUB --name $NODE_NAME" >&2
        echo "  If this hub admits by invite only, ask its operator for a token and add --token." >&2
      fi
    fi
  else
    echo "Warning: the node did not start. Start it by hand with: anet up" >&2
  fi
fi

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

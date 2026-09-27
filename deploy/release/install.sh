#!/bin/sh
# AgentNetwork (anet) installer — https://agentnetwork.org.cn
#
# Downloads the prebuilt `anet` binary for your platform and installs it,
# after checking it against the signed release manifest. Needs curl, gzip
# and ssh-keygen (OpenSSH 8.1 or later — macOS and most Linux systems have
# it). No npm/node/go.
#
# Usage:
#   curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh
#   … | sh -s -- --system          → /usr/local/bin (sudo)
#   … | sh -s -- --prefix DIR
#   … | sh -s -- --agents          also wire anet into the coding agents found here
#   … | sh -s -- --hub https://hub.agentnetwork.org.cn --name my-box
#                                  also start the node and register it with a hub
#
# Already installed? Run `anet update` instead: it checks the same signed
# manifest with the release key built into the binary, and needs nothing
# from this script.
#
# WHAT IS CHECKED, and every check stops the install with the installed
# version untouched:
#
#   1. release.json is signed by the anet release key (ssh-keygen -Y verify
#      against the key written below, namespace anet-release@agentnetwork.org.cn)
#   2. it has not expired
#   3. it is not older than the anet already installed at the destination
#   4. the sha256 of the .gz, and of the binary inside it, are the ones it names
#   5. the binary reports the version and the module set it names for its
#      variant (the default variant must not contain `shell`)
#
# There is no flag to skip any of these.
#
# WHAT IS NOT: this script itself, when it is piped into sh. `curl | sh`
# trusts the host that served it and that host's TLS certificate. To not
# trust the host, check the script before running it — the release public
# key is published in SECURITY.md and README.md of
# https://github.com/ANetResearch/ANet:
#
#   curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh
#   curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh.sig
#   echo 'anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN1PbNot6BeA6oxH7zpMtXpZk6opSAFkGvT2dhrZody3' > allowed_signers
#   ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn \
#     -n anet-release@agentnetwork.org.cn -s install.sh.sig < install.sh && sh install.sh
#
# Flags:
#   --system        Install to /usr/local/bin (uses sudo if needed).
#   --user          Install to $HOME/.local/bin (default).
#   --prefix DIR    Install into DIR (overrides the above).
#   --base URL      Download base, https only (or set ANET_INSTALL_BASE).
#   --shell         Install the variant that can run operator-approved commands
#                   on this machine. See "--shell" below before using it.
#   --agents        After installing, wire anet into the coding agents found on
#                   this machine (`anet agents wire --all`). --agents=claude,codex
#                   wires only those.
#   --hub URL       Start the node and register it with this hub.
#   --name NAME     The name to register under (default: this machine's hostname).
#   --token INVITE  An admission token, for a hub that admits by invite only.
#                   Not needed by a hub that admits openly, which is the default;
#                   its operator tells you if you need one.
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
#
# Everything below is a function and the last line calls main: a download
# cut short defines half a function and runs nothing.

# The release key. DEV KEY — 正式发布前由产品负责人替换.
# Same line as internal/release/allowed_signers, SECURITY.md and README.md;
# a test in internal/release keeps them equal.
release_allowed_signers() {
  echo 'anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN1PbNot6BeA6oxH7zpMtXpZk6opSAFkGvT2dhrZody3'
}
# Printed in messages only; ssh-keygen checks the key itself.
release_key_fingerprint() {
  echo 'SHA256:jU+lPusEKAueZbobKBk1MIN+ruBrmyPei8XKAqVfkzA'
}

usage() {
  cat <<'EOF'
anet installer — verifies the signed release manifest, then installs.

  curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh
  … | sh -s -- [flags]

  --system          install to /usr/local/bin (sudo if needed)
  --user            install to ~/.local/bin (default)
  --prefix DIR      install into DIR
  --base URL        download base, https only (or ANET_INSTALL_BASE)
  --shell           the variant that can run operator-approved commands
  --agents[=LIST]   wire anet into coding agents found here (all, or LIST)
  --hub URL         start the node and register it with this hub
  --name NAME       name to register under (default: hostname)
  --token INVITE    admission token for an invite-only hub

Already installed? Use `anet update`.
Verify this script before running it: see SECURITY.md in
https://github.com/ANetResearch/ANet
EOF
}

say()  { printf '%s\n' "$*"; }
warn() { printf 'Warning: %s\n' "$*" >&2; }
die()  { printf 'Error: %s\n' "$*" >&2; printf 'Nothing was installed or changed.\n' >&2; exit 1; }

# fetch URL OUT [MAXBYTES] — https only, and never follow a redirect off
# https. MAXBYTES bounds what is read before anything has been verified:
# the manifest and its signature are small, and anything bigger is not them.
fetch() {
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 15 \
    ${3:+--max-filesize "$3"} -H 'Cache-Control: no-cache' -o "$2" "$1"
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else return 1; fi
}

# verify FILE SIG — the manual command from the header, against the key
# above. Prints what ssh-keygen said on failure.
verify_sig() {
  if ! out=$(ssh-keygen -Y verify -f "$TMP/allowed_signers" -I "$IDENTITY" -n "$NAMESPACE" \
      -s "$2" < "$1" 2>&1); then
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    die "$(basename "$1") is not signed by the anet release key ($(release_key_fingerprint)).
  Either the download host is serving something the release key did not sign,
  or this ssh-keygen is older than OpenSSH 8.1 ($(ssh -V 2>&1 | head -1))."
  fi
}

# Manifest fields. The manifest is written by build-release.sh with one
# field per line and one asset per line, and read here only AFTER its
# signature has verified; a field that is missing or appears twice is an
# error, not a default.
mf_top() {
  v=$(sed -n "s/^  \"$1\": \"\\([^\"]*\\)\",\\{0,1\\}\$/\\1/p" "$MF")
  n=$(sed -n "/^  \"$1\": /p" "$MF" | grep -c . || true)
  if [ "$n" = 0 ] && [ "${2:-}" = optional ]; then return 0; fi
  [ "$n" = 1 ] || die "release.json: field $1 is missing or repeated"
  printf '%s' "$v"
}
mf_block_line() { # BLOCK KEY → the one line for KEY inside the top-level BLOCK
  l=$(sed -n "/^  \"$1\": {\$/,/^  },\\{0,1\\}\$/p" "$MF" | grep "^    \"$2\": {")
  [ "$(printf '%s\n' "$l" | grep -c .)" = 1 ] || return 1
  printf '%s' "$l"
}
field() { # LINE KEY → string value
  printf '%s' "$1" | sed -n "s/.*\"$2\": \"\\([^\"]*\\)\".*/\\1/p"
}

# vercmp A B → prints -1, 0 or 1. x.y.z[-pre]; a pre-release sorts before
# its release. The same order as release.CompareVersions in Go.
vercmp() {
  awk -v a="$1" -v b="$2" '
    function cmp(a, b,   pa, pb, i, na, nb, ca, cb, x, y) {
      pa = ""; pb = ""
      if ((i = index(a, "-")) > 0) { pa = substr(a, i + 1); a = substr(a, 1, i - 1) }
      if ((i = index(b, "-")) > 0) { pb = substr(b, i + 1); b = substr(b, 1, i - 1) }
      na = split(a, ca, "."); nb = split(b, cb, ".")
      for (i = 1; i <= 3; i++) {
        x = (i <= na) ? ca[i] + 0 : 0; y = (i <= nb) ? cb[i] + 0 : 0
        if (x < y) return -1
        if (x > y) return 1
      }
      if (pa == pb) return 0
      if (pa == "") return 1
      if (pb == "") return -1
      return (pa < pb) ? -1 : 1
    }
    BEGIN { print cmp(a, b) }'
}

# has_cmd "init" — whether the installed anet lists `anet init` in its
# help. Asked this way because an older anet hands an unknown verb to the
# daemon and reports a missing daemon rather than a missing command.
has_cmd() {
  "$DEST" help --all 2>/dev/null | grep -Eq "^[[:space:]]+anet $1([^a-z-]|\$)"
}

parse_args() {
  PREFIX=""; USER_MODE=1; BASE_OVERRIDE=""; HUB=""; NODE_NAME=""; INVITE=""
  VARIANT="default"; ASSET_PREFIX="anet"; AGENTS=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --system)     USER_MODE=0 ;;
      --user)       USER_MODE=1 ;;
      --prefix)     [ $# -ge 2 ] || die "--prefix needs a directory"; PREFIX="$2"; shift ;;
      --prefix=*)   PREFIX="${1#--prefix=}" ;;
      --base)       [ $# -ge 2 ] || die "--base needs a URL"; BASE_OVERRIDE="$2"; shift ;;
      --base=*)     BASE_OVERRIDE="${1#--base=}" ;;
      --hub)        [ $# -ge 2 ] || die "--hub needs a URL"; HUB="$2"; shift ;;
      --hub=*)      HUB="${1#--hub=}" ;;
      --name)       [ $# -ge 2 ] || die "--name needs a value"; NODE_NAME="$2"; shift ;;
      --name=*)     NODE_NAME="${1#--name=}" ;;
      --token)      [ $# -ge 2 ] || die "--token needs a value"; INVITE="$2"; shift ;;
      --token=*)    INVITE="${1#--token=}" ;;
      --shell)      VARIANT="shell"; ASSET_PREFIX="anet-shell" ;;
      --agents)     AGENTS="--all" ;;
      --agents=*)   AGENTS="$(printf '%s' "${1#--agents=}" | tr ',' ' ')" ;;
      -h|--help)    usage; exit 0 ;;
      *)            printf 'Error: unknown flag: %s\n' "$1" >&2; usage >&2; exit 2 ;;
    esac
    shift
  done

  BASES="${ANET_INSTALL_BASE:-} https://agentnetwork.org.cn https://hub.agentnetwork.org.cn"
  [ -n "$BASE_OVERRIDE" ] && BASES="$BASE_OVERRIDE"
  for b in $BASES; do
    case "$b" in
      https://*) ;;
      *) die "download base $b is not https:// — this installer fetches over https only" ;;
    esac
  done

  if [ -z "$PREFIX" ]; then
    if [ "$USER_MODE" = 1 ]; then PREFIX="$HOME/.local/bin"; else PREFIX="/usr/local/bin"; fi
  fi
  DEST="${PREFIX}/anet"
}

detect_platform() {
  OS="$(uname -s)"
  case "$OS" in
    Linux*)  OS_TAG="linux" ;;
    Darwin*) OS_TAG="darwin" ;;
    *) die "unsupported OS: $OS (linux/darwin only)" ;;
  esac
  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64|amd64)  ARCH_TAG="amd64" ;;
    aarch64|arm64) ARCH_TAG="arm64" ;;
    *) die "unsupported architecture: $ARCH (amd64/arm64 only)" ;;
  esac
  PLAT="${OS_TAG}-${ARCH_TAG}"
  ASSET="${ASSET_PREFIX}-${PLAT}"
}

check_tools() {
  command -v curl >/dev/null 2>&1 || die "curl is required"
  command -v gunzip >/dev/null 2>&1 || die "gunzip (gzip) is required"
  sha256 /dev/null >/dev/null 2>&1 || die "sha256sum or shasum is required to check the download"
  if ! command -v ssh-keygen >/dev/null 2>&1; then
    die "ssh-keygen is required to verify the release signature, and this system has none.
  Install OpenSSH's client tools and run this again:
    Debian/Ubuntu: sudo apt-get install openssh-client
    Fedora/RHEL:   sudo dnf install openssh
    Alpine:        sudo apk add openssh-keygen
  (macOS ships it.) There is no option to install without checking the signature."
  fi
}

# fetch_manifest: the first base that serves something shaped like a
# manifest and a signature wins; everything it serves is then checked, and
# a failed check ends the install rather than moving on to the next base.
# (A host that answers every path with an HTML page is "not serving", not
# "serving a bad signature".)
fetch_manifest() {
  BASE=""
  for b in $BASES; do
    b="${b%/}"
    say "→ release manifest: ${b}/dl/release.json"
    if fetch "$b/dl/release.json" "$TMP/release.json" 1048576 2>"$TMP/curl.err" \
       && fetch "$b/dl/release.json.sig" "$TMP/release.json.sig" 16384 2>"$TMP/curl.err" \
       && head -c 1 "$TMP/release.json" | grep -q '{' \
       && head -1 "$TMP/release.json.sig" | grep -q '^-----BEGIN SSH SIGNATURE-----$'; then
      BASE="$b"; break
    fi
    err="$(head -1 "$TMP/curl.err" 2>/dev/null | sed 's/^curl: ([0-9]*) //')"
    say "  (not served here${err:+: $err}; trying next…)"
    : > "$TMP/curl.err"
  done
  [ -n "$BASE" ] || die "no download base served a release manifest (tried: $BASES)"
  MF="$TMP/release.json"
  verify_sig "$MF" "$TMP/release.json.sig"
  say "  signature ok  (release key $(release_key_fingerprint))"
}

read_manifest() {
  SCHEMA="$(mf_top schema)"
  [ "$SCHEMA" = "anet-release/1" ] || die "release.json: schema $SCHEMA is not one this installer reads"
  VERSION="$(mf_top version)"
  COMMIT="$(mf_top commit)"
  EXPIRES="$(mf_top expires_at)"
  NEXT_FP="$(mf_top next_key_fingerprint optional)"
  printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' \
    || die "release.json: version '$VERSION' is not x.y.z"
  printf '%s' "$EXPIRES" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' \
    || die "release.json: expires_at '$EXPIRES' is malformed"

  # Same shape on both sides, so string order is time order.
  NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  awk -v now="$NOW" -v until="$EXPIRES" 'BEGIN { exit !(now < until) }' \
    || die "the release manifest expired at $EXPIRES (it is now $NOW here).
  If this machine's clock is right, $BASE is serving a stale release;
  --base URL tries another download host (every host is checked the same way)."

  VLINE="$(mf_block_line variants "$VARIANT")" || die "release.json has no '$VARIANT' variant"
  MODS="$(printf '%s' "$VLINE" | sed -n 's/.*"modules": \[\([^]]*\)\].*/\1/p' | tr -d '" ')"
  ALINE="$(mf_block_line assets "$ASSET")" || die "release $VERSION has no build $ASSET"
  [ "$(field "$ALINE" variant)" = "$VARIANT" ] || die "release.json: $ASSET is not the $VARIANT variant"
  GZ_SHA="$(field "$ALINE" gz_sha256)"
  RAW_SHA="$(field "$ALINE" sha256)"
  GZ_SIZE="$(printf '%s' "$ALINE" | sed -n 's/.*"gz_size": \([0-9]*\).*/\1/p')"
  printf '%s' "$GZ_SIZE" | grep -Eq '^[1-9][0-9]*$' || die "release.json: bad gz_size for $ASSET"
  printf '%s' "$GZ_SHA" | grep -Eq '^[0-9a-f]{64}$' || die "release.json: bad gz_sha256 for $ASSET"
  printf '%s' "$RAW_SHA" | grep -Eq '^[0-9a-f]{64}$' || die "release.json: bad sha256 for $ASSET"
  say "  release $VERSION  commit $(printf '%s' "$COMMIT" | cut -c1-12)  valid until $EXPIRES"
}

# check_downgrade: the anet already at DEST, if any, must not be newer.
# A validly signed OLD release is exactly what a host that wants to roll
# a fix back would serve.
check_downgrade() {
  INSTALLED=""
  if [ -x "$DEST" ]; then
    INSTALLED="$("$DEST" version 2>/dev/null | sed -n '1s/^anet \([0-9][^ ]*\) .*/\1/p')" || INSTALLED=""
  fi
  if [ -z "$INSTALLED" ]; then
    [ -e "$DEST" ] && warn "cannot read the version of the existing $DEST; it will be replaced"
    return 0
  fi
  case "$(vercmp "$VERSION" "$INSTALLED")" in
    -1) die "$DEST is anet $INSTALLED, newer than the release on offer ($VERSION).
  Refusing to downgrade. If $BASE is a stale mirror, --base URL tries another
  download host; if you meant to downgrade, remove $DEST first." ;;
    0)  say "  $DEST is already $INSTALLED; reinstalling the same version" ;;
    *)  say "  upgrading $DEST: $INSTALLED → $VERSION" ;;
  esac
}

fetch_asset() {
  if [ "$VARIANT" = shell ]; then
    say "→ ${ASSET}.gz (shell variant: CAN run operator-approved commands on this machine)"
  else
    say "→ ${ASSET}.gz"
  fi
  fetch "$BASE/dl/${ASSET}.gz" "$TMP/${ASSET}.gz" "$GZ_SIZE" || die "could not download $BASE/dl/${ASSET}.gz"
  # The compressed file first: nothing unverified reaches gunzip.
  got="$(sha256 "$TMP/${ASSET}.gz")"
  [ "$got" = "$GZ_SHA" ] || die "sha256 mismatch for ${ASSET}.gz
    want $GZ_SHA
    got  $got"
  gunzip -c "$TMP/${ASSET}.gz" > "$TMP/$ASSET" || die "could not decompress ${ASSET}.gz"
  got="$(sha256 "$TMP/$ASSET")"
  [ "$got" = "$RAW_SHA" ] || die "sha256 mismatch for $ASSET
    want $RAW_SHA
    got  $got"
  chmod 755 "$TMP/$ASSET"
  say "  sha256 ok (.gz and binary)"
}

# check_binary PATH: ask the new binary what it is. The module line comes
# from its own registry, so it is the binary's account of what the linker
# kept.
check_binary() {
  out="$("$1" version 2>&1)" || die "the downloaded binary does not run on this machine: $out"
  got_ver="$(printf '%s\n' "$out" | sed -n '1s/^anet \([^ ]*\) .*/\1/p')"
  got_mods="$(printf '%s\n' "$out" | sed -n 's/^modules: //p')"
  [ "$got_mods" = "(none)" ] && got_mods=""
  [ "$got_ver" = "$VERSION" ] || die "the binary reports version '$got_ver', the manifest says $VERSION"
  [ "$got_mods" = "$MODS" ] || die "the binary reports modules '$got_mods', the manifest says '$MODS' for the $VARIANT variant"
  case ",$got_mods," in
    *,shell,*) [ "$VARIANT" = shell ] || die "the default build reports the shell module; refusing it" ;;
  esac
  say "  modules ok: ${got_mods:-(none)}"
}

# install_binary: stage the checked download beside the destination, ask
# the staged copy what it is (check_binary), then rename it over the old
# one — so the old binary is either untouched or replaced whole. The
# self-check runs on the staged copy, not in $TMP: /tmp is mounted noexec on
# many hardened hosts, and the install directory is one binaries run from by
# definition. A failure removes the staged copy (see cleanup).
install_binary() {
  SUDO=""
  if mkdir -p "$PREFIX" 2>/dev/null && [ -w "$PREFIX" ]; then
    :
  elif [ "$USER_MODE" = 0 ]; then
    say "→ ${PREFIX} needs elevated permission; using sudo…"
    SUDO="sudo"
    $SUDO mkdir -p "$PREFIX" || die "could not create $PREFIX"
  else
    die "cannot write to ${PREFIX}"
  fi
  # rm first: cp onto an existing path writes through a symlink planted there.
  STAGED="${DEST}.new"
  { $SUDO rm -f "$STAGED" && $SUDO cp "$TMP/$ASSET" "$STAGED" && $SUDO chmod 755 "$STAGED"; } \
    || die "could not write $STAGED"
  check_binary "$STAGED"
  $SUDO mv -f "$STAGED" "$DEST" || die "could not write $DEST"
  STAGED=""
  # macOS: clear quarantine + ad-hoc sign so the first exec doesn't stall on Gatekeeper.
  if [ "$OS_TAG" = darwin ]; then
    xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true
    if command -v codesign >/dev/null 2>&1; then codesign --force --sign - "$DEST" >/dev/null 2>&1 || true; fi
  fi
  say ""
  say "✓ Installed anet $VERSION → $DEST"
  case ":$PATH:" in
    *":$PREFIX:"*) ;;
    *)
      say ""
      say "  ${PREFIX} is not on your PATH. Add this to your shell profile (~/.zshrc or ~/.bashrc):"
      say "    export PATH=\"$PREFIX:\$PATH\""
      say "  then open a new terminal (or run the export now)."
      ;;
  esac
  if [ -n "$INSTALLED" ]; then
    say "  Daemons already running keep the old binary until restarted: anet stop --all && anet up --all"
  fi
}

# From here on the binary is installed; a step that fails is reported and
# the rest still run, because each prints how to do it by hand.
run_init() {
  say ""
  if has_cmd init; then
    say "→ anet init (explicit safe defaults: inbound closed, no public capabilities, spending limits 0)"
    "$DEST" init || warn "anet init did not complete; run it by hand: anet init"
  else
    say "→ skipping anet init: anet $VERSION has no \`anet init\` command yet"
  fi
}

join_hub() {
  [ -n "$HUB" ] || return 0
  [ -n "$NODE_NAME" ] || NODE_NAME="$(hostname 2>/dev/null || echo anet-node)"
  say ""
  say "→ Starting the node…"
  if ! "$DEST" up >/dev/null 2>&1; then
    warn "the node did not start. Start it by hand with: anet up"
    return 0
  fi
  # `up` returns once the control port is listening, but registration
  # needs the daemon to have finished opening its ledger.
  i=0; while [ $i -lt 20 ]; do
    "$DEST" status >/dev/null 2>&1 && break
    i=$((i+1)); sleep 1
  done
  say "→ Registering with ${HUB} as \"${NODE_NAME}\"…"
  if [ -n "$INVITE" ]; then
    set -- hub-register "$HUB" --name "$NODE_NAME" --token "$INVITE"
  else
    set -- hub-register "$HUB" --name "$NODE_NAME"
  fi
  if "$DEST" "$@"; then
    say "✓ Joined ${HUB}"
  else
    warn "registration did not complete. The node is running; retry with:"
    if [ -n "$INVITE" ]; then
      say "    anet hub-register $HUB --name $NODE_NAME --token <your invite>" >&2
    else
      say "    anet hub-register $HUB --name $NODE_NAME" >&2
      say "  If this hub admits by invite only, ask its operator for a token and add --token." >&2
    fi
  fi
}

wire_agents() {
  [ -n "$AGENTS" ] || return 0
  say ""
  if has_cmd "agents wire"; then
    say "→ anet agents wire $AGENTS"
    # shellcheck disable=SC2086 # AGENTS is a list of tool names
    "$DEST" agents wire $AGENTS || warn "anet agents wire did not complete; run it by hand: anet agents wire $AGENTS"
  else
    say "→ skipping --agents: anet $VERSION has no \`anet agents wire\` command yet;"
    say "  \`anet install --agent <tool>\` writes the older persona text instead"
  fi
}

report() {
  say ""
  if has_cmd doctor; then
    "$DEST" doctor || true
  else
    "$DEST" version || true
  fi
  cat <<'EOF'

Next:
  anet up                      # start your node in the background (survives this shell)
  anet hub-register https://hub.agentnetwork.org.cn --name <you>
  anet status                  # your identity (AID), data dir, console URL
  anet update                  # later: update in place, checked against the release key

Try the network with an official free agent — net.echo sends back what you send it:
  anet find --cap net.echo
  anet delegate <aid from the list> --cap net.echo --args '{"text":"hello"}'
or tell your coding agent: "use anet to call net.echo on the official echo agent with
'hello' and show me the reply".

Out of the box this node accepts no delegations and runs nothing for anyone;
`anet peers allow <aid>` is how you let a specific peer in.
EOF
}

# cleanup: on any exit, the download dir, and a staged binary that was
# never renamed into place.
cleanup() {
  if [ -n "${STAGED:-}" ]; then ${SUDO:-} rm -f "$STAGED" 2>/dev/null || true; fi
  rm -rf "$TMP"
}

main() {
  set -eu
  IDENTITY="anet-release@agentnetwork.org.cn"
  NAMESPACE="anet-release@agentnetwork.org.cn"
  STAGED=""; SUDO=""
  parse_args "$@"
  detect_platform
  check_tools

  TMP="$(mktemp -d)"
  trap cleanup EXIT
  trap 'exit 130' INT TERM HUP
  release_allowed_signers > "$TMP/allowed_signers"

  say "→ Installing anet for ${PLAT} → ${DEST}"
  fetch_manifest
  read_manifest
  check_downgrade
  fetch_asset
  # Signature, expiry, downgrade and both sha256 have passed; only now is
  # anything outside $TMP written, and the last check (what the binary says
  # it is) runs on the staged copy before it replaces anything.
  install_binary
  run_init
  join_hub
  wire_agents
  report
}

main "$@"

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
#                                  also start the node and join a hub
#
# Already installed? Run `anet update` instead: it checks the same signed
# manifest with the release key built into the binary, and needs nothing
# from this script.
#
# An earlier release on this machine (AgentNetwork 1.x from npm or from
# clawnet.cc, anet 0.1 or 0.2) is taken over without a question: its daemon
# is stopped, its command removed or replaced, its data directory renamed
# aside (never deleted), and PATH set so that a new terminal runs the new
# anet. See "EARLIER INSTALLATIONS" below. The installer never waits for
# input; sudo is only used as `sudo -n`.
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
# There is no flag to skip any of these. The manifest and its signature are
# kept beside the installed binary (anet.release.json, anet.release.json.sig)
# so that `anet doctor` can check them, and the binary, again later.
#
# WHAT IS NOT: this script itself, when it is piped into sh. `curl | sh`
# trusts the host that served it and that host's TLS certificate. To not
# trust the host, check the script before running it — the release public
# key is published in SECURITY.md and README.md of
# https://github.com/ANetResearch/ANet:
#
#   curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh
#   curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh.sig
#   echo 'anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn,anet-official@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAMTUwPlzeKmU7qr+eicaQVuxmltc5mY1sTmwfhIJJEL' > allowed_signers
#   ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s install.sh.sig < install.sh && sh install.sh
#
# Flags:
#   --system        Install to /usr/local/bin (uses sudo -n if needed).
#   --user          Install to $HOME/.local/bin (default).
#   --prefix DIR    Install into DIR (overrides the above).
#   --base URL      Download base, https only (or set ANET_INSTALL_BASE).
#                   Without it, the project's hosts are tried first and then
#                   the npm package of the build on npmmirror, jsDelivr and
#                   unpkg (see mirror_bases); every host is checked the same way.
#   --shell         Install the variant that can run operator-approved commands
#                   on this machine. See "--shell" below before using it.
#   --without LIST  Turn these modules off in config.json (a comma-separated
#                   list; names are those of `anet version`'s modules line).
#                   Written before anything is installed, only ever added to.
#   --agents        After installing, wire anet into the coding agents found on
#                   this machine (`anet agents wire --all`). --agents=claude,codex
#                   wires only those.
#   --hub URL       Start the node and join this hub (anet hub-register), as the
#                   last step. Its lines are printed as they are: on a hub that
#                   admits by AID, the last one is the `anet-hub admit <AID>` line
#                   to send to the hub's operator, and joining finishes on its own
#                   once they run it. Without a terminal (an agent running this),
#                   joining works too; the recovery passphrase is set later with
#                   `anet identity protect`.
#   --name NAME     The name to join under (default: this machine's hostname).
#   --no-modify-path  Do not add the install directory to the shell profiles;
#                   the command for them is printed instead.
#   --help          Show this help.
#
# --shell installs a DIFFERENT BINARY, not a setting.
#
# The default binary does not run remote commands (the shell module is not in
# it, which `go tool nm` can be made to confirm on the downloaded file).
# `--shell` fetches the build that has that module. Even that build runs
# nothing until an operator writes a `modules.shell` config block naming the
# commands, and lists the AIDs allowed to call them; an empty list refuses
# everyone. Full contract: https://docs.agentnetwork.org.cn/docs/en/latest/topics/shell.html
#
# What the default binary can do instead, and how to turn it off (the same
# text, in both languages, is internal/a2ashape InstallHeader; a test keeps
# the two equal):
#
#   缺省二进制不执行远程命令（shell 模块不在其中）。运营者配置自动回复之后，它会为对端运行本机 agent，未信任对端只在沙箱里运行。不想要这项功能：--without autoreply 让它永不启动；要二进制里没有这段代码：按 https://docs.agentnetwork.org.cn/docs/zh-cn/latest/topics/modules.html 用 build.sh --without autoreply 自建。
#   The default binary does not run remote commands (the shell module is not in it). Once the operator configures auto-reply, it runs a local agent for peers, and untrusted peers only inside a sandbox. If you do not want this feature: --without autoreply keeps it from ever starting; to leave the code out of the binary, build it yourself with build.sh --without autoreply as described at https://docs.agentnetwork.org.cn/docs/en/latest/topics/modules.html.
#
# --without installs a setting, not a different binary: the names are written
# into the `without` list of config.json by `anet init --without`, run by the
# downloaded binary after its checks and before it replaces anything. A name
# that is not a module of the chosen variant, a running daemon, a module whose
# configuration is already in config.json, and `--shell` together with
# `--without shell` each stop the install with nothing installed or changed.
# To turn a module on again: anet down, anet init --with <module>, anet up.
#
# Everything below is a function and the last line calls main: a download
# cut short defines half a function and runs nothing.

# The anet release key, held by the project's maintainers (not a user key:
# it only vouches that a release came from the project).
# Same line as internal/release/allowed_signers, SECURITY.md and README.md;
# a test in internal/release keeps them equal.
release_allowed_signers() {
  echo 'anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn,anet-official@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAMTUwPlzeKmU7qr+eicaQVuxmltc5mY1sTmwfhIJJEL'
}
# Printed in messages only; ssh-keygen checks the key itself.
release_key_fingerprint() {
  echo 'SHA256:/4FMm/jgZcBII3z3O3r81Y8SxFfugdLRu3zj2gnclD4'
}

usage() {
  cat <<'EOF'
anet installer — verifies the signed release manifest, then installs.

  curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh
  … | sh -s -- [flags]

  --system          install to /usr/local/bin (sudo -n if needed)
  --user            install to ~/.local/bin (default)
  --prefix DIR      install into DIR
  --base URL        download base, https only (or ANET_INSTALL_BASE)
  --shell           the variant that can run operator-approved commands
  --without LIST    turn these modules off (comma-separated), e.g. autoreply
  --agents[=LIST]   wire anet into coding agents found here (all, or LIST)
  --hub URL         start the node and join this hub
  --name NAME       name to join under (default: hostname)
  --no-modify-path  leave the shell profiles alone (PATH is set by you)

Already installed? Use `anet update`. An earlier release (AgentNetwork 1.x,
anet 0.1/0.2) is taken over: stopped, replaced, its data moved aside.
Verify this script before running it: see SECURITY.md in
https://github.com/ANetResearch/ANet
EOF
}

say()  { printf '%s\n' "$*"; }
warn() { printf 'Warning: %s\n' "$*" >&2; }
# die: the error, and then either that nothing was changed or, once an
# earlier release has been taken over (stopped, removed, moved), what was
# done, since that stays done.
die() {
  printf 'Error: %s\n' "$*" >&2
  if [ -n "${TMP:-}" ] && grep -Eq '^(stopped|removed|replaced|moved)[|]' "$TMP/notes" 2>/dev/null; then
    printf 'The new binary was not installed. Already done:\n' >&2
    grep -E '^(stopped|removed|replaced|moved|manual)[|]' "$TMP/notes" | sed 's/^[a-z]*|/  - /' >&2
  else
    printf 'Nothing was installed or changed.\n' >&2
  fi
  exit 1
}

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

# removed_flag FLAG: the cli_removed sentence of anet's reason table for a
# flag 0.3.0 removed, in the language anet would choose (Chinese when
# ANET_LANG begins with zh, English otherwise), on stderr, and exit 2 before
# anything is installed. The text is a2ashape.Sentence("cli_removed", …)
# word for word (internal/release tests it).
removed_flag() {
  _lang="$(printf '%s' "${ANET_LANG:-}" | tr '[:upper:]' '[:lower:]')"
  case "$_lang" in
    zh*) printf '%s 已在 0.3.0 删除；hub 已不再使用邀请码；请删掉这个参数，直接运行 anet hub-register <网址>，再把它打印的 admit 命令发给运营者。\n' "install.sh $1" >&2 ;;
    *)   printf '%s was removed in 0.3.0; hubs no longer use invites; remove this flag, run anet hub-register <url> and send the admit command it prints to the operator.\n' "install.sh $1" >&2 ;;
  esac
  exit 2
}

# add_without LIST — append module names (comma-separated) to WITHOUT, once
# each. A name is lower-case letters and digits only; whether the module
# exists in the chosen variant is checked against the manifest later.
add_without() {
  [ -n "$1" ] || die "--without needs a module name, e.g. --without autoreply"
  for _m in $(printf '%s' "$1" | tr ',' ' '); do
    case "$_m" in
      *[!a-z0-9]*) die "--without: '$_m' is not a module name (lower-case letters and digits, comma-separated)" ;;
    esac
    case ",$WITHOUT," in *",$_m,"*) ;; *) WITHOUT="${WITHOUT:+$WITHOUT,}$_m" ;; esac
  done
}

parse_args() {
  PREFIX=""; USER_MODE=1; BASE_OVERRIDE=""; HUB=""; NODE_NAME=""
  VARIANT="default"; ASSET_PREFIX="anet"; AGENTS=""; WITHOUT=""; MODIFY_PATH=1
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
      --token|--token=*) removed_flag --token ;;
      --token-file|--token-file=*) removed_flag --token-file ;;
      --shell)      VARIANT="shell"; ASSET_PREFIX="anet-shell" ;;
      --without)    [ $# -ge 2 ] || die "--without needs a module name, e.g. --without autoreply"; add_without "$2"; shift ;;
      --without=*)  add_without "${1#--without=}" ;;
      --agents)     AGENTS="--all" ;;
      --agents=*)   AGENTS="$(printf '%s' "${1#--agents=}" | tr ',' ' ')" ;;
      --no-modify-path) MODIFY_PATH=0 ;;
      -h|--help)    usage; exit 0 ;;
      *)            printf 'Error: unknown flag: %s\n' "$1" >&2; usage >&2; exit 2 ;;
    esac
    shift
  done

  case ",$WITHOUT," in
    *,shell,*) [ "$VARIANT" != shell ] || die "--shell installs the shell module and --without shell turns it off; use one of them" ;;
  esac

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
    MINGW*|MSYS*|CYGWIN*) die "this is Windows; install with PowerShell instead: see https://docs.agentnetwork.org.cn/docs/en/latest/intro/install.html#installing-on-windows" ;;
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
  mirror_bases
}

# mirror_bases: after the project's own hosts, the npm package of this
# build, @agentnetwork/<asset prefix>-<os>-<cpu>, whose directory dl/ holds
# release.json, its signature and the .gz exactly as /dl/ does. Content
# delivery networks for npm serve those files by path; npmmirror does so
# only for packages admitted to its file access list and answers 403 until
# then. The order is that of release.MirrorBases in `anet update`. An
# explicit --base is used alone.
mirror_bases() {
  [ -z "$BASE_OVERRIDE" ] || return 0
  case "$ARCH_TAG" in amd64) _cpu=x64 ;; *) _cpu="$ARCH_TAG" ;; esac
  _pkg="@agentnetwork/${ASSET_PREFIX}-${OS_TAG}-${_cpu}"
  BASES="$BASES https://registry.npmmirror.com/${_pkg}/latest/files https://cdn.jsdelivr.net/npm/${_pkg}@latest https://unpkg.com/${_pkg}@latest"
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
  check_without
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

# check_without: every --without name is a module of the chosen variant (its
# modules list in the manifest), or shell, which the default variant does not
# contain and which `anet init` accepts all the same.
check_without() {
  [ -n "$WITHOUT" ] || return 0
  for _m in $(printf '%s' "$WITHOUT" | tr ',' ' '); do
    case ",$MODS,shell," in
      *",$_m,"*) ;;
      *) die "--without $_m: the $VARIANT build has no module of that name (its modules: ${MODS:-none})" ;;
    esac
  done
}

# apply_without: write the --without names into config.json with the
# downloaded binary's own `anet init`, which refuses a running daemon and a
# module whose configuration is already there and then changes nothing. Run
# on the staged copy after its self-check and before it replaces the
# installed binary, so a refusal leaves nothing installed or changed.
apply_without() {
  [ -n "$WITHOUT" ] || return 0
  say "→ anet init --without $WITHOUT"
  out="$("$1" init --without "$WITHOUT" 2>&1)" && _rc=0 || _rc=$?
  if [ "$_rc" -ne 0 ]; then
    printf '%s\n' "$out" >&2
    if grep -Eq '^(stopped|removed|replaced|moved)[|]' "${TMP:-/nonexistent}/notes" 2>/dev/null; then
      printf 'The new binary was not installed. Already done:\n' >&2
      grep -E '^(stopped|removed|replaced|moved|manual)[|]' "$TMP/notes" | sed 's/^[a-z]*|/  - /' >&2
    else
      printf 'Nothing was installed or changed.\n' >&2
    fi
    exit "$_rc"
  fi
  printf '%s\n' "$out" | sed 's/^/  /'
}

# check_downgrade: the anet already at DEST, if any, must not be newer.
# A validly signed OLD release is exactly what a host that wants to roll
# a fix back would serve. Nothing at DEST is run here: classify_anet has
# read the version of a binary that carries one of the two module paths,
# and a file that carries neither is not a program of AgentNetwork or anet
# and is neither run nor replaced.
check_downgrade() {
  INSTALLED=""
  # An earlier release at DEST is replaced whatever its number says:
  # AgentNetwork 1.x counts from 1, above this project's 0.3.
  if [ -e "$DEST" ] || [ -L "$DEST" ]; then
    classify_anet "$DEST"
    case "$CL_KIND" in
      npm-legacy|bin-legacy)
        say "  $DEST is an earlier release (${CL_VER:-version unknown}); it will be replaced"
        return 0 ;;
      npm-current|bin-current)
        # The npm launcher is not run: its version is that of its package.
        INSTALLED=$CL_VER ;;
      *)
        if [ -L "$DEST" ] && [ ! -e "$DEST" ]; then
          say "  $DEST is a link to a file that no longer exists; it will be replaced"
          return 0
        fi
        if [ -f "$CL_TARGET" ] && LC_ALL=C grep -q 'github.com/ANetResearch/ANet' "$CL_TARGET" 2>/dev/null; then
          warn "cannot read the version of the existing $DEST; it will be replaced"
          return 0
        fi
        die "$DEST exists and is not a program of AgentNetwork or anet; it was not run and is not changed.
  Move it away, or install into another directory with --prefix DIR, then run this again." ;;
    esac
  fi
  [ -n "$INSTALLED" ] || return 0
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
    # Never a password prompt: sudo -n, or the command to run first.
    can_sudo || die "${PREFIX} needs root, and sudo needs a password, which this installer never asks for.
  Run \`sudo -v\` in this terminal and then the same install command again,
  or install for this user only (without --system)."
    say "→ ${PREFIX} needs elevated permission; using sudo -n…"
    SUDO="sudo -n"
    $SUDO mkdir -p "$PREFIX" </dev/null || die "could not create $PREFIX"
  else
    die "cannot write to ${PREFIX}"
  fi
  # rm first: cp onto an existing path writes through a symlink planted there.
  STAGED="${DEST}.new"
  { $SUDO rm -f "$STAGED" && $SUDO cp "$TMP/$ASSET" "$STAGED" && $SUDO chmod 755 "$STAGED"; } \
    || die "could not write $STAGED"
  check_binary "$STAGED"
  # Every check has passed. The earlier releases are taken over before the
  # new binary takes the name: their daemons stop and their data moves
  # aside before `anet init` writes anything (apply_without), and the npm
  # package goes before its command link could be confused with ours.
  survey_anets
  stop_legacy
  move_legacy_data
  if [ "$DATA_HELD" = 1 ] && [ -n "$WITHOUT" ]; then
    note manual "--without was not applied, since $DATA_DIR still holds data of an earlier release. After moving it: anet init --without $WITHOUT"
  else
    apply_without "$STAGED"
  fi
  remove_npm_legacy
  $SUDO mv -f "$STAGED" "$DEST" || die "could not write $DEST"
  STAGED=""
  record_release
  # macOS: clear quarantine so the first exec doesn't stall on Gatekeeper.
  # Ad-hoc sign only an arm64 binary whose signature does not verify: the
  # release's darwin/arm64 binaries carry the Go linker's ad-hoc signature
  # (check_binary has just run this one), darwin/amd64 needs none, and
  # re-signing rewrites the file, so its sha256 would no longer be the
  # manifest's and `anet doctor` would report the install unverified.
  if [ "$OS_TAG" = darwin ]; then
    xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true
    if [ "$ARCH_TAG" = arm64 ] && command -v codesign >/dev/null 2>&1 \
       && ! codesign --verify "$DEST" >/dev/null 2>&1; then
      codesign --force --sign - "$DEST" >/dev/null 2>&1 || true
    fi
  fi
  replace_legacy_bins
  note_others
  setup_path
  say ""
  say "✓ Installed anet $VERSION → $DEST"
  upgrade_report
  if [ -n "$INSTALLED" ]; then
    say "  Daemons already running keep the old binary until restarted: anet stop --all && anet up --all"
  fi
}

# record_release: keep the manifest this install was checked against, and
# its signature, beside the binary ($DEST.release.json and .sig; `anet
# update` writes the same pair). `anet doctor` verifies them again with the
# release key inside the binary and compares the binary's sha256 with the
# manifest. The install does not depend on them, so a failure is a warning.
record_release() {
  for f in release.json.sig release.json; do
    { $SUDO rm -f "${DEST}.$f" && $SUDO cp "$TMP/$f" "${DEST}.$f" && $SUDO chmod 644 "${DEST}.$f"; } || {
      warn "could not write ${DEST}.$f; anet doctor will report the release signature as unknown"
      return 0
    }
  done
}

# ---------------------------------------------------------------------------
# EARLIER INSTALLATIONS. Before the new binary takes the name `anet`, every
# other `anet` this user can reach is found and classified (survey_anets,
# classify_anet), and the installer takes over from those of an earlier
# release without asking anything. sudo is only ever `sudo -n`: a password
# prompt is never shown, and what needs one is printed as a command instead.
#
# Earlier releases ("legacy"):
#   AgentNetwork 1.x  the npm package @agentnetwork/anet 1.x (a Node.js shim
#                     that runs the binary of @agentnetwork/anet-<os>-<cpu>),
#                     and the binaries of https://clawnet.cc/install.sh. The
#                     binary contains the Go module path of that project,
#                     ChatChatTech/AgentNetwork; `anet version` prints the bare
#                     version (1.1.15) and starts nothing.
#   anet 0.1, 0.2     binaries of the earlier releases of this project:
#                     `anet version` prints "anet 0.2.1 (commit …)".
# A file named anet that has neither module path is not run, not changed and
# not removed; if it comes first on PATH, the PATH block below shadows it,
# and if it sits at the destination itself, the install is refused before
# anything is changed (check_downgrade).
# An anet of 0.3 or later, and the npm launcher of 0.3 or later, are left in
# place, and reported.
#
# What is done, in this order, each step reported in "Upgraded from":
#   1. the legacy daemon is stopped with the legacy binary's own command
#      (`anet stop` for 1.x, `anet stop --all` for 0.x), with a time limit;
#   2. a data directory of an earlier release is renamed aside, never
#      deleted, and only when no legacy daemon still holds it (datadir_kind);
#   3. the npm package of 1.x is removed with npm itself;
#   4. a legacy binary elsewhere is replaced by a symbolic link to the new
#      binary in a directory this user can write, and removed in one that
#      only root can (a root-owned name must not lead to a file the user can
#      change);
#   5. the shell profiles get a marked block that puts the install directory
#      first on PATH (unless --no-modify-path), and a fresh login shell is
#      asked which `anet` it runs.
# ---------------------------------------------------------------------------

# note CATEGORY TEXT — one line of the "Upgraded from" report.
note() { printf '%s|%s\n' "$1" "$2" >> "$TMP/notes"; }

# run_timeout SECS CMD…: run CMD with a time limit (macOS has no timeout(1)).
# On expiry, the process this function started, and only that one, is sent
# SIGTERM by its PID; the status is then 124.
run_timeout() {
  _rt_s=$1; shift
  "$@" &
  _rt_pid=$!
  ( _rt_i=0
    while [ "$_rt_i" -lt "$_rt_s" ]; do
      sleep 1; _rt_i=$((_rt_i + 1))
      kill -0 "$_rt_pid" 2>/dev/null || exit 0
    done
    : > "$TMP/timeout.$_rt_pid"
    kill -TERM "$_rt_pid" 2>/dev/null ) >/dev/null 2>&1 &
  _rt_w=$!
  _rt_rc=0
  # (stderr of wait: the shell's own "Terminated" line on a time-out)
  { wait "$_rt_pid" || _rt_rc=$?; } 2>/dev/null
  kill -TERM "$_rt_w" 2>/dev/null || true
  wait "$_rt_w" 2>/dev/null || true
  if [ -e "$TMP/timeout.$_rt_pid" ]; then rm -f "$TMP/timeout.$_rt_pid"; return 124; fi
  return "$_rt_rc"
}

# can_sudo: whether sudo works without a password, asked without a prompt.
can_sudo() { command -v sudo >/dev/null 2>&1 && sudo -n true </dev/null >/dev/null 2>&1; }

# clean_text: untrusted program output made printable — control characters
# (escape sequences included) and empty lines removed, at most 6 lines of
# 160 characters.
clean_text() { LC_ALL=C tr -d '\000-\010\013-\037\177' | grep -v '^[[:space:]]*$' | cut -c1-160 | head -n 6 || true; }

# resolve_link PATH: PATH with the symbolic links of its last component
# followed (readlink without -f, which older macOS lacks) and its directory
# made physical.
resolve_link() {
  _rl=$1; _rl_n=0
  while [ -L "$_rl" ] && [ "$_rl_n" -lt 40 ]; do
    _rl_t=$(readlink "$_rl") || break
    case "$_rl_t" in /*) _rl=$_rl_t ;; *) _rl="$(dirname "$_rl")/$_rl_t" ;; esac
    _rl_n=$((_rl_n + 1))
  done
  if _rl_d=$(CDPATH='' cd -P -- "$(dirname "$_rl")" 2>/dev/null && pwd -P); then
    _rl="$_rl_d/$(basename "$_rl")"
  fi
  printf '%s\n' "$_rl"
}

# json_str FILE KEY: the first string value of KEY in a package.json.
json_str() {
  sed -n "s/^[[:space:]]*\"$2\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$1" 2>/dev/null | head -n 1
}

# semver_ok V: V is x.y.z[-pre] and nothing else; output of a program found
# on this machine is untrusted text.
semver_ok() {
  printf '%s' "$1" | grep -Eq '^[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}(-[0-9A-Za-z.-]{1,40})?$'
}

# probe_version BIN: what BIN version prints, as x.y.z, or nothing. Run only
# for a binary that carries the module path of anet or of AgentNetwork 1.x,
# neither of which starts or reads anything for `version`; it still runs in
# an empty HOME, with a bare environment and a time limit.
probe_version() {
  mkdir -p "$TMP/probe-home"
  run_timeout 5 env -i HOME="$TMP/probe-home" PATH=/usr/bin:/bin LC_ALL=C "$1" version \
    </dev/null >"$TMP/probe.out" 2>/dev/null || true
  _pv=$(head -n 1 "$TMP/probe.out" | LC_ALL=C tr -d '\r' | cut -c1-200 \
    | sed -n 's/^anet v\{0,1\}\([0-9][0-9A-Za-z.-]*\).*$/\1/p; s/^v\{0,1\}\([0-9][0-9A-Za-z.-]*\)$/\1/p' | head -n 1)
  if semver_ok "$_pv"; then printf '%s' "$_pv"; fi
}

# npm_plat: npm's name of this platform (darwin-arm64, linux-x64, …).
npm_plat() {
  case "$ARCH_TAG" in amd64) printf '%s-x64' "$OS_TAG" ;; *) printf '%s-%s' "$OS_TAG" "$ARCH_TAG" ;; esac
}

# classify_anet PATH — sets:
#   CL_KIND    npm-legacy, npm-current, bin-legacy, bin-current, other
#   CL_FAM     v1 (AgentNetwork 1.x) or v0 (anet 0.1/0.2), for legacy kinds
#   CL_VER     its version, when it could be read
#   CL_TARGET  PATH with its links followed
#   CL_PKG, CL_NAME, CL_NPMPREFIX   the npm package directory, its name and
#              the npm prefix it is installed under
#   CL_STOP, CL_STOPNODE   the binary to run `stop` with (1: through node)
classify_anet() {
  CL_KIND=other; CL_FAM=""; CL_VER=""; CL_PKG=""; CL_NAME=""; CL_NPMPREFIX=""; CL_STOP=""; CL_STOPNODE=""
  CL_TARGET=$(resolve_link "$1")
  [ -f "$CL_TARGET" ] || return 0
  case "$CL_TARGET" in
    */node_modules/@agentnetwork/*/bin/anet)
      CL_PKG=${CL_TARGET%/bin/anet}
      _pj="$CL_PKG/package.json"
      CL_NAME=$(json_str "$_pj" name)
      CL_VER=$(json_str "$_pj" version)
      case "$CL_NAME" in
        @agentnetwork/anet|@agentnetwork/anet-*) ;;
        *) CL_NAME=""; CL_VER=""; return 0 ;;
      esac
      semver_ok "$CL_VER" || { CL_VER=""; return 0; }
      case "$CL_PKG" in */lib/node_modules/*) CL_NPMPREFIX=${CL_PKG%%/lib/node_modules/*} ;; esac
      # 1.x names its repository ChatChatTech/AgentNetwork; anet's npm
      # packages exist from 0.3.4 on and name ANetResearch/ANet.
      if grep -q 'ChatChatTech/AgentNetwork' "$_pj" 2>/dev/null \
         || [ "$(vercmp "$CL_VER" 0.3.0)" = -1 ] \
         || { [ "$(vercmp "$CL_VER" 1.0.0)" != -1 ] && ! grep -q 'ANetResearch/ANet' "$_pj" 2>/dev/null; }; then
        CL_KIND=npm-legacy; CL_FAM=v1
        if [ "$(head -c 2 "$CL_TARGET" 2>/dev/null)" != '#!' ]; then
          CL_STOP=$CL_TARGET
        else
          for _c in "$CL_PKG/node_modules/@agentnetwork/anet-$(npm_plat)/bin/anet" \
                    "$(dirname "$CL_PKG")/anet-$(npm_plat)/bin/anet"; do
            if [ -f "$_c" ] && [ -x "$_c" ]; then CL_STOP=$_c; break; fi
          done
          if [ -z "$CL_STOP" ] && command -v node >/dev/null 2>&1; then CL_STOP=$CL_TARGET; CL_STOPNODE=1; fi
        fi
      else
        CL_KIND=npm-current
      fi
      return 0 ;;
  esac
  [ -x "$CL_TARGET" ] || return 0
  if LC_ALL=C grep -q 'ChatChatTech/AgentNetwork' "$CL_TARGET" 2>/dev/null; then
    CL_VER=$(probe_version "$CL_TARGET")
    CL_KIND=bin-legacy; CL_FAM=v1; CL_STOP=$CL_TARGET
  elif LC_ALL=C grep -q 'github.com/ANetResearch/ANet' "$CL_TARGET" 2>/dev/null; then
    CL_VER=$(probe_version "$CL_TARGET")
    [ -n "$CL_VER" ] || return 0
    if [ "$(vercmp "$CL_VER" 0.3.0)" = -1 ]; then
      CL_KIND=bin-legacy; CL_FAM=v0; CL_STOP=$CL_TARGET
    else
      CL_KIND=bin-current
    fi
  fi
}

# survey_anets: every `anet` this user can reach — each directory of PATH
# (walked here, since `which -a` is not everywhere), npm's global bin
# directory, and the usual places — classified, one record per line in
# $TMP/anets: kind|fam|version|path|target|pkg|name|npmprefix|stop|stopnode|isdest
survey_anets() {
  : > "$TMP/cands"; : > "$TMP/anets"
  _ifs=$IFS; IFS=:; set -f
  for _d in $PATH; do survey_add "$_d/anet"; done
  IFS=$_ifs; set +f
  if command -v npm >/dev/null 2>&1; then
    run_timeout 20 env NPM_CONFIG_UPDATE_NOTIFIER=false npm prefix -g </dev/null >"$TMP/npm.prefix" 2>/dev/null || true
    _np=$(head -n 1 "$TMP/npm.prefix")
    case "$_np" in /*) survey_add "$_np/bin/anet" ;; esac
  fi
  for _c in /usr/local/bin/anet /opt/homebrew/bin/anet "$HOME/.local/bin/anet" "$HOME/.npm-global/bin/anet" "$DEST"; do
    survey_add "$_c"
  done
  _dest_t=$(resolve_link "$DEST")
  while IFS= read -r _p; do
    classify_anet "$_p"
    _isdest=0
    if [ "$_p" = "$DEST" ] || [ "$CL_TARGET" = "$_dest_t" ]; then _isdest=1; fi
    printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' "$CL_KIND" "$CL_FAM" "$CL_VER" "$_p" "$CL_TARGET" \
      "$CL_PKG" "$CL_NAME" "$CL_NPMPREFIX" "$CL_STOP" "$CL_STOPNODE" "$_isdest" >> "$TMP/anets"
  done < "$TMP/cands"
}

survey_add() {
  case "$1" in /*) ;; *) return 0 ;; esac   # a relative PATH entry is not a place
  case "$1" in *'|'*) return 0 ;; esac
  _sa=$(printf '%s' "$1" | sed 's://*:/:g')
  [ -e "$_sa" ] || [ -L "$_sa" ] || return 0
  grep -Fxq -- "$_sa" "$TMP/cands" 2>/dev/null && return 0
  printf '%s\n' "$_sa" >> "$TMP/cands"
}

# legacy_label FAM: the version an earlier release of family FAM was found
# at, for names and the report ("1.x"/"0.2" when none could be read).
legacy_label() {
  _ll=$(awk -F'|' -v f="$1" '($1=="npm-legacy"||$1=="bin-legacy") && $2==f && $3!="" {print $3; exit}' "$TMP/anets")
  if [ -n "$_ll" ]; then printf '%s' "$_ll"; elif [ "$1" = v1 ]; then printf '1.x'; else printf '0.2'; fi
}

# stop_legacy: each family's daemon, with the first legacy binary of that
# family that can be run, under this user's HOME, with a time limit.
stop_legacy() {
  _st_done=""
  while IFS='|' read -r _k _f _v _p _t _pk _nm _np _sb _sn _dst; do
    case "$_k" in npm-legacy|bin-legacy) ;; *) continue ;; esac
    case " $_st_done " in *" $_f "*) continue ;; esac
    _label="AgentNetwork ${_v:-1.x}"; [ "$_f" = v0 ] && _label="anet ${_v:-0.x}"
    if [ -z "$_sb" ]; then
      note manual "The daemon of $_label could not be stopped here: its binary was not found beside $_p. Stop it before using the new anet."
      continue
    fi
    if [ "$_f" = v1 ]; then _verb=stop; else _verb="stop --all"; fi
    if [ -n "$_sn" ]; then _cmd="node $_sb $_verb"; else _cmd="$_sb $_verb"; fi
    say "→ stopping the daemon of $_label: $_cmd"
    _rc=0
    # shellcheck disable=SC2086 # _verb is one or two words
    if [ -n "$_sn" ]; then
      run_timeout 60 node "$_sb" $_verb </dev/null >"$TMP/stop.out" 2>&1 || _rc=$?
    else
      run_timeout 60 "$_sb" $_verb </dev/null >"$TMP/stop.out" 2>&1 || _rc=$?
    fi
    clean_text < "$TMP/stop.out" | sed 's/^/    /'
    if [ "$_rc" = 0 ]; then
      note stopped "the daemon of $_label ($_cmd)"
      _st_done="$_st_done $_f"
    else
      note manual "Stopping the daemon of $_label failed (status $_rc); stop it by hand: $_cmd"
    fi
  done < "$TMP/anets"
}

# identity_file_age FILE: older, newer, current or invalid — the identity
# file's part of the rule of daemon.ClassifyDataDir. An identity file is a
# CBOR map: anet 0.2's has no key 0 (its first key is 1), this version's
# starts with key 0, the type tag "anet.identity/2".
identity_file_age() {
  _h=$(od -An -tx1 -N2 "$1" 2>/dev/null | tr -d ' \n')
  case "$_h" in
    a[1-9a-f]00|b[0-7]00) ;;
    a[1-9a-f]0[1-9a-f]|a[1-9a-f]1[0-7]|b[0-7]0[1-9a-f]|b[0-7]1[0-7]) echo older; return 0 ;;
    *) echo invalid; return 0 ;;
  esac
  _n=$(LC_ALL=C tr -c 'a-z0-9./' '\n' < "$1" | sed -n 's|^.*anet\.identity/\([0-9][0-9]*\)$|\1|p' | head -n 1)
  if ! printf '%s' "$_n" | grep -Eq '^(0|[1-9][0-9]{0,8})$'; then echo invalid
  elif [ "$_n" -gt 2 ]; then echo newer
  elif [ "$_n" -lt 2 ]; then echo older
  else echo current; fi
}

# legacy_defaults: the constants of this part.
#   V1_MARKERS  entries AgentNetwork 1.x writes when its daemon first starts
#               and that no release of anet writes (daemon.V1Markers)
#   V1_ENTRIES  what 1.x keeps in its data directory, moved out of a mixed
#               directory; not its daemon.log, a name 0.3 writes too
#   V1_API      the address 1.x's daemon serves
#   BASE_PATH   the PATH a new terminal starts from, before its profiles
legacy_defaults() {
  V1_MARKERS='api_token agent_token scoped_tokens.json peer-journal.json anet/identity.json anet/api_token'
  V1_ENTRIES='anet anet.db anet.db-shm anet.db-wal api_token agent_token scoped_tokens.json peer-journal.json cas orgs state daemon.pid'
  V1_API=127.0.0.1:3998
  BASE_PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
  PATH_BEGIN='# >>> anet installer: PATH >>>'
  PATH_END='# <<< anet installer: PATH <<<'
  DATA_KIND=""; DATA_HELD=0; LEGACY_LEFT=0; PATH_CHECK=""; PATH_NOW=""
}

# datadir_kind DIR: none, current, older, newer, invalid, v1 or mixed — the
# rule of daemon.ClassifyDataDir, which the daemon refuses to start by
# (identity_v02 for older and v1); TestInstallShDataDirKindAgrees holds the
# two equal. The identity file decides first; without one, the 1.x markers
# do, and an identity of 0.3 under ids/ makes the directory mixed.
#
# Mixed directories: a 1.x directory into which anet 0.3 has written. Files
# of `anet init` alone (config.json, the peer lists) hold no identity and do
# not count: such a directory is v1 and is moved aside whole, those files
# with it, and the new `anet init` writes them again. When it holds an
# identity of 0.3 (identity.kel, or ids/<name>/identity.kel of 0.3), that identity
# may already be registered with a hub, so the directory stays where it is
# and only the 1.x entries are moved out of it (V1_ENTRIES).
datadir_kind() {
  if [ ! -d "$1" ] || [ -z "$(ls -A "$1" 2>/dev/null)" ]; then echo none; return 0; fi
  _v1=0
  for _m in $V1_MARKERS; do
    if [ -e "$1/$_m" ] || [ -L "$1/$_m" ]; then _v1=1; break; fi
  done
  if [ -f "$1/identity.kel" ]; then
    _age=$(identity_file_age "$1/identity.kel")
    if [ "$_age" = current ] && [ "$_v1" = 1 ]; then echo mixed; else echo "$_age"; fi
    return 0
  fi
  if [ "$_v1" = 0 ]; then echo current; return 0; fi
  for _f in "$1"/ids/*/identity.kel; do
    if [ -f "$_f" ] && [ "$(identity_file_age "$_f")" = current ]; then echo mixed; return 0; fi
  done
  echo v1
}

# answers HOST:PORT: whether something answers HTTP on a loopback address.
answers() {
  case "$1" in 127.0.0.1:[0-9]*|localhost:[0-9]*) ;; *) return 1 ;; esac
  _code=$(curl -s -g --noproxy '*' -o /dev/null --max-time 3 -w '%{http_code}' "http://$1/" 2>/dev/null || true)
  [ -n "$_code" ] && [ "$_code" != 000 ]
}

# data_busy DIR KIND: whether a legacy daemon may still hold DIR. AgentNetwork
# 1.x writes its PID to daemon.pid and serves 127.0.0.1:3998 (V1_API); anet
# 0.2 serves the control_addr of its config.json. Any doubt counts as busy.
data_busy() {
  case "$2" in
    v1|mixed)
      if [ -f "$1/daemon.pid" ]; then
        _pid=$(head -c 32 "$1/daemon.pid" | tr -cd '0-9')
        if [ -n "$_pid" ] && ps -p "$_pid" -o comm= 2>/dev/null | grep -q anet; then return 0; fi
      fi
      answers "$V1_API" ;;
    older)
      _ca=$(json_str "$1/config.json" control_addr)
      [ -n "$_ca" ] && answers "$_ca" ;;
    *) return 1 ;;
  esac
}

# describe_legacy DIR: what of value a moved directory holds, for the report.
describe_legacy() {
  _di=""; _dt=""; _do=""
  for _e in anet/identity.json identity.kel; do [ -e "$1/$_e" ] && _di="${_di:+$_di, }$_e"; done
  for _e in api_token anet/api_token agent_token scoped_tokens.json control_token.txt; do
    [ -e "$1/$_e" ] && _dt="${_dt:+$_dt, }$_e"
  done
  for _e in orgs anet.db config.json; do [ -e "$1/$_e" ] && _do="${_do:+$_do, }$_e"; done
  printf 'identity: %s; API tokens: %s; other data: %s' "${_di:-none}" "${_dt:-none}" "${_do:-none}"
}

# move_legacy_data: the data directory the new anet will use (ANET_DATA_DIR,
# else ANET_HOME, else ~/.anet), renamed aside when it is an earlier
# release's: DIR-legacy-<version>-<UTC time>. Nothing is deleted. Sets
# DATA_HELD=1 when it had to stay because a legacy daemon may still use it.
move_legacy_data() {
  DATA_HELD=0
  DATA_DIR=${ANET_DATA_DIR:-${ANET_HOME:-$HOME/.anet}}
  DATA_DIR=${DATA_DIR%/}
  DATA_KIND=$(datadir_kind "$DATA_DIR")
  case "$DATA_KIND" in
    older|v1|mixed) ;;
    newer) note kept "$DATA_DIR was written by a newer anet; it is left as it is."; return 0 ;;
    invalid) note manual "$DATA_DIR has an identity file that does not read ($DATA_DIR/identity.kel); it is left as it is. See anet doctor."; return 0 ;;
    *) return 0 ;;
  esac
  if [ "$DATA_KIND" = older ]; then _lab=$(legacy_label v0); else _lab=$(legacy_label v1); fi
  _ts=$(date -u +%Y%m%dT%H%M%SZ)
  _to="${DATA_DIR}-legacy-${_lab}-${_ts}"
  [ ! -e "$_to" ] || _to="${_to}-$$"
  # A daemon that stop_legacy asked to stop may exit after its stop command
  # returns: wait for it, at most 10 seconds, before deciding that the
  # directory is still in use.
  if [ -n "${_st_done:-}" ]; then
    _dw=0
    while [ "$_dw" -lt 10 ] && data_busy "$DATA_DIR" "$DATA_KIND"; do
      sleep 1; _dw=$((_dw + 1))
    done
  fi
  if data_busy "$DATA_DIR" "$DATA_KIND"; then
    DATA_HELD=1
    note manual "$DATA_DIR holds data of an earlier release that a running daemon may still use, so it was not moved. Stop that daemon, then: mv '$DATA_DIR' '$_to' && anet init"
    return 0
  fi
  say "→ moving the data of the earlier release aside: $DATA_DIR → $_to"
  if [ "$DATA_KIND" = mixed ]; then
    mkdir -m 700 "$_to" || { DATA_HELD=1; note manual "could not create $_to; move the 1.x files out of $DATA_DIR by hand"; return 0; }
    for _e in $V1_ENTRIES; do
      if [ -e "$DATA_DIR/$_e" ] || [ -L "$DATA_DIR/$_e" ]; then
        mv "$DATA_DIR/$_e" "$_to/" || note manual "could not move $DATA_DIR/$_e to $_to"
      fi
    done
    note moved "the AgentNetwork $_lab files of $DATA_DIR → $_to; the anet 0.3 identity in $DATA_DIR stays where it is. Moved: $(describe_legacy "$_to")."
  else
    if ! mv "$DATA_DIR" "$_to"; then
      DATA_HELD=1
      note manual "could not rename $DATA_DIR; run: mv '$DATA_DIR' '$_to' && anet init"
      return 0
    fi
    note moved "$DATA_DIR → $_to ($(describe_legacy "$_to")). Nothing was deleted; the new anet starts with a new identity."
  fi
}

# remove_npm_legacy: the npm package of 1.x, with npm, under the prefix it is
# installed in; with sudo -n when that prefix is not this user's. If npm is
# not there or fails, the command link alone is removed when it is a link
# into that package, and the npm command is printed. The npm launcher of 0.3
# is removed the same way only when its command is the destination itself
# (an npm prefix of ~/.local): the new binary takes that name, and npm is
# kept consistent with what is there.
remove_npm_legacy() {
  : > "$TMP/npm.done"
  while IFS='|' read -r _k _f _v _p _t _pk _nm _np _sb _sn _dst; do
    [ "$_k" = npm-legacy ] || { [ "$_k" = npm-current ] && [ "$_dst" = 1 ]; } || continue
    grep -Fxq -- "$_pk" "$TMP/npm.done" && continue
    printf '%s\n' "$_pk" >> "$TMP/npm.done"
    _show="npm uninstall -g${_np:+ --prefix $_np} $_nm"
    _npm=$(command -v npm 2>/dev/null || true)
    _ok=0; _how=""
    if [ -n "$_npm" ]; then
      say "→ removing the npm package $_nm $_v: $_show"
      if [ -w "$(dirname "$_pk")" ] && { [ -z "$_np" ] || [ -w "$_np/bin" ]; }; then
        run_timeout 180 env NPM_CONFIG_UPDATE_NOTIFIER=false "$_npm" uninstall -g --no-fund --no-audit --loglevel=error \
          ${_np:+--prefix "$_np"} "$_nm" </dev/null >"$TMP/npm.out" 2>&1 && _ok=1
      elif can_sudo; then
        _how=" (sudo -n)"
        run_timeout 180 sudo -n env PATH="$PATH" NPM_CONFIG_UPDATE_NOTIFIER=false "$_npm" uninstall -g --no-fund --no-audit --loglevel=error \
          ${_np:+--prefix "$_np"} "$_nm" </dev/null >"$TMP/npm.out" 2>&1 && _ok=1
        _show="sudo $_show"
      else
        _show="sudo $_show"
        : > "$TMP/npm.out"
      fi
      clean_text < "$TMP/npm.out" | sed 's/^/    /'
    fi
    if [ "$_ok" = 1 ] && [ ! -e "$_pk" ]; then
      if [ "$_k" = npm-current ]; then
        note removed "npm package $_nm $_v ($_p), whose command was the destination of this install$_how"
      else
        note removed "npm package $_nm $_v ($_p)$_how"
      fi
      continue
    fi
    if [ -L "$_p" ] && [ "$(resolve_link "$_p")" = "$_t" ]; then
      if rm -f "$_p" 2>/dev/null || { can_sudo && sudo -n rm -f "$_p" </dev/null 2>/dev/null; }; then
        note removed "the command $_p (a link into the npm package $_nm $_v)"
      fi
    fi
    note manual "The files of the npm package $_nm $_v are still in $_pk; remove them with: $_show"
  done < "$TMP/anets"
}

# replace_legacy_bins: each legacy binary that is not the destination. In a
# directory this user can write it becomes a symbolic link to the new
# binary: a script, service or agent configuration that names the old path
# then runs the new anet, and so does this terminal, whose shell may have
# remembered that path. In a directory only root can write it is removed
# with sudo -n instead (a link from there would lead root to a file the user
# can change), unless the new binary is root's too (--system).
replace_legacy_bins() {
  while IFS='|' read -r _k _f _v _p _t _pk _nm _np _sb _sn _dst; do
    [ "$_k" = bin-legacy ] && [ "$_dst" != 1 ] || continue
    _label="AgentNetwork ${_v:-1.x}"; [ "$_f" = v0 ] && _label="anet ${_v:-0.x}"
    _dir=$(dirname "$_p")
    _tmp="$_p.anet-new.$$"
    if [ -w "$_dir" ]; then
      if ln -s "$DEST" "$_tmp" 2>/dev/null && mv -f "$_tmp" "$_p"; then
        note replaced "$_p ($_label) → a link to $DEST"
      else
        rm -f "$_tmp" 2>/dev/null || true
        note manual "could not replace $_p ($_label); run: ln -sf '$DEST' '$_p'"
        LEGACY_LEFT=1
      fi
    elif can_sudo; then
      if [ -n "$SUDO" ]; then
        if sudo -n ln -s "$DEST" "$_tmp" </dev/null 2>/dev/null && sudo -n mv -f "$_tmp" "$_p" </dev/null 2>/dev/null; then
          note replaced "$_p ($_label) → a link to $DEST (sudo -n)"; continue
        fi
        sudo -n rm -f "$_tmp" </dev/null 2>/dev/null || true
      elif sudo -n rm -f "$_p" </dev/null 2>/dev/null; then
        note removed "$_p ($_label) (sudo -n)"; continue
      fi
      note manual "could not remove $_p ($_label); run: sudo rm '$_p'"
      LEGACY_LEFT=1
    else
      note manual "$_p is $_label and needs root to remove; run: sudo rm '$_p'"
      LEGACY_LEFT=1
    fi
  done < "$TMP/anets"
}

# note_others: the anets that are not taken over, for the report.
note_others() {
  while IFS='|' read -r _k _f _v _p _t _pk _nm _np _sb _sn _dst; do
    [ "$_dst" = 1 ] && continue
    case "$_k" in
      npm-current) note kept "$_p: the npm launcher $_nm $_v, which runs its own release. Update it with npm update -g $_nm, or remove it with npm uninstall -g $_nm." ;;
      bin-current) [ "$_t" = "$(resolve_link "$DEST")" ] || note kept "$_p: anet $_v." ;;
      other) note kept "$_p: not an AgentNetwork binary; not run and not changed." ;;
    esac
  done < "$TMP/anets"
}

# --- PATH ------------------------------------------------------------------

# path_expr: PREFIX as written into a profile ($HOME/… when under HOME).
path_expr() {
  case "$PREFIX" in
    "$HOME"/*) printf '$HOME/%s' "${PREFIX#"$HOME"/}" ;;
    *) printf '%s' "$PREFIX" ;;
  esac
}

path_block() { # sh|fish
  _pe=$(path_expr)
  printf '%s\n' "$PATH_BEGIN" \
    "# Puts the directory of anet first on PATH. Written by the anet installer;" \
    "# to manage PATH yourself, delete this block and install with --no-modify-path."
  if [ "$1" = fish ]; then
    printf '%s\n' "if test \"\$PATH[1]\" != \"$_pe\"" "    set -gx PATH \"$_pe\" \$PATH" "end"
  else
    printf '%s\n' "case \"\$PATH\" in \"$_pe\"|\"$_pe\":*) ;; *) export PATH=\"$_pe:\$PATH\" ;; esac"
  fi
  printf '%s\n' "$PATH_END"
}

# write_block FILE sh|fish: the marked block in FILE, once, last in the file.
# Prints unchanged, written or an error; the rest of FILE is kept as it is,
# and FILE is written in place (a link to a dotfiles repository stays one).
write_block() {
  _wb=$(path_block "$2")
  if [ -f "$1" ]; then
    _nb=$(grep -cxF "$PATH_BEGIN" "$1" || true)
    _ne=$(grep -cxF "$PATH_END" "$1" || true)
    if [ "$_nb" != "$_ne" ]; then echo "error: $1 has an unterminated anet block"; return 0; fi
    _cur=$(awk -v b="$PATH_BEGIN" -v e="$PATH_END" '$0==b{f=1} f{print} $0==e{f=0}' "$1")
    _last=$(awk 'NF{l=$0} END{print l}' "$1")
    if [ "$_cur" = "$_wb" ] && [ "$_last" = "$PATH_END" ]; then echo unchanged; return 0; fi
    awk -v b="$PATH_BEGIN" -v e="$PATH_END" '
      $0==b { s=1; next }
      s { if ($0==e) s=0; next }
      /^[ \t]*$/ { blank = blank $0 "\n"; next }
      { printf "%s", blank; blank = ""; print }' "$1" > "$TMP/rc.new" || { echo "error: cannot read $1"; return 0; }
  elif [ -e "$1" ]; then
    echo "error: $1 is not a file"; return 0
  else
    mkdir -p "$(dirname "$1")" 2>/dev/null || { echo "error: cannot create $(dirname "$1")"; return 0; }
    : > "$TMP/rc.new"
  fi
  { if [ -s "$TMP/rc.new" ]; then printf '\n'; fi; printf '%s\n' "$_wb"; } >> "$TMP/rc.new"
  if cat "$TMP/rc.new" > "$1" 2>/dev/null; then echo written; else echo "error: cannot write $1"; fi
}

# profile_files: kind|file per line — the login shell's own files (created
# when missing), then those other shells already have. zsh reads ZDOTDIR.
profile_files() {
  _zd=${ZDOTDIR:-$HOME}
  _fish="${XDG_CONFIG_HOME:-$HOME/.config}/fish"
  if [ -f "$HOME/.bash_profile" ]; then _bl="$HOME/.bash_profile"
  elif [ -f "$HOME/.bash_login" ]; then _bl="$HOME/.bash_login"
  else _bl="$HOME/.profile"; fi
  case "$(basename "${SHELL:-sh}")" in
    zsh)  printf 'sh|%s\nsh|%s\n' "$_zd/.zprofile" "$_zd/.zshrc" ;;
    bash) printf 'sh|%s\nsh|%s\n' "$_bl" "$HOME/.bashrc" ;;
    fish) printf 'fish|%s\n' "$_fish/conf.d/anet.fish" ;;
    *)    printf 'sh|%s\n' "$HOME/.profile" ;;
  esac
  for _pf in "$_zd/.zprofile" "$_zd/.zshrc" "$HOME/.bash_profile" "$HOME/.bash_login" "$HOME/.bashrc" "$HOME/.profile"; do
    [ -f "$_pf" ] && printf 'sh|%s\n' "$_pf"
  done
  [ -d "$_fish" ] && printf 'fish|%s\n' "$_fish/conf.d/anet.fish"
  return 0
}

# exec_detached CMD…: exec CMD in a new session, without a controlling
# terminal, so that an interactive shell started to answer a question cannot
# take the terminal from this script. Only ever called as a background job
# (run_timeout), where exec replaces that job and not this script.
exec_detached() {
  if command -v setsid >/dev/null 2>&1; then exec setsid "$@"; fi
  if command -v perl >/dev/null 2>&1; then exec perl -MPOSIX -e 'POSIX::setsid(); exec { $ARGV[0] } @ARGV or exit 127' "$@"; fi
  exec "$@"
}

# fresh_anet: the `anet` a new terminal runs — what a login, interactive
# shell of the user's $SHELL resolves, started with an empty environment
# (HOME, USER, the shell, a base PATH) and stdin from /dev/null, with a time
# limit. Prints the path, or nothing when it cannot be told.
fresh_anet() {
  _sh=${SHELL:-/bin/sh}
  [ -x "$_sh" ] || return 0
  case "$(basename "$_sh")" in sh|bash|zsh|dash|ksh|mksh|yash|fish) ;; *) return 0 ;; esac
  : > "$TMP/fresh.out"
  run_timeout 15 exec_detached env -i HOME="$HOME" USER="${USER:-}" LOGNAME="${LOGNAME:-}" SHELL="$_sh" TERM=dumb \
    PATH="$BASE_PATH" ${ZDOTDIR:+"ZDOTDIR=$ZDOTDIR"} ${XDG_CONFIG_HOME:+"XDG_CONFIG_HOME=$XDG_CONFIG_HOME"} \
    "$_sh" -lic 'command -v anet' </dev/null >"$TMP/fresh.out" 2>/dev/null || true
  LC_ALL=C tr -d '\r' < "$TMP/fresh.out" | grep '^/' | tail -n 1 || true
}

# is_dest PATH: whether PATH is, or leads to, the installed binary.
is_dest() { [ -n "$1" ] && { [ "$1" = "$DEST" ] || [ "$(resolve_link "$1")" = "$(resolve_link "$DEST")" ]; }; }

# setup_path: make `anet` in a new terminal the new binary. A block is
# written when the install directory is not on PATH, when another anet comes
# first on it (one that could not be removed, a 0.3 npm launcher, or a file
# that is not anet), or when a fresh login shell does not resolve `anet` to
# DEST. Then a fresh shell is asked again, and the result is one line.
setup_path() {
  _need=0
  case ":$PATH:" in *":$PREFIX:"*) ;; *) _need=1 ;; esac
  _cur=$(command -v anet 2>/dev/null || true)
  is_dest "$_cur" || _need=1
  _fresh=$(fresh_anet)
  is_dest "$_fresh" || _need=1
  _sh=$(basename "${SHELL:-sh}")
  if [ "$_need" = 1 ]; then
    if [ "$MODIFY_PATH" = 0 ]; then
      note path "PATH was not changed (--no-modify-path). Put $PREFIX first on PATH in your shell profile: export PATH=\"$PREFIX:\$PATH\""
    elif [ "$(id -u)" = 0 ]; then
      note path "running as root: shell profiles were not changed. Put $PREFIX first on PATH for each user who runs anet."
    else
      case "$PREFIX" in
        *[\"\$\`\\]*) note manual "$PREFIX contains a character that cannot go into a profile; put it on PATH by hand." ;;
        *)
          profile_files | awk '!seen[$0]++' > "$TMP/profiles"
          while IFS='|' read -r _kind _file; do
            _r=$(write_block "$_file" "$_kind")
            case "$_r" in
              written)   note path "added the anet PATH block to $_file" ;;
              unchanged) ;;
              *)         note manual "${_r#error: }; put $PREFIX first on PATH in it by hand" ;;
            esac
          done < "$TMP/profiles"
          _fresh=$(fresh_anet) ;;
      esac
    fi
  fi
  if [ -z "$_fresh" ] && ! [ -x "${SHELL:-/bin/sh}" ]; then
    _check="New terminals: not checked (no login shell in \$SHELL)."
  elif is_dest "$_fresh"; then
    _check="New terminals: \`anet\` is $DEST ($_sh -lic 'command -v anet')."
  elif [ -n "$_fresh" ]; then
    _check="New terminals: \`anet\` is $_fresh, not $DEST ($_sh -lic 'command -v anet'); a later line of a shell profile puts its directory first."
  else
    _check="New terminals: $_sh did not say which anet it runs ($_sh -lic 'command -v anet'); check with: command -v anet"
  fi
  PATH_CHECK=$_check
  # This terminal: its PATH cannot be changed from here, and its shell may
  # have remembered the path of a command that is now gone; assigning PATH
  # makes bash and zsh forget those.
  _cur=$(command -v anet 2>/dev/null || true)
  if ! is_dest "$_cur" || grep -Eq '^(removed|replaced)[|]' "$TMP/notes"; then
    if [ "$_sh" = fish ]; then
      PATH_NOW="set -gx PATH \"$PREFIX\" \$PATH"
    else
      PATH_NOW="export PATH=\"$PREFIX:\$PATH\""
    fi
  fi
}

# upgrade_report: the PATH result, and "Upgraded from" when an earlier
# release was found (or another anet is worth naming).
upgrade_report() {
  say ""
  say "  $PATH_CHECK"
  if [ -n "$PATH_NOW" ]; then
    say "  This terminal keeps its PATH; run this once here, or open a new terminal:"
    say "    $PATH_NOW"
  fi
  _from=$(awk -F'|' '$1=="npm-legacy"||$1=="bin-legacy" {
      v = ($3 != "") ? $3 : (($2 == "v1") ? "1.x" : "0.x"); s = v (($1 == "npm-legacy") ? " (npm)" : "")
      if (!seen[s]++) o = o ((o != "") ? ", " : "") s } END { print o }' "$TMP/anets")
  # Data of a release whose command was not found is named too.
  case "$DATA_KIND" in
    older)   grep -q '^[a-z-]*-legacy|v0|' "$TMP/anets" || _from="${_from:+$_from, }0.2 (data)" ;;
    v1|mixed) grep -q '^[a-z-]*-legacy|v1|' "$TMP/anets" || _from="${_from:+$_from, }1.x (data)" ;;
  esac
  if [ -n "$_from" ]; then
    say ""
    say "Upgraded from anet $_from"
  elif [ -s "$TMP/notes" ]; then
    say ""
    say "Other anet installations"
  else
    return 0
  fi
  for _cat in stopped removed replaced moved kept path manual; do
    case "$_cat" in
      stopped) _h="Stopped:" ;; removed) _h="Removed:" ;; replaced) _h="Replaced:" ;;
      moved) _h="Moved aside:" ;; kept) _h="Left in place:" ;; path) _h="PATH:" ;;
      manual) _h="Needs a command from you:" ;;
    esac
    grep "^$_cat|" "$TMP/notes" 2>/dev/null | sed "s/^$_cat|//" > "$TMP/notes.cat" || true
    [ -s "$TMP/notes.cat" ] || continue
    say "  $_h"
    sed 's/^/    - /' "$TMP/notes.cat"
  done
}

# From here on the binary is installed; a step that fails is reported and
# the rest still run, because each prints how to do it by hand.
run_init() {
  say ""
  if [ "$DATA_HELD" = 1 ]; then
    say "→ skipping anet init: $DATA_DIR still holds data of an earlier release (see above)"
    return 0
  fi
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
    return 1
  fi
  # `up` returns once the control port is listening, but registration
  # needs the daemon to have finished opening its ledger.
  i=0; while [ $i -lt 20 ]; do
    "$DEST" status >/dev/null 2>&1 && break
    i=$((i+1)); sleep 1
  done
  say "→ anet hub-register ${HUB} --name ${NODE_NAME}"
  # Its lines as they are, and nothing after them: they say what happened
  # and what to do, the admit line last when the hub has to admit this AID
  # (LEC §4.10). Its exit status is the installer's.
  "$DEST" hub-register "$HUB" --name "$NODE_NAME"
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
  if [ -z "$HUB" ]; then
    cat <<'EOF'

Next:
  anet up                      # start your node in the background (survives this shell)
  anet hub-register https://hub.agentnetwork.org.cn --name <you>
EOF
  else
    printf '\nNext:\n'
  fi
  cat <<'EOF'
  anet status                  # your identity (AID), data dir, console URL
  anet doctor                  # check again later: signature, hub, what this node lets in
  anet update                  # later: update in place, checked against the release key

Use it from your coding agent:
  anet agents wire --all       # or one tool: anet agents wire claude|codex|cursor|opencode|hermes|workbuddy
then restart the agent and ask it: "use anet to list the agents on the network
(list_agents) and show me what each one offers". From the shell: anet find

Out of the box this node accepts no delegations and runs nothing for anyone;
`anet peers allow <aid>` is how you let a specific peer in.
EOF
}

# cleanup: on any exit, the download dir, and a staged binary that was
# never renamed into place.
cleanup() {
  if [ -n "${STAGED:-}" ]; then ${SUDO:-} rm -f "$STAGED" </dev/null 2>/dev/null || true; fi
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
  legacy_defaults
  : > "$TMP/notes"; : > "$TMP/anets"

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
  wire_agents
  report
  # Last, so that what the hub said — the admit line, when there is one —
  # is the last thing printed.
  join_hub
}

main "$@"

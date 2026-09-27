#!/usr/bin/env bash
# deploy/release/build-release.sh — cross-compile `anet` for every supported
# platform, in both variants, write the signed release manifest, and stage a
# self-hosted download tree in dist/.
#
# Pure Go, CGO_ENABLED=0, so cross-compiling is just GOOS/GOARCH.
#
# It was not always. This script used to carry a zig toolchain download and
# per-target musl C compilers because the binary linked mattn/go-sqlite3.
# The runtime became pure Go and nothing removed the machinery, so the
# release path still demanded a C cross-compiler it no longer used — and,
# being a Mac-only path (`shasum`, `clang -arch`), it could not be run from
# the Linux box where the code is written. That is why dist/ went stale.
#
# TWO VARIANTS per platform:
#
#   anet-<os>-<arch>          the default build. Cannot execute commands on
#                             its host: `module/shell` is not linked in, and
#                             `go tool nm` says so.
#   anet-shell-<os>-<arch>    built with `-tags shell`. Can run the commands
#                             its operator names in the config, for callers
#                             its operator lists. See docs/SHELL-zh.md.
#
# Shipping only the second would put command execution on every machine that
# ran the one-line install. Shipping only the first would leave the people
# who need it building from source. So both go out, under names that say
# which is which, and the installer defaults to the one that cannot execute.
#
# SIGNED (A2A-DESIGN §13.2). What a user's installer trusts is not this
# host, nor TLS, nor checksums.txt served next to the binaries — anyone who
# can replace the binaries can replace that file too. It trusts release.json,
# signed with the release key:
#
#   ssh-keygen -Y sign -n anet-release@agentnetwork.org.cn
#
# The manifest names the version, the full commit, the commit time, when it
# was signed and when it stops being accepted, the sha256 of every .gz and of
# the binary inside it, the module set each variant must report, and the
# fingerprint of the next release key. install.sh and `anet update` refuse
# anything that does not match it. install.sh is signed too (install.sh.sig)
# so it can be checked before it is run; see SECURITY.md.
#
# THE OFFICIAL MANIFEST (A2A-DESIGN §15). Which agents the project runs is
# not something a binary learns from a hub: it is internal/official/
# manifest.json — the AID, name, hub and capabilities of each official
# agent, a seq and a validity period — signed with the same release key in
# its own namespace,
#
#   ssh-keygen -Y sign -n anet-official@agentnetwork.org.cn
#
# and compiled in (go:embed), so it has to be committed before the build
# that carries it. `--official` writes and signs it from
# deploy/official/official-agents.txt; a release build refuses to start when
# the committed pair does not verify, is signed by another key, lists other
# agents than official-agents.txt, is refused by internal/official, or would
# expire before the release does (or within ANET_OFFICIAL_MIN_DAYS).
#
# Output (dist/):
#   release.json, release.json.sig   the signed manifest
#   anet-<plat>.gz, anet-shell-<plat>.gz
#   install.sh, install.sh.sig       the installer, signed
#   checksums.txt                    sha256 of each .gz and raw binary — for
#                                    people reading by eye; nothing trusts it
#   VERSION                          the release version, likewise
#
# Publishing: install.sh and install.sh.sig at the root of the download
# host, everything else under /dl/.
#
# Usage:
#   ANET_RELEASE_KEY=~/.ssh/anet-release ./deploy/release/build-release.sh
#                                                every platform, both variants
#   ANET_RELEASE_KEY=… ./deploy/release/build-release.sh linux-amd64
#                                                only the listed platforms
#   ANET_RELEASE_KEY=… ./deploy/release/build-release.sh --resign
#                                                re-date and re-sign the
#                                                manifest already in dist/,
#                                                without rebuilding
#   ANET_RELEASE_KEY=… ./deploy/release/build-release.sh --official
#                                                write and sign
#                                                internal/official/manifest.json
#                                                from deploy/official/
#                                                official-agents.txt; commit
#                                                the two files, then build
#   ./deploy/release/build-release.sh --unsigned [platform…]
#                                                build without signing; the
#                                                result is NOT a release and
#                                                no installer will accept it
#
# Environment:
#   ANET_RELEASE_KEY        path to the release private key. It never lives
#                           in this repository. A .pub path works too when the
#                           private key is loaded in ssh-agent.
#   ANET_RELEASE_TTL_DAYS   how long the manifest is accepted (default 90).
#                           Past that, installs and updates stop until a newer
#                           manifest is signed — run --resign before then if
#                           there is no new release.
#   ANET_OFFICIAL_TTL_DAYS  --official: how long the official manifest marks
#                           its agents (default 365, at most 731). A binary
#                           past it marks no agent official until updated.
#   ANET_OFFICIAL_MIN_DAYS  a release build: the least validity the committed
#                           official manifest must have left (default 180).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DIST="$ROOT/dist"
REL_DIR="$(cd "$(dirname "$0")" && pwd)"
# -w drops DWARF; -s would ALSO drop the symbol table, and is deliberately
# not used.
#
# The pluggability claim is that a downloaded binary can be checked, not
# merely trusted: `go tool nm anet | grep -c module/shell` must be able to
# answer 0 on the file a user actually has. `-s` makes that command return
# "no symbols" — which reads like a passing check while proving nothing. The
# 1.4 MB it saves (14,320 K → 15,760 K) is not worth turning a verifiable
# property back into a promise.
LDFLAGS='-w'
VPKG=github.com/ANetResearch/ANet/internal/version

# The trust anchors, read from the Go source so the manifest, the installer
# and the binary cannot disagree about them. See internal/release/keys.go.
NAMESPACE=anet-release@agentnetwork.org.cn
OFFICIAL_NAMESPACE=anet-official@agentnetwork.org.cn
IDENTITY=anet-release@agentnetwork.org.cn
ALLOWED="$ROOT/internal/release/allowed_signers"
# The official manifest, as committed and embedded, and its source.
OFF_MF="$ROOT/internal/official/manifest.json"
OFF_SRC="$ROOT/deploy/official/official-agents.txt"

info() { printf '\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m✓ %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}
size_of() { wc -c < "$1" | tr -d ' '; }
# utc <epoch> → YYYY-MM-DDTHH:MM:SSZ, the one timestamp shape the manifest
# uses (install.sh compares these as strings).
utc() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }
# "a,b,c" → "a", "b", "c"
json_list() { [ -n "$1" ] && printf '"%s"' "$1" | sed 's/,/", "/g'; true; }

MODE=build
case "${1:-}" in
  --resign)   MODE=resign; shift ;;
  --official) MODE=official; shift ;;
  --unsigned) MODE=unsigned; shift ;;
  -h|--help)  sed -n '2,/^set -euo pipefail/{/^set -euo/!p}' "$0"; exit 0 ;;
esac

TTL_DAYS="${ANET_RELEASE_TTL_DAYS:-90}"
case "$TTL_DAYS" in ''|*[!0-9]*) die "ANET_RELEASE_TTL_DAYS must be a number of days" ;; esac
[ "$TTL_DAYS" -ge 1 ] && [ "$TTL_DAYS" -le 366 ] || die "ANET_RELEASE_TTL_DAYS must be between 1 and 366"

# ── trust anchors ────────────────────────────────────────────────────────
[ -f "$ALLOWED" ] || die "missing $ALLOWED"
KEY_FP="$(awk '{print $3" "$4}' "$ALLOWED" | ssh-keygen -lf - 2>/dev/null | awk '{print $2}')" \
  || die "cannot read the release key from $ALLOWED"
[ -n "$KEY_FP" ] || die "cannot read the release key from $ALLOWED (is ssh-keygen installed?)"
NEXT_FP="$(sed -n 's/^const NextKeyFingerprint = "\(.*\)"$/\1/p' "$ROOT/internal/release/keys.go")"

# check_key: the key we are about to sign with must be the one every binary
# and installer from this commit trusts. A release signed by any other key
# would be refused by all of them — better to find that out here.
check_key() {
  [ "$MODE" = unsigned ] && return 0
  command -v ssh-keygen >/dev/null 2>&1 || die "ssh-keygen (OpenSSH ≥ 8.1) is required to sign"
  [ -n "${ANET_RELEASE_KEY:-}" ] || die "ANET_RELEASE_KEY is not set — point it at the release private key (never commit it), or use --unsigned for a build that is not a release"
  [ -r "$ANET_RELEASE_KEY" ] || die "ANET_RELEASE_KEY=$ANET_RELEASE_KEY is not readable"
  local fp
  fp="$(ssh-keygen -lf "$ANET_RELEASE_KEY" 2>/dev/null | awk '{print $2}')"
  [ "$fp" = "$KEY_FP" ] || die "ANET_RELEASE_KEY is $fp, but internal/release/allowed_signers trusts $KEY_FP"
}

# sign <file> [namespace]: sign, then verify with the committed
# allowed_signers — the same command a user runs by hand. The namespace is
# the release one unless given: the official manifest is signed in its own.
sign() {
  [ "$MODE" = unsigned ] && return 0
  local ns="${2:-$NAMESPACE}"
  rm -f "$1.sig"
  # Not silenced: with a passphrase on the key, ssh-keygen asks for it.
  ssh-keygen -Y sign -f "$ANET_RELEASE_KEY" -n "$ns" "$1" \
    || die "signing $(basename "$1") failed"
  ssh-keygen -Y verify -f "$ALLOWED" -I "$IDENTITY" -n "$ns" -s "$1.sig" < "$1" >/dev/null \
    || die "$(basename "$1").sig does not verify against internal/release/allowed_signers in namespace $ns"
  ok "signed $(basename "$1")  ($KEY_FP, $ns)"
}

NOW="$(date -u +%s)"
RELEASED="$(utc "$NOW")"
EXPIRES="$(utc $((NOW + TTL_DAYS * 86400)))"

# ── --resign: new dates on the manifest already in dist/ ─────────────────
if [ "$MODE" = resign ]; then
  check_key
  MF="$DIST/release.json"
  [ -f "$MF" ] || die "no $MF to re-sign; build a release first"
  # Every asset the manifest names must still be the file in dist/: a
  # re-sign vouches for these bytes again, so it looks at them again.
  ASSET_LINES="$(grep '^    "anet[a-z0-9-]*": {"variant"' "$MF" || true)"
  [ -n "$ASSET_LINES" ] || die "$MF names no assets in the layout this script writes; rebuild instead of re-signing"
  while IFS= read -r line; do
    name="$(printf '%s' "$line" | sed -n 's/^    "\([a-z0-9-]*\)": .*/\1/p')"
    want="$(printf '%s' "$line" | sed -n 's/.*"gz_sha256": "\([0-9a-f]*\)".*/\1/p')"
    [ -f "$DIST/$name.gz" ] || die "$name.gz is named in the manifest but missing from dist/"
    [ "$(sha256_of "$DIST/$name.gz")" = "$want" ] || die "$name.gz no longer matches the manifest"
  done <<EOF_ASSETS
$ASSET_LINES
EOF_ASSETS
  # key_fingerprint names the key that signs, and a reader refuses a
  # manifest whose key_fingerprint is not the signer's: after a key
  # rotation, re-signing with the new key must say so. The next-key
  # commitment is the one this checkout makes. `|` as the sed delimiter
  # because fingerprints are base64 and contain `/`.
  sed -e "s|^  \"released_at\": \"[^\"]*\",\$|  \"released_at\": \"$RELEASED\",|" \
      -e "s|^  \"expires_at\": \"[^\"]*\",\$|  \"expires_at\": \"$EXPIRES\",|" \
      -e "s|^  \"key_fingerprint\": \"[^\"]*\",\$|  \"key_fingerprint\": \"$KEY_FP\",|" \
      -e "s|^  \"next_key_fingerprint\": \"[^\"]*\",\$|  \"next_key_fingerprint\": \"$NEXT_FP\",|" \
      "$MF" > "$MF.new"
  for want in "\"released_at\": \"$RELEASED\"" "\"expires_at\": \"$EXPIRES\"" \
              "\"key_fingerprint\": \"$KEY_FP\"" "\"next_key_fingerprint\": \"$NEXT_FP\""; do
    grep -qF "$want" "$MF.new" || { rm -f "$MF.new"; die "could not rewrite $want into $MF"; }
  done
  mv "$MF.new" "$MF"
  sign "$MF"
  ok "re-signed: released $RELEASED, valid until $EXPIRES"
  exit 0
fi

# ── the official manifest (A2A-DESIGN §15) ───────────────────────────────
# official_entries <out>: the agents of deploy/official/official-agents.txt,
# one manifest line each, in the layout --official writes and the release
# build compares with what is committed. Everything the Go reader
# (internal/official) would refuse is refused here first, with the line.
official_entries() {
  local out="$1" line id oaid hub caps name seen_ids=" " seen_aids=" " n=0 line_no=0
  [ -f "$OFF_SRC" ] || die "missing $OFF_SRC"
  : > "$out"
  while IFS= read -r line || [ -n "$line" ]; do
    line_no=$((line_no + 1))
    line="${line#"${line%%[![:space:]]*}"}"
    case "$line" in ''|'#'*) continue ;; esac
    # read, not `set -- $line`: no glob expansion of a name like "*".
    read -r id oaid hub caps name <<<"$line"
    [ -n "$name" ] || die "official-agents.txt line $line_no: want <id> <aid> <hub> <caps|-> <name…>"
    [[ "$id" =~ ^[a-z0-9][a-z0-9-]{0,63}$ ]] || die "line $line_no: id '$id' must be [a-z0-9-], at most 64"
    [[ "$oaid" =~ ^[a-z0-9]{1,128}$ ]] || die "line $line_no: '$oaid' is not an AID"
    [[ "$hub" =~ ^https?://[^/?#@[:space:]]+(/[^?#[:space:]]*)?$ ]] || die "line $line_no: hub '$hub' is not an http(s) base URL"
    [ "$caps" = - ] && caps=""
    [ -z "$caps" ] || [[ "$caps" =~ ^[a-z0-9][a-z0-9._-]{0,127}(,[a-z0-9][a-z0-9._-]{0,127})*$ ]] \
      || die "line $line_no: caps '$caps' must be comma-separated capability ids"
    case "$name" in *'"'*|*'\'*) die "line $line_no: the name may not contain a double quote or backslash" ;; esac
    # A control character (a tab inside the name) is not valid in a JSON
    # string, and the reader refuses it.
    [[ "$name" =~ [[:cntrl:]] ]] && die "line $line_no: the name contains a control character"
    [ "$(printf '%s' "$name" | wc -c)" -le 128 ] || die "line $line_no: the name is longer than 128 bytes"
    case "$seen_ids" in *" $id "*) die "line $line_no: id $id is listed twice" ;; esac
    case "$seen_aids" in *" $oaid "*) die "line $line_no: aid $oaid is listed twice" ;; esac
    seen_ids="$seen_ids$id "; seen_aids="$seen_aids$oaid "
    printf '    {"id": "%s", "name": "%s", "aid": "%s", "hub": "%s", "caps": [%s]}\n' \
      "$id" "$name" "$oaid" "$hub" "$(json_list "$caps")" >> "$out"
    n=$((n + 1))
  done < "$OFF_SRC"
  [ "$n" -le 256 ] || die "$n official agents, more than the reader accepts (256)"
}

# official_reader_accepts: the reader that matters is the binary's — build
# a throwaway test binary from this checkout and let internal/official
# judge the pair it embeds (signature, key, strict layout; not the date).
official_reader_accepts() {
  ( cd "$ROOT" && go test -count=1 -run '^TestTheEmbeddedManifestVerifies$' ./internal/official/ >/dev/null )
}

# ── --official: write and sign the official manifest ─────────────────────
# One field per line and one agent per line, the layout the release-build
# check below reads with sed. The Go reader (internal/official) is strict:
# unknown members, a duplicate AID or id, a malformed field or a
# key_fingerprint that is not the signer's are refused, and a binary with a
# refused manifest marks no one — so the result is read back by that reader
# before this mode reports success, and the committed pair is put back when
# it is refused.
if [ "$MODE" = official ]; then
  check_key
  OFF_TTL="${ANET_OFFICIAL_TTL_DAYS:-365}"
  case "$OFF_TTL" in ''|*[!0-9]*) die "ANET_OFFICIAL_TTL_DAYS must be a number of days" ;; esac
  [ "$OFF_TTL" -ge 1 ] && [ "$OFF_TTL" -le 731 ] || die "ANET_OFFICIAL_TTL_DAYS must be between 1 and 731"
  PREV_SEQ=0
  if [ -f "$OFF_MF" ]; then
    PREV_SEQ="$(sed -n 's/^  "seq": \([0-9][0-9]*\),$/\1/p' "$OFF_MF")"
    [ -n "$PREV_SEQ" ] || die "cannot read seq from $OFF_MF; it is not in the layout this script writes"
  fi
  SEQ=$((PREV_SEQ + 1))
  OFF_EXPIRES="$(utc $((NOW + OFF_TTL * 86400)))"

  ENTRIES="$(mktemp)"
  OFF_PREV="$(mktemp -d)"
  trap 'rm -rf "$ENTRIES" "$OFF_PREV" "$OFF_MF.new" "$OFF_MF.new.sig"' EXIT
  official_entries "$ENTRIES"
  N="$(wc -l < "$ENTRIES" | tr -d ' ')"

  info "official manifest: seq $SEQ, $N agent(s), valid until $OFF_EXPIRES"
  {
    printf '{\n'
    printf '  "schema": "anet-official/1",\n'
    printf '  "seq": %s,\n' "$SEQ"
    printf '  "issued_at": "%s",\n' "$RELEASED"
    printf '  "expires_at": "%s",\n' "$OFF_EXPIRES"
    printf '  "key_fingerprint": "%s",\n' "$KEY_FP"
    if [ "$N" -eq 0 ]; then
      printf '  "agents": []\n'
    else
      printf '  "agents": [\n'
      sed '$!s/$/,/' "$ENTRIES"
      printf '  ]\n'
    fi
    printf '}\n'
  } > "$OFF_MF.new"
  sign "$OFF_MF.new" "$OFFICIAL_NAMESPACE"
  for f in "$OFF_MF" "$OFF_MF.sig"; do
    if [ -f "$f" ]; then cp -p "$f" "$OFF_PREV/"; fi
  done
  mv "$OFF_MF.new.sig" "$OFF_MF.sig"
  mv "$OFF_MF.new" "$OFF_MF"
  if ! official_reader_accepts; then
    for f in manifest.json manifest.json.sig; do
      if [ -f "$OFF_PREV/$f" ]; then cp -p "$OFF_PREV/$f" "$ROOT/internal/official/$f"; else rm -f "$ROOT/internal/official/$f"; fi
    done
    die "internal/official refuses the manifest just written (the previous pair is back in place); run: go test -run TestTheEmbeddedManifestVerifies -v ./internal/official/"
  fi
  ok "internal/official/manifest.json(.sig): seq $SEQ, $N agent(s), valid until $OFF_EXPIRES"
  echo "  commit internal/official/manifest.json and manifest.json.sig, then build the release"
  exit 0
fi

# check_official: the official manifest this commit embeds verifies against
# the committed release key in its namespace, names the signing key, lists
# exactly the agents deploy/official/official-agents.txt does (an edit to
# the source that was never signed is not released as if it were), is read
# by internal/official, and stays valid past the release being cut — a
# binary installed on the release's last day still marks the official
# agents for a while after.
check_official() {
  [ "$MODE" = unsigned ] && return 0
  [ -f "$OFF_MF" ] && [ -f "$OFF_MF.sig" ] || die "missing internal/official/manifest.json(.sig) — run build-release.sh --official"
  ssh-keygen -Y verify -f "$ALLOWED" -I "$IDENTITY" -n "$OFFICIAL_NAMESPACE" -s "$OFF_MF.sig" < "$OFF_MF" >/dev/null \
    || die "internal/official/manifest.json.sig does not verify in namespace $OFFICIAL_NAMESPACE — run build-release.sh --official and commit"
  grep -qF "  \"key_fingerprint\": \"$KEY_FP\"," "$OFF_MF" \
    || die "internal/official/manifest.json does not name the signing key $KEY_FP — run build-release.sh --official and commit"
  local want got n
  want="$(mktemp)"; got="$(mktemp)"
  official_entries "$want"
  grep '^    {"id": ' "$OFF_MF" | sed 's/,$//' > "$got" || true
  n="$(wc -l < "$want" | tr -d ' ')"
  if ! cmp -s "$want" "$got"; then
    rm -f "$want" "$got"
    die "internal/official/manifest.json does not list the agents deploy/official/official-agents.txt does — run build-release.sh --official and commit"
  fi
  rm -f "$want" "$got"
  official_reader_accepts \
    || die "internal/official refuses the committed manifest — run: go test -run TestTheEmbeddedManifestVerifies -v ./internal/official/"
  local exp min_days min
  exp="$(sed -n 's/^  "expires_at": "\(.*\)",$/\1/p' "$OFF_MF")"
  min_days="${ANET_OFFICIAL_MIN_DAYS:-180}"
  case "$min_days" in ''|*[!0-9]*) die "ANET_OFFICIAL_MIN_DAYS must be a number of days" ;; esac
  min="$(utc $((NOW + min_days * 86400)))"
  # The timestamps have one fixed shape, so they compare as strings.
  [[ "$exp" > "$EXPIRES" ]] && [[ "$exp" > "$min" ]] \
    || die "the official manifest expires at ${exp:-?}: before the release ($EXPIRES) or within $min_days days — run build-release.sh --official and commit"
  ok "official manifest: $(sed -n 's/^  "seq": \([0-9]*\),$/seq \1/p' "$OFF_MF"), $n agent(s), valid until $exp"
}

# ── what is being released ───────────────────────────────────────────────
VERSION="$(sed -n 's/.*V = "\([^"]*\)".*/\1/p' "$ROOT/internal/version/version.go")"
[ -n "$VERSION" ] || die "cannot read version from internal/version/version.go"
COMMIT_FULL="$(cd "$ROOT" && git rev-parse HEAD)"
COMMIT="$(cd "$ROOT" && git rev-parse --short HEAD)"
# A release built from uncommitted work is not the commit it names. `git
# diff --quiet` alone compares the work tree with the index, so staged
# changes and untracked files passed it; this compares with HEAD and looks
# at untracked files too (dist/ itself is git-ignored).
( cd "$ROOT" && git diff --quiet HEAD -- ) || die "working tree differs from HEAD — commit before cutting a release"
[ -z "$(cd "$ROOT" && git status --porcelain --untracked-files=normal)" ] \
  || die "untracked or unstaged files present — commit or remove them before cutting a release"
# BuiltAt is the commit time, not the wall clock: two builds of one commit
# then carry the same stamp, and with -trimpath and CGO off the same bytes,
# so anyone can rebuild and compare against the manifest's sha256.
BUILT="$(cd "$ROOT" && TZ=UTC0 git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd HEAD)"
# A release is built from what the commit's go.mod names, not from sibling
# checkouts a go.work happens to point at: otherwise the manifest names a
# commit that does not reproduce the binary. Unsigned builds keep the
# caller's workspace, which is what a development build wants.
[ "$MODE" = unsigned ] || export GOWORK=off
GOVERSION="$(go env GOVERSION)"

check_key
check_official

ALL="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"
TARGETS="${*:-$ALL}"
for plat in $TARGETS; do
  case " $ALL " in *" $plat "*) ;; *) die "unknown platform $plat (known: $ALL)" ;; esac
done

# build_one <platform> <variant> <variant-tags> <asset-prefix>
build_one() {
  local plat="$1" variant="$2" tags="$3" prefix="$4" goos goarch out
  goos="${plat%%-*}"; goarch="${plat##*-}"
  out="$DIST/${prefix}-${plat}"

  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -C "$ROOT" -trimpath -tags "$tags" \
      -ldflags "$LDFLAGS -X $VPKG.Commit=$COMMIT -X $VPKG.BuiltAt=$BUILT -X $VPKG.Tags=$tags" \
      -o "$out" ./cmd/anet/ || die "build failed: $plat ${tags:-default}"

  local raw_sha raw_size gz_sha gz_size
  raw_sha="$(sha256_of "$out")"; raw_size="$(size_of "$out")"
  # -n: no name, no timestamp in the gzip header, so the .gz is as
  # reproducible as the binary.
  gzip -9 -n -f "$out"
  gz_sha="$(sha256_of "$out.gz")"; gz_size="$(size_of "$out.gz")"
  printf '%s  %s\n%s  %s\n' "$raw_sha" "${prefix}-${plat}" "$gz_sha" "${prefix}-${plat}.gz" >> "$DIST/checksums.txt"
  printf '    "%s": {"variant": "%s", "os": "%s", "arch": "%s", "gz_sha256": "%s", "gz_size": %s, "sha256": "%s", "size": %s}\n' \
    "${prefix}-${plat}" "$variant" "$goos" "$goarch" "$gz_sha" "$gz_size" "$raw_sha" "$raw_size" >> "$DIST/.assets"
  ok "dist/${prefix}-${plat}.gz"
}

info "release anet v${VERSION} (${COMMIT}, built ${BUILT}) → $DIST"
rm -rf "$DIST"; mkdir -p "$DIST"
: > "$DIST/checksums.txt"
: > "$DIST/.assets"
printf '%s\n' "$VERSION" > "$DIST/VERSION"

for plat in $TARGETS; do
  info "build $plat"
  build_one "$plat" default ""      "anet"
  build_one "$plat" shell   "shell" "anet-shell"
done

cp "$REL_DIR/install.sh" "$DIST/install.sh"

# ── module sets ──────────────────────────────────────────────────────────
# What each variant's `anet version` prints on its `modules:` line. That
# line is read off the module registry, which the linker populates, so it
# is the binary's own account of what it contains — and the thing
# install.sh and `anet update` compare against before they install. It is
# taken from a build for THIS machine, which can run; the symbol check
# below is what ties every other platform to it.
info "module sets"
HOSTBIN="$(mktemp -d)"
trap 'rm -rf "$HOSTBIN"' EXIT
host_modules() { # <tags> → modules line of a host build, kept at $HOSTBIN/anet-<variant>
  local bin="$HOSTBIN/anet-${1:-default}"
  CGO_ENABLED=0 go build -C "$ROOT" -trimpath -tags "$1" \
    -ldflags "$LDFLAGS -X $VPKG.Commit=$COMMIT -X $VPKG.BuiltAt=$BUILT -X $VPKG.Tags=$1" \
    -o "$bin" ./cmd/anet/ || die "host build failed (${1:-default})"
  local out ver mods
  out="$("$bin" version)" || die "host build does not run"
  ver="$(printf '%s\n' "$out" | sed -n '1s/^anet \([^ ]*\) .*/\1/p')"
  [ "$ver" = "$VERSION" ] || die "host build reports version '$ver', expected $VERSION"
  mods="$(printf '%s\n' "$out" | sed -n 's/^modules: //p')"
  [ "$mods" = "(none)" ] && mods=""
  printf '%s' "$mods"
}
MODS_DEFAULT="$(host_modules "")"
MODS_SHELL="$(host_modules shell)"
case ",$MODS_DEFAULT," in *,shell,*) die "the default build reports the shell module: $MODS_DEFAULT" ;; esac
case ",$MODS_SHELL," in *,shell,*) ;; *) die "the shell build does not report the shell module: $MODS_SHELL" ;; esac
ok "default: $MODS_DEFAULT"
ok "shell:   $MODS_SHELL"

# ── symbols, on every platform ───────────────────────────────────────────
# The claim each variant's name makes, checked rather than asserted — on
# every platform, not only the one this script runs on: `go tool nm` reads
# ELF and Mach-O alike, and a check that skipped three of four targets left
# three of four shipped defaults unexamined. Beyond shell/no-shell, every
# platform's set of linked module packages must equal that of the host build
# whose `modules:` line went into the manifest above — not merely the first
# target's, which need not be this machine's platform — so the module line
# holds for binaries this machine cannot run.
info "verify: symbols on every platform"
pkgset() { go tool nm "$1" | grep -oE 'ANet/(module/[a-z0-9_]+|internal/mcpserv)\.' | sort -u | tr '\n' ' '; }
REF_D="$(pkgset "$HOSTBIN/anet-default")"; REF_S="$(pkgset "$HOSTBIN/anet-shell")"
[ -n "$REF_D" ] && [ -n "$REF_S" ] || die "no module packages in the host builds: the comparison below would be vacuous"
for plat in $TARGETS; do
  gunzip -kf "$DIST/anet-${plat}.gz" && gunzip -kf "$DIST/anet-shell-${plat}.gz"
  # A stripped binary answers 0 here for the wrong reason, so the absence of
  # symbols is itself a failure rather than a pass.
  go tool nm "$DIST/anet-${plat}" >/dev/null 2>&1 || die "no symbol table in the shipped binary: the check below would pass vacuously"
  d=$(go tool nm "$DIST/anet-${plat}" | grep -c 'module/shell' || true)
  s=$(go tool nm "$DIST/anet-shell-${plat}" | grep -c 'module/shell' || true)
  pd="$(pkgset "$DIST/anet-${plat}")"; ps="$(pkgset "$DIST/anet-shell-${plat}")"
  rm -f "$DIST/anet-${plat}" "$DIST/anet-shell-${plat}"
  [ "$d" -eq 0 ] || die "the default $plat build contains module/shell ($d symbols)"
  [ "$s" -gt 0 ] || die "the shell $plat build does not contain module/shell"
  [ "$pd" = "$REF_D" ] || die "default $plat links different modules from the host build: [$pd] vs [$REF_D]"
  [ "$ps" = "$REF_S" ] || die "shell $plat links different modules from the host build: [$ps] vs [$REF_S]"
  ok "$plat: default=$d shell=$s module/shell symbols; module packages match"
done

# ── the manifest ─────────────────────────────────────────────────────────
# One field per line, one asset per line, in this exact layout: install.sh
# reads it with sed after verifying the signature, and has no JSON parser
# to be lenient with.
info "release.json"
MF="$DIST/release.json"
{
  printf '{\n'
  printf '  "schema": "anet-release/1",\n'
  printf '  "version": "%s",\n' "$VERSION"
  printf '  "commit": "%s",\n' "$COMMIT_FULL"
  printf '  "built_at": "%s",\n' "$BUILT"
  printf '  "released_at": "%s",\n' "$RELEASED"
  printf '  "expires_at": "%s",\n' "$EXPIRES"
  printf '  "go": "%s",\n' "$GOVERSION"
  printf '  "key_fingerprint": "%s",\n' "$KEY_FP"
  printf '  "next_key_fingerprint": "%s",\n' "$NEXT_FP"
  printf '  "variants": {\n'
  printf '    "default": {"asset_prefix": "anet", "tags": "", "modules": [%s]},\n' "$(json_list "$MODS_DEFAULT")"
  printf '    "shell": {"asset_prefix": "anet-shell", "tags": "shell", "modules": [%s]}\n' "$(json_list "$MODS_SHELL")"
  printf '  },\n'
  printf '  "assets": {\n'
  sed '$!s/$/,/' "$DIST/.assets"
  printf '  }\n'
  printf '}\n'
} > "$MF"
rm -f "$DIST/.assets"
ok "release.json: $VERSION, valid until $EXPIRES"

sign "$MF"
sign "$DIST/install.sh"

echo
if [ "$MODE" = unsigned ]; then
  printf '\033[1;33m! UNSIGNED — not a release; install.sh and anet update will refuse it\033[0m\n'
fi
ok "release staged: $DIST"
echo "  version     $VERSION  (commit $COMMIT, built $BUILT)"
echo "  platforms   $TARGETS"
echo "  variants    default, shell"
echo "  manifest    $MF (valid until $EXPIRES)"
echo "  key         $KEY_FP   next: ${NEXT_FP:-none}"
echo "  publish     install.sh + install.sh.sig at the host root; everything else under /dl/"

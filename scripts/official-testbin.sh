#!/usr/bin/env bash
# official-testbin.sh — build an anet binary that marks the given AIDs "anet.official", for tests.
#
# A real binary learns which agents are official from internal/official/manifest.json, signed with the
# release key (A2A-DESIGN §15) and compiled in. A test network has its own official instance (an
# anet-official backend behind a closed + public_capabilities daemon, whose AID exists only once that
# daemon has been created) and no release key, so a joint test that wants to see the mark — and to see
# that a namesake does NOT get it (joint-official.sh, B6-01) — needs a binary whose manifest lists that
# test AID and whose trust is a key the test holds.
#
# This script makes one without any switch in the product: `go build -overlay` replaces, for this one
# build, the three embedded files that decide it —
#
#   internal/official/manifest.json      a manifest listing the AIDs given here
#   internal/official/manifest.json.sig  its SSHSIG, namespace anet-official@agentnetwork.org.cn
#   internal/release/allowed_signers     the throwaway key that signed it
#
# The key is generated in a private temporary directory and its private half deleted before the build:
# nothing else can ever be signed with it. The same overlay makes the binary's release trust that
# throwaway key, so `anet update` in it refuses every real release — a test binary cannot update itself
# into production. Never ship it: `anet version` shows a commit ending in "+official-test", and
# `anet doctor` names the test key.
#
# Usage:
#   scripts/official-testbin.sh -o OUT [-t TAGS] [--target GOOS/GOARCH] [--hub URL] [--ttl-hours N] \
#       [ID=]AID[:CAP,CAP…] …
#
#   -o OUT        output directory (created 0700; if it exists it must be this user's and not writable
#                 by others). Written: OUT/anet, OUT/official/{manifest.json,manifest.json.sig,
#                 allowed_signers,key.pub,overlay.json}
#   -t TAGS       build tags, as for build.sh (e.g. "shell")
#   --target      cross-compile (CGO off), e.g. linux/amd64 for the test hosts; the self-check that runs
#                 the binary is skipped when it cannot run here
#   --hub URL     the hub written into each entry (display only; default http://127.0.0.1)
#   --ttl-hours   how long the manifest marks its agents (default 72)
#   ID=AID:CAPS   one official agent; ID defaults to official-<n>, CAPS to none
#
# Prerequisites: go (with the workspace env sourced, as for any build in this repo — the build must
# resolve ./cmd/anet to THIS checkout, which is checked), ssh-keygen (OpenSSH ≥ 8.1), python3 (for the
# self-check). No network, no daemon, no ports, no processes left behind.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VPKG=github.com/ANetResearch/ANet/internal/version
NS_RELEASE=anet-release@agentnetwork.org.cn
NS_OFFICIAL=anet-official@agentnetwork.org.cn
IDENTITY=anet-release@agentnetwork.org.cn

info() { printf '\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m✓ %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

OUT="" TAGS="" TARGET="" HUB="http://127.0.0.1" TTL_H=72
ENTRIES=()
while [ $# -gt 0 ]; do
  case "$1" in
    -o) OUT="${2:?-o needs a directory}"; shift 2 ;;
    -t) TAGS="${2-}"; shift 2 ;;
    --target) TARGET="${2:?--target needs GOOS/GOARCH}"; shift 2 ;;
    --hub) HUB="${2:?--hub needs a URL}"; shift 2 ;;
    --ttl-hours) TTL_H="${2:?--ttl-hours needs a number}"; shift 2 ;;
    -h|--help) sed -n '2,/^set -euo pipefail/{/^set -euo/!p}' "$0"; exit 0 ;;
    -*) die "unknown option $1 (see --help)" ;;
    *) ENTRIES+=("$1"); shift ;;
  esac
done
[ -n "$OUT" ] || die "-o OUT is required (see --help)"
[ ${#ENTRIES[@]} -ge 1 ] || die "name at least one AID to mark official"
case "$TTL_H" in ''|*[!0-9]*) die "--ttl-hours must be a number" ;; esac
[ "$TTL_H" -ge 1 ] || die "--ttl-hours must be at least 1"
[[ "$HUB" =~ ^https?://[^/?#@[:space:]]+(/[^?#[:space:]]*)?$ ]] || die "--hub '$HUB' is not an http(s) base URL"
case "$OUT" in /*) ;; *) OUT="$PWD/$OUT" ;; esac
for c in go ssh-keygen; do command -v "$c" >/dev/null 2>&1 || die "$c is required"; done

# ── the output directory: this user's, and no one else's to change ────────
if [ -e "$OUT" ]; then
  [ -d "$OUT" ] && [ ! -L "$OUT" ] || die "$OUT exists and is not a directory"
  [ "$(stat -c %u "$OUT")" = "$(id -u)" ] || die "$OUT belongs to another user"
  case "$(stat -c %A "$OUT")" in ?????w????|????????w?) die "$OUT is writable by other users" ;; esac
fi
( umask 077; mkdir -p "$OUT/official" ) || die "cannot create $OUT"
[ "$(stat -c %u "$OUT/official")" = "$(id -u)" ] || die "$OUT/official belongs to another user"

# ── the build must be of this checkout ────────────────────────────────────
DIR="$(go -C "$ROOT" list -f '{{.Dir}}' ./cmd/anet 2>/dev/null)" || die "go cannot resolve ./cmd/anet in $ROOT (source the workspace env.sh)"
[ "$DIR" = "$ROOT/cmd/anet" ] || die "./cmd/anet resolves to $DIR, not this checkout ($ROOT): source this worktree's env.sh"

# ── the throwaway key ─────────────────────────────────────────────────────
KEYDIR="$(mktemp -d)"
trap 'rm -rf "$KEYDIR"' EXIT
chmod 700 "$KEYDIR"
ssh-keygen -q -t ed25519 -N '' -C "anet official-test key (throwaway)" -f "$KEYDIR/key" >/dev/null \
  || die "ssh-keygen could not make a key"
FP="$(ssh-keygen -lf "$KEYDIR/key.pub" | awk '{print $2}')"
PUB="$(awk '{print $1" "$2}' "$KEYDIR/key.pub")"
printf '%s namespaces="%s,%s" %s\n' "$IDENTITY" "$NS_RELEASE" "$NS_OFFICIAL" "$PUB" > "$OUT/official/allowed_signers"
cp "$KEYDIR/key.pub" "$OUT/official/key.pub"

# ── the manifest, in the layout build-release.sh --official writes ────────
utc() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }
NOW="$(date -u +%s)"
LINES=() SEEN=" " n=0
for e in "${ENTRIES[@]}"; do
  n=$((n + 1))
  id="official-$n"; rest="$e"
  case "$rest" in *=*) id="${rest%%=*}"; rest="${rest#*=}" ;; esac
  aid="${rest%%:*}"; caps=""
  case "$rest" in *:*) caps="${rest#*:}" ;; esac
  [[ "$id" =~ ^[a-z0-9][a-z0-9-]{0,63}$ ]] || die "id '$id' must be [a-z0-9-]"
  [[ "$aid" =~ ^[a-z0-9]{1,128}$ ]] || die "'$aid' is not an AID"
  [ -z "$caps" ] || [[ "$caps" =~ ^[a-z0-9][a-z0-9._-]{0,127}(,[a-z0-9][a-z0-9._-]{0,127})*$ ]] \
    || die "caps '$caps' must be comma-separated capability ids"
  case "$SEEN" in *" $aid "*|*" id:$id "*) die "$id / $aid is listed twice" ;; esac
  SEEN="$SEEN$aid id:$id "
  capj=""; [ -z "$caps" ] || capj="$(printf '"%s"' "$caps" | sed 's/,/", "/g')"
  LINES+=("$(printf '    {"id": "%s", "name": "%s", "aid": "%s", "hub": "%s", "caps": [%s]}' "$id" "$id" "$aid" "$HUB" "$capj")")
done
MF="$OUT/official/manifest.json"
{
  printf '{\n'
  printf '  "schema": "anet-official/1",\n'
  printf '  "seq": 1,\n'
  printf '  "issued_at": "%s",\n' "$(utc $((NOW - 300)))"
  printf '  "expires_at": "%s",\n' "$(utc $((NOW + TTL_H * 3600)))"
  printf '  "key_fingerprint": "%s",\n' "$FP"
  printf '  "agents": [\n'
  for i in "${!LINES[@]}"; do
    if [ "$i" -lt $((${#LINES[@]} - 1)) ]; then printf '%s,\n' "${LINES[$i]}"; else printf '%s\n' "${LINES[$i]}"; fi
  done
  printf '  ]\n'
  printf '}\n'
} > "$MF"
rm -f "$MF.sig"
ssh-keygen -Y sign -q -f "$KEYDIR/key" -n "$NS_OFFICIAL" "$MF" >/dev/null 2>&1 || die "signing the manifest failed"
rm -rf "$KEYDIR"   # nothing more is ever signed with it
ssh-keygen -Y verify -f "$OUT/official/allowed_signers" -I "$IDENTITY" -n "$NS_OFFICIAL" -s "$MF.sig" < "$MF" >/dev/null \
  || die "the manifest does not verify against the key just made"
ok "manifest: ${#LINES[@]} agent(s), test key $FP, valid ${TTL_H}h"

# ── build with the three files overlaid ───────────────────────────────────
python3 - "$ROOT" "$OUT/official" > "$OUT/official/overlay.json" <<'PY' || die "python3 is required to write overlay.json"
import json, sys
root, d = sys.argv[1], sys.argv[2]
print(json.dumps({"Replace": {
    root + "/internal/official/manifest.json": d + "/manifest.json",
    root + "/internal/official/manifest.json.sig": d + "/manifest.json.sig",
    root + "/internal/release/allowed_signers": d + "/allowed_signers",
}}, indent=2))
PY
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)+official-test"
info "build anet (${TAGS:-default}${TARGET:+, $TARGET}) → $OUT/anet"
if [ -n "$TARGET" ]; then
  export CGO_ENABLED=0 GOOS="${TARGET%%/*}" GOARCH="${TARGET##*/}"
fi
go build -C "$ROOT" -overlay "$OUT/official/overlay.json" -trimpath -tags "$TAGS" \
  -ldflags "-w -X $VPKG.Commit=$COMMIT -X $VPKG.Tags=$TAGS" -o "$OUT/anet" ./cmd/anet \
  || die "build failed"

# ── self-check: the binary reads the manifest as intended ─────────────────
if [ -z "$TARGET" ] || [ "$TARGET" = "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ]; then
  CHK="$(mktemp -d)"
  trap 'rm -rf "$KEYDIR" "$CHK"' EXIT
  mkdir -p "$CHK/home" "$CHK/xdg" "$CHK/data"; chmod 700 "$CHK/xdg"
  REP="$(env -i PATH="$PATH" HOME="$CHK/home" XDG_RUNTIME_DIR="$CHK/xdg" ANET_DATA_DIR="$CHK/data" \
         "$OUT/anet" doctor --json 2>/dev/null || true)"
  printf '%s' "$REP" | python3 -c '
import json, sys
want_n, want_fp = int(sys.argv[1]), sys.argv[2]
o = json.load(sys.stdin).get("official") or {}
if o.get("status") != "ok" or o.get("agents") != want_n or o.get("key_fingerprint") != want_fp:
    sys.exit("the built binary reports official=%r" % o)' "${#LINES[@]}" "$FP" \
    || die "self-check failed: the overlay did not take"
  ok "self-check: anet doctor reports ${#LINES[@]} official agent(s) under the test key"
else
  ok "cross-compiled for $TARGET: self-check skipped (run '$OUT/anet doctor --json' there: official.status must be ok)"
fi
echo "  binary      $OUT/anet   (commit $COMMIT — a test build; never ship it)"
echo "  manifest    $MF"
echo "  official    ${ENTRIES[*]}"

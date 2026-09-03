#!/usr/bin/env bash
# deploy/release/build-release.sh — cross-compile `anet` for every supported
# platform, in both variants, and stage a self-hosted download tree in dist/.
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
# TWO VARIANTS per platform, which is the whole reason this file changed:
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
# Output (dist/):
#   VERSION                     the release version (from internal/version)
#   anet-<plat>.gz              gzip'd binary, per platform, per variant
#   anet-shell-<plat>.gz
#   checksums.txt               sha256 of each *raw* binary (install.sh
#                               verifies after gunzip)
#   install.sh                  copied from this dir
#
# Usage:
#   ./deploy/release/build-release.sh                 # every platform, both variants
#   ./deploy/release/build-release.sh linux-amd64     # only the listed platforms
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

info() { printf '\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m✓ %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

VERSION="$(sed -n 's/.*V = "\([^"]*\)".*/\1/p' "$ROOT/internal/version/version.go")"
[ -n "$VERSION" ] || die "cannot read version from internal/version/version.go"
COMMIT="$(cd "$ROOT" && git rev-parse --short HEAD 2>/dev/null || echo unknown)"
# A release built from uncommitted work is not the commit it names.
( cd "$ROOT" && git diff --quiet 2>/dev/null ) || die "working tree is dirty — commit before cutting a release"
BUILT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"
  else shasum -a 256 "$1"; fi
}

ALL="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"
TARGETS="${*:-$ALL}"

# build_one <platform> <variant-tag> <asset-prefix>
build_one() {
  local plat="$1" tags="$2" prefix="$3" goos goarch out
  goos="${plat%%-*}"; goarch="${plat##*-}"
  out="$DIST/${prefix}-${plat}"

  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -C "$ROOT" -trimpath -tags "$tags" \
      -ldflags "$LDFLAGS -X $VPKG.Commit=$COMMIT -X $VPKG.BuiltAt=$BUILT -X $VPKG.Tags=$tags" \
      -o "$out" ./cmd/anet/ || die "build failed: $plat ${tags:-default}"

  ( cd "$DIST" && sha256_of "${prefix}-${plat}" >> checksums.txt )
  gzip -9 -f "$out"
  ok "dist/${prefix}-${plat}.gz"
}

VPKG=github.com/ANetResearch/ANet/internal/version

info "release anet v${VERSION} (${COMMIT}) → $DIST"
rm -rf "$DIST"; mkdir -p "$DIST"
: > "$DIST/checksums.txt"
printf '%s\n' "$VERSION" > "$DIST/VERSION"

for plat in $TARGETS; do
  info "build $plat"
  build_one "$plat" ""      "anet"
  build_one "$plat" "shell" "anet-shell"
done

cp "$REL_DIR/install.sh" "$DIST/install.sh"

# The claim each variant's name makes, checked rather than asserted. A
# default binary that turned out to contain the shell module would be the
# one defect in this release nobody would notice until it mattered.
info "verify: the default build must not contain module/shell"
for plat in $TARGETS; do
  case "$plat" in "$(go env GOOS)-$(go env GOARCH)") ;; *) continue ;; esac
  gunzip -kf "$DIST/anet-${plat}.gz" && gunzip -kf "$DIST/anet-shell-${plat}.gz"
  # A stripped binary answers 0 here for the wrong reason, so the absence of
  # symbols is itself a failure rather than a pass.
  go tool nm "$DIST/anet-${plat}" >/dev/null 2>&1 || die "no symbol table in the shipped binary: the check below would pass vacuously"
  d=$(go tool nm "$DIST/anet-${plat}" | grep -c 'module/shell' || true)
  s=$(go tool nm "$DIST/anet-shell-${plat}" | grep -c 'module/shell' || true)
  rm -f "$DIST/anet-${plat}" "$DIST/anet-shell-${plat}"
  [ "$d" -eq 0 ] || die "the default $plat build contains module/shell ($d symbols)"
  [ "$s" -gt 0 ] || die "the shell $plat build does not contain module/shell"
  ok "$plat: default=$d shell=$s symbols"
done

echo
ok "release staged: $DIST"
echo "  version     $VERSION  (commit $COMMIT)"
echo "  platforms   $TARGETS"
echo "  variants    default, shell"
echo "  checksums   $DIST/checksums.txt"

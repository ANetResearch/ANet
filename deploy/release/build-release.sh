#!/usr/bin/env bash
# deploy/release/build-release.sh — cross-compile the `anet` CLI for all supported platforms and
# stage a self-hosted download tree under dist/.
#
# The `anet` binary needs CGO (mattn/go-sqlite3 + sqlite_fts5), so cross-compiling needs a C
# cross-toolchain. We use:
#   • macOS targets  → the system clang (`clang -arch <arch>`) — no extra deps on a Mac host.
#   • linux targets  → a standalone `zig cc` (auto-downloaded to ~/.cache/anet-zig), static musl.
#
# Output (dist/):
#   VERSION                     the release version (from internal/version)
#   anet-darwin-arm64.gz        gzip'd binary per platform
#   anet-darwin-amd64.gz
#   anet-linux-amd64.gz
#   anet-linux-arm64.gz
#   checksums.txt               sha256 of each *raw* binary (verified post-gunzip by install.sh)
#   install.sh                  copied from this dir (the installer users curl|sh)
#
# Usage:
#   ./deploy/release/build-release.sh                 # build every platform
#   ./deploy/release/build-release.sh linux-amd64 ... # build only the listed platforms
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CLI="$ROOT"
DIST="$ROOT/dist"
REL_DIR="$(cd "$(dirname "$0")" && pwd)"

ZIG_VER="0.16.0"
ZIG_HOST="aarch64-macos"   # this build host; only affects which zig tarball we fetch
ZIG_HOME="$HOME/.cache/anet-zig"
ZIG_BIN="$ZIG_HOME/zig-${ZIG_HOST}-${ZIG_VER}/zig"

TAGS="sqlite_fts5"
LDFLAGS='-s -w'

info() { printf '\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m✓ %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

VERSION="$(sed -n 's/.*V = "\([^"]*\)".*/\1/p' "$CLI/internal/version/version.go")"
[ -n "$VERSION" ] || die "cannot read version from internal/version/version.go"

ALL="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"
TARGETS="${*:-$ALL}"

need_zig() { case " $TARGETS " in *" linux-"*) return 0 ;; esac; return 1; }

ensure_zig() {
  need_zig || return 0
  [ -x "$ZIG_BIN" ] && return 0
  info "fetching zig ${ZIG_VER} (linux cross C compiler)"
  mkdir -p "$ZIG_HOME"
  ( cd "$ZIG_HOME"
    curl -fSL --progress-bar -o zig.tar.xz \
      "https://ziglang.org/download/${ZIG_VER}/zig-${ZIG_HOST}-${ZIG_VER}.tar.xz"
    tar -xf zig.tar.xz && rm -f zig.tar.xz )
  [ -x "$ZIG_BIN" ] || die "zig not found at $ZIG_BIN after extract"
}

build_one() {
  local plat="$1" goos goarch out
  goos="${plat%%-*}"; goarch="${plat##*-}"
  out="$DIST/anet-${plat}"

  info "build $plat"
  case "$plat" in
    darwin-arm64)
      CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
        CC="clang -arch arm64" CXX="clang++ -arch arm64" \
        go build -C "$CLI" -tags "$TAGS" -ldflags "$LDFLAGS" -o "$out" ./cmd/anet/ ;;
    darwin-amd64)
      CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 \
        CC="clang -arch x86_64" CXX="clang++ -arch x86_64" \
        go build -C "$CLI" -tags "$TAGS" -ldflags "$LDFLAGS" -o "$out" ./cmd/anet/ ;;
    linux-amd64)
      CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
        CC="$ZIG_BIN cc -target x86_64-linux-musl" \
        CXX="$ZIG_BIN c++ -target x86_64-linux-musl" \
        go build -C "$CLI" -tags "$TAGS" -ldflags "$LDFLAGS -extldflags '-static'" -o "$out" ./cmd/anet/ ;;
    linux-arm64)
      CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
        CC="$ZIG_BIN cc -target aarch64-linux-musl" \
        CXX="$ZIG_BIN c++ -target aarch64-linux-musl" \
        go build -C "$CLI" -tags "$TAGS" -ldflags "$LDFLAGS -extldflags '-static'" -o "$out" ./cmd/anet/ ;;
    *) die "unknown platform: $plat (want one of: $ALL)" ;;
  esac

  # sha256 of the raw binary (install.sh verifies this after gunzip), then gzip for transport.
  ( cd "$DIST" && shasum -a 256 "anet-${plat}" >> checksums.txt )
  gzip -9 -f "$out"
  ok "dist/anet-${plat}.gz"
}

ensure_zig
info "release anet v${VERSION} → $DIST"
rm -rf "$DIST"; mkdir -p "$DIST"
: > "$DIST/checksums.txt"
printf '%s\n' "$VERSION" > "$DIST/VERSION"

for plat in $TARGETS; do build_one "$plat"; done

cp "$REL_DIR/install.sh" "$DIST/install.sh"

echo
ok "release staged: $DIST"
echo "  version     $VERSION"
echo "  platforms   $TARGETS"
echo "  checksums   $DIST/checksums.txt"

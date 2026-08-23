#!/usr/bin/env bash
# build.sh — build with the commit stamped in.
#
# The version constant is hand-maintained and answers "which release is
# this". It cannot answer "is the binary running in production the one I
# just built", because every build between two releases carries the same
# string. The commit stamp answers that, and any check comparing what a
# service reports against what was shipped needs it to mean something.
#
#   bash scripts/build.sh                            build for this machine
#   GOOS=linux GOARCH=amd64 bash scripts/build.sh    cross-build
#   OUT=/tmp/anet bash scripts/build.sh              choose the output path
set -euo pipefail
cd "$(dirname "$0")/.."

PKG=github.com/ANetResearch/ANet/internal/version
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
# -dirty, because a binary built from uncommitted work is not the commit
# it names, and saying so is cheaper than discovering it later.
git diff --quiet 2>/dev/null || COMMIT="$COMMIT-dirty"
BUILT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
OUT=${OUT:-./anet}

CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X $PKG.Commit=$COMMIT -X $PKG.BuiltAt=$BUILT" \
  -o "$OUT" ./cmd/anet
echo "built $OUT  commit=$COMMIT  at=$BUILT"

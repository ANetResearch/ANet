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
#   TAGS=shell bash scripts/build.sh                 pick the build tags
#
# TAGS carries both kinds of tag: `no_<name>` removes a module that is in
# by default, `shell` adds the one that is not. A release builds this
# script more than once — see the variant list in docs/DISTRIBUTIONS-zh.md
# — and the tag string is what distinguishes the artifacts, so it is
# stamped into the binary alongside the commit. A build that cannot say
# which variant it is turns "does this machine accept remote commands"
# into a question answered by guessing.
set -euo pipefail
cd "$(dirname "$0")/.."

PKG=github.com/ANetResearch/ANet/internal/version
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
# -dirty, because a binary built from uncommitted work is not the commit
# it names, and saying so is cheaper than discovering it later.
git diff --quiet 2>/dev/null || COMMIT="$COMMIT-dirty"
BUILT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
OUT=${OUT:-./anet}
TAGS=${TAGS:-}

CGO_ENABLED=0 go build -trimpath -tags "$TAGS" \
  -ldflags "-s -w -X $PKG.Commit=$COMMIT -X $PKG.BuiltAt=$BUILT -X $PKG.Tags=$TAGS" \
  -o "$OUT" ./cmd/anet
echo "built $OUT  commit=$COMMIT  at=$BUILT  tags=${TAGS:-<default>}"

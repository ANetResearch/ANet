#!/usr/bin/env bash
# One-click build for anet. Builds the anet binary into the repo root with the flags it needs
# (CGO on for SQLite, sqlite_fts5 build tag). Run from anywhere.
#
#   ./build.sh            build the binary (fast)
#   ./build.sh --check    gofmt + go vet + go test, then build (full pre-commit check)
#   ./build.sh -c         same as --check
#
# The binary lands at ./anet (git-ignored).
set -euo pipefail

# Resolve the repo root (this script's dir) so it works no matter the caller's cwd.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export CGO_ENABLED=1
TAGS="sqlite_fts5"

CHECK=0
case "${1:-}" in
  -c|--check) CHECK=1 ;;
  "") ;;
  *) echo "usage: build.sh [--check]" >&2; exit 2 ;;
esac

bold() { printf '\033[1;36m== %s ==\033[0m\n' "$*"; }

cd "$ROOT"

if [ "$CHECK" -eq 1 ]; then
  bold "gofmt"
  # Auto-format, and report which files changed (empty = already clean).
  fmtout="$(gofmt -l -w internal cmd)"
  [ -n "$fmtout" ] && printf 'formatted:\n%s\n' "$fmtout" || echo "clean"

  bold "go vet"
  go vet -tags "$TAGS" ./...

  bold "go test"
  go test -tags "$TAGS" ./...
fi

bold "build"
go build -tags "$TAGS" -o anet ./cmd/anet/

# macOS: ad-hoc code-sign the freshly built binary. Locally built (unsigned) binaries can make the very
# first exec hang for seconds while Gatekeeper/syspolicyd assesses them — and a wedged syspolicyd leaves
# UE-state zombie processes that even `kill -9` can't reap until reboot. An ad-hoc signature (`-s -`) makes
# the assessment cheap and avoids the stall. Best-effort: skip silently if codesign is unavailable.
if [ "$(uname -s)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then
  codesign --force --sign - "$ROOT/anet" >/dev/null 2>&1 || true
fi

printf '\033[1;32mbuilt:\033[0m %s\n' "$ROOT/anet"
# NOTE: we deliberately do NOT run `anet version` here to verify — the first exec of a freshly written
# binary can stall on Gatekeeper. Verify yourself in a new terminal: `anet version`.

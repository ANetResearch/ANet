#!/usr/bin/env bash
# One-click build for anet. Run from anywhere; the binary lands at ./anet.
#
#   ./build.sh            build the binary (fast)
#   ./build.sh --check    gofmt + go vet + go test, then build (full pre-commit check)
#   ./build.sh -c         same as --check
#
# Pure Go: CGO is off and there is no build tag. It used to force CGO_ENABLED=1
# and a `sqlite_fts5` tag, for a C SQLite driver the tree no longer uses —
# modernc.org/sqlite is pure Go, and `sqlite_fts5` appears in no source file.
# The cost of leaving that in was not a broken build; it was every developer
# needing a C toolchain for a binary that does not link one.
#
# The binary lands at ./anet (git-ignored).
set -euo pipefail

# Resolve the repo root (this script's dir) so it works no matter the caller's cwd.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export CGO_ENABLED=0
# The opt-in tags. `go test ./...` cannot see code behind an additive tag, so
# a check that does not name them is a check that never compiles, vets or
# tests those modules — for `shell`, the one module that executes commands on
# the host, that is the last place to have no coverage.
OPTIN="shell"

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
  go vet ./...
  go vet -tags "$OPTIN" ./...

  bold "go test"
  go test ./...
  bold "go test -tags $OPTIN"
  go test -tags "$OPTIN" ./...

  # The default binary must not contain the opt-in modules. Checked here and
  # not only in CI, because this is the script a developer runs before they
  # push, and "it is absent by default" is the claim the whole tag rests on.
  bold "opt-in modules must be absent by default"
  go build -o "$ROOT/.anet-tagcheck" ./cmd/anet/
  for m in $OPTIN; do
    n=$(go tool nm "$ROOT/.anet-tagcheck" | grep -c "module/$m" || true)
    [ "$n" -eq 0 ] || { echo "module/$m is linked into the default build ($n symbols)" >&2; exit 1; }
    echo "  module/$m absent by default ✓"
  done
  rm -f "$ROOT/.anet-tagcheck"
fi

bold "build"
go build -o anet ./cmd/anet/

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

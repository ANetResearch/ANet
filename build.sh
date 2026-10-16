#!/usr/bin/env bash
# One-click build for anet. Run from anywhere; the binary lands at ./anet.
#
#   ./build.sh            build the binary (fast)
#   ./build.sh --check    gofmt + go vet + go test + the build-tag checks, then build
#                         (full pre-commit check)
#   ./build.sh -c         same as --check
#   ./build.sh --without autoreply,x402   leave modules out of the binary
#                         (-tags no_<module>); the names are those of the
#                         `anet version` modules line. A module that has no
#                         build tag yet (build_tag_pending) is turned off in
#                         the configuration instead: anet init --without <m>.
#   ./build.sh --with shell               add the shell module (-tags shell)
#   scripts/tagcheck.sh recipes           the self-build recipes, as examples
#                                         of --without
#
# Neither runs the race detector, which needs cgo. The whole-repo race run is
#
#   CGO_ENABLED=1 go test -race -timeout 45m ./...
#
# and needs the -timeout: internal/daemon alone takes about 660-714 s under
# -race, past go test's default limit of 10 minutes per test binary, and the
# run then fails as "test timed out" with no failing test. CI's race job
# passes the same flag.
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
# the host, that is the last place to have no coverage. scripts/tagcheck.sh
# holds the full lists, both directions.
OPTIN="shell"

CHECK=0
TAGS=""
WITHOUT=""
WITH=""

# build_tag_pending M ZH EN — the sentence build_tag_pending of anet's reason
# table (internal/a2ashape; a test keeps this text equal to it) in the
# language anet would choose (LC_ALL, LC_MESSAGES, LANG; Chinese when it begins
# with zh), on stderr, and exit 2 before anything is built.
build_tag_pending() {
  case "${LC_ALL:-${LC_MESSAGES:-${LANG:-}}}" in
    zh*) printf '这个版本还不能在构建时去掉%s模块（%s），随 v0.3.x 提供；现在可以用 --without %s 在配置里关闭它，什么都没有构建。\n' "$2" "$1" "$1" >&2 ;;
    *)   printf 'This version cannot leave the %s module (%s) out at build time yet; that comes in v0.3.x. For now you can turn it off in the configuration with --without %s; nothing was built.\n' "$3" "$1" "$1" >&2 ;;
  esac
  exit 2
}

usage() { echo "usage: build.sh [--check] [--without <module>[,<module>...]] [--with shell]" >&2; exit 2; }

while [ $# -gt 0 ]; do
  case "$1" in
    -c|--check) CHECK=1 ;;
    --without)  [ $# -ge 2 ] || usage; WITHOUT="${WITHOUT:+$WITHOUT,}$2"; shift ;;
    --without=*) WITHOUT="${WITHOUT:+$WITHOUT,}${1#--without=}" ;;
    --with)     [ $# -ge 2 ] || usage; WITH="${WITH:+$WITH,}$2"; shift ;;
    --with=*)   WITH="${WITH:+$WITH,}${1#--with=}" ;;
    *) usage ;;
  esac
  shift
done

# Names to tags. The lists live in scripts/tagcheck.sh (one source); a name
# that is in neither is not a module of this tree.
if [ -n "$WITHOUT" ] || [ -n "$WITH" ]; then
  SUBTAGS="$(bash "$ROOT/scripts/tagcheck.sh" list)"
  PENDING="$(bash "$ROOT/scripts/tagcheck.sh" pending)"
  for m in $(printf '%s' "$WITHOUT" | tr ',' ' '); do
    if printf '%s\n' "$SUBTAGS" | grep -qx "no_$m"; then
      case ",$TAGS," in *",no_$m,"*) ;; *) TAGS="${TAGS:+$TAGS,}no_$m" ;; esac
    elif printf '%s\n' "$PENDING" | grep -qx "$m"; then
      case "$m" in
        autoreply) build_tag_pending autoreply 自动回复 auto-reply ;;
        console)   build_tag_pending console 控制台 console ;;
        *)         build_tag_pending "$m" "$m" "$m" ;;
      esac
    elif [ "$m" = shell ]; then
      :  # shell is not in the default build; there is nothing to leave out
    else
      echo "build.sh: --without $m: there is no module of that name (the modules line of anet version lists them)" >&2
      exit 2
    fi
  done
  for m in $(printf '%s' "$WITH" | tr ',' ' '); do
    if [ "$m" = shell ] && printf '%s\n' "$SUBTAGS" | grep -qx shell; then
      case ",$TAGS," in *",shell,"*) ;; *) TAGS="${TAGS:+$TAGS,}shell" ;; esac
    else
      echo "build.sh: --with $m: only shell can be added (--with shell)" >&2
      exit 2
    fi
  done
fi

bold() { printf '\033[1;36m== %s ==\033[0m\n' "$*"; }

cd "$ROOT"

if [ "$CHECK" -eq 1 ]; then
  bold "gofmt"
  # Auto-format, and report which files changed (empty = already clean).
  # The whole tree, as CI's `gofmt -l .` checks it: module/, provider/ and
  # tools/ used to be left out, so a --check that passed here could still
  # fail CI's gofmt step.
  fmtout="$(gofmt -l -w .)"
  [ -n "$fmtout" ] && printf 'formatted:\n%s\n' "$fmtout" || echo "clean"

  bold "go vet"
  go vet ./...
  go vet -tags "$OPTIN" ./...

  bold "go test"
  go test ./...
  bold "go test -tags $OPTIN"
  go test -tags "$OPTIN" ./...

  # Both directions of every tag, by symbol count: the default binary must
  # not contain the opt-in modules, and each `no_<name>` must remove all of
  # its module — alone, and all of them together. Checked here and not only
  # in CI, because this is the script a developer runs before they push,
  # and "it is absent from this build" is the claim every tag rests on.
  # The same script is what CI runs, so the two agree on what a module's
  # code looks like in a binary.
  bold "go vet with every subtractive tag"
  go vet -tags "$(bash "$ROOT/scripts/tagcheck.sh" list | grep '^no_' | paste -sd, -)" ./...
  bold "build tags, both directions"
  bash "$ROOT/scripts/tagcheck.sh" all
fi

bold "build"
if [ -n "$TAGS" ]; then
  echo "tags: $TAGS"
  go build -tags "$TAGS" -o anet ./cmd/anet/
else
  go build -o anet ./cmd/anet/
fi

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

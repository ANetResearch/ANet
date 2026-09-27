#!/usr/bin/env bash
# tagcheck.sh — prove each build tag does what its name says, by symbol count.
#
#   bash scripts/tagcheck.sh check <tags>   one tag set (CI: one matrix row)
#   bash scripts/tagcheck.sh deps [<tags>]  SI-8: the kernel never reaches a2a-go
#   bash scripts/tagcheck.sh all            everything below (build.sh --check)
#   bash scripts/tagcheck.sh list           the two tag lists, one per line
#
# A tag set is comma separated, as `go build -tags` takes it. EVERY tag in
# it is checked, in its own direction:
#
#   no_<m>  subtractive  the default build contains m, the tagged one none of it
#   <m>     additive     the default build contains none of m, the tagged one does
#
# The CI step this replaces checked only the first tag of a combination
# row, so "no_x402,no_mcp,…" proved x402 was gone and said nothing about
# the other eight. And it checked one direction per job: nothing here ever
# asked whether the default binary still carried a module a tag had been
# moved away from.
#
# One script for CI and for `build.sh --check`, so the two cannot drift
# into different ideas of what a module's code looks like in a binary.
#
# A tag whose module is not in this tree yet (no_a2a before module/a2a
# lands) is still checked in the direction that can fail — nothing of it
# may be in the lean build — and says it skipped the other. An unknown tag
# is an error: `-tags no_x420` builds the default binary without a word,
# and a matrix row with a typo would otherwise pass forever.
set -euo pipefail
cd "$(dirname "$0")/.."
export CGO_ENABLED=0

# In by default, removed with -tags no_<m>.
SUBTRACTIVE="anetlink p2p blackboard org cas service mcp x402 a2a"
# Absent by default, added with -tags <m>. shell executes commands on the
# host; taskboard keeps caller text on a board anyone can read (A2A-DESIGN
# §16). For both, the build nobody asked about is the one that must lack it.
ADDITIVE="shell taskboard"

# pattern <m> — what the module's code is called in `go tool nm` output.
#
# Most modules are module/<m>, some with a provider/<m> half. MCP is not a
# module: it is internal/mcpserv plus internal/agentwire, the code that
# wires it into coding tools. a2a is the exception A2A-DESIGN §16 names:
# the derived pattern would not see the SDK, and an a2a-go that stays
# linked after module/a2a is gone is the half of the claim that matters.
pattern() {
  case $1 in
    a2a) echo 'module/a2a|a2aproject/a2a-go' ;;
    mcp) echo 'internal/mcpserv|internal/agentwire' ;;
    *)   echo "provider/$1|module/$1" ;;
  esac
}

# intree <m> — the parts of the pattern that are packages in this tree.
# A part with no Go files here (provider/x402, or internal/agentwire before
# it lands) cannot be expected to show up in any binary.
intree() {
  local part out=()
  IFS='|' read -ra parts <<<"$(pattern "$1")"
  for part in "${parts[@]}"; do
    case $part in module/*|provider/*|internal/*) ;; *) continue ;; esac
    compgen -G "$part/*.go" >/dev/null && out+=("$part")
  done
  echo "${out[*]:-}"
}

is_in() { local w; for w in $2; do [ "$w" = "$1" ] && return 0; done; return 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# nmfile <tags> — the symbol table of ./cmd/anet built with those tags,
# built once per tag set per run.
nmfile() {
  local key=${1:-default}; key=${key//,/+}
  if [ ! -f "$WORK/$key.nm" ]; then
    go build -tags "$1" -o "$WORK/$key.bin" ./cmd/anet >&2 || return 1
    go tool nm "$WORK/$key.bin" >"$WORK/$key.nm.tmp" || return 1
    mv "$WORK/$key.nm.tmp" "$WORK/$key.nm"
    rm -f "$WORK/$key.bin"
  fi
  echo "$WORK/$key.nm"
}

count() { grep -cE "$1" "$2" || true; }

# check <tags> — every tag in the set, each in its own direction, against
# one default binary and one binary built with the whole set.
check() {
  local set=$1 t m dir pat base var n part fail=0 present
  [ -n "$set" ] || { echo "tagcheck: check needs a tag set" >&2; return 2; }
  # Explicit, not errexit: a function called as `check … || fail=1` runs
  # with errexit off, and a build that failed would be counted as a
  # binary with no symbols in it.
  base=$(nmfile "") || { echo "  ✗ the default build failed" >&2; return 1; }
  var=$(nmfile "$set") || { echo "  ✗ the -tags $set build failed" >&2; return 1; }
  echo "tags $set"
  for t in ${set//,/ }; do
    case $t in
      no_*) m=${t#no_}; dir=sub
            is_in "$m" "$SUBTRACTIVE" || { echo "  ✗ $t: not a subtractive tag of this repo" >&2; fail=1; continue; } ;;
      *)    m=$t; dir=add
            is_in "$m" "$ADDITIVE" || { echo "  ✗ $t: not an additive tag of this repo" >&2; fail=1; continue; } ;;
    esac
    pat=$(pattern "$m")
    present=$(intree "$m")
    # "with" is the binary that should carry the module, "without" the
    # one that must not. Which binary is which is the tag's direction.
    local with=$base without=$var
    [ "$dir" = add ] && { with=$var; without=$base; }
    n=$(count "$pat" "$without")
    if [ "$n" -ne 0 ]; then
      if [ "$dir" = sub ]; then echo "  ✗ $m: $n symbols ($pat) still linked into the -tags $set build" >&2
      else echo "  ✗ $m: $n symbols ($pat) linked into the DEFAULT build" >&2; fi
      fail=1; continue
    fi
    if [ -z "$present" ]; then
      echo "  - $m: not in this tree yet; checked absent, presence skipped"
      continue
    fi
    for part in $present; do
      n=$(count "$part" "$with")
      if [ "$n" -eq 0 ]; then
        if [ "$dir" = sub ]; then echo "  ✗ $m: the default build contains nothing of $part" >&2
        else echo "  ✗ $m: -tags $set did not link $part in" >&2; fi
        fail=1; continue 2
      fi
    done
    if [ "$dir" = sub ]; then echo "  ✓ $m: default=$(count "$pat" "$with") -tags=0"
    else echo "  ✓ $m: default=0 -tags=$(count "$pat" "$with")"; fi
  done
  return $fail
}

# deps [<tags>] — SI-8: neither the kernel nor the MCP server may depend on
# the A2A SDK, whatever module/a2a does with it. Checked on the import
# graph rather than on a binary, because a kernel that reaches a2a-go is
# wrong even in a build where module/a2a happens to link it anyway.
deps() {
  local tags=${1:-} graph hits
  graph=$(go list -deps -tags "$tags" ./internal/mcpserv ./internal/daemon) \
    || { echo "  ✗ go list failed (tags ${tags:-default})" >&2; return 1; }
  hits=$(grep a2aproject <<<"$graph" || true)
  if [ -n "$hits" ]; then
    printf '  ✗ internal/mcpserv or internal/daemon depends on a2aproject (tags %s):\n%s\n' \
      "${tags:-default}" "$hits" >&2
    return 1
  fi
  echo "  ✓ internal/mcpserv, internal/daemon: no a2aproject in the import graph (tags ${tags:-default})"
}

all() {
  local m fail=0 every=""
  for m in $SUBTRACTIVE; do
    check "no_$m" || fail=1
    every="${every:+$every,}no_$m"
  done
  # All of them at once: a module can be cleanly removable alone and
  # still be what keeps another one linked.
  check "$every" || fail=1
  for m in $ADDITIVE; do check "$m" || fail=1; done
  echo "dependency closure"
  deps "" || fail=1
  deps "no_a2a" || fail=1
  return $fail
}

case "${1:-}" in
  check) check "${2:-}" ;;
  deps)  deps "${2:-}" ;;
  all)   all ;;
  list)  for m in $SUBTRACTIVE; do echo "no_$m"; done; for m in $ADDITIVE; do echo "$m"; done ;;
  *) sed -n '2,6p' "$0" >&2; exit 2 ;;
esac

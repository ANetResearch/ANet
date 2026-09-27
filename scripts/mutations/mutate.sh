#!/usr/bin/env bash
# mutate.sh — run a joint-level mutation: build the joint binary set with a patch from this directory
# applied, run scripts/joint.sh against it, and say whether the run caught it.
#
# A patch names the repository it applies to on a line "Repository: ANetCore|ANet|ANetHub" before its
# first "diff --git" (ANet when there is none) and what must go red on a line "Expect: …". Nothing here
# writes to a checkout: the working trees of the three repositories (tracked and untracked files, not
# ignored ones) are copied into a scratch directory, the patches are applied to the copies, and the
# binaries are built from them with a go.work of their own. Several patches may be given together.
#
#   mutate.sh list                         the patches, their repository and what each expects
#   mutate.sh build -o DIR PATCH…          the mutated binary set into DIR (anet, anetfixture, anetpeer,
#                                          anet-official, anet-hub, anet-hub-admin). For a test host,
#                                          build here, copy DIR there, and run
#                                            JOINT_BIN=DIR JOINT_CANARY_ONLY=1 J=/tmp/jx scripts/joint.sh
#   mutate.sh check J                      after such a run: did section C find a canary? Exit 0 when it
#                                          did (the mutation was caught), 1 when it did not, 2 when there
#                                          is no section C report under J
#   mutate.sh canary PATCH…                build, run joint.sh (JOINT_CANARY_ONLY=1) and check, here
#
# Environment:
#   CORE_SRC, HUB_SRC   the ANetCore and ANetHub checkouts (default ../ANetCore, ../ANetHub beside this
#                       repository); ANet is this checkout
#   GOOS, GOARCH        passed to go build (cross-building for a test host)
#   MUT_SCRATCH         where the scratch copy goes (default: a new directory under ${TMPDIR:-/tmp});
#                       removed afterwards unless MUT_KEEP=1
#   J                   work directory of the canary run (default /tmp/joint-mut-<uid>); JOINT_PORT_BASE
#                       and the other joint.sh variables pass through
#
# The SI-1 patches (si1-*.patch) are caught by section C alone, which is why `canary` runs joint.sh with
# JOINT_CANARY_ONLY=1. si4-skip-step7.patch is caught by section 8/11: build it and run joint.sh in full.
set -uo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
SCRIPTS=$(cd "$HERE/.." && pwd -P)
ANET_SRC=$(cd "$SCRIPTS/.." && pwd -P)
CORE_SRC=${CORE_SRC:-$ANET_SRC/../ANetCore}
HUB_SRC=${HUB_SRC:-$ANET_SRC/../ANetHub}

die(){ printf 'mutate.sh: %s\n' "$*" >&2; exit 2; }
usage(){ sed -n '2,31p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 2; }

# repo_of PATCH — the repository a patch applies to.
repo_of(){
  local r
  r=$(awk '/^diff --git /{exit} /^Repository:/{print $2; exit}' "$1")
  case "${r:-ANet}" in ANetCore|ANet|ANetHub) printf '%s' "${r:-ANet}" ;; *) return 1 ;; esac
}
expect_of(){ local e; e=$(awk '/^diff --git /{exit} /^Expect:/{sub(/^Expect: */, ""); print; exit}' "$1"); printf '%s' "${e:-(see the patch header)}"; }
src_of(){ case $1 in ANetCore) echo "$CORE_SRC" ;; ANet) echo "$ANET_SRC" ;; ANetHub) echo "$HUB_SRC" ;; esac; }

# copy_tree SRC DST — the working tree of SRC (what `git ls-files -co --exclude-standard` lists), into DST.
copy_tree(){
  mkdir -p "$2" || return 1
  git -C "$1" ls-files -z -co --exclude-standard \
    | tar -C "$1" --null -T - --ignore-failed-read -cf - 2>/dev/null \
    | tar -C "$2" -xf -
}

cmd_list(){
  local p
  for p in "$HERE"/*.patch; do
    [ -e "$p" ] || continue
    printf '%-40s %-9s %s\n' "$(basename "$p")" "$(repo_of "$p" || echo '?')" "$(expect_of "$p")"
  done
}

# cmd_build OUT PATCH… — the scratch copy, the patches, the build.
cmd_build(){
  local out=$1; shift
  [ $# -ge 1 ] || die "build: no patch given"
  command -v go >/dev/null || die "go is not on PATH (build on a machine with go, then pass the directory as JOINT_BIN)"
  local r p repo scratch
  for r in "$CORE_SRC" "$HUB_SRC"; do [ -d "$r/.git" ] || [ -f "$r/.git" ] || die "not a checkout: $r (set CORE_SRC / HUB_SRC)"; done
  scratch=${MUT_SCRATCH:-$(mktemp -d "${TMPDIR:-/tmp}/anet-mutate.XXXXXX")} || die "no scratch directory"
  mkdir -p "$scratch" || die "cannot create $scratch"
  [ -z "$(ls -A "$scratch")" ] || die "MUT_SCRATCH=$scratch is not empty"
  [ "${MUT_KEEP:-0}" = 1 ] || trap 'rm -rf -- "'"$scratch"'"' EXIT
  for r in ANetCore ANet ANetHub; do
    copy_tree "$(src_of "$r")" "$scratch/$r" || die "cannot copy $(src_of "$r")"
    # A repository of its own, so that git apply resolves paths against the copy and nothing else.
    git -C "$scratch/$r" init -q || die "git init $scratch/$r"
  done
  for p in "$@"; do
    [ -f "$p" ] || die "no such patch: $p"
    repo=$(repo_of "$p") || die "$p names an unknown repository"
    git -C "$scratch/$repo" apply "$(cd "$(dirname "$p")" && pwd -P)/$(basename "$p")" \
      || die "$(basename "$p") does not apply to $repo (the tree moved on; regenerate the patch)"
    echo "  applied $(basename "$p") to $repo"
  done
  printf 'go %s\n\nuse (\n\t./ANetCore\n\t./ANet\n\t./ANetHub\n)\n' \
    "$(awk '/^go /{print $2; exit}' "$scratch/ANet/go.mod")" > "$scratch/go.work"
  mkdir -p "$out" || die "cannot create $out"
  out=$(cd "$out" && pwd -P)
  local b
  export GOWORK=$scratch/go.work CGO_ENABLED=0 GOFLAGS=${GOFLAGS:-}
  for b in anet:ANet:./cmd/anet anetfixture:ANet:./tools/anetfixture anetpeer:ANet:./tools/anetpeer \
           anet-official:ANet:./cmd/anet-official anet-hub:ANetHub:./cmd/anet-hub \
           anet-hub-admin:ANetHub:./cmd/anet-hub-admin; do
    IFS=: read -r name repo pkg <<<"$b"
    go build -C "$scratch/$repo" -o "$out/$name" "$pkg" || die "build $name failed"
  done
  echo "  mutated binaries in $out"
}

# cmd_check J — the section C scan reports of a finished run.
cmd_check(){
  local can=$1/run/canary n
  ls "$can"/scan-*.json >/dev/null 2>&1 || { echo "no section C report under $can (did section C run?)"; return 2; }
  n=$(python3 - "$can" <<'PY'
import glob, json, os, sys
total, where = 0, {}
for p in sorted(glob.glob(os.path.join(sys.argv[1], "scan-*.json"))):
    try:
        hits = json.load(open(p)).get("hits") or []
    except (OSError, ValueError):
        continue
    total += len(hits)
    for h in hits:
        where.setdefault(os.path.basename(p)[5:-5], set()).add(h["canary"])
for k in sorted(where):
    print("  %-14s %s" % (k, ", ".join(sorted(where[k]))), file=sys.stderr)
print(total)
PY
)
  if [ "${n:-0}" -gt 0 ]; then
    echo "caught: section C found $n canary hits (above: surface and canaries)"
    return 0
  fi
  echo "SURVIVED: section C found no canary; the mutation went unnoticed"
  return 1
}

cmd_canary(){
  [ $# -ge 1 ] || die "canary: no patch given"
  local bin tmp
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/anet-mutbin.XXXXXX") || die "no temporary directory"
  bin=$tmp/bin
  ( cmd_build "$bin" "$@" ) || { rm -rf -- "$tmp"; exit 2; }
  local j=${J:-/tmp/joint-mut-$(id -u)}
  echo "  joint.sh, section C only, J=$j"
  JOINT_BIN=$bin JOINT_CANARY_ONLY=1 J=$j bash "$SCRIPTS/joint.sh"
  echo "  joint.sh exited $? (red is expected)"
  rm -rf -- "$tmp"
  cmd_check "$j"
}

[ $# -ge 1 ] || usage
case $1 in
  list) cmd_list ;;
  build)
    shift
    [ "${1:-}" = -o ] && [ -n "${2:-}" ] || die "build: -o DIR is required"
    out=$2; shift 2
    cmd_build "$out" "$@" ;;
  check) [ -n "${2:-}" ] || die "check: J is required"; cmd_check "$2" ;;
  canary) shift; cmd_canary "$@" ;;
  -h|--help|help) usage ;;
  *) die "unknown command $1 (list, build, check, canary)" ;;
esac

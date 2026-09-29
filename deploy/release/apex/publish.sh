#!/bin/sh
# publish.sh — put anet's own files for agentnetwork.org.cn into a release
# root that the website's deployment never writes to (docs/notes/0038).
#
#   publish.sh --dist DIR [--root R] [--allowed-signers F]
#       a build-release.sh dist/: install.sh and install.sh.sig at R,
#       release.json(.sig), VERSION, checksums.txt and *.gz under R/dl/
#   publish.sh --web DIR [--root R]
#       SKILL.md and llms.txt from DIR (this directory of the repository)
#
# R defaults to /var/www/anet-release. The agentnetwork nginx site serves
# /install.sh, /install.sh.sig, /dl/, /SKILL.md, /llms.txt and /llms-full.txt
# (the same file as SKILL.md) from it — nginx-locations.conf here. The
# website is built and rsynced into /var/www/agentnetwork/public from its own
# repository; whatever that build carries under those names is no longer
# served. The hub site keeps its own copy of the download files: publish a
# dist there too with --root /data/projs/anet-hub/public.
#
# Checks before anything served changes: every *.gz matches checksums.txt;
# with --allowed-signers (the repository's internal/release/allowed_signers),
# release.json and install.sh verify against the release key. dl/ is swapped
# in one rename; each other file is replaced by a rename of its own.
set -eu
umask 022

ROOT=/var/www/anet-release
DIST=""
WEB=""
SIGNERS=""
NS=anet-release@agentnetwork.org.cn

die() { printf 'publish.sh: %s\n' "$*" >&2; exit 1; }
while [ $# -gt 0 ]; do
  case "$1" in
    --root) [ $# -ge 2 ] || die "--root needs a directory"; ROOT="$2"; shift ;;
    --dist) [ $# -ge 2 ] || die "--dist needs a directory"; DIST="$2"; shift ;;
    --web) [ $# -ge 2 ] || die "--web needs a directory"; WEB="$2"; shift ;;
    --allowed-signers) [ $# -ge 2 ] || die "--allowed-signers needs a file"; SIGNERS="$2"; shift ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
  shift
done
[ -n "$DIST$WEB" ] || die "nothing to publish: pass --dist DIR and/or --web DIR"
[ -d "$ROOT" ] || die "$ROOT does not exist (create it: install -d -m 0755 $ROOT)"

# put SRC NAME: replace ROOT/NAME with SRC by a rename in the same directory.
put() {
  install -m 0644 "$1" "$ROOT/.$2.new.$$"
  mv -f "$ROOT/.$2.new.$$" "$ROOT/$2"
  echo "  $ROOT/$2"
}

if [ -n "$DIST" ]; then
  for f in release.json release.json.sig VERSION checksums.txt install.sh install.sh.sig; do
    [ -f "$DIST/$f" ] || die "$DIST/$f is missing"
  done
  ls "$DIST"/*.gz >/dev/null 2>&1 || die "no .gz in $DIST"
  if [ -n "$SIGNERS" ]; then
    for f in release.json install.sh; do
      ssh-keygen -Y verify -f "$SIGNERS" -I "$NS" -n "$NS" -s "$DIST/$f.sig" < "$DIST/$f" >/dev/null ||
        die "$f does not verify against $SIGNERS"
    done
    echo "signatures: release.json, install.sh Good"
  else
    echo "signatures: not checked (no --allowed-signers)"
  fi

  NEW="$ROOT/.dl.new.$$"
  rm -rf "$NEW"
  install -d -m 0755 "$NEW"
  install -m 0644 "$DIST/release.json" "$DIST/release.json.sig" "$DIST/VERSION" "$DIST/checksums.txt" "$DIST"/*.gz "$NEW/"
  (cd "$NEW" && grep '\.gz$' checksums.txt | sha256sum -c --quiet -) || { rm -rf "$NEW"; die "a .gz does not match checksums.txt"; }
  for g in "$NEW"/*.gz; do
    grep -q "  $(basename "$g")\$" "$NEW/checksums.txt" || { rm -rf "$NEW"; die "$(basename "$g") is not in checksums.txt"; }
  done
  if [ -e "$ROOT/dl" ]; then
    OLD="$ROOT/.dl.old.$$"
    mv "$ROOT/dl" "$OLD"
    mv "$NEW" "$ROOT/dl"
    rm -rf "$OLD"
  else
    mv "$NEW" "$ROOT/dl"
  fi
  echo "  $ROOT/dl/ ($(cat "$ROOT/dl/VERSION"), $(ls "$ROOT"/dl/*.gz | wc -l) archives)"
  put "$DIST/install.sh.sig" install.sh.sig
  put "$DIST/install.sh" install.sh
fi

if [ -n "$WEB" ]; then
  for f in SKILL.md llms.txt; do
    [ -f "$WEB/$f" ] || die "$WEB/$f is missing"
  done
  put "$WEB/SKILL.md" SKILL.md
  put "$WEB/llms.txt" llms.txt
fi

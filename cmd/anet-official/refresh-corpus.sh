#!/usr/bin/env bash
# Copy anet's public documentation into the corpus that anet-official
# compiles in (go:embed) and serves through docs.search and docs.get.
#
#   ./refresh-corpus.sh          replace corpus/ with the current documents
#   ./refresh-corpus.sh --check  exit 1 if corpus/ differs from the documents
#
# The corpus is a copy, not a link, on purpose: go:embed cannot reach
# outside the package directory, and a copy committed next to the code is
# what makes a build reproducible — the corpus CID a binary reports is the
# CID of files anyone can check out. Run this and commit the result when the
# documents change; a release build runs --check.
#
# Only documents meant for the public belong here. docs/notes/ (survey
# reports with production details) and working lists are deliberately not
# in SOURCES.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
DEST="$HERE/corpus"

SOURCES=(
  README.md
  SECURITY.md
  docs/GUIDE-zh.md
  docs/ARCHITECTURE-zh.md
  docs/A2A-DESIGN-zh.md
  docs/CAPABILITIES-zh.md
  docs/CONTRACTS-zh.md
  docs/PAYMENT-zh.md
  docs/INSTALL-DEBIAN-zh.md
  docs/DISTRIBUTIONS-zh.md
  docs/SHELL-zh.md
  docs/AUTO-REPLY-zh.md
  deploy/official/README.md
)

check=0
case "${1:-}" in
  --check) check=1 ;;
  "") ;;
  *) echo "usage: refresh-corpus.sh [--check]" >&2; exit 2 ;;
esac

if [ "$check" -eq 1 ]; then
  stale=0
  for s in "${SOURCES[@]}"; do
    if ! cmp -s "$ROOT/$s" "$DEST/$s"; then
      echo "stale: $s" >&2
      stale=1
    fi
  done
  # Anything in corpus/ that is not a listed source is stale too.
  while IFS= read -r f; do
    rel="${f#"$DEST"/}"
    found=0
    for s in "${SOURCES[@]}"; do [ "$s" = "$rel" ] && found=1; done
    [ "$found" -eq 1 ] || { echo "not a source: $rel" >&2; stale=1; }
  done < <(find "$DEST" -type f -name '*.md' | sort)
  [ "$stale" -eq 0 ] && echo "corpus up to date (${#SOURCES[@]} documents)"
  exit "$stale"
fi

find "$DEST" -type f -name '*.md' -delete
for s in "${SOURCES[@]}"; do
  mkdir -p "$DEST/$(dirname "$s")"
  cp "$ROOT/$s" "$DEST/$s"
done
find "$DEST" -type d -empty -delete 2>/dev/null || true
mkdir -p "$DEST"
echo "corpus refreshed: ${#SOURCES[@]} documents"

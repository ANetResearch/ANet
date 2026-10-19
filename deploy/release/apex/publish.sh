#!/bin/sh
# publish.sh — put anet's own files for agentnetwork.org.cn into a release
# root that the website's deployment never writes to (README.md here).
#
#   publish.sh --dist DIR [--root R] [--allowed-signers F]
#       a build-release.sh dist/: install.sh and install.sh.sig at R,
#       release.json(.sig), VERSION, checksums.txt and *.gz under R/dl/.
#       A root that already serves site.json takes it only with --web in
#       the same run, which brings site.json's "version" along.
#   publish.sh --web DIR [--root R]
#       SKILL.md, llms.txt and site.json from DIR (this directory of the
#       repository). The served site.json gets "version" here: the release
#       R/dl/ serves, or the one --dist publishes in the same run, so the
#       website never names a release that cannot be downloaded.
#   publish.sh --zcode DIR [--root R]
#       the ZCode marketplace that deploy/zcode/build.py wrote to DIR:
#       R/zcode/marketplace.json, R/zcode/assets/ and every plugin.zip under
#       R/zcode/plugins/. An archive is never replaced: a version that is
#       already served with other bytes stops the run, and archives of
#       earlier versions stay. marketplace.json goes in last.
#
# R defaults to /var/www/anet-release. The agentnetwork nginx site serves
# /install.sh, /install.sh.sig, /dl/, /SKILL.md, /llms.txt, /llms-full.txt
# (the same file as SKILL.md) and /site.json from it — nginx-locations.conf
# here. The website is built and deployed from its own repository; whatever
# that build carries under those names is not served. The hub site keeps its
# own copy of the download files: publish a dist there too, with --root set
# to the hub site's public directory.
#
# Every check runs before anything served changes: the dist and web files
# are all there; with --allowed-signers (the repository's
# internal/release/allowed_signers), release.json and install.sh verify
# against the release key; site.json can take the version; every *.gz
# matches checksums.txt. dl/ is swapped in one rename; each other file is
# replaced by a rename of its own.
set -eu
umask 022

ROOT=/var/www/anet-release
DIST=""
WEB=""
ZCODE=""
SIGNERS=""
NS=anet-release@agentnetwork.org.cn

die() { printf 'publish.sh: %s\n' "$*" >&2; exit 1; }
while [ $# -gt 0 ]; do
  case "$1" in
    --root) [ $# -ge 2 ] || die "--root needs a directory"; ROOT="$2"; shift ;;
    --dist) [ $# -ge 2 ] || die "--dist needs a directory"; DIST="$2"; shift ;;
    --web) [ $# -ge 2 ] || die "--web needs a directory"; WEB="$2"; shift ;;
    --zcode) [ $# -ge 2 ] || die "--zcode needs a directory"; ZCODE="$2"; shift ;;
    --allowed-signers) [ $# -ge 2 ] || die "--allowed-signers needs a file"; SIGNERS="$2"; shift ;;
    -h|--help) awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
  shift
done
[ -n "$DIST$WEB$ZCODE" ] || die "nothing to publish: pass --dist DIR, --web DIR and/or --zcode DIR"
[ -d "$ROOT" ] || die "$ROOT does not exist (create it: install -d -m 0755 $ROOT)"

# put SRC NAME: replace ROOT/NAME with SRC by a rename in the same directory.
put() {
  install -m 0644 "$1" "$ROOT/.$2.new.$$"
  mv -f "$ROOT/.$2.new.$$" "$ROOT/$2"
  echo "  $ROOT/$2"
}

# --- checks: nothing served changes before all of them pass ----------------

if [ -n "$DIST" ]; then
  # A new dl/ alone would leave site.json naming a release dl/ no longer
  # serves (the old one is deleted when the new one goes in).
  if [ -z "$WEB" ] && [ -e "$ROOT/site.json" ]; then
    die "$ROOT/site.json names the release dl/ serves now: publish the text in the same run (add --web deploy/release/apex) so it names this one"
  fi
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
fi

if [ -n "$WEB" ]; then
  for f in SKILL.md llms.txt site.json; do
    [ -f "$WEB/$f" ] || die "$WEB/$f is missing"
  done
  # site.json names the release the website offers: the one dl/ serves once
  # this run is done.
  if [ -n "$DIST" ]; then VFILE="$DIST/VERSION"; else VFILE="$ROOT/dl/VERSION"; fi
  [ -f "$VFILE" ] || die "$VFILE is missing: site.json names the release dl/ serves, so publish one first (add --dist DIR)"
  VER=$(cat "$VFILE")
  case "$VER" in
    ''|[!0-9]*|*[!0-9A-Za-z.+-]*)
      die "$VFILE does not hold a release version: publish the VERSION build-release.sh wrote (--dist DIR)" ;;
  esac
  # The source is the served file without "version"; its first line is
  # exactly {"v": N, and "version" goes in right after it.
  head -n 1 "$WEB/site.json" | grep -Eqx '[{]"v": [0-9]+,' ||
    die "$WEB/site.json must begin with the line {\"v\": N, (publish.sh adds \"version\" after it): restore it from the repository"
  if grep -q '"version"[[:space:]]*:' "$WEB/site.json"; then
    die "$WEB/site.json must not carry \"version\": publish.sh takes it from the release under dl/; delete the key"
  fi
  GEN=$(mktemp "${TMPDIR:-/tmp}/anet-site.json.XXXXXX") || die "cannot make a temporary file: set TMPDIR to a writable directory"
  trap 'rm -f "$GEN"' EXIT
  trap 'exit 1' HUP INT TERM
  sed '1s/^{"v": \([0-9][0-9]*\),$/{"v": \1, "version": "'"$VER"'",/' "$WEB/site.json" > "$GEN" ||
    die "cannot write the site.json to publish ($GEN): check the space in ${TMPDIR:-/tmp}"
fi

if [ -n "$ZCODE" ]; then
  [ -f "$ZCODE/marketplace.json" ] || die "$ZCODE/marketplace.json is missing: run deploy/zcode/build.py --out $ZCODE"
  command -v python3 >/dev/null 2>&1 || die "python3 is needed to check $ZCODE/marketplace.json"
  # Every archive the marketplace names is in DIR with the SHA-256 it states,
  # and none replaces an archive already served with other bytes.
  ZFILES=$(python3 - "$ZCODE" "$ROOT/zcode" <<'PY'
import hashlib, json, os, sys
src, served = sys.argv[1], sys.argv[2]
m = json.load(open(os.path.join(src, "marketplace.json"), encoding="utf-8"))
for p in m["plugins"]:
    s = p["source"]
    if s.get("source") != "url" or s.get("type") != "zip" or not s["url"].startswith("https://") or "/zcode/" not in s["url"]:
        sys.exit("plugin %s: source must be an https:// zip under /zcode/" % p["name"])
    rel = s["url"].split("/zcode/", 1)[1]
    if rel.startswith("/") or ".." in rel.split("/"):
        sys.exit("plugin %s: archive path %s leaves /zcode/" % (p["name"], rel))
    data = open(os.path.join(src, rel), "rb").read()
    if hashlib.sha256(data).hexdigest() != s["sha256"]:
        sys.exit("plugin %s: %s does not match the SHA-256 in marketplace.json" % (p["name"], rel))
    old = os.path.join(served, rel)
    if os.path.exists(old) and open(old, "rb").read() != data:
        sys.exit("plugin %s: %s is already served with other bytes; publish a new version" % (p["name"], rel))
    print(rel)
PY
  ) || die "the ZCode marketplace in $ZCODE does not pass the checks above"
fi

# --- publish ----------------------------------------------------------------

if [ -n "$DIST" ]; then
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
  put "$WEB/SKILL.md" SKILL.md
  put "$WEB/llms.txt" llms.txt
  put "$GEN" site.json
  echo "  (site.json: version $VER)"
fi

if [ -n "$ZCODE" ]; then
  install -d -m 0755 "$ROOT/zcode"
  for rel in $ZFILES; do
    install -d -m 0755 "$ROOT/zcode/$(dirname "$rel")"
    if [ ! -e "$ROOT/zcode/$rel" ]; then
      install -m 0644 "$ZCODE/$rel" "$ROOT/zcode/$rel.new.$$"
      mv -f "$ROOT/zcode/$rel.new.$$" "$ROOT/zcode/$rel"
    fi
    echo "  $ROOT/zcode/$rel"
  done
  for icon in "$ZCODE"/assets/*/icon.png; do
    [ -f "$icon" ] || continue
    name=$(basename "$(dirname "$icon")")
    install -d -m 0755 "$ROOT/zcode/assets/$name"
    install -m 0644 "$icon" "$ROOT/zcode/assets/$name/.icon.png.new.$$"
    mv -f "$ROOT/zcode/assets/$name/.icon.png.new.$$" "$ROOT/zcode/assets/$name/icon.png"
  done
  install -m 0644 "$ZCODE/marketplace.json" "$ROOT/zcode/.marketplace.json.new.$$"
  mv -f "$ROOT/zcode/.marketplace.json.new.$$" "$ROOT/zcode/marketplace.json"
  echo "  $ROOT/zcode/marketplace.json"
fi

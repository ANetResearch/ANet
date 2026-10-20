#!/usr/bin/env bash
# deploy/release/ps1-pins.sh - write the release pins into install.ps1.
#
#   ps1-pins.sh <assets> <version> < deploy/release/install.ps1 > dist/install.ps1
#
# <assets> is the list of builds that build-release.sh collects while it
# builds (dist/.assets, one manifest line per build). The four Windows builds
# must each be listed exactly once, and no other Windows build may be:
#
#   anet-windows-amd64  anet-windows-arm64
#   anet-shell-windows-amd64  anet-shell-windows-arm64
#
# The template on stdin must hold exactly one pins block,
#
#   # BEGIN RELEASE PINS (written by deploy/release/build-release.sh; do not edit)
#   $AnetRelease = $null
#   # END RELEASE PINS
#
# and its middle line is replaced by the version and the SHA-256 and size of
# each .gz and of the binary inside it. install.ps1 checks every download
# against these values before it runs anything it downloaded; the script is
# then signed with the release key like install.sh.
#
# On any error this prints the reason on stderr, writes nothing on stdout
# and exits with status 1.
set -euo pipefail

die() { printf 'ps1-pins.sh: %s\n' "$*" >&2; exit 1; }

[ $# -eq 2 ] || die "usage: ps1-pins.sh <assets> <version> < install.ps1 > install.ps1.out"
ASSETS="$1"
VERSION="$2"
[ -f "$ASSETS" ] || die "$ASSETS does not exist"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "version '$VERSION' is not x.y.z"

BEGIN_MARK='# BEGIN RELEASE PINS (written by deploy/release/build-release.sh; do not edit)'
END_MARK='# END RELEASE PINS'
NULL_LINE='$AnetRelease = $null'

n_windows=$(grep -c '"os": "windows"' "$ASSETS" || true)
[ "$n_windows" -eq 4 ] || die "$ASSETS lists $n_windows Windows builds; the four Windows builds are required"

PINS="\$AnetRelease = @{
  Version = '$VERSION'
  Assets = @{"
for name in anet-windows-amd64 anet-windows-arm64 anet-shell-windows-amd64 anet-shell-windows-arm64; do
  lines=$(grep -c "^    \"$name\": {" "$ASSETS" || true)
  [ "$lines" -eq 1 ] || die "$ASSETS lists $name $lines times; it must be listed once"
  line=$(grep "^    \"$name\": {" "$ASSETS")
  field() { printf '%s' "$line" | sed -n "s/.*\"$1\": \"\\([^\"]*\\)\".*/\\1/p"; }
  number() { printf '%s' "$line" | sed -n "s/.*\"$1\": \\([0-9]*\\)[,}].*/\\1/p"; }
  variant=$(field variant); os=$(field os); arch=$(field arch)
  gz_sha=$(field gz_sha256); sha=$(field sha256)
  gz_size=$(number gz_size); size=$(number size)
  want_variant=default
  case "$name" in anet-shell-*) want_variant=shell ;; esac
  [ "$variant" = "$want_variant" ] || die "$name: variant '$variant', expected $want_variant"
  [ "$os" = windows ] || die "$name: os '$os', expected windows"
  [ "$arch" = "${name##*-}" ] || die "$name: arch '$arch', expected ${name##*-}"
  [[ "$gz_sha" =~ ^[0-9a-f]{64}$ ]] || die "$name: gz_sha256 '$gz_sha' is not 64 lower-case hex digits"
  [[ "$sha" =~ ^[0-9a-f]{64}$ ]] || die "$name: sha256 '$sha' is not 64 lower-case hex digits"
  [[ "$gz_size" =~ ^[1-9][0-9]{0,11}$ ]] || die "$name: gz_size '$gz_size' is not a positive size"
  [[ "$size" =~ ^[1-9][0-9]{0,11}$ ]] || die "$name: size '$size' is not a positive size"
  PINS="$PINS
    '$name' = @{ GzSha256 = '$gz_sha'; GzSize = $gz_size; Sha256 = '$sha'; Size = $size }"
done
PINS="$PINS
  }
}"

OUT=$(mktemp)
trap 'rm -f "$OUT"' EXIT
# The block must be exactly the three template lines, once; anything else
# is a template this script does not know how to fill.
if ! PINS="$PINS" awk -v begin="$BEGIN_MARK" -v end="$END_MARK" -v null="$NULL_LINE" '
  BEGIN { pins = ENVIRON["PINS"]; nb = 0; ne = 0; nn = 0; ok = 1 }
  {
    if ($0 == begin) { nb++; bl = NR }
    if ($0 == end) { ne++; el = NR }
    if ($0 == null) { nn++; nl = NR; print pins; next }
    print
  }
  END {
    if (nb != 1 || ne != 1 || nn != 1 || nl != bl + 1 || el != nl + 1) {
      printf "ps1-pins.sh: the template does not hold exactly one pins block (BEGIN %d, END %d, $AnetRelease = $null %d)\n", nb, ne, nn > "/dev/stderr"
      exit 1
    }
  }' > "$OUT"; then
  exit 1
fi
cat "$OUT"

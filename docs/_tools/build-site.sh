#!/usr/bin/env bash
# build-site.sh - build the documentation in both languages and lay the result
# out the way Read the Docs does:
#
#   <out>/index.html          redirects by browser language (default zh-cn)
#   <out>/en/latest/          English (source language)
#   <out>/zh-cn/latest/       Simplified Chinese
#
# usage: docs/_tools/build-site.sh [OUT_DIR]   (default: docs/_site)
# env:   ANET_HUB_CODES path of ANetHub/internal/wire3/codes.txt, for the code
#                       collision check (default: ../ANetHub beside the checkout)
#        SPHINX_BUILD  command used to run sphinx-build (default: sphinx-build)
#        BUILD_WRAPPER wrapper for heavy jobs, for example "heavy.sh --"
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs="$(dirname "$here")"
out="${1:-$docs/_site}"
sphinx="${SPHINX_BUILD:-sphinx-build}"
wrapper="${BUILD_WRAPPER:-}"

# The reason codes of anet and of the hub must not collide by accident. The
# check needs the hub register, which is in the ANetHub repository.
hub_codes="${ANET_HUB_CODES:-$docs/../../ANetHub/internal/wire3/codes.txt}"
if [ -f "$hub_codes" ]; then
    python3 "$here/check_code_collisions.py" --hub-codes "$hub_codes"
else
    echo "== code collisions: skipped (set ANET_HUB_CODES to the hub register)"
fi

rm -rf "$out"
mkdir -p "$out/en" "$out/zh-cn"

echo "== en"
SPHINX_LANGUAGE=en $wrapper $sphinx -W --keep-going -E -b html "$docs" "$out/en/latest"
echo "== zh_CN"
SPHINX_LANGUAGE=zh_CN $wrapper $sphinx -W --keep-going -E -b html "$docs" "$out/zh-cn/latest"

rm -rf "$out/en/latest/.doctrees" "$out/zh-cn/latest/.doctrees"

cat > "$out/index.html" <<'HTML'
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ANet documentation</title>
<link rel="canonical" href="zh-cn/latest/">
<noscript><meta http-equiv="refresh" content="0; url=zh-cn/latest/"></noscript>
<script>
(function () {
  var langs = (navigator.languages && navigator.languages.length) ? navigator.languages : [navigator.language || ""];
  var target = "zh-cn/latest/";
  for (var i = 0; i < langs.length; i++) {
    var l = String(langs[i]).toLowerCase();
    if (l.indexOf("zh") === 0) { target = "zh-cn/latest/"; break; }
    if (l.indexOf("en") === 0) { target = "en/latest/"; break; }
  }
  location.replace(target + location.hash);
})();
</script>
</head>
<body>
<p><a href="zh-cn/latest/">简体中文</a> | <a href="en/latest/">English</a></p>
</body>
</html>
HTML

echo "site written to $out"

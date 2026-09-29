#!/usr/bin/env bash
# Runs every a2a-go reproduction and prints one summary line each.
# Exit status of each program: 0 = behaviour reproduced, 1 = not reproduced
# (fixed upstream?), 2 = setup error.
set -uo pipefail
cd "$(dirname "$0")"
export GOWORK=off
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
for d in a1-default-values a2-json-presence a3-duplicate-members a4-base64-linebreaks a5-version-header \
	a6-extensions-comma a7-legacy-extensions-header a8-extensions-response-header a9-unsigned-card \
	a10-unknown-securityscheme a12-stream-errors; do
	go run "./$d" > "$out/$d.txt" 2>&1
	case $? in
	0) s="reproduced" ;;
	1) s="NOT reproduced" ;;
	*) s="SETUP ERROR" ;;
	esac
	printf '%-32s %s\n' "$d" "$s"
done
go run ./a1x-cross-sdk-vectors "$out/a1x" > /dev/null 2>&1 && echo "a1x-cross-sdk-vectors            vectors written (verify with ../a2a-python, ../a2a-js)"

#!/usr/bin/env bash
# X13: the same x402 PaymentRequirements JSON read by three common JSON
# readers gives two different amounts and payees. Go's encoding/json matches
# member names case-insensitively and keeps the last match; Python's json and
# JavaScript's JSON.parse match exactly. A client or signing service written
# in one language and a merchant or facilitator written in another can
# therefore show one amount and sign or settle another, unless the spec
# requires I-JSON (RFC 7493: no duplicate names) and rejects member names
# that differ only by case.
#
# Needs python3, node and go on PATH; no packages.
# Run: bash x13-case-variant-members.sh
# Exit status: 0 = the readers disagree (behaviour reproduced), 1 = they
# agree, 2 = setup error.
set -euo pipefail

req='{"scheme":"exact","network":"eip155:8453","asset":"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
"amount":"1000","payTo":"0x1111111111111111111111111111111111111111","maxTimeoutSeconds":60,
"AMOUNT":"900000000","PayTo":"0x2222222222222222222222222222222222222222"}'

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf '%s' "$req" > "$tmp/req.json"

py=$(python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); print(r["amount"], r["payTo"])' "$tmp/req.json")
js=$(node -e 'const r=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")); console.log(r.amount, r.payTo)' "$tmp/req.json")

mkdir "$tmp/go"
cat > "$tmp/go/main.go" <<'GO'
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// The x402 v2 PaymentRequirements fields, as a Go SDK declares them.
type PaymentRequirements struct {
	Scheme            string `json:"scheme"`
	Network           string `json:"network"`
	Asset             string `json:"asset"`
	Amount            string `json:"amount"`
	PayTo             string `json:"payTo"`
	MaxTimeoutSeconds int    `json:"maxTimeoutSeconds"`
}

func main() {
	b, _ := os.ReadFile(os.Args[1])
	var r PaymentRequirements
	if err := json.Unmarshal(b, &r); err != nil {
		fmt.Println("error", err)
		os.Exit(2)
	}
	fmt.Println(r.Amount, r.PayTo)
}
GO
printf 'module x13\n\ngo 1.22\n' > "$tmp/go/go.mod"
go_out=$(cd "$tmp/go" && GOWORK=off GOFLAGS= go run . "$tmp/req.json")

echo "input:                   $(tr -d '\n' < "$tmp/req.json")"
echo "python3 json:            amount payTo = $py"
echo "node JSON.parse:         amount payTo = $js"
echo "go encoding/json struct: amount payTo = $go_out"
echo "expected: the object is rejected (member names differing only by case), or all readers agree"
if [ "$py" = "$go_out" ] && [ "$js" = "$go_out" ]; then
	echo "reproduced: no"
	exit 1
fi
echo "reproduced: yes"

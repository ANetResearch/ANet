//go:build ignore

// dump_reasons writes the reason table of internal/a2ashape as JSON, one
// object per row, in table order. gen_reason_codes.py runs it; it is not part
// of any build (see the build constraint above).
package main

import (
	"encoding/json"
	"os"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

type row struct {
	Key       string `json:"key"`
	Code      string `json:"code"`
	Variant   string `json:"variant,omitempty"`
	Classes   string `json:"classes"`
	Level     string `json:"level,omitempty"`
	Effect    string `json:"effect,omitempty"`
	X402      string `json:"x402,omitempty"`
	NextKind  string `json:"next_kind,omitempty"`
	Next      string `json:"next,omitempty"`
	En        string `json:"en"`
	Zh        string `json:"zh"`
	NoSentenc bool   `json:"no_sentence,omitempty"`
}

var classNames = []struct {
	c    a2ashape.Class
	name string
}{
	{a2ashape.ClassR, "R"}, {a2ashape.ClassF, "F"}, {a2ashape.ClassFP, "FP"},
	{a2ashape.ClassIP, "IP"}, {a2ashape.ClassW, "W"}, {a2ashape.ClassM, "M"},
	{a2ashape.ClassE, "E"}, {a2ashape.ClassC, "C"}, {a2ashape.ClassS, "S"},
	{a2ashape.ClassI, "I"}, {a2ashape.ClassT, "T"}, {a2ashape.ClassModel, "Model"},
}

var nextNames = map[a2ashape.NextKind]string{
	a2ashape.NextCommand: "command", a2ashape.NextNoneNeeded: "none_needed",
	a2ashape.NextAuto: "auto", a2ashape.NextRetryLater: "retry_later",
	a2ashape.NextChangeInput: "change_input", a2ashape.NextContact: "contact",
	a2ashape.NextStartOver: "start_over", a2ashape.NextFutile: "futile",
}

func main() {
	var out []row
	for _, r := range a2ashape.Rows() {
		cl := ""
		for _, n := range classNames {
			if r.Class&n.c != 0 {
				if cl != "" {
					cl += ","
				}
				cl += n.name
			}
		}
		out = append(out, row{
			Key: r.Key(), Code: r.Code, Variant: r.Variant, Classes: cl,
			Level: string(r.Level), Effect: r.A2A.Effect, X402: r.A2A.X402,
			NextKind: nextNames[r.NextKind], Next: r.Next,
			En: r.En, Zh: r.Zh, NoSentenc: r.NoSentence,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(out); err != nil {
		os.Exit(1)
	}
}

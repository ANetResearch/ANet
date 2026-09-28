package jsonread

import (
	"encoding/json"
	"testing"
)

func TestReadsAlike(t *testing.T) {
	type opt struct {
		Amount  string         `json:"amount"`
		PayTo   string         `json:"payTo"`
		Timeout int            `json:"timeout,omitempty"`
		Extra   map[string]any `json:"extra,omitempty"`
	}
	for raw, want := range map[string]bool{
		`{"amount":"1","payTo":"p"}`:                       true,
		`{"amount":"1","payTo":"p","other":{"Amount":2}}`:  true, // members Go does not read
		`{"payTo":"p"}`:                                    true, // amount absent is "" to both
		`{"amount":"1","payTo":"p","amount":"1"}`:          true,
		`{"amount":"1","payTo":"p","extra":{"n":1.50}}`:    true, // same number, other spelling
		`{"amount":"1","payTo":"p","extra":{"a":1,"A":2}}`: true, // a map keeps names as written
		`{"amount":"1","AMOUNT":"900","payTo":"p"}`:        false,
		`{"AMOUNT":"900","amount":"1","payTo":"p"}`:        true, // the last match is the one both read
		`{"Amount":"900","payTo":"p"}`:                     false,
		`{"amount":"1","payto":"q","payTo":"p"}`:           true,
		`{"amount":"1","payTo":"p","PAYTO":"q"}`:           false,
		`{"amount":"1","payTo":"p","Timeout":5}`:           false,
	} {
		var v opt
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := ReadsAlike([]byte(raw), &v); got != want {
			t.Errorf("ReadsAlike(%s) = %v, want %v", raw, got, want)
		}
	}
}

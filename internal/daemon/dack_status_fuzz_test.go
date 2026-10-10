package daemon

import (
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// FuzzDecodeStatusAnswer is the engine's reading of a /relay/status answer
// (ENG-D1, D4, DLV-W-18a) on any bytes a hub may send: it never panics; an
// answer it takes says queued or unknown of every version asked about, or
// confirms the row with an outcome a token of one of the row's ack_keys
// carries; a token that does not check is never a confirmation.
func FuzzDecodeStatusAnswer(f *testing.F) {
	var k1, k2 seal.AckKey
	for i := range k1 {
		k1[i], k2[i] = byte(i), byte(255-i)
	}
	keys := []seal.AckKey{k1, k2}
	vers := []interactions.OutboxVer{{Ver: 1}, {Ver: 2}}
	tok := func(k seal.AckKey, o seal.Outcome) string {
		d, err := k.MakeDAck(o, seal.Note{})
		if err != nil {
			f.Fatal(err)
		}
		return hubapi.B64(d[:])
	}
	answer := func(items ...hubapi.RelayStatusResult) []byte {
		b, _ := json.Marshal(hubapi.RelayStatusResponse{Items: items})
		return b
	}
	q := hubapi.RelayStatusResult{State: hubapi.RelayStatusQueued}
	u := hubapi.RelayStatusResult{State: hubapi.RelayStatusUnknown}
	f.Add(answer(q, u))
	f.Add(answer(u, hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: tok(k2, seal.OutcomeProcessed)}))
	f.Add(answer(hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: tok(k1, seal.OutcomeRefused)}, q))
	f.Add(answer(hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: hubapi.B64(make([]byte, seal.DAckLen))}, q))
	f.Add(answer(q))
	f.Add([]byte(`{"items":[{"state":"done"},{"state":"queued"}]}`))
	f.Add([]byte(`{"items":[{"state":"later"},{"state":"queued"}]}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, body []byte) {
		res, invalid, _, err := decodeStatusAnswer(body, vers, keys)
		if invalid < 0 || invalid > len(vers) {
			t.Fatalf("%d invalid tokens among %d items", invalid, len(vers))
		}
		if err != nil {
			if res.confirmed || len(res.states) != 0 {
				t.Fatalf("a failed answer concluded something: %+v", res)
			}
			return
		}
		if res.confirmed {
			if res.outcome != seal.OutcomeProcessed && res.outcome != seal.OutcomeRefused {
				t.Fatalf("confirmed with outcome %d", res.outcome)
			}
			var out hubapi.RelayStatusResponse
			if json.Unmarshal(body, &out) != nil {
				t.Fatal("confirmed from an answer that is no JSON")
			}
			checked := false
			for _, it := range out.Items {
				if it.State != hubapi.RelayStatusDone {
					continue
				}
				if d, err := hubapi.DecodeB64(it.DAck, -1); err == nil {
					if _, o, _, _, ok := seal.VerifyDAck(keys, d); ok && o == res.outcome {
						checked = true
					}
				}
			}
			if !checked {
				t.Fatal("confirmed without a token that checks")
			}
			return
		}
		if len(res.states) != len(vers) {
			t.Fatalf("states %v for %d versions", res.states, len(vers))
		}
		for v, st := range res.states {
			if st != interactions.OutboxQueued && st != interactions.OutboxUnknown {
				t.Fatalf("version %d: state %q", v, st)
			}
		}
	})
}

// FuzzParseRetryAfterHeader is the engine's reading of a hub's Retry-After
// (§2.5): on any value it never panics, and a wait it takes lies between
// the floor and a day — a hub cannot make a row wait longer, nor spin it.
func FuzzParseRetryAfterHeader(f *testing.F) {
	for _, s := range []string{"0", "1", "30", "3600", "86400", "999999999999", "-5", " 7 ", "Wed, 21 Oct 2015 07:28:00 GMT", "", "soon"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		wait, ok := parseRetryAfterHeader(v)
		if !ok {
			if wait != 0 {
				t.Fatalf("%q: refused with a wait of %s", v, wait)
			}
			return
		}
		if wait < retryAfterMin || wait > retryMaxDelay {
			t.Fatalf("%q: a wait of %s", v, wait)
		}
	})
}

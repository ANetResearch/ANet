//go:build !no_x402

package x402

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/liabtree"
	"github.com/ANetResearch/ANetCore/relayauth"
)

// fakeHub is the hub as a node audits it: a supply-epoch chain signed with
// a real key, liability proofs from real trees, the account reads answered
// only to a request the account holder signed (relayauth v2, with the
// replay rule of RC1), and an optional ts_stale floor.
//
// It signs everything for real, because the thing under test is whether the
// node verifies: a fake that served unchecked records would let a verifier
// pass by doing nothing.
type fakeHub struct {
	t    *testing.T
	ctrl *identity.Controller
	node *identity.Controller

	mu     sync.Mutex
	epochs []*fakeEpoch
	// proofOf, when set, replaces the answer to a liability-proof read.
	proofOf func(e *fakeEpoch, aid string) (int, any)
	// unavailable lists the days the hub answers proof_unavailable for.
	unavailable map[uint64]bool

	entries     []ledgerEntry
	page        int
	balance     int64
	redemptions []map[string]any

	// staleFloor, when above 0, makes the hub answer ts_stale (with that
	// floor) to a signed time at or below it.
	staleFloor int64
	seen       map[uint64]bool
	replays    int
	unsigned   int
	stale      int
	proofReads int
}

type fakeEpoch struct {
	rec  *ael.EventRecord
	raw  []byte
	ep   ael.SupplyEpoch
	tree *liabtree.Tree
	hw   uint64
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	hub, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	node, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	return &fakeHub{t: t, ctrl: hub, node: node, seen: map[uint64]bool{}, unavailable: map[uint64]bool{}}
}

// today is the current UTC day number.
func today() uint64 { return uint64(time.Now().UnixMilli() / dayMillis) }

// addEpoch publishes an epoch for toDay in which the node holds balance and
// the ledger's high-water mark is hw. Epochs are one day each.
func (f *fakeHub) addEpoch(toDay, balance, hw uint64, others ...uint64) *fakeEpoch {
	f.t.Helper()
	return f.addEpochAt(toDay, balance, hw, time.Now().UnixMilli(), others...)
}

func (f *fakeHub) addEpochAt(toDay, balance, hw uint64, publishedAt int64, others ...uint64) *fakeEpoch {
	f.t.Helper()
	leaves := []liabtree.Leaf{{AID: f.node.AID(), Balance: balance}}
	for i, o := range others {
		leaves = append(leaves, liabtree.Leaf{AID: "bafy-other-" + strconv.Itoa(i), Balance: o})
	}
	tree, err := liabtree.Build(leaves, toDay, rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	e := &fakeEpoch{tree: tree, hw: hw, ep: ael.SupplyEpoch{FromDay: toDay, ToDay: toDay, Root: tree.Root(),
		Outstanding: tree.Outstanding(), Depth: tree.Depth()}}
	f.sign(e, publishedAt)
	f.epochs = append(f.epochs, e)
	return e
}

// sign makes (or remakes) the epoch's record at the next place of the chain.
func (f *fakeHub) sign(e *fakeEpoch, publishedAt int64) {
	f.t.Helper()
	rec, err := ael.SupplyEpochRecord(f.ctrl.AID(), e.ep)
	if err != nil {
		f.t.Fatal(err)
	}
	rec.Seq, rec.PrevID, rec.Timestamp = 0, ael.GenesisPrev(), publishedAt
	rec.CriticalExtensions = []string{}
	if n := len(f.epochs); n > 0 {
		last := f.epochs[n-1].rec
		rec.Seq, rec.PrevID = last.Seq+1, last.ID
	}
	if err := rec.Sign(f.ctrl); err != nil {
		f.t.Fatal(err)
	}
	raw, err := marshalRecord(rec)
	if err != nil {
		f.t.Fatal(err)
	}
	e.rec, e.raw = rec, raw
}

func (f *fakeHub) entry(e *fakeEpoch) map[string]any {
	return map[string]any{"seq": e.rec.Seq, "id": e.rec.ID, "prev_id": e.rec.PrevID,
		"from_day": e.ep.FromDay, "to_day": e.ep.ToDay, "root": hexOf(e.ep.Root), "outstanding": e.ep.Outstanding,
		"record": base64.RawURLEncoding.EncodeToString(e.raw)}
}

// holder checks a signed account read the way the hub does: relayauth v2
// by the AID in the path, over this hub's AID, the method and the request
// target; a time at or below the floor is ts_stale, a time seen twice is
// replayed.
func (f *fakeHub) holder(w http.ResponseWriter, r *http.Request, action string) bool {
	ts, _ := strconv.ParseUint(r.Header.Get(relayauth.HeaderTS), 10, 64)
	seq, _ := strconv.ParseUint(r.Header.Get(relayauth.HeaderSeq), 10, 64)
	sig, err := relayauth.DecodeSig(r.Header.Get(relayauth.HeaderSig))
	aid := r.PathValue("aid")
	ok := err == nil && f.node != nil && aid == f.node.AID() && r.Header.Get(relayauth.HeaderAID) == aid &&
		identity.VerifyObject(f.node.KEL(), aid, seq, ts,
			relayauth.PreimageV2(action, aid, f.ctrl.AID(), ts, r.Method, r.URL.RequestURI(), nil), sig) == nil
	f.mu.Lock()
	defer f.mu.Unlock()
	if !ok {
		f.unsigned++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"reason":"unauthorized","error":"auth"}`))
		return false
	}
	if f.staleFloor > 0 && int64(ts) <= f.staleFloor {
		f.stale++
		w.Header().Set("X-ANet-TS-Floor", strconv.FormatInt(f.staleFloor, 10))
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"reason": "ts_stale", "error": "stale", "floor": f.staleFloor})
		return false
	}
	if f.seen[ts] {
		f.replays++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"reason":"replayed","error":"replayed"}`))
		return false
	}
	f.seen[ts] = true
	return true
}

func (f *fakeHub) serve() *httptest.Server {
	f.t.Helper()
	mux := http.NewServeMux()
	kelBytes, err := identity.MarshalKEL(f.ctrl.KEL())
	if err != nil {
		f.t.Fatal(err)
	}
	mux.HandleFunc("GET /hub/identity", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"aid": f.ctrl.AID()})
	})
	mux.HandleFunc("GET /agents/{aid}/kel", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"kel": base64.StdEncoding.EncodeToString(kelBytes)})
	})
	mux.HandleFunc("GET /x402/issuance", func(w http.ResponseWriter, r *http.Request) {
		from, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
		out := []map[string]any{}
		f.mu.Lock()
		for _, e := range f.epochs {
			if e.rec.Seq >= from {
				out = append(out, f.entry(e))
			}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": out})
	})
	mux.HandleFunc("GET /x402/issuance/head", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if n := len(f.epochs); n > 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"seq": f.epochs[n-1].rec.Seq, "id": f.epochs[n-1].rec.ID})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /agents/{aid}/liability-proof", func(w http.ResponseWriter, r *http.Request) {
		if !f.holder(w, r, relayauth.ActionLiabProof) {
			return
		}
		day, _ := strconv.ParseUint(r.URL.Query().Get("to_day"), 10, 64)
		f.mu.Lock()
		f.proofReads++
		var found *fakeEpoch
		for _, e := range f.epochs {
			if e.ep.ToDay == day {
				found = e
			}
		}
		un := f.unavailable[day]
		f.mu.Unlock()
		if found == nil || un {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"reason":"proof_unavailable","error":"no proof"}`))
			return
		}
		status, body := http.StatusOK, any(nil)
		if f.proofOf != nil {
			status, body = f.proofOf(found, r.PathValue("aid"))
		}
		if body == nil {
			p, ok := found.tree.Proof(r.PathValue("aid"))
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"reason":"proof_unavailable","error":"no proof"}`))
				return
			}
			body = proofBody(p, found)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("GET /agents/{aid}/balance", func(w http.ResponseWriter, r *http.Request) {
		if !f.holder(w, r, relayauth.ActionBalance) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"credits": f.balance})
	})
	mux.HandleFunc("GET /agents/{aid}/redemptions", func(w http.ResponseWriter, r *http.Request) {
		if !f.holder(w, r, relayauth.ActionRedemptions) {
			return
		}
		page := f.redemptions
		if page == nil {
			page = []map[string]any{}
		}
		var sum uint64
		for _, x := range f.redemptions {
			n, _ := asInt64(x["amount"])
			sum += uint64(n)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"redemptions": page, "total": len(f.redemptions), "sum": sum,
			"returned": len(page), "window_days": 14})
	})
	mux.HandleFunc("GET /agents/{aid}/ledger", func(w http.ResponseWriter, r *http.Request) {
		if !f.holder(w, r, relayauth.ActionLedger) {
			return
		}
		page := f.entries
		if f.page > 0 && len(page) > f.page {
			page = page[len(page)-f.page:] // the newest entries
		}
		var sum int64
		var oldest uint64
		for i, e := range f.entries {
			sum += e.Delta
			if i == 0 {
				oldest = e.Seq
			}
		}
		if page == nil {
			page = []ledgerEntry{}
		}
		out := map[string]any{"entries": page, "total": len(f.entries), "sum": sum, "returned": len(page),
			"window_days": 14, "oldest_seq": oldest}
		if len(page) < len(f.entries) {
			out["truncated"] = true
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	f.t.Cleanup(srv.Close)
	return srv
}

// proofBody is the hub's liability-proof answer.
func proofBody(p liabtree.Proof, e *fakeEpoch) any {
	return struct {
		liabtree.WireProof
		FromDay  uint64 `json:"from_day"`
		LedgerHW uint64 `json:"ledger_hw"`
		Record   string `json:"record"`
	}{p.Wire(), e.ep.FromDay, e.hw, base64.RawURLEncoding.EncodeToString(e.raw)}
}

// hubbed wires a module to the fake hub.
func (f *fakeHub) hubbed() (*Module, *testHost) {
	f.t.Helper()
	srv := f.serve()
	h := newHost(f.t)
	h.hub = f.ctrl
	h.url = srv.URL
	h.self = f.node
	return newModule(f.t, h), h
}

// stamp is a ledger time for ms (the hub's layout).
func stamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

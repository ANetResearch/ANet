package daemon

// Fuzz targets for the receive pipeline (docs/notes/0033): whatever a peer
// seals, and whatever bytes arrive where an envelope should be. Under plain
// `go test` they run their seeds only.
//
// The daemons are built once per fuzz worker (registeredPair on the
// *testing.F) and each input gets interactions of its own, so one input's
// task does not decide how the next is judged.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// rxFuzz is one worker's network: a requester and a provider that accepts
// it, a third registered daemon, and a stranger.
type rxFuzz struct {
	srv             *httptest.Server
	req, prov, oth  *Daemon
	stranger        sender
	n               int
	reqKEL, reqKeys []byte
}

func newRxFuzz(f *testing.F) *rxFuzz {
	quietLog(f) // a log line per refused envelope, millions of times, is most of what a worker would do
	srv, req, prov := registeredPair(f)
	oth := newTestDaemon(f, srv.URL, false)
	if err := oth.RegisterWithHub(context.Background(), srv.URL, "Oth", nil, ""); err != nil {
		f.Fatal(err)
	}
	kel, err := identity.MarshalKEL(req.self.KEL())
	if err != nil {
		f.Fatal(err)
	}
	return &rxFuzz{srv: srv, req: req, prov: prov, oth: oth, stranger: newStranger(f), reqKEL: kel,
		reqKeys: req.enc.SignedSet()}
}

// Routes of FuzzReceiveInner.
const (
	routeToProvider  = iota // a message for a task the provider holds from req
	routeToRequester        // a message, status or result for a task req started with prov
	routeDelegate           // a delegation from req to prov
	routeUnknownIX          // any type, for an interaction nobody holds
	routeCount
)

const fuzzRequestCID = "bafyreifuzzrequestcid"

// fuzzAttachment builds the one file a message or delegation carries.
func fuzzAttachment(mode uint8, name, mime string, data []byte) []delegation.Attachment {
	if mode%4 == 0 {
		return nil
	}
	a := delegation.Attachment{Name: name, Mime: mime, Size: int64(len(data)), Data: data}
	switch mode % 4 {
	case 1:
		a.CID, _ = anetcid.SumRaw(data)
	case 2:
		a.CID = "bafkreibogus"
	case 3:
		a.CID, _ = anetcid.SumRaw(data)
		a.Data = nil
	}
	return []delegation.Attachment{a}
}

// signedReceipt is prov's receipt over deliverable for ix, as a result
// carries it.
func signedReceipt(t *testing.T, x *rxFuzz, ix string, deliverable []byte) []byte {
	cid, err := anetcid.Sum(deliverable)
	if err != nil {
		t.Fatal(err)
	}
	rc := &evidence.Receipt{InteractionID: ix, RequesterAID: x.req.AID(), ProviderAID: x.prov.AID(),
		RequestCID: fuzzRequestCID, ResultCID: cid, CompletedAt: uint64(time.Now().UnixMilli())}
	if err := rc.Sign(x.prov.self); err != nil {
		t.Fatal(err)
	}
	b, err := rc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fuzzTaskDoc is req's signed TaskDoc: a text task, or a capability call
// with args.
func fuzzTaskDoc(t *testing.T, c *identity.Controller, goal, capID, args string) ([]byte, *tsir.TaskDoc) {
	task := tsir.Task{Intent: tsir.Intent{Summary: goal, Body: goal}}
	if capID != "" {
		task.Requires = []tsir.Require{{ID: capID, Type: RequireTypeCapability, Necessity: "must"}}
		task.Contexts = []tsir.Context{{Key: "args", Value: args, Format: "json"}}
	}
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{task}}
	if err := td.Sign(c); err != nil {
		return nil, nil
	}
	doc, err := coredet.Marshal(td)
	if err != nil {
		return nil, nil
	}
	return doc, td
}

type snapshot struct {
	state    interactions.State
	stateSeq int64
	msgs     int
	receipt  int
	pay      string
}

func snap(t *testing.T, d *Daemon, id string) (snapshot, bool) {
	ix, err := d.ix.Get(id)
	if err != nil {
		return snapshot{}, false
	}
	msgs, err := d.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot{state: ix.State, stateSeq: ix.StateSeq, msgs: len(msgs), receipt: len(ix.Receipt), pay: ix.PayState}, true
}

// checkView projects id on d the way every door does and holds it to what a
// client may rely on: it encodes, a2a-go reads it, SI-6 holds, and a
// receipt said verified verifies.
func checkView(t *testing.T, d *Daemon, id string, provKEL []identity.SignedEvent) {
	ix, err := d.ix.Get(id)
	if err != nil {
		return
	}
	task, err := d.taskView(ix, viewOpts{artifacts: true, inline: true})
	if err != nil {
		t.Fatalf("taskView(%s): %v", id, err)
	}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("the projection of %s does not encode: %v", id, err)
	}
	var sdk a2a.Task
	if err := json.Unmarshal(b, &sdk); err != nil {
		t.Fatalf("a2a-go does not read the projection of %s: %v\n%s", id, err, b)
	}
	_, isCap := sdk.Metadata[a2ashape.KeySkill]
	if _, hasES := sdk.Metadata[a2ashape.KeyEffectStatus]; isCap && sdk.Status.State.Terminal() && !hasES {
		t.Fatalf("SI-6: terminal capability task without %s\n%s", a2ashape.KeyEffectStatus, b)
	}
	if sdk.Status.State == a2a.TaskStateCompleted && sdk.Metadata[a2ashape.KeyReceiptVerified] == nil {
		t.Fatalf("SI-6: completed task without %s\n%s", a2ashape.KeyReceiptVerified, b)
	}
	if ix.Role == interactions.RoleOutbound && ix.ReceiptVerified == interactions.VerificationVerified {
		rc, err := evidence.UnmarshalReceipt(ix.Receipt)
		if err != nil {
			t.Fatalf("%s: a receipt stored as verified does not decode: %v", id, err)
		}
		cid, _ := anetcid.Sum(ix.Result)
		if rc.Verify(provKEL, uint64(time.Now().UnixMilli())) != nil || rc.ResultCID != cid ||
			rc.InteractionID != id || rc.ProviderAID != ix.PeerAID {
			t.Fatalf("%s: a receipt stored as verified does not verify over the stored result", id)
		}
	}
	if _, err := json.Marshal(a2ashape.TaskForStream(task)); err != nil {
		t.Fatalf("the stream form of %s does not encode: %v", id, err)
	}
}

// FuzzReceiveInner: messages sealed and signed by a real sender, with every
// field of the inner message and its body chosen by the fuzzer — the
// metadata JSON, the files, the TaskDoc, the attached KEL and key set, the
// clock. Whatever arrives:
//
//   - the pipeline answers with one of its three classes and does not panic;
//   - an envelope from anyone but the task's peer changes nothing stored;
//   - the requester of a task cannot end it as completed, failed or
//     rejected on the provider's side;
//   - the same envelope a second time changes nothing;
//   - the task projects to an A2A Task a client can read, SI-6 holds, and a
//     receipt stored as verified verifies over the stored result.
func FuzzReceiveInner(f *testing.F) {
	type seed struct {
		route, from, typ     uint8
		verb, text, msgID    string
		meta                 []byte
		attMode              uint8
		attName, attMime     string
		attData, deliverable []byte
		receiptOK            bool
		capID, args          string
		kel, keys            []byte
		skewMS               int64
	}
	seeds := []seed{
		{route: routeToProvider, verb: delegation.ChatText, text: "more input", msgID: "c1", meta: []byte(`{"a2a.messageId":"x"}`)},
		{route: routeToProvider, verb: delegation.ChatText, text: "file", attMode: 1, attName: "../a.txt", attMime: "text/plain", attData: []byte("hello")},
		{route: routeToProvider, verb: delegation.ChatText, meta: []byte(`{"x402.payment.status":"payment-submitted","x402.payment.payload":{"x402Version":1}}`)},
		{route: routeToProvider, verb: delegation.KindCancel},
		{route: routeToProvider, verb: delegation.ChatEndRequest},
		{route: routeToRequester, typ: 0, verb: delegation.ChatText, text: "reply", meta: []byte(`{"anet.state":"working","anet.final":true}`)},
		{route: routeToRequester, typ: 1, verb: delegation.StateInputRequired, text: "pay", capID: "text.echo",
			meta: []byte(`{"x402.payment.status":"payment-required","x402.payment.required":{"x402Version":1,"accepts":[{"scheme":"exact","network":"anet","maxAmountRequired":"100","payTo":"x","asset":"credit","resource":"r","description":"d","mimeType":"application/json","maxTimeoutSeconds":60}]}}`)},
		{route: routeToRequester, typ: 1, verb: delegation.StateFailed, meta: []byte(`{"anet.effect_status":"OK","anet.receipt_verified":"verified"}`)},
		{route: routeToRequester, typ: 2, verb: delegation.StatusDone, deliverable: []byte(`{"status":"OK","output":{"n":1}}`), receiptOK: true, capID: "text.echo",
			meta: []byte(`{"anet.state":"completed","anet.effect_status":"OK"}`)},
		{route: routeToRequester, typ: 2, verb: delegation.StatusDone, deliverable: []byte("transcript"), receiptOK: true,
			meta: []byte(`{"x402.payment.receipts":[{"success":true,"transaction":"t","network":"anet"}],"x402.payment.status":"payment-completed"}`)},
		{route: routeToRequester, typ: 2, verb: delegation.StatusFailed, deliverable: []byte("it broke")},
		{route: routeDelegate, text: "a task", attMode: 1, attName: "spec.md", attMime: "text/markdown", attData: []byte("# spec")},
		{route: routeDelegate, text: "invoke", capID: "text.echo", args: `{"on":true}`},
		{route: routeUnknownIX, typ: 0, verb: delegation.ChatText, text: "early"},
		{route: routeToProvider, from: 1, verb: delegation.ChatText, text: "stranger"},
		{route: routeToRequester, from: 2, typ: 2, verb: delegation.StatusDone, deliverable: []byte("x"), receiptOK: true},
		{route: routeToProvider, verb: delegation.ChatText, text: "k", kel: []byte{0xa0}, keys: []byte("not a key set")},
		{route: routeToProvider, verb: delegation.ChatText, text: "old", skewMS: -int64(20 * 24 * time.Hour / time.Millisecond)},
	}
	x := newRxFuzz(f)
	// The requester's own KEL and key set attached as they are: mutations
	// of them reach the KEL merge (step 6) and the key set's high-water
	// rule (step 8) with near-valid bytes.
	seeds = append(seeds,
		seed{route: routeToProvider, verb: delegation.ChatText, text: "k", kel: x.reqKEL, keys: x.reqKeys},
		seed{route: routeDelegate, text: "a task", kel: x.reqKEL, keys: x.reqKeys})
	for _, s := range seeds {
		f.Add(s.route, s.from, s.typ, s.verb, s.text, s.msgID, s.meta, s.attMode, s.attName, s.attMime, s.attData,
			s.deliverable, s.receiptOK, s.capID, s.args, s.kel, s.keys, s.skewMS)
	}
	provKEL := x.prov.self.KEL()
	f.Fuzz(func(t *testing.T, route, from, typ uint8, verb, text, msgID string, meta []byte, attMode uint8,
		attName, attMime string, attData, deliverable []byte, receiptOK bool, capID, args string,
		kel, keys []byte, skewMS int64) {
		x.n++
		if x.n%200 == 0 {
			clearMailbox(t, x.srv, x.req.AID())
			clearMailbox(t, x.srv, x.prov.AID())
			clearMailbox(t, x.srv, x.oth.AID())
		}
		route %= routeCount
		atts := fuzzAttachment(attMode, attName, attMime, attData)
		var to *Daemon
		var s sender
		var sealType, id string
		var body []byte
		var err error
		switch route {
		case routeToProvider, routeDelegate:
			to, s = x.prov, senderOf(x.req)
		default:
			to, s = x.req, senderOf(x.prov)
		}
		legit := from%3 == 0
		switch from % 3 {
		case 1:
			s = x.stranger
		case 2:
			s = senderOf(x.oth)
		}
		switch route {
		case routeToProvider:
			id = fmt.Sprintf("ix_fzp_%d", x.n)
			if err := x.prov.ix.Create(interactions.New{ID: id, Role: interactions.RoleInbound, PeerAID: x.req.AID(),
				Goal: "a task", RequestCID: fuzzRequestCID, Trust: interactions.TrustPeer,
				IsCapability: capID != ""}); err != nil {
				t.Fatal(err)
			}
			sealType = seal.TypeMessage
			body, err = (&delegation.ChatMsg{Kind: verb, Body: text, Attachments: atts, MsgID: msgID, Metadata: meta}).Marshal()
		case routeToRequester, routeUnknownIX:
			id = fmt.Sprintf("ix_fzr_%d", x.n)
			if route == routeToRequester {
				goal := "a task"
				if capID != "" {
					goal = "invoke capability " + capID
				}
				if err := x.req.ix.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: x.prov.AID(),
					Goal: goal, RequestCID: fuzzRequestCID, IsCapability: capID != ""}); err != nil {
					t.Fatal(err)
				}
			}
			switch typ % 3 {
			case 0:
				sealType = seal.TypeMessage
				body, err = (&delegation.ChatMsg{Kind: verb, Body: text, Attachments: atts, MsgID: msgID, Metadata: meta}).Marshal()
			case 1:
				sealType = seal.TypeStatus
				body, err = (&delegation.StatusMsg{State: verb, Text: text, Metadata: meta, At: uint64(time.Now().UnixMilli())}).Marshal()
			case 2:
				sealType = seal.TypeResult
				rr := &delegation.ResultResp{Status: verb, Deliverable: deliverable, Metadata: meta}
				if receiptOK {
					rr.Receipt = signedReceipt(t, x, id, deliverable)
					rr.KEL, _ = identity.MarshalKEL(provKEL)
				}
				body, err = rr.Marshal()
			}
		case routeDelegate:
			id = fmt.Sprintf("ix_fzd_%d", x.n)
			doc, td := fuzzTaskDoc(t, x.req.self, text, capID, args)
			if td == nil {
				return
			}
			sealType = seal.TypeDelegate
			body, err = (&delegation.DelegateReq{TaskDoc: doc, Envelope: td.Envelope, InteractionID: id,
				Attachments: atts, Payment: deliverable, ContextID: msgID}).Marshal()
		}
		if err != nil {
			return // a body the sender could not have encoded either
		}
		env, err := craftE(t, s, to, sealType, id, body, func(in *seal.SealedInner) {
			if len(kel) > 0 {
				in.KEL = kel
			}
			if len(keys) > 0 {
				in.Keys = keys
			}
			in.TS = uint64(int64(in.TS) + skewMS%int64(30*24*time.Hour/time.Millisecond))
			in.Exp = in.TS + messageLifetimeMS
		})
		if err != nil {
			// seal.Seal refuses to seal it (a KEL that does not decode, a
			// time window past the limit): the receiver's seal.Open holds
			// the same rules, and ANetCore's own targets cover them.
			return
		}

		before, held := snap(t, to, id)
		r := receive(t, to, env)
		if r.class < rxAccepted || r.class > rxTransient {
			t.Fatalf("receive answered class %d (%s)", r.class, r.reason)
		}
		if r.class != rxAccepted && r.reason == "" {
			t.Fatalf("a refusal without a reason: %+v", r)
		}
		after, _ := snap(t, to, id)
		if held && !legit && after != before {
			t.Fatalf("an envelope from %s, not the task's peer, changed it: %+v -> %+v (%+v)", s.aid, before, after, r)
		}
		if route == routeToProvider && legit && after.state != before.state && verb != delegation.ChatEndRequest {
			switch after.state {
			case interactions.StateCompleted, interactions.StateFailed, interactions.StateRejected:
				t.Fatalf("the requester's message ended the provider's task as %s (%+v)", after.state, r)
			}
		}
		if route == routeToRequester && legit && typ%3 != 2 && after.receipt != before.receipt {
			t.Fatalf("a %s gave the task a receipt", sealType)
		}
		// The same envelope again changes nothing.
		if r.class != rxTransient {
			if r2 := receive(t, to, env); r2.class == rxAccepted && r.class == rxDropped {
				t.Fatalf("a refused envelope was accepted the second time: %+v then %+v", r, r2)
			}
			if again, _ := snap(t, to, id); again != after {
				t.Fatalf("the same envelope twice changed the task again: %+v -> %+v", after, again)
			}
		}
		checkView(t, to, id, provKEL)
	})
}

// craftE is craft that returns seal.Seal's refusal instead of failing.
func craftE(t *testing.T, s sender, to *Daemon, typ, ix string, body []byte, edit func(*seal.SealedInner)) ([]byte, error) {
	now := uint64(time.Now().UnixMilli())
	kel, err := identity.MarshalKEL(s.kel)
	if err != nil {
		t.Fatal(err)
	}
	in := &seal.SealedInner{From: s.aid, KeyStateSeq: s.ksn, To: to.AID(), Type: typ, IX: ix,
		MID: seal.NewMID(), TS: now, Exp: now + messageLifetimeMS, Body: body, KEL: kel, Keys: s.keys}
	if edit != nil {
		edit(in)
	}
	key, err := seal.SelectKey(keySetOf(t, to), now)
	if err != nil {
		t.Fatal(err)
	}
	return seal.Seal(in, key, s.sign)
}

// FuzzReceiveEnvelope: bytes where an envelope should be, from the hub
// mailbox or a transport module, seeded with envelopes a real sender
// sealed. The pipeline answers each with one of its three classes, and an
// envelope it cannot open is dropped from the mailbox but left
// unacknowledged on a direct path (F22).
func FuzzReceiveEnvelope(f *testing.F) {
	x := newRxFuzz(f)
	ctx := context.Background()
	id, err := x.req.Delegate(ctx, x.prov.AID(), "a task", nil)
	if err != nil {
		f.Fatal(err)
	}
	for _, env := range queuedFor(f, x.srv, x.prov.AID()) {
		f.Add(env, false)
	}
	f.Add(sealFrom(f, x.req, x.prov, seal.TypeMessage, id, chatBody(f, "hello", "m1")), false)
	f.Add(sealFrom(f, x.req, x.prov, seal.TypeMessage, id, chatBody(f, "hello", "m2")), true)
	f.Add(sealFrom(f, x.prov, x.req, seal.TypeStatus, id, mustMarshal(f, &delegation.StatusMsg{State: delegation.StateWorking,
		At: uint64(time.Now().UnixMilli())})), false)
	f.Add([]byte{}, false)
	f.Add([]byte{0xa0}, true)
	f.Fuzz(func(t *testing.T, env []byte, direct bool) {
		for _, to := range []*Daemon{x.prov, x.req} {
			r := to.receiveEnvelopeVia(ctx, env, rxPath{direct: direct})
			if r.class < rxAccepted || r.class > rxTransient {
				t.Fatalf("receive answered class %d (%s)", r.class, r.reason)
			}
			if _, oerr := seal.Open(env, to.AID(), to.enc); oerr != nil {
				want := rxDropped
				if direct {
					want = rxTransient
				}
				if r.class != want {
					t.Fatalf("an envelope that does not open: %+v, want class %d", r, want)
				}
			}
		}
	})
}

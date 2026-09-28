package a2ashape_test

// Fuzz targets for the projection of what a peer controls (docs/notes/0033):
// its message metadata and bodies, its deliverable and result metadata, the
// names and types of its files, its quote and receipts. Under plain
// `go test` they run their seeds only.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

var (
	fuzzStates = []interactions.State{interactions.StateSubmitted, interactions.StateWorking,
		interactions.StateInputRequired, interactions.StateCompleted, interactions.StateFailed,
		interactions.StateCanceled, interactions.StateRejected}
	fuzzPays = []string{interactions.PayNone, interactions.PayRequired, interactions.PaySubmitted,
		interactions.PayCompleted, interactions.PayFailed, interactions.PayRejected}
	fuzzKinds = []string{interactions.MsgText, interactions.MsgStatus, interactions.MsgPayment,
		interactions.MsgCancel, interactions.MsgEndRequest}
	fuzzVerif = []interactions.Verification{interactions.VerificationUnknown,
		interactions.VerificationVerified, interactions.VerificationUnverified}
)

// fuzzInput is one projected task: the local facts fixed by selectors, the
// peer's by bytes.
type fuzzInput struct {
	outbound, isCap, kernel      bool
	state, pay, kind, verif      uint8
	result                       []byte // the deliverable (peer's on an outbound task)
	resultMeta                   string // the result's metadata (peer's on an outbound task)
	peerMeta, peerBody, peerID   string // the peer's latest message
	attName, attMime             string // a file on it
	payRequired, payReceipts     []byte // the quote (peer's) and the stored receipts
	localMeta                    string // this node's own message
	kernelStatus, kernelReceipts string // the kernel's payment derivation, when kernel
}

func (in fuzzInput) source(peerMeta string) a2ashape.Source {
	role := interactions.RoleInbound
	if in.outbound {
		role = interactions.RoleOutbound
	}
	ix := &interactions.Interaction{ID: "ix_fuzz", Role: role, PeerAID: peer, Goal: "a goal",
		State: fuzzStates[int(in.state)%len(fuzzStates)], StateAt: now, StateSeq: 4,
		IsCapability: in.isCap, Result: in.result, ResultMeta: in.resultMeta,
		ReceiptVerified: fuzzVerif[int(in.verif)%len(fuzzVerif)],
		PayState:        fuzzPays[int(in.pay)%len(fuzzPays)], PayRequired: in.payRequired, PayReceipts: in.payReceipts}
	if in.isCap {
		ix.Goal = "invoke capability text.echo"
	}
	requester, provider := peer, self
	if in.outbound {
		requester, provider = self, peer
	}
	msgs := []interactions.Message{
		{Seq: 1, InteractionID: ix.ID, SenderAID: requester, Kind: interactions.MsgText, Body: "hello", MsgID: "m1",
			Metadata: in.localMeta},
		{Seq: 2, InteractionID: ix.ID, SenderAID: peer, Kind: fuzzKinds[int(in.kind)%len(fuzzKinds)], Body: in.peerBody,
			MsgID: in.peerID, Metadata: peerMeta},
		{Seq: 3, InteractionID: ix.ID, SenderAID: self, Kind: interactions.MsgStatus, MsgID: "m3",
			Metadata: in.localMeta},
	}
	_ = provider
	atts := []interactions.Attachment{{Seq: 1, InteractionID: ix.ID, MsgSeq: 2, Name: in.attName, Mime: in.attMime,
		Size: 5, CID: "bafkreiattachment"}}
	src := a2ashape.Source{Interaction: ix, Messages: msgs, Attachments: atts}
	if in.kernel {
		src.Payment = map[string]any{}
		if in.kernelStatus != "" {
			src.Payment[a2ashape.KeyX402Status] = in.kernelStatus
		}
		// The kernel derives its list from the stored receipts
		// (PaymentStatusMeta, receiptList): always a list.
		var rc []any
		if json.Unmarshal([]byte(in.kernelReceipts), &rc) == nil && rc != nil {
			src.Payment[a2ashape.KeyX402Receipts] = rc
		}
	}
	return src
}

// safeName stands in for the daemon's safeName: no path, no control
// characters.
func safeName(s string) string {
	s = s[strings.LastIndexAny(s, `/\`)+1:]
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

var fuzzOpts = a2ashape.Options{Artifacts: true, InlineFiles: true, SafeName: safeName,
	LoadFile: func(string) ([]byte, error) { return []byte("bytes"), nil }}

// nodeKeys are the task-level metadata keys that state this node's own
// view: its role, the peer it deals with, the state's sequence number, the
// effect status of a capability, whether it verified the receipt, and the
// payment status and receipts it stands behind. None of them may follow
// what a peer wrote in a message.
var nodeKeys = []string{a2ashape.KeyRole, a2ashape.KeyPeerAID, a2ashape.KeyStateSeq, a2ashape.KeyEffectStatus,
	a2ashape.KeyReceiptVerified, a2ashape.KeyX402Status, a2ashape.KeyX402Receipts, a2ashape.KeySkill}

// FuzzProject: any stored task projects to a Task that a2a-go reads, that
// keeps SI-6, whose node-stated metadata does not depend on the peer's
// message metadata, and in which a peer's message does not get its own
// messageId, a settlement this node did not verify, or a file name
// SafeName did not make.
func FuzzProject(f *testing.F) {
	seeds := []fuzzInput{
		{outbound: true, state: 1, peerMeta: `{"anet.state":"working"}`, peerBody: "on it", peerID: "p2"},
		{outbound: true, isCap: true, state: 3, result: []byte(`{"status":"OK","output":"x"}`),
			resultMeta: `{"anet.effect_status":"OK","anet.state":"completed"}`, verif: 1},
		{outbound: true, isCap: true, state: 4, result: []byte("not delivered to peer (gone)"),
			resultMeta: `{"anet.state":"failed","anet.reason":"undeliverable","anet.effect_status":"UNAVAILABLE"}`,
			kind:       1, peerMeta: `{"anet.effect_status":"OK","x402.payment.status":"payment-completed"}`},
		{outbound: true, isCap: true, state: 2, pay: 1, kind: 1, payRequired: []byte(`{"x402Version":1,"accepts":[{"scheme":"exact","maxAmountRequired":"1e400"}]}`),
			peerMeta: `{"x402.payment.status":"payment-required","x402.payment.required":{"accepts":[]}}`},
		{outbound: true, isCap: true, state: 3, pay: 2, kind: 2,
			peerMeta:    `{"x402.payment.status":"payment-completed","x402.payment.receipts":[{"success":true,"transaction":"t"}]}`,
			payReceipts: []byte(`[{"success":true,"transaction":"t","extensions":{"anet.settlement_verified":"unverified"}}]`)},
		{outbound: true, state: 1, kind: 0, peerBody: "file", attName: "../../etc/passwd\x00.txt", attMime: "text/html",
			peerMeta: `{"a2a.messageId":"client-id"}`, peerID: "p2"},
		{outbound: false, isCap: true, state: 2, pay: 1, kernel: true, kernelStatus: "payment-required",
			kernelReceipts: `[]`, peerMeta: `{"x402.payment.status":"payment-submitted","x402.payment.payload":{}}`, kind: 2},
		{outbound: true, isCap: true, state: 5, kernel: true, kernelStatus: "payment-submitted",
			kernelReceipts: `[{"success":true}]`, kind: 1, peerMeta: `{"x402.payment.status":"payment-verified"}`},
		{outbound: true, state: 4, kind: 1, peerMeta: `{"anet.reason":"x","anet.retry_after_ms":1e999}`},
		{outbound: true, isCap: true, state: 4, result: []byte(`{"capability":"text.echo","status":"FAILED","message":"quota exceeded"}`), verif: 1},
	}
	for _, s := range seeds {
		f.Add(s.outbound, s.isCap, s.kernel, s.state, s.pay, s.kind, s.verif, s.result, s.resultMeta, s.peerMeta,
			s.peerBody, s.peerID, s.attName, s.attMime, s.payRequired, s.payReceipts, s.localMeta, s.kernelStatus,
			s.kernelReceipts, uint16(0))
	}
	f.Fuzz(func(t *testing.T, outbound, isCap, kernel bool, state, pay, kind, verif uint8, result []byte,
		resultMeta, peerMeta, peerBody, peerID, attName, attMime string, payRequired, payReceipts []byte,
		localMeta, kernelStatus, kernelReceipts string, flip uint16) {
		// A knob the mutator turns cheaply: the case of one letter of the
		// deliverable ("status" to "Status"), which byte mutations rarely
		// hit and which adds no coverage for the fuzzer to keep.
		result = flipCase(result, flip)
		in := fuzzInput{outbound: outbound, isCap: isCap, kernel: kernel, state: state, pay: pay, kind: kind,
			verif: verif, result: result, resultMeta: resultMeta, peerMeta: peerMeta, peerBody: peerBody,
			peerID: peerID, attName: attName, attMime: attMime, payRequired: payRequired, payReceipts: payReceipts,
			localMeta: localMeta, kernelStatus: kernelStatus, kernelReceipts: kernelReceipts}
		task := a2ashape.Project(in.source(peerMeta), fuzzOpts)
		ours, err := json.Marshal(task)
		if err != nil {
			t.Fatalf("the projection does not encode: %v", err)
		}
		var sdk a2a.Task
		if err := json.Unmarshal(ours, &sdk); err != nil {
			t.Fatalf("a2a-go does not read the projection: %v\n%s", err, ours)
		}
		if err := checkSI6(&sdk); err != nil {
			t.Fatalf("SI-6: %v\n%s", err, ours)
		}
		var back a2ashape.Task
		if err := json.Unmarshal(ours, &back); err != nil {
			t.Fatalf("the projection does not read back: %v\n%s", err, ours)
		}
		for _, stream := range []a2ashape.Task{a2ashape.TaskForStream(task), a2ashape.ByReference(task)} {
			if _, err := json.Marshal(stream); err != nil {
				t.Fatalf("the stream form does not encode: %v", err)
			}
		}

		// The effect status stated is one the deliverable or the result's
		// metadata says, read as written (every reader but Go's decoder
		// takes member names as written), or this node's own fallback.
		if es, ok := task.Metadata[a2ashape.KeyEffectStatus].(string); ok && isCap {
			said := func(b []byte, key string) string {
				var m map[string]any
				dec := json.NewDecoder(bytes.NewReader(b))
				dec.UseNumber()
				if dec.Decode(&m) != nil {
					return ""
				}
				v, _ := m[key].(string)
				return v
			}
			if es != said(result, "status") && es != said([]byte(resultMeta), a2ashape.KeyEffectStatus) &&
				es != "UNVERIFIED" && es != "UNAVAILABLE" {
				t.Fatalf("effect_status %q is not what the deliverable (%s) or the result metadata (%s) says", es, result, resultMeta)
			}
		}

		// The same task with the peer's message carrying no metadata.
		plain := a2ashape.Project(in.source(""), fuzzOpts)
		if task.Status.State != plain.Status.State {
			t.Fatalf("the peer's metadata moved the state: %s, without it %s", task.Status.State, plain.Status.State)
		}
		for _, k := range nodeKeys {
			if !reflect.DeepEqual(task.Metadata[k], plain.Metadata[k]) {
				t.Fatalf("the peer's message metadata %s changed %s: %v, without it %v", peerMeta, k,
					task.Metadata[k], plain.Metadata[k])
			}
		}

		// Messages: the peer's own id is not taken, the peer's settlement
		// claims are not stated, and file names are SafeName's.
		var peerMD map[string]any
		_ = json.Unmarshal([]byte(peerMeta), &peerMD)
		claimedID, _ := peerMD[a2ashape.KeyMessageID].(string)
		msgs := append([]a2ashape.Message{}, task.History...)
		if task.Status.Message != nil {
			msgs = append(msgs, *task.Status.Message)
		}
		for _, a := range task.Artifacts {
			checkParts(t, a.Parts, attName)
		}
		for _, m := range msgs {
			if claimedID != "" && m.ID == claimedID && claimedID != peerID && claimedID != "m1" && claimedID != "m3" &&
				!bytes.Contains([]byte(localMeta), []byte(claimedID)) {
				t.Fatalf("a message took the id %q from the peer's metadata", claimedID)
			}
			checkParts(t, m.Parts, attName)
		}
		if outbound && task.Status.Message != nil {
			md := task.Status.Message.Metadata
			if md[a2ashape.KeyX402Status] == a2ashape.PaymentCompleted && task.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentCompleted {
				t.Fatalf("status.message says payment-completed; this node says %v\n%s", task.Metadata[a2ashape.KeyX402Status], ours)
			}
			if rc, ok := md[a2ashape.KeyX402Receipts]; ok {
				own, has := task.Metadata[a2ashape.KeyX402Receipts]
				if !has {
					own = []any{}
				}
				if !sameJSON(rc, own) {
					t.Fatalf("status.message states receipts %v; this node states %v\n%s", rc, own, ours)
				}
			}
		}
	})
}

// checkParts: a file part's name is what SafeName made of the peer's.
func checkParts(t *testing.T, parts []a2ashape.Part, attName string) {
	t.Helper()
	for _, p := range parts {
		if p.Kind == a2ashape.PartText || p.Kind == a2ashape.PartData {
			continue
		}
		if p.Filename != safeName(attName) {
			t.Fatalf("file part named %q; SafeName makes %q", p.Filename, safeName(attName))
		}
	}
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// FuzzMessageJSON: a message as a local client writes it (module/a2a reads
// a2a-go's message into this form) either is refused or reads into parts
// of exactly one kind, which write back to JSON that reads the same.
func FuzzMessageJSON(f *testing.F) {
	f.Add([]byte(`{"messageId":"m","role":"ROLE_USER","parts":[{"text":"hi"}]}`))
	f.Add([]byte(`{"messageId":"m","role":"ROLE_USER","parts":[{"raw":"aGVsbG8=","filename":"a.txt","mediaType":"text/plain"}]}`))
	f.Add([]byte(`{"messageId":"m","role":"ROLE_AGENT","parts":[{"data":{"n":1e400,"k":[1,2]}}],"metadata":{"a":1}}`))
	f.Add([]byte(`{"messageId":"m","role":"ROLE_USER","parts":[{"url":"https://x/y","text":"both"}]}`))
	f.Add([]byte(`{"messageId":"m","role":"ROLE_USER","parts":[{"data":null}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var m a2ashape.Message
		if json.Unmarshal(b, &m) != nil {
			return
		}
		for _, p := range m.Parts {
			if p.Kind < a2ashape.PartText || p.Kind > a2ashape.PartData {
				t.Fatalf("a part of no kind read: %+v", p)
			}
		}
		out, err := json.Marshal(m)
		if err != nil {
			// A part the reader took and the writer refuses would be a
			// message that cannot be answered or stored.
			t.Fatalf("a message that reads does not write: %v\n%s", err, b)
		}
		var again a2ashape.Message
		if err := json.Unmarshal(out, &again); err != nil {
			t.Fatalf("a message does not read back: %v\n%s", err, out)
		}
		out2, err := json.Marshal(again)
		if err != nil || !bytes.Equal(out, out2) {
			t.Fatalf("round trip changed the message:\n%s\n%s", out, out2)
		}
	})
}

// flipCase changes the case of the ASCII letter at i-1 (0: none) of b, in
// a copy.
func flipCase(b []byte, i uint16) []byte {
	if i == 0 || int(i) > len(b) {
		return b
	}
	c := b[i-1]
	if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
		b = append([]byte(nil), b...)
		b[i-1] = c ^ 0x20
	}
	return b
}

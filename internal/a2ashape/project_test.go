package a2ashape_test

// project_test.go is the SI-6 contract test (A2A-DESIGN §1, §17): tasks
// are written with the store operations the daemon uses, projected, decoded
// by a2a-go into a2a.Task and encoded again, and the result is checked
// against the "Task 表示" rules of §11.5 and the invariant that A2A
// COMPLETED implies neither effect OK nor a verified receipt.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
)

const (
	self = "did:anet:self"
	peer = "did:anet:peer"
	now  = int64(1790000000123)
)

func openStore(t *testing.T) *interactions.Store {
	t.Helper()
	st, err := interactions.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetClock(func() int64 { return now })
	return st
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func msg(t *testing.T, st *interactions.Store, ix, from, kind, body, id string, meta map[string]any) int64 {
	t.Helper()
	var mb []byte
	if meta != nil {
		mb, _ = json.Marshal(meta)
	}
	seq, _, err := st.AddMessageRecord(interactions.MessageRecord{InteractionID: ix, SenderAID: from,
		Kind: kind, Body: body, MsgID: id, Metadata: mb})
	must(t, err)
	return seq
}

func setState(t *testing.T, st *interactions.Store, ix string, s interactions.State) {
	t.Helper()
	_, err := st.SetState(ix, s)
	must(t, err)
}

func receipt(t *testing.T, ix, requester, provider, resultCID string) []byte {
	t.Helper()
	b, err := (&evidence.Receipt{InteractionID: ix, RequesterAID: requester, ProviderAID: provider,
		RequestCID: "bafyrequest", ResultCID: resultCID, CompletedAt: uint64(now)}).Marshal()
	must(t, err)
	return b
}

func capDoc(t *testing.T, capID, args string) []byte {
	t.Helper()
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{
		Intent:   tsir.Intent{Summary: "invoke capability " + capID, Body: "invoke capability " + capID},
		Requires: []tsir.Require{{ID: capID, Type: "capability", Necessity: "must"}},
		Contexts: []tsir.Context{{Key: "args", Value: args, Format: "json"}, {Key: "anet.nonce", Value: "n", Visibility: "private"}},
	}}}
	b, err := coredet.Marshal(td)
	must(t, err)
	return b
}

func jsonOf(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return b
}

// contract runs a projection through a2a-go: the SDK must read it, and
// write back the same bytes. It returns the SDK's view, which is what the
// assertions below look at — what a client of the local interface sees.
func contract(t *testing.T, task a2ashape.Task) *a2a.Task {
	t.Helper()
	ours := jsonOf(t, task)
	var sdk a2a.Task
	semanticallySame(t, ours, &sdk)
	if again := jsonOf(t, &sdk); !bytes.Equal(ours, again) {
		t.Errorf("a2a-go re-encodes the projection differently:\nours: %s\nsdk:  %s", ours, again)
	}
	if err := checkSI6(&sdk); err != nil {
		t.Errorf("SI-6: %v\n%s", err, ours)
	}
	return &sdk
}

// checkSI6 is the invariant as a client can check it from the A2A task
// alone: a terminal capability task names its effect status, a completed
// task names its receipt verification, and the two are separate keys that
// the state does not replace.
func checkSI6(task *a2a.Task) error {
	_, isCap := task.Metadata[a2ashape.KeySkill]
	es, hasES := task.Metadata[a2ashape.KeyEffectStatus]
	if isCap && task.Status.State.Terminal() && !hasES {
		return fmt.Errorf("terminal capability task %s has no %s", task.Status.State, a2ashape.KeyEffectStatus)
	}
	if !isCap && hasES {
		return fmt.Errorf("text task carries %s=%v", a2ashape.KeyEffectStatus, es)
	}
	if task.Status.State == a2a.TaskStateCompleted {
		rv, ok := task.Metadata[a2ashape.KeyReceiptVerified].(string)
		if !ok {
			return fmt.Errorf("completed task has no %s", a2ashape.KeyReceiptVerified)
		}
		switch rv {
		case a2ashape.ReceiptVerified, a2ashape.ReceiptUnverified, a2ashape.ReceiptUnknown:
		default:
			return fmt.Errorf("%s=%q", a2ashape.KeyReceiptVerified, rv)
		}
	}
	return nil
}

// The checker has teeth: each rule rejects the task that breaks it.
func TestSI6CheckerRefuses(t *testing.T) {
	cases := map[string]*a2a.Task{
		"capability without effect": {Status: a2a.TaskStatus{State: a2a.TaskStateFailed},
			Metadata: map[string]any{a2ashape.KeySkill: "x"}},
		"completed capability without effect": {Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			Metadata: map[string]any{a2ashape.KeySkill: "x", a2ashape.KeyReceiptVerified: "verified"}},
		"completed text without verification": {Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			Metadata: map[string]any{}},
		"verification merged into a bool": {Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			Metadata: map[string]any{a2ashape.KeyReceiptVerified: true}},
	}
	for name, task := range cases {
		if checkSI6(task) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func safe(name string) string { return "safe-" + filepath.Base(name) }

// A completed text task, from the requester's side: history without the
// control rows, the reply first among the artifacts, then the receipt,
// then the reply's attachment.
func TestTextTaskCompleted(t *testing.T) {
	st := openStore(t)
	const ix = "ix_text"
	must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "write a haiku",
		RequestCID: "bafyrequest", ContextID: "ctx_1", TaskNonce: "n"}))
	msg(t, st, ix, self, interactions.MsgText, "write a haiku", "msg_1", map[string]any{a2ashape.KeyMessageID: "client-1"})
	setState(t, st, ix, interactions.StateWorking)
	msg(t, st, ix, peer, interactions.MsgText, "about what?", "msg_2", nil)
	msg(t, st, ix, self, interactions.MsgText, "the sea", "msg_3", nil)
	msg(t, st, ix, peer, interactions.MsgStatus, "thinking", "st_1", nil)
	msg(t, st, ix, peer, interactions.MsgText, "50%", "msg_4", map[string]any{a2ashape.KeyState: "working"})
	msg(t, st, ix, peer, interactions.MsgPayment, "", "msg_5", map[string]any{a2ashape.KeyX402Status: "payment-verified"})
	reply := msg(t, st, ix, peer, interactions.MsgText, "waves fold\nsalt remembers\nthe shore", "msg_6", nil)
	must(t, st.AddAttachment(ix, reply, interactions.Attachment{Name: "../../.bashrc", Mime: "image/png", Size: 3, CID: "bafkimg", Data: []byte{1, 2, 3}}))
	msg(t, st, ix, self, interactions.MsgEndRequest, "", "msg_7", nil)

	tr, err := transcript.EncodeV2("n", []transcript.Message{
		{From: "requester", Body: "write a haiku"}, {From: "provider", Body: "about what?"},
		{From: "requester", Body: "the sea"},
		{From: "provider", Body: "waves fold\nsalt remembers\nthe shore",
			Attachments: []transcript.Attachment{{Name: "../../.bashrc", Mime: "image/png", Size: 3, CID: "bafkimg"}}},
	})
	must(t, err)
	must(t, st.Finish(ix, interactions.Finish{State: interactions.StateCompleted, Result: tr, ResultCID: "bafyresult",
		Receipt: receipt(t, ix, self, peer, "bafyresult"), Verified: interactions.VerificationVerified,
		Meta: []byte(`{"anet.state":"completed"}`)}))

	task, err := a2ashape.ProjectStored(st, ix, a2ashape.Options{Artifacts: true, InlineFiles: true, SafeName: safe,
		ProviderKEL: "a2VsCg=="})
	must(t, err)
	sdk := contract(t, task)

	if sdk.Status.State != a2a.TaskStateCompleted || sdk.ID != ix || sdk.ContextID != "ctx_1" || sdk.Status.Timestamp == nil ||
		sdk.Status.Timestamp.UnixMilli() != now || sdk.Status.Message != nil {
		t.Fatalf("status %+v", sdk.Status)
	}
	for k, want := range map[string]any{a2ashape.KeyReceiptVerified: "verified", a2ashape.KeyPeerAID: peer,
		a2ashape.KeyRole: "requester", a2ashape.KeyRequestCID: "bafyrequest", a2ashape.KeyResultCID: "bafyresult"} {
		if sdk.Metadata[k] != want {
			t.Errorf("metadata %s = %v, want %v", k, sdk.Metadata[k], want)
		}
	}
	// History: the four turns, in order, requester as user, provider as
	// agent; no status, progress, payment or end-request row.
	var got []string
	for _, m := range sdk.History {
		got = append(got, fmt.Sprintf("%s:%s:%s", m.Role, m.ID, m.Parts[0].Text()))
		if m.TaskID != ix || m.ContextID != "ctx_1" {
			t.Errorf("message %s not bound to its task: %+v", m.ID, m)
		}
	}
	want := []string{"ROLE_USER:client-1:write a haiku", "ROLE_AGENT:msg_2:about what?", "ROLE_USER:msg_3:the sea",
		"ROLE_AGENT:msg_6:waves fold\nsalt remembers\nthe shore"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("history\n got %q\nwant %q", got, want)
	}
	// The history carries a file's metadata, never its bytes (0017 Q12):
	// name, type, size and the content id to fetch it by.
	if file := sdk.History[3].Parts[1]; file.Raw() != nil || file.URL() != a2a.URL(a2ashape.AttachmentURI(ix, "bafkimg")) ||
		file.Filename != "safe-.bashrc" || file.MediaType != "image/png" || file.Metadata[a2ashape.KeyCID] != "bafkimg" ||
		file.Metadata[a2ashape.KeyAttachmentCID] != "bafkimg" || file.Metadata[a2ashape.KeySize] != 3.0 {
		t.Fatalf("reply attachment in the history %+v", file)
	}
	// Artifacts: the reply only, its text and then its file (0017 Q21 P1);
	// the receipt is metadata, not output (P2).
	if len(sdk.Artifacts) != 1 || sdk.Artifacts[0].ID != a2ashape.ArtifactReply || len(sdk.Artifacts[0].Parts) != 2 {
		t.Fatalf("artifacts %+v", sdk.Artifacts)
	}
	if sdk.Artifacts[0].Parts[0].Text() != "waves fold\nsalt remembers\nthe shore" {
		t.Errorf("reply %+v", sdk.Artifacts[0].Parts)
	}
	if f := sdk.Artifacts[0].Parts[1]; string(f.Raw()) != "\x01\x02\x03" || f.Filename != "safe-.bashrc" {
		t.Errorf("reply file %+v", f)
	}
	rc, _ := sdk.Metadata[a2ashape.KeyReceipt].(map[string]any)
	if rc["verified"] != "verified" || rc["result_cid"] != "bafyresult" || rc["provider_aid"] != peer ||
		rc["provider_kel"] != "a2VsCg==" || rc["receipt_cid"] == nil || rc["receipt"] == "" {
		t.Errorf("receipt metadata %v", rc)
	}

	// The same task without inline bytes and without a name sanitizer:
	// files by reference, and no peer-chosen name.
	ref, err := a2ashape.ProjectStored(st, ix, a2ashape.Options{Artifacts: true})
	must(t, err)
	sdk = contract(t, ref)
	f := sdk.Artifacts[0].Parts[1]
	if f.URL() != a2a.URL(a2ashape.AttachmentURI(ix, "bafkimg")) || f.Filename != "" || f.Raw() != nil {
		t.Fatalf("by-reference part %+v", f)
	}
	if !strings.HasPrefix(string(f.URL()), "anet:attachment?interaction_id=ix_text&cid=bafkimg") {
		t.Fatalf("attachment uri %s", f.URL())
	}

	// History length and artifacts off (ListTasks' default).
	one := 1
	short, err := a2ashape.ProjectStored(st, ix, a2ashape.Options{HistoryLength: &one})
	must(t, err)
	sdk = contract(t, short)
	if len(sdk.History) != 1 || sdk.History[0].ID != "msg_6" || sdk.Artifacts != nil || sdk.Metadata[a2ashape.KeyReceipt] != nil {
		t.Fatalf("historyLength=1: %+v", sdk)
	}
	zero := 0
	none, err := a2ashape.ProjectStored(st, ix, a2ashape.Options{HistoryLength: &zero})
	must(t, err)
	if none.History != nil {
		t.Fatalf("historyLength=0 kept %d messages", len(none.History))
	}
}

// SI-6: a receipt that could not be checked does not change the state and
// is not dropped — COMPLETED with receipt_verified=unverified.
func TestUnverifiedReceiptStaysCompleted(t *testing.T) {
	for _, v := range []interactions.Verification{interactions.VerificationUnverified, interactions.VerificationUnknown} {
		st := openStore(t)
		const ix = "ix_unv"
		must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "g", ContextID: "c"}))
		msg(t, st, ix, self, interactions.MsgText, "g", "m1", nil)
		tr, _ := transcript.EncodeV2("n", []transcript.Message{{From: "requester", Body: "g"}, {From: "provider", Body: "ok"}})
		must(t, st.Finish(ix, interactions.Finish{State: interactions.StateCompleted, Result: tr, ResultCID: "bafyr",
			Receipt: receipt(t, ix, self, peer, "bafyr"), Verified: v}))
		task, err := a2ashape.ProjectStored(st, ix, a2ashape.Options{Artifacts: true})
		must(t, err)
		sdk := contract(t, task)
		want := map[interactions.Verification]string{interactions.VerificationUnverified: "unverified",
			interactions.VerificationUnknown: "unknown"}[v]
		if sdk.Status.State != a2a.TaskStateCompleted || sdk.Metadata[a2ashape.KeyReceiptVerified] != want {
			t.Fatalf("%q: state %s, receipt_verified %v", v, sdk.Status.State, sdk.Metadata[a2ashape.KeyReceiptVerified])
		}
		if sdk.Artifacts[0].Parts[0].Text() != "ok" {
			t.Fatalf("%q: reply %+v", v, sdk.Artifacts[0])
		}
	}
}

// An inbound text task waiting for the requester: status.message is the
// provider's (this node's) latest message, with its attachment.
func TestInputRequiredCarriesProviderMessage(t *testing.T) {
	st := openStore(t)
	const ix = "ix_in"
	must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleInbound, PeerAID: peer, Goal: "draw a cat",
		ContextID: "c", Trust: interactions.TrustPeer}))
	msg(t, st, ix, peer, interactions.MsgText, "draw a cat", "", nil) // the opening row has no id on this side
	seq := msg(t, st, ix, self, interactions.MsgText, "like this?", "msg_q", nil)
	must(t, st.AddAttachment(ix, seq, interactions.Attachment{Name: "cat.png", Mime: "image/png", Size: 2, CID: "bafkcat", Data: []byte{9, 9}}))
	setState(t, st, ix, interactions.StateInputRequired)

	task, err := a2ashape.ProjectStored(st, ix, a2ashape.Options{InlineFiles: true, SafeName: safe})
	must(t, err)
	sdk := contract(t, task)
	m := sdk.Status.Message
	if sdk.Status.State != a2a.TaskStateInputRequired || m == nil || m.Role != a2a.MessageRoleAgent || m.ID != "msg_q" ||
		m.Parts[0].Text() != "like this?" || string(m.Parts[1].Raw()) != "\x09\x09" {
		t.Fatalf("status %+v / %+v", sdk.Status, m)
	}
	if sdk.Metadata[a2ashape.KeyRole] != "provider" || sdk.Metadata[a2ashape.KeyReceiptVerified] != nil ||
		sdk.Metadata[a2ashape.KeyTrust] != "peer" || sdk.Metadata[a2ashape.KeyStateSeq] != 2.0 {
		t.Fatalf("metadata %v", sdk.Metadata)
	}
	// The opening row, written without a message id, gets a stable one.
	if len(sdk.History) != 2 || sdk.History[0].Role != a2a.MessageRoleUser || sdk.History[0].ID != ix+".m1" {
		t.Fatalf("history %+v", sdk.History)
	}
	// Once the requester answers, the provider's message no longer
	// explains the state.
	msg(t, st, ix, peer, interactions.MsgText, "yes but orange", "msg_a", nil)
	setState(t, st, ix, interactions.StateWorking)
	task, err = a2ashape.ProjectStored(st, ix, a2ashape.Options{})
	must(t, err)
	if sdk = contract(t, task); sdk.Status.Message != nil {
		t.Fatalf("working task kept a stale status message: %+v", sdk.Status.Message)
	}
	// Progress after that does.
	msg(t, st, ix, self, interactions.MsgStatus, "drawing", "st_p", map[string]any{"pct": 40})
	task, err = a2ashape.ProjectStored(st, ix, a2ashape.Options{})
	must(t, err)
	if sdk = contract(t, task); sdk.Status.Message == nil || sdk.Status.Message.Parts[0].Text() != "drawing" {
		t.Fatalf("progress not shown: %+v", sdk.Status)
	}
	if len(sdk.History) != 3 {
		t.Fatalf("a status row entered the history: %d messages", len(sdk.History))
	}
}

// capTask writes an outbound capability task and its answer the way the
// daemon does: the request TaskDoc, the opening row, and a result whose
// deliverable carries the effect status.
func capTask(t *testing.T, st *interactions.Store, ix string, state interactions.State, deliverable string, meta string) {
	t.Helper()
	must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "invoke capability cas.put",
		RequestCID: "bafyrequest", RequestDoc: capDoc(t, "cas.put", `{"key":"k","n":3}`), ContextID: "c", IsCapability: true}))
	msg(t, st, ix, self, interactions.MsgText, `invoke capability cas.put args={"key":"k","n":3}`, "msg_req", nil)
	if deliverable == "" {
		return
	}
	var mb []byte
	if meta != "" {
		mb = []byte(meta)
	}
	must(t, st.Finish(ix, interactions.Finish{State: state, Result: []byte(deliverable), ResultCID: "bafyresult",
		Receipt: receipt(t, ix, self, peer, "bafyresult"), Verified: interactions.VerificationVerified, Meta: mb}))
}

// SI-6 and §4.3: a capability whose effect could not be verified is
// COMPLETED with anet.effect_status=UNVERIFIED — not OK, and not failed.
func TestCapabilityUnverifiedIsCompletedNotOK(t *testing.T) {
	st := openStore(t)
	capTask(t, st, "ix_cap", interactions.StateCompleted,
		`{"capability":"cas.put","status":"UNVERIFIED","nonce":"n","verifiable":false,"message":"sent, no readback"}`,
		`{"anet.effect_status":"UNVERIFIED","anet.state":"completed"}`)
	task, err := a2ashape.ProjectStored(st, "ix_cap", a2ashape.Options{Artifacts: true})
	must(t, err)
	sdk := contract(t, task)
	if sdk.Status.State != a2a.TaskStateCompleted || sdk.Metadata[a2ashape.KeyEffectStatus] != "UNVERIFIED" ||
		sdk.Metadata[a2ashape.KeyReceiptVerified] != "verified" || sdk.Metadata[a2ashape.KeySkill] != "cas.put" {
		t.Fatalf("state %s metadata %v", sdk.Status.State, sdk.Metadata)
	}
	// The history is the one request, as the DataPart a client sends.
	if len(sdk.History) != 1 || sdk.History[0].ID != "msg_req" || sdk.History[0].Role != a2a.MessageRoleUser {
		t.Fatalf("history %+v", sdk.History)
	}
	req, _ := sdk.History[0].Parts[0].Data().(map[string]any)
	args, _ := req["args"].(map[string]any)
	if req["skill"] != "cas.put" || args["key"] != "k" || args["n"] != 3.0 {
		t.Fatalf("request part %v", req)
	}
	// The deliverable is the one artifact; the receipt is metadata.
	if len(sdk.Artifacts) != 1 || sdk.Artifacts[0].ID != a2ashape.ArtifactResult || sdk.Metadata[a2ashape.KeyReceipt] == nil {
		t.Fatalf("artifacts %+v metadata %v", sdk.Artifacts, sdk.Metadata)
	}
	if d, _ := sdk.Artifacts[0].Parts[0].Data().(map[string]any); d["status"] != "UNVERIFIED" {
		t.Fatalf("deliverable %v", d)
	}
}

// §4.3 row by row, as the daemon writes each answer.
func TestEffectStatesTable(t *testing.T) {
	cases := []struct {
		name        string
		state       interactions.State
		deliverable string
		meta        string
		wantState   a2a.TaskState
		wantEffect  string
		wantReason  any
		wantRetry   any
	}{
		{"ok", interactions.StateCompleted, `{"status":"OK","verifiable":true}`, `{"anet.effect_status":"OK"}`,
			a2a.TaskStateCompleted, "OK", nil, nil},
		{"failed", interactions.StateFailed, `{"status":"FAILED","message":"boom"}`, `{"anet.effect_status":"FAILED"}`,
			a2a.TaskStateFailed, "FAILED", nil, nil},
		{"busy", interactions.StateRejected, `{"status":"UNAVAILABLE","message":"busy"}`,
			`{"anet.effect_status":"UNAVAILABLE","anet.retry_after_ms":60000}`, a2a.TaskStateRejected, "UNAVAILABLE", nil, 60000.0},
		{"not served", interactions.StateRejected, `{"status":"UNAVAILABLE","message":"this node does not serve cas.put"}`,
			`{"anet.effect_status":"UNAVAILABLE","anet.reason":"capability_not_served"}`, a2a.TaskStateRejected, "UNAVAILABLE", "capability_not_served", nil},
		{"unavailable, no reason given", interactions.StateRejected, `{"status":"UNAVAILABLE","message":"device offline"}`,
			`{"anet.effect_status":"UNAVAILABLE"}`, a2a.TaskStateRejected, "UNAVAILABLE", a2ashape.ReasonUnavailable, nil},
		{"interrupted", interactions.StateFailed, `{"status":"UNVERIFIED","message":"the provider stopped"}`,
			`{"anet.effect_status":"UNVERIFIED","anet.reason":"interrupted"}`, a2a.TaskStateFailed, "UNVERIFIED", "interrupted", nil},
		{"result metadata lost", interactions.StateCompleted, `{"status":"OK"}`, ``, a2a.TaskStateCompleted, "OK", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := openStore(t)
			capTask(t, st, "ix", c.state, c.deliverable, c.meta)
			task, err := a2ashape.ProjectStored(st, "ix", a2ashape.Options{Artifacts: true})
			must(t, err)
			sdk := contract(t, task)
			if sdk.Status.State != c.wantState || sdk.Metadata[a2ashape.KeyEffectStatus] != c.wantEffect ||
				sdk.Metadata[a2ashape.KeyReason] != c.wantReason || sdk.Metadata[a2ashape.KeyRetryAfterMS] != c.wantRetry {
				t.Fatalf("state %s metadata %v", sdk.Status.State, sdk.Metadata)
			}
			if c.wantState != a2a.TaskStateCompleted {
				var d map[string]any
				_ = json.Unmarshal([]byte(c.deliverable), &d)
				if m := sdk.Status.Message; m == nil || m.Parts[0].Text() != d["message"] {
					t.Fatalf("status message %+v, want the deliverable's message", sdk.Status.Message)
				}
			}
		})
	}
}

// A capability task that ended with no answer still names its effect: a
// refusal attempted nothing, a cancel leaves it unknown.
func TestCapabilityWithoutAnswer(t *testing.T) {
	t.Run("refused by policy", func(t *testing.T) {
		st := openStore(t)
		capTask(t, st, "ix", "", "", "")
		msg(t, st, "ix", peer, interactions.MsgStatus, "this node did not accept the task: denied", "st_1",
			map[string]any{a2ashape.KeyReason: "denied"})
		setState(t, st, "ix", interactions.StateRejected)
		task, err := a2ashape.ProjectStored(st, "ix", a2ashape.Options{Artifacts: true})
		must(t, err)
		sdk := contract(t, task)
		if sdk.Metadata[a2ashape.KeyEffectStatus] != "UNAVAILABLE" || sdk.Metadata[a2ashape.KeyReason] != "denied" ||
			sdk.Status.Message == nil || sdk.Status.Message.ID != "st_1" || sdk.Artifacts != nil {
			t.Fatalf("%+v", sdk)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		st := openStore(t)
		capTask(t, st, "ix", "", "", "")
		msg(t, st, "ix", self, interactions.MsgCancel, "", "c1", nil)
		setState(t, st, "ix", interactions.StateCanceled)
		task, err := a2ashape.ProjectStored(st, "ix", a2ashape.Options{})
		must(t, err)
		sdk := contract(t, task)
		if sdk.Metadata[a2ashape.KeyEffectStatus] != "UNVERIFIED" || sdk.Metadata[a2ashape.KeyCancelRequested] != nil {
			t.Fatalf("%v", sdk.Metadata)
		}
	})
	t.Run("pending approval", func(t *testing.T) {
		st := openStore(t)
		capTask(t, st, "ix", "", "", "")
		msg(t, st, "ix", peer, interactions.MsgStatus, "", "st_q", map[string]any{a2ashape.KeyInbound: "pending_approval"})
		task, err := a2ashape.ProjectStored(st, "ix", a2ashape.Options{})
		must(t, err)
		sdk := contract(t, task)
		if sdk.Status.State != a2a.TaskStateSubmitted || sdk.Metadata[a2ashape.KeyInbound] != "pending_approval" ||
			sdk.Metadata[a2ashape.KeyEffectStatus] != nil || sdk.Status.Message == nil {
			t.Fatalf("%+v", sdk)
		}
	})
}

// A number no float64 holds, in metadata a peer wrote, reaches the client
// as text: a2a-go would refuse the whole task over it (and module/a2a with
// it every ListTasks page the task is on).
func TestPeerNumbersStayReadable(t *testing.T) {
	st := openStore(t)
	const ix = "ix_num"
	must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "g", ContextID: "c"}))
	msg(t, st, ix, self, interactions.MsgText, "g", "m1", nil)
	msg(t, st, ix, peer, interactions.MsgText, "?", "m2", map[string]any{"n": json.RawMessage(`1e400`)})
	setState(t, st, ix, interactions.StateInputRequired)
	task, err := a2ashape.ProjectStored(st, ix, a2ashape.Options{})
	must(t, err)
	sdk := contract(t, task)
	if sdk.Status.Message == nil || sdk.Status.Message.Metadata["n"] != "1e400" {
		t.Fatalf("status message %+v", sdk.Status.Message)
	}
}

// A cancel sent after a payment was submitted leaves the task open; the
// task says the cancel is pending (§4.2).
func TestCancelRequested(t *testing.T) {
	st := openStore(t)
	const ix = "ix_c"
	must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "g", ContextID: "c"}))
	msg(t, st, ix, self, interactions.MsgText, "g", "m1", nil)
	setState(t, st, ix, interactions.StateWorking)
	msg(t, st, ix, self, interactions.MsgCancel, "", "m2", nil)
	task, err := a2ashape.ProjectStored(st, ix, a2ashape.Options{})
	must(t, err)
	sdk := contract(t, task)
	if sdk.Status.State != a2a.TaskStateWorking || sdk.Metadata[a2ashape.KeyCancelRequested] != true || len(sdk.History) != 1 {
		t.Fatalf("%+v", sdk)
	}
}

// A PAYMENT_REQUIRED answer is input-required with the a2a-x402
// payment-required message; a paid answer carries the settlement.
func TestPaymentKeys(t *testing.T) {
	quote := `{"x402Version":2,"accepts":[{"scheme":"anet-credit","network":"hub:did:anet:h","amount":"5","payTo":"did:anet:peer"}]}`
	st := openStore(t)
	capTask(t, st, "ix_q", interactions.StateInputRequired,
		`{"capability":"cas.put","status":"PAYMENT_REQUIRED","message":"costs 5","payment_required":`+quote+`}`,
		`{"anet.effect_status":"PAYMENT_REQUIRED","anet.state":"input-required"}`)
	task, err := a2ashape.ProjectStored(st, "ix_q", a2ashape.Options{})
	must(t, err)
	sdk := contract(t, task)
	m := sdk.Status.Message
	if sdk.Status.State != a2a.TaskStateInputRequired || m == nil || m.Metadata[a2ashape.KeyX402Status] != "payment-required" ||
		m.Parts[0].Text() != "costs 5" || sdk.Metadata[a2ashape.KeyX402Status] != "payment-required" {
		t.Fatalf("%+v / %+v", sdk.Status, m)
	}
	req, _ := m.Metadata[a2ashape.KeyX402Required].(map[string]any)
	if acc, _ := req["accepts"].([]any); len(acc) != 1 {
		t.Fatalf("x402.payment.required %v", m.Metadata[a2ashape.KeyX402Required])
	}
	// The quote is signed and stored like an answer, but it is not the
	// task's output: no anet.result for a stream to hand over as the work.
	task, err = a2ashape.ProjectStored(st, "ix_q", a2ashape.Options{Artifacts: true})
	must(t, err)
	if sdk = contract(t, task); sdk.Artifacts != nil {
		t.Fatalf("a quote projected as artifacts: %+v", sdk.Artifacts)
	}
	// Nor is its message why the task ended when the requester walks away.
	msg(t, st, "ix_q", self, interactions.MsgCancel, "", "c1", nil)
	setState(t, st, "ix_q", interactions.StateCanceled)
	task, err = a2ashape.ProjectStored(st, "ix_q", a2ashape.Options{Artifacts: true})
	must(t, err)
	if sdk = contract(t, task); sdk.Status.State != a2a.TaskStateCanceled || sdk.Status.Message != nil || sdk.Artifacts != nil {
		t.Fatalf("canceled after a quote: %+v / %+v", sdk.Status, sdk.Artifacts)
	}

	st = openStore(t)
	capTask(t, st, "ix_p", interactions.StateCompleted,
		`{"capability":"cas.put","status":"OK","paid":{"transaction":"tx1","amount":"5","network":"hub:did:anet:h","receipt":"UkVD"}}`,
		`{"anet.effect_status":"OK"}`)
	task, err = a2ashape.ProjectStored(st, "ix_p", a2ashape.Options{})
	must(t, err)
	sdk = contract(t, task)
	rcs, _ := sdk.Metadata[a2ashape.KeyX402Receipts].([]any)
	if sdk.Metadata[a2ashape.KeyX402Status] != "payment-completed" || len(rcs) != 1 || sdk.Status.Message == nil ||
		sdk.Status.Message.Metadata[a2ashape.KeyX402Status] != "payment-completed" {
		t.Fatalf("%+v", sdk)
	}
	r0, _ := rcs[0].(map[string]any)
	ext, _ := r0["extensions"].(map[string]any)
	if r0["success"] != true || r0["transaction"] != "tx1" || ext[a2ashape.KeySettlementReceipt] != "UkVD" {
		t.Fatalf("receipt %v", r0)
	}

	// The same-task flow's columns (§8.3), which the payment work fills:
	// pay_state and the stored settlement responses.
	src, err := a2ashape.Load(st, "ix_p")
	must(t, err)
	src.Interaction.Result = []byte(`{"capability":"cas.put","status":"UNAVAILABLE","message":"could not deliver"}`)
	src.Interaction.State = interactions.StateFailed
	src.Interaction.PayState = interactions.PayCompleted
	src.Interaction.PayReceipts = []byte(`[{"success":true,"transaction":"tx2","network":"n"}]`)
	sdk = contract(t, a2ashape.Project(src, a2ashape.Options{}))
	if sdk.Status.State != a2a.TaskStateFailed || sdk.Status.Message.Metadata[a2ashape.KeyX402Status] != "payment-completed" ||
		sdk.Status.Message.Parts[0].Text() != "could not deliver" || sdk.Metadata[a2ashape.KeyEffectStatus] != "UNAVAILABLE" {
		t.Fatalf("%+v", sdk)
	}
	if rcs, _ := sdk.Status.Message.Metadata[a2ashape.KeyX402Receipts].([]any); len(rcs) != 1 {
		t.Fatalf("final message receipts %v", sdk.Status.Message.Metadata)
	}
	// a2a-x402 §9: a payment failure's code is in the message and in the
	// task's metadata.
	src.Interaction.PayState = interactions.PayFailed
	src.Interaction.ResultMeta = `{"x402.payment.error":"EXPIRED_PAYMENT"}`
	sdk = contract(t, a2ashape.Project(src, a2ashape.Options{}))
	if sdk.Status.Message.Metadata[a2ashape.KeyX402Error] != "EXPIRED_PAYMENT" || sdk.Metadata[a2ashape.KeyX402Error] != "EXPIRED_PAYMENT" ||
		sdk.Status.Message.Metadata[a2ashape.KeyX402Status] != "payment-failed" {
		t.Fatalf("payment failure: %+v / %v", sdk.Status.Message.Metadata, sdk.Metadata)
	}
	src.Interaction.ResultMeta = ""

	src.Interaction.State = interactions.StateInputRequired
	src.Interaction.Result, src.Interaction.Receipt, src.Interaction.PayReceipts = nil, nil, nil
	src.Interaction.PayState = interactions.PayRequired
	src.Interaction.PayRequired = []byte(quote)
	sdk = contract(t, a2ashape.Project(src, a2ashape.Options{}))
	if m := sdk.Status.Message; m == nil || m.Metadata[a2ashape.KeyX402Status] != "payment-required" || m.Metadata[a2ashape.KeyX402Required] == nil {
		t.Fatalf("pay_state=required: %+v", sdk.Status)
	}
}

// Every state, for both kinds of task, with and without an answer, passes
// the contract and SI-6 — including the combinations no test above names.
func TestSI6Sweep(t *testing.T) {
	states := []interactions.State{interactions.StateSubmitted, interactions.StateWorking, interactions.StateInputRequired,
		interactions.StateCompleted, interactions.StateFailed, interactions.StateCanceled, interactions.StateRejected}
	answers := []string{"", `{"status":"OK"}`, `{"status":"UNVERIFIED"}`, `{"status":"FAILED"}`, `{"status":"UNAVAILABLE"}`,
		`{"status":"PAYMENT_REQUIRED"}`, `not json`}
	for _, capability := range []bool{true, false} {
		for _, st := range states {
			for _, ans := range answers {
				for _, v := range []interactions.Verification{interactions.VerificationVerified, interactions.VerificationUnknown} {
					ix := &interactions.Interaction{ID: "ix", Role: interactions.RoleOutbound, PeerAID: peer, State: st,
						StateAt: now, StateSeq: 3, IsCapability: capability, ContextID: "c", ReceiptVerified: v, Goal: "g"}
					if capability {
						ix.RequestDoc = capDoc(t, "x.y", `{}`)
					}
					if ans != "" {
						ix.Result, ix.Receipt = []byte(ans), receipt(t, "ix", self, peer, "r")
					}
					task := a2ashape.Project(a2ashape.Source{Interaction: ix}, a2ashape.Options{Artifacts: true})
					sdk := contract(t, task)
					// checkSI6 finds capability tasks by anet.skill; a
					// projection that dropped it would make the check vacuous.
					if _, hasSkill := sdk.Metadata[a2ashape.KeySkill]; hasSkill != capability {
						t.Fatalf("%v %s %q: anet.skill present = %v", capability, st, ans, hasSkill)
					}
					if sdk.Status.State != a2a.TaskState(a2ashape.StateOf(st)) {
						t.Fatalf("%v %s %q: projected state %s", capability, st, ans, sdk.Status.State)
					}
					if capability && st == interactions.StateCompleted && sdk.Metadata[a2ashape.KeyEffectStatus] == "OK" && ans != `{"status":"OK"}` {
						t.Fatalf("%s %q: completed was read as effect OK", st, ans)
					}
				}
			}
		}
	}
}

func TestStateMapping(t *testing.T) {
	for _, st := range []interactions.State{interactions.StateSubmitted, interactions.StateWorking, interactions.StateInputRequired,
		interactions.StateCompleted, interactions.StateFailed, interactions.StateCanceled, interactions.StateRejected} {
		ts := a2ashape.StateOf(st)
		if !ts.Valid() || ts.Terminal() != st.IsTerminal() {
			t.Errorf("%s -> %s", st, ts)
		}
		if back, ok := a2ashape.StoreState(ts); !ok || back != st {
			t.Errorf("%s -> %s -> %s", st, ts, back)
		}
	}
	if st, ok := a2ashape.StoreState("input-required"); !ok || st != interactions.StateInputRequired {
		t.Errorf("store spelling: %s %v", st, ok)
	}
	for _, ts := range []a2ashape.TaskState{a2ashape.TaskStateAuthRequired, a2ashape.TaskStateUnspecified, "bogus"} {
		if _, ok := a2ashape.StoreState(ts); ok {
			t.Errorf("%s maps to a stored state", ts)
		}
	}
}

func TestProjectStoredNotFound(t *testing.T) {
	_, err := a2ashape.ProjectStored(openStore(t), "ix_none", a2ashape.Options{})
	if !isNotFound(err) {
		t.Fatalf("got %v", err)
	}
}

func isNotFound(err error) bool {
	e, ok := err.(*a2ashape.Error)
	return ok && e.Name == a2ashape.ErrTaskNotFound.Name
}

// Source.Payment, the payment flow's own reading of its columns (the
// daemon's PaymentStatusMeta), decides the x402 keys, why a task waits on a
// payment, and anet.cancel_requested (A2A-DESIGN §8.2, §11.5, 0017 Q3). A
// failed attempt that leaves the quote open still asks for payment.
func TestKernelPaymentMeta(t *testing.T) {
	quote := `{"x402Version":2,"accepts":[{"scheme":"anet-credit","network":"hub:did:anet:h","amount":"5","payTo":"did:anet:peer"}]}`
	st := openStore(t)
	capTask(t, st, "ix_k", interactions.StateInputRequired, "", "")
	setState(t, st, "ix_k", interactions.StateInputRequired)
	src, err := a2ashape.Load(st, "ix_k")
	must(t, err)
	src.Interaction.PayState = interactions.PayRequired
	src.Interaction.PayRequired = []byte(quote)
	src.Payment = map[string]any{
		a2ashape.KeyX402Status: a2ashape.PaymentRequired, a2ashape.KeyX402Required: json.RawMessage(quote),
		a2ashape.KeyX402Receipts: []json.RawMessage{}, a2ashape.KeyQuoteExpiresAt: int64(1790086400123),
		a2ashape.KeyReason: "needs_operator_approval",
	}
	sdk := contract(t, a2ashape.Project(src, a2ashape.Options{}))
	if sdk.Metadata[a2ashape.KeyReason] != "needs_operator_approval" || sdk.Metadata[a2ashape.KeyX402Required] == nil ||
		sdk.Metadata[a2ashape.KeyQuoteExpiresAt] == nil || sdk.Metadata[a2ashape.KeyX402Status] != "payment-required" {
		t.Fatalf("waiting: %v", sdk.Metadata)
	}
	if m := sdk.Status.Message; m == nil || m.Metadata[a2ashape.KeyX402Required] == nil {
		t.Fatalf("waiting, status message: %+v", sdk.Status)
	}

	// A failed attempt (pay_state=failed, still input-required): the
	// message says payment-failed with the code, and carries the quote.
	src.Interaction.PayState = interactions.PayFailed
	src.Payment[a2ashape.KeyX402Status] = a2ashape.PaymentFailed
	src.Payment[a2ashape.KeyX402Error] = "INSUFFICIENT_FUNDS"
	src.Payment[a2ashape.KeyX402Receipts] = []json.RawMessage{json.RawMessage(`{"success":false,"errorReason":"insufficient_funds","network":"n","transaction":""}`)}
	sdk = contract(t, a2ashape.Project(src, a2ashape.Options{}))
	m := sdk.Status.Message
	if sdk.Status.State != a2a.TaskStateInputRequired || m == nil || m.Metadata[a2ashape.KeyX402Status] != "payment-failed" ||
		m.Metadata[a2ashape.KeyX402Error] != "INSUFFICIENT_FUNDS" || m.Metadata[a2ashape.KeyX402Required] == nil {
		t.Fatalf("after a failed attempt: %+v", sdk.Status)
	}
	if rc, _ := sdk.Metadata[a2ashape.KeyX402Receipts].([]any); len(rc) != 1 || sdk.Metadata[a2ashape.KeyX402Error] != "INSUFFICIENT_FUNDS" {
		t.Fatalf("after a failed attempt, metadata: %v", sdk.Metadata)
	}

	// A peer's number too large for a float64 in the quote stays readable.
	src.Payment[a2ashape.KeyX402Required] = json.RawMessage(`{"x402Version":2,"n":1e400,"accepts":[]}`)
	sdk = contract(t, a2ashape.Project(src, a2ashape.Options{}))
	if r, _ := sdk.Metadata[a2ashape.KeyX402Required].(map[string]any); r["n"] != "1e400" {
		t.Fatalf("quote number: %v", sdk.Metadata[a2ashape.KeyX402Required])
	}

	// Once the task has ended, the reason that ended it stays.
	src.Interaction.State = interactions.StateFailed
	src.Interaction.ResultMeta = `{"anet.reason":"quote_expired"}`
	sdk = contract(t, a2ashape.Project(src, a2ashape.Options{}))
	if sdk.Metadata[a2ashape.KeyReason] != "quote_expired" {
		t.Fatalf("terminal reason: %v", sdk.Metadata[a2ashape.KeyReason])
	}

	// anet.cancel_requested follows the kernel when it derived one: a
	// cancel on a task nothing was paid for is not a pending request.
	st = openStore(t)
	const ix = "ix_kc"
	must(t, st.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "g", ContextID: "c"}))
	msg(t, st, ix, self, interactions.MsgText, "g", "m1", nil)
	setState(t, st, ix, interactions.StateWorking)
	msg(t, st, ix, self, interactions.MsgCancel, "", "m2", nil)
	src, err = a2ashape.Load(st, ix)
	must(t, err)
	src.Payment = map[string]any{}
	if sdk = contract(t, a2ashape.Project(src, a2ashape.Options{})); sdk.Metadata[a2ashape.KeyCancelRequested] != nil {
		t.Fatalf("cancel_requested without a payment: %v", sdk.Metadata)
	}
	src.Payment = map[string]any{a2ashape.KeyX402Status: a2ashape.PaymentSubmitted, a2ashape.KeyCancelRequested: true}
	if sdk = contract(t, a2ashape.Project(src, a2ashape.Options{})); sdk.Metadata[a2ashape.KeyCancelRequested] != true ||
		sdk.Metadata[a2ashape.KeyX402Status] != "payment-submitted" {
		t.Fatalf("cancel_requested after a payment: %v", sdk.Metadata)
	}
}

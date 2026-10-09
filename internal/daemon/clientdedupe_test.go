package daemon

// A local client's retry of a message (clientdedupe.go; correctness-ux
// CUX-D1 to D7, SI-65 to SI-67): LOCAL-1's client part, the decision table
// (P-CUX1), the content the digest covers (M1 to M4, M41, M42), the narrow
// window after a task ends (M5 to M7) and the unanswered mark (CUX-D5).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// vecLocal1Client is the client part of LOCAL-1.
type vecLocal1Client struct {
	Keys map[string]struct {
		Key string `json:"key"`
	} `json:"keys"`
	Client struct {
		Labels   map[string]string `json:"labels"`
		Messages []struct {
			Name       string   `json:"name"`
			Message    string   `json:"message"`
			Extensions []string `json:"extensions"`
			Canon      string   `json:"canon"`
			ClientKey  string   `json:"client_key"`
			Digest     string   `json:"client_digest"`
		} `json:"messages"`
		Pairs []struct {
			A string `json:"a"`
			B string `json:"b"`
		} `json:"pairs"`
	} `json:"client"`
}

// decodeLiteral reads a client message as the control plane and the local
// A2A interface do: numbers as the client wrote them.
func decodeLiteral(t testing.TB, text string) a2ashape.Message {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var m a2ashape.Message
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return m
}

// contentOf is what a send holds a message's id to: a payment decision, or
// the input of any other message.
func contentOf(t testing.TB, m a2ashape.Message, ext []string) clientContent {
	t.Helper()
	if hasX402Meta(m.Metadata) {
		return paymentContent(m, ext)
	}
	in, err := parseTaskInput(m, a2ashape.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	return inputContent(in, m, ext)
}

// LOCAL-1, the client part (core L37, L38): golden/py/gen_v016.py computes
// the canonical content, the client key and the digest of each message with
// its own implementation; here they come from the daemon's code, from the
// message as the client wrote it, and must agree byte for byte. The two
// pairs are one messageId with two contents each (9007199254740993 and
// 9007199254740992; 1 and 1.0): one key, two digests.
func TestLOCAL1Client(t *testing.T) {
	b, err := os.ReadFile("../runtime/interactions/testdata/vec-local-1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vecLocal1Client
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Client.Labels["client_key"] != clientKeyLabel || v.Client.Labels["client_digest"] != clientMsgLabel {
		t.Fatalf("labels %v", v.Client.Labels)
	}
	macKey, err := hex.DecodeString(v.Keys["mac_key"].Key)
	if err != nil || len(macKey) != 32 {
		t.Fatalf("mac_key %q", v.Keys["mac_key"].Key)
	}
	if len(v.Client.Messages) < 4 || len(v.Client.Pairs) != 2 {
		t.Fatalf("%d messages, %d pairs", len(v.Client.Messages), len(v.Client.Pairs))
	}
	digests := map[string]string{}
	keys := map[string]string{}
	kinds := map[string]bool{}
	for _, m := range v.Client.Messages {
		msg := decodeLiteral(t, m.Message)
		c := contentOf(t, msg, m.Extensions)
		kinds[c.kind] = true
		canon, err := clientCanon(c)
		if err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		if string(canon) != m.Canon {
			t.Errorf("%s: canon\n got  %s\n want %s", m.Name, canon, m.Canon)
		}
		if got := hex.EncodeToString(clientKey(msg.ID)); got != m.ClientKey {
			t.Errorf("%s: client_key %s, want %s", m.Name, got, m.ClientKey)
		}
		if got := hex.EncodeToString(clientDigest(macKey, canon)); got != m.Digest {
			t.Errorf("%s: client_digest %s, want %s", m.Name, got, m.Digest)
		}
		digests[m.Name], keys[m.Name] = m.Digest, m.ClientKey
	}
	for _, k := range []string{clientKindText, clientKindSkill, clientKindPayment} {
		if !kinds[k] {
			t.Errorf("no %s message in the vector", k)
		}
	}
	for _, p := range v.Client.Pairs {
		if keys[p.A] == "" || keys[p.A] != keys[p.B] || digests[p.A] == digests[p.B] {
			t.Errorf("%s and %s: keys %s %s, digests %s %s; want one key and two digests",
				p.A, p.B, keys[p.A], keys[p.B], digests[p.A], digests[p.B])
		}
	}
}

// P-CUX1: decideClientMessage against CUX-D3 and D4, cell by cell, over
// {no copy, open, completed, failed that may have run, failed undeliverable,
// rejected, canceled} × {same, other content} × {answered, unanswered} ×
// {inside, at, past the 10 minutes} × {new task, follow-up}.
func TestDecideClientMessage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	type state struct {
		name   string
		st     interactions.State
		reason string
	}
	states := []state{
		{"submitted", interactions.StateSubmitted, ""},
		{"working", interactions.StateWorking, ""},
		{"input-required", interactions.StateInputRequired, ""},
		{"completed", interactions.StateCompleted, ""},
		{"failed", interactions.StateFailed, "backend_failed"},
		{"failed, no reason", interactions.StateFailed, ""},
		{"failed undeliverable", interactions.StateFailed, a2ashape.ReasonUndeliverable},
		{"rejected", interactions.StateRejected, "busy"},
		{"canceled", interactions.StateCanceled, ""},
	}
	ages := []time.Duration{time.Second, clientRetryWindow - time.Millisecond, clientRetryWindow,
		clientRetryWindow + time.Millisecond, time.Hour}
	digest, other := []byte("digest-of-the-first-attempt....."), []byte("digest-of-other-content........")
	for _, followUp := range []bool{false, true} {
		if got := decideClientMessage(nil, digest, now); got != clientFresh {
			t.Fatalf("no copy: %s", got)
		}
		for _, s := range states {
			for _, same := range []bool{true, false} {
				for _, unanswered := range []bool{false, true} {
					for _, age := range ages {
						prior := &clientPrior{followUp: followUp, state: s.st, reason: s.reason,
							stateAt: now.Add(-age), digest: digest, unanswered: unanswered}
						newDigest := other
						if same {
							newDigest = append([]byte(nil), digest...)
						}
						var want clientVerdict
						terminal := s.st.IsTerminal()
						mayHaveRun := s.st == interactions.StateCompleted ||
							(s.st == interactions.StateFailed && s.reason != a2ashape.ReasonUndeliverable)
						switch {
						case followUp && same:
							want = clientRetry
						case followUp:
							want = clientReusedFollowUp
						case !terminal && same:
							want = clientRetry
						case !terminal:
							want = clientReusedOpen
						case same && unanswered && mayHaveRun && age <= clientRetryWindow:
							want = clientRetry
						default:
							want = clientFresh
						}
						if got := decideClientMessage(prior, newDigest, now); got != want {
							t.Errorf("follow-up %v, %s, same %v, unanswered %v, %s after: %s, want %s",
								followUp, s.name, same, unanswered, age, got, want)
						}
					}
				}
			}
		}
	}
}

// What a messageId is held to is the message's content, not how it was sent
// (CUX-D1; M1 to M4): the text, the args, the files, the client's metadata
// and the extensions it activated count; the messageId, the context, the
// task, a preview asked for (the extension or anet.preview_mode) and the
// client's own a2a.* keys do not.
func TestTheCanonIsTheContent(t *testing.T) {
	file := func(name string, b string) a2ashape.Part { return a2ashape.RawPart([]byte(b), name, "text/plain") }
	base := a2ashape.Message{ID: "m-1", Role: a2ashape.RoleUser, Parts: []a2ashape.Part{
		a2ashape.TextPart("hello"), file("a.txt", "AAAA")}, Metadata: map[string]any{"trace": "t-1"}}
	canon := func(m a2ashape.Message, ext ...string) string {
		t.Helper()
		b, err := clientCanon(contentOf(t, m, ext))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	want := canon(base)
	same := map[string]func(m *a2ashape.Message) []string{
		"another messageId":   func(m *a2ashape.Message) []string { m.ID = "m-2"; return nil },
		"a context":           func(m *a2ashape.Message) []string { m.ContextID = "ctx"; return nil },
		"a task":              func(m *a2ashape.Message) []string { m.TaskID = "ix_1"; return nil },
		"the preview asked":   func(*a2ashape.Message) []string { return []string{previewExtensionURI} },
		"anet.preview_mode":   func(m *a2ashape.Message) []string { m.Metadata["anet.preview_mode"] = "on"; return nil },
		"the client's a2a.*":  func(m *a2ashape.Message) []string { m.Metadata["a2a.messageId"] = "x"; return nil },
		"a role left unset":   func(m *a2ashape.Message) []string { m.Role = a2ashape.RoleUnspecified; return nil },
		"message extensions":  func(m *a2ashape.Message) []string { m.Extensions = []string{"urn:x"}; return nil },
		"the metadata copied": func(m *a2ashape.Message) []string { m.Metadata = map[string]any{"trace": "t-1"}; return nil },
	}
	for name, f := range same {
		m := base
		m.Metadata = map[string]any{"trace": "t-1"}
		ext := f(&m)
		if got := canon(m, ext...); got != want {
			t.Errorf("%s changed the content:\n %s\n %s", name, got, want)
		}
	}
	differ := map[string]func(m *a2ashape.Message) []string{
		"other text":         func(m *a2ashape.Message) []string { m.Parts[0] = a2ashape.TextPart("hello!"); return nil },
		"a file's bytes":     func(m *a2ashape.Message) []string { m.Parts[1] = file("a.txt", "AAAB"); return nil },
		"a file's name":      func(m *a2ashape.Message) []string { m.Parts[1] = file("b.txt", "AAAA"); return nil },
		"no file":            func(m *a2ashape.Message) []string { m.Parts = m.Parts[:1]; return nil },
		"other metadata":     func(m *a2ashape.Message) []string { m.Metadata["trace"] = "t-2"; return nil },
		"an extension":       func(*a2ashape.Message) []string { return []string{"https://example.org/ext/v1"} },
		"a second text part": func(m *a2ashape.Message) []string { m.Parts = append(m.Parts, a2ashape.TextPart("")); return nil },
	}
	for name, f := range differ {
		m := base
		m.Parts = append([]a2ashape.Part(nil), base.Parts...)
		m.Metadata = map[string]any{"trace": "t-1"}
		ext := f(&m)
		if got := canon(m, ext...); got == want {
			t.Errorf("%s did not change the content: %s", name, got)
		}
	}
	// A capability call: its args, to the literal (M2, M42).
	call := func(args string) string {
		return canon(decodeLiteral(t, `{"messageId":"m","role":"ROLE_USER","parts":[{"data":{"skill":"calc.add","args":`+args+`}}]}`))
	}
	for _, p := range [][2]string{
		{`{"a":9007199254740993}`, `{"a":9007199254740992}`},
		{`{"n":1}`, `{"n":1.0}`},
		{`{"n":1}`, `{"n":2}`},
		{`{"n":1}`, `{"n":1,"m":null}`},
		{`{"s":"x"}`, `{"s":"x "}`},
	} {
		if call(p[0]) == call(p[1]) {
			t.Errorf("args %s and %s are one content", p[0], p[1])
		}
	}
	if !strings.Contains(call(`{"a":9007199254740993}`), "9007199254740993") {
		t.Errorf("the literal was not kept: %s", call(`{"a":9007199254740993}`))
	}
	// A payment decision: submit and reject under one id are two contents
	// (CUX-D6); the text beside it is not part of it.
	pay := func(status, text string) string {
		return canon(a2ashape.Message{ID: "m", Parts: []a2ashape.Part{a2ashape.TextPart(text)},
			Metadata: map[string]any{"x402.payment.status": status}})
	}
	if pay("payment-submitted", "a") == pay("payment-rejected", "a") {
		t.Error("submit and reject are one content")
	}
	if pay("payment-submitted", "a") != pay("payment-submitted", "b") {
		t.Error("a payment decision's text is part of its content")
	}
}

// The canonical form itself: RFC 8785 member order by UTF-16 code units,
// its string escapes, and numbers as written (core L38).
func TestCanonJSON(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{`{"\ue000":1,"\ud83d\ude00":2,"\u00fc":3}`, "{\"\u00fc\":3,\"\U0001F600\":2,\"\ue000\":1}"},
		{`["\u0001\b\t\n\f\r\"\\/\u007f\u2028"]`, "[\"\\u0001\\b\\t\\n\\f\\r\\\"\\\\/\u007f\u2028\"]"},
		{`[1.0,1e2,-0,9007199254740993,0.1000]`, `[1.0,1e2,-0,9007199254740993,0.1000]`},
		{`{"x":[true,false,null,{}],"y":[]}`, `{"x":[true,false,null,{}],"y":[]}`},
	} {
		dec := json.NewDecoder(strings.NewReader(c.in))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		got, err := canonJSON(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.in, got, c.want)
		}
	}
	// A value built in Go encodes as its JSON does.
	got, err := canonJSON(map[string]any{"i": 7, "f": 0.5, "s": []string{"a"}})
	if err != nil || string(got) != `{"f":0.5,"i":7,"s":["a"]}` {
		t.Fatalf("Go values: %s %v", got, err)
	}
}

// --- through the task seam ---

// clientSend sends m to prov through req's local A2A seam, answering at once.
func clientSend(t *testing.T, req *Daemon, prov string, m a2ashape.Message, ext ...string) (a2ashape.Task, error) {
	t.Helper()
	return req.TaskSeam().Send(context.Background(), prov, module.TaskSend{Message: m, ReturnImmediately: true, Extensions: ext})
}

// mustSend is clientSend that must succeed.
func mustSend(t *testing.T, req *Daemon, prov string, m a2ashape.Message, ext ...string) a2ashape.Task {
	t.Helper()
	task, err := clientSend(t, req, prov, m, ext...)
	if err != nil {
		t.Fatalf("send %s: %v", m.ID, err)
	}
	return task
}

// reusedAs asserts err is MESSAGE_ID_REUSED of variant on task id.
func reusedAs(t *testing.T, err error, variant, id string) {
	t.Helper()
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Key != "message_id_reused."+variant || ref.Task == nil || ref.Task.ID != id {
		t.Fatalf("got %v, want message_id_reused.%s naming %s", err, variant, id)
	}
}

func deduplicated(task a2ashape.Task) bool { return task.Metadata[a2ashape.KeyDeduplicated] == true }

func callMsg(id, args string) a2ashape.Message {
	var a map[string]any
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&a); err != nil {
		panic(err)
	}
	return a2ashape.Message{ID: id, Role: a2ashape.RoleUser,
		Parts: []a2ashape.Part{a2ashape.DataPart(map[string]any{"skill": "calc.add", "args": a})}}
}

// SI-65: the same messageId on an open task is the same message only with
// the same content; any difference — text (M1), args (M2, M42 at the 53-bit
// edge), files (M3) — is refused, nothing written and nothing sent.
func TestOneIDOneContentWhileOpen(t *testing.T) {
	_, req, prov := registeredPair(t)
	cases := []struct {
		name        string
		first, next a2ashape.Message
	}{
		{"text", textMsg("hello", "", "m-text"), textMsg("hello, again", "", "m-text")},
		{"args", callMsg("m-args", `{"n":1}`), callMsg("m-args", `{"n":2}`)},
		{"53 bits", callMsg("m-big", `{"a":9007199254740993}`), callMsg("m-big", `{"a":9007199254740992}`)},
		{"one point zero", callMsg("m-one", `{"n":1}`), callMsg("m-one", `{"n":1.0}`)},
		{"files", a2ashape.Message{ID: "m-file", Role: a2ashape.RoleUser, Parts: []a2ashape.Part{a2ashape.TextPart("look"),
			a2ashape.RawPart([]byte("first image"), "a.png", "image/png")}},
			a2ashape.Message{ID: "m-file", Role: a2ashape.RoleUser, Parts: []a2ashape.Part{a2ashape.TextPart("look"),
				a2ashape.RawPart([]byte("other image"), "a.png", "image/png")}}},
	}
	for _, c := range cases {
		first := mustSend(t, req, prov.AID(), c.first)
		n, sent := outboundCount(t, req), outboxSent(t, req)
		again, err := clientSend(t, req, prov.AID(), c.first)
		if err != nil || again.ID != first.ID || !deduplicated(again) {
			t.Fatalf("%s: the same message again = %s %v (deduplicated %v), want %s", c.name, again.ID, err, deduplicated(again), first.ID)
		}
		_, err = clientSend(t, req, prov.AID(), c.next)
		reusedAs(t, err, "open", first.ID)
		if outboundCount(t, req) != n || outboxSent(t, req) != sent {
			t.Fatalf("%s: a refused or repeated message wrote or sent something", c.name)
		}
		if deduplicated(first) {
			t.Fatalf("%s: a first attempt is marked deduplicated", c.name)
		}
	}
}

// outboxSent counts the messages req recorded on its outbound tasks.
func outboxSent(t *testing.T, d *Daemon) int {
	t.Helper()
	page, err := d.ix.ListPage(interactions.ListFilter{Role: interactions.RoleOutbound, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ix := range page.Items {
		n += countMsgs(t, d, ix.ID)
	}
	return n
}

// M4: a retry that asks for a preview, or no longer does, is the same
// message (one attempt streamed with the extension, one not).
func TestARetryIsTheSameWithOrWithoutAPreview(t *testing.T) {
	_, req, prov := registeredPair(t)
	m := textMsg("translate this", "ctx-p", "m-preview")
	m.Metadata = map[string]any{"anet.preview_mode": "on", "trace": "t"}
	first := mustSend(t, req, prov.AID(), m, previewExtensionURI)
	m.Metadata = map[string]any{"trace": "t"}
	again := mustSend(t, req, prov.AID(), m)
	if again.ID != first.ID || !deduplicated(again) || outboundCount(t, req) != 1 {
		t.Fatalf("retry without the preview = %s (deduplicated %v), %d tasks; want %s", again.ID, deduplicated(again),
			outboundCount(t, req), first.ID)
	}
}

// A follow-up under a messageId already used on its task: the same message
// is answered with the task, whatever its state; other content is refused
// with the follow-up sentence (CUX-D3, D7).
func TestAFollowUpUnderAUsedID(t *testing.T) {
	_, req, prov := registeredPair(t)
	task := mustSend(t, req, prov.AID(), textMsg("hello", "", "m-1"))
	f := mustSend(t, req, prov.AID(), followUp(task.ID, "and one more thing", "m-2"))
	n := countMsgs(t, req, task.ID)
	again := mustSend(t, req, prov.AID(), followUp(task.ID, "and one more thing", "m-2"))
	if again.ID != f.ID || !deduplicated(again) || countMsgs(t, req, task.ID) != n {
		t.Fatalf("the same follow-up again was recorded or not marked: %v, %d → %d messages", deduplicated(again), n, countMsgs(t, req, task.ID))
	}
	_, err := clientSend(t, req, prov.AID(), followUp(task.ID, "something else", "m-2"))
	reusedAs(t, err, "followup", task.ID)
	// The first message's id on the task is used too.
	_, err = clientSend(t, req, prov.AID(), followUp(task.ID, "not the goal", "m-1"))
	reusedAs(t, err, "followup", task.ID)
	// Once the task has ended the same follow-up is still that message, and
	// another is still refused: a follow-up names its task.
	if err := req.ix.Finish(task.ID, interactions.Finish{State: interactions.StateCompleted, Result: []byte("done"),
		Verified: interactions.VerificationUnknown}, interactions.CausePeerEnded); err != nil {
		t.Fatal(err)
	}
	if again := mustSend(t, req, prov.AID(), followUp(task.ID, "and one more thing", "m-2")); !deduplicated(again) {
		t.Fatal("the same follow-up on the ended task is not that message")
	}
	_, err = clientSend(t, req, prov.AID(), followUp(task.ID, "something else", "m-2"))
	reusedAs(t, err, "followup", task.ID)
	if n2 := countMsgs(t, req, task.ID); n2 != n {
		t.Fatalf("messages %d → %d", n, n2)
	}
}

// endTask ends ix in state st with anet.reason reason, at the store's clock.
func endTask(t *testing.T, d *Daemon, ix string, st interactions.State, reason string) {
	t.Helper()
	var meta []byte
	if reason != "" {
		meta, _ = json.Marshal(map[string]any{a2ashape.KeyReason: reason, a2ashape.KeyRetryAfterMS: 1000})
	}
	if err := d.ix.Finish(ix, interactions.Finish{State: st, Result: []byte("x"), Verified: interactions.VerificationUnknown,
		Meta: meta}, interactions.CausePeerEnded); err != nil {
		t.Fatal(err)
	}
}

// clockAt sets the store's clock (the one state_at is written on) to at.
func clockAt(d *Daemon, at *time.Time) { d.ix.SetClock(func() int64 { return at.UnixMilli() }) }

// sendCutOff sends m and goes away before the answer: the call's request
// ends while it waits for the task (CUX-D5).
func sendCutOff(t *testing.T, req *Daemon, prov string, m a2ashape.Message) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := req.TaskSeam().Send(ctx, prov, module.TaskSend{Message: m})
		done <- err
	}()
	var id string
	waitUntil(t, "the message is recorded", func() bool {
		c, err := req.ix.FindByClientKey(interactions.ClientKeyQuery{Key: clientKey(m.ID), PeerAID: prov})
		if err == nil {
			id = c.Interaction.ID
		}
		return err == nil
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a send cut off returned %v", err)
	}
	return id
}

// unanswered reports the stored mark of the message under m's id on ix.
func unanswered(t *testing.T, d *Daemon, ix, messageID string) bool {
	t.Helper()
	c, err := d.ix.FindByClientKey(interactions.ClientKeyQuery{Key: clientKey(messageID), TaskID: ix})
	if err != nil {
		t.Fatal(err)
	}
	return c.Unanswered
}

// SI-66, SI-67 (M5, M6, M7): once a task has ended, the same messageId with
// the same content is given that task only when the call that sent it was
// cut off before its answer, the task may have run, and it ended no more
// than 10 minutes before (counted from when it ended, not when it began);
// everything else is a new task (an internal note item).
func TestTheWindowAfterATaskEnds(t *testing.T) {
	_, req, prov := registeredPair(t)
	now := time.Now()
	clockAt(req, &now)

	// M5: answered, then completed: the same id again is a new task.
	m := textMsg("book the room", "", "m-answered")
	first := mustSend(t, req, prov.AID(), m)
	if unanswered(t, req, first.ID, m.ID) {
		t.Fatal("an answered call is marked unanswered")
	}
	endTask(t, req, first.ID, interactions.StateCompleted, "")
	if again := mustSend(t, req, prov.AID(), m); again.ID == first.ID || deduplicated(again) {
		t.Fatalf("an answered call's id after its task completed returned %s, want a new task", again.ID)
	}

	// M6: cut off, the task begun long ago and ended just now: the window
	// counts from the end.
	m = textMsg("book the other room", "", "m-cut")
	cut := sendCutOff(t, req, prov.AID(), m)
	if !unanswered(t, req, cut, m.ID) {
		t.Fatal("a call cut off before its answer is not marked")
	}
	now = now.Add(20 * time.Minute)
	endTask(t, req, cut, interactions.StateCompleted, "")
	now = now.Add(clientRetryWindow)
	again := mustSend(t, req, prov.AID(), m)
	if again.ID != cut || !deduplicated(again) {
		t.Fatalf("the retry %s after the task ended = %s (deduplicated %v), want %s",
			clientRetryWindow, again.ID, deduplicated(again), cut)
	}
	now = now.Add(time.Millisecond)
	if past := mustSend(t, req, prov.AID(), m); past.ID == cut {
		t.Fatal("a retry past the window was given the ended task")
	}

	// Other content under the cut-off id, inside the window: a new task.
	m = textMsg("first content", "", "m-cut-2")
	cut = sendCutOff(t, req, prov.AID(), m)
	endTask(t, req, cut, interactions.StateFailed, "backend_failed")
	if other := mustSend(t, req, prov.AID(), textMsg("second content", "", "m-cut-2")); other.ID == cut {
		t.Fatal("other content under a cut-off id was given its ended task")
	}
	m2 := textMsg("third content", "", "m-cut-3")
	cut = sendCutOff(t, req, prov.AID(), m2)
	endTask(t, req, cut, interactions.StateFailed, "backend_failed")
	if r := mustSend(t, req, prov.AID(), m2); r.ID != cut {
		t.Fatal("a failure that may have run is not in the window")
	}

	// M7: not run for certain — rejected (a quota with retry 1 s, retried
	// after 2 s), canceled, never delivered: a new task.
	for _, c := range []struct {
		st     interactions.State
		reason string
	}{
		{interactions.StateRejected, "quota_caller_per_min"},
		{interactions.StateCanceled, ""},
		{interactions.StateFailed, a2ashape.ReasonUndeliverable},
	} {
		m := textMsg("not run: "+string(c.st)+c.reason, "", "m-"+string(c.st)+c.reason)
		cut := sendCutOff(t, req, prov.AID(), m)
		endTask(t, req, cut, c.st, c.reason)
		now = now.Add(2 * time.Second)
		if again := mustSend(t, req, prov.AID(), m); again.ID == cut || deduplicated(again) {
			t.Fatalf("%s %s: the retry was given the task that did not run", c.st, c.reason)
		}
	}
}

// CUX-D5: a call is unanswered only when its request ended before the answer
// was ready — the client went away, or its own deadline passed. A call
// answered at once, or at the end of the control plane's wait bound with the
// task still running, is answered, and a client that goes away afterwards
// changes nothing. A retry cut off in its turn marks the message too.
func TestUnansweredOnlyBeforeReply(t *testing.T) {
	_, req, prov := registeredPair(t)
	seam := req.TaskSeam()

	// Answered at once; the request ends after.
	ctx, cancel := context.WithCancel(context.Background())
	m := textMsg("one", "", "m-answered")
	task, err := seam.Send(ctx, prov.AID(), module.TaskSend{Message: m, ReturnImmediately: true})
	cancel()
	if err != nil || unanswered(t, req, task.ID, m.ID) {
		t.Fatalf("an answered call: %v, unanswered %v", err, unanswered(t, req, task.ID, m.ID))
	}

	// Answered at the end of the wait bound, still running.
	m = textMsg("two", "", "m-bound")
	task, err = req.sendTask(context.Background(), controlScope, prov.AID(), module.TaskSend{Message: m}, 50*time.Millisecond)
	if err != nil || task.Status.State.Terminal() || unanswered(t, req, task.ID, m.ID) {
		t.Fatalf("a call answered at its bound: %v, %s, unanswered %v", err, task.Status.State, unanswered(t, req, task.ID, m.ID))
	}

	// Cut off by the client.
	m = textMsg("three", "", "m-gone")
	id := sendCutOff(t, req, prov.AID(), m)
	if !unanswered(t, req, id, m.ID) {
		t.Fatal("a call whose client went away is not marked")
	}

	// The client's own deadline.
	m = textMsg("four", "", "m-deadline")
	dctx, dcancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer dcancel()
	task, err = seam.Send(dctx, prov.AID(), module.TaskSend{Message: m})
	if err != nil || !unanswered(t, req, task.ID, m.ID) {
		t.Fatalf("a call past its client's deadline: %v, unanswered %v", err, unanswered(t, req, task.ID, m.ID))
	}

	// A retry of an answered message, cut off in its turn.
	m = textMsg("five", "", "m-retry")
	first := mustSend(t, req, prov.AID(), m)
	rctx, rcancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := seam.Send(rctx, prov.AID(), module.TaskSend{Message: m})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	rcancel()
	<-done
	if !unanswered(t, req, first.ID, m.ID) {
		t.Fatal("a retry cut off before its answer does not mark the message")
	}
}

// M41: the stored digest is an HMAC under the task's mac_key (core L36), not
// under its data key: the two derivations are not interchangeable.
func TestTheStoredDigestIsUnderTheMACKey(t *testing.T) {
	_, req, prov := registeredPair(t)
	m := callMsg("m-mac", `{"a":9007199254740993}`)
	task := mustSend(t, req, prov.AID(), m)
	c, err := req.ix.FindByClientKey(interactions.ClientKeyQuery{Key: clientKey(m.ID), TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	canon, err := clientCanon(contentOf(t, m, nil))
	if err != nil {
		t.Fatal(err)
	}
	mk, err := req.ix.MACKey(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hmac.Equal(c.Digest, clientDigest(mk, canon)) {
		t.Fatalf("stored digest %x, want HMAC(mac_key, canon) %x", c.Digest, clientDigest(mk, canon))
	}
	if raw := rawDataKey(t, req, task.ID); bytes.Equal(c.Digest, clientDigest(raw, canon)) {
		t.Fatal("the digest is keyed with the data key itself")
	}
	if !bytes.Contains(canon, []byte("9007199254740993")) {
		t.Fatalf("canon %s lost the literal", canon)
	}
}

// rawDataKey is task ix's data key as stored, read past the store.
func rawDataKey(t *testing.T, d *Daemon, ix string) []byte {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(d.layout.InteractionsDir(), "interactions.db")+"?_pragma=busy_timeout(15000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var k []byte
	if err := db.QueryRow(`SELECT key FROM ix_dek WHERE ix=?`, ix).Scan(&k); err != nil {
		t.Fatal(err)
	}
	return k
}

// CUX-D5: the unanswered mark is written under the message's send lock — the
// one its lookup took, with the context the client gave (none here, though
// the daemon minted one for the task) — so a retry's lookup sees the mark or
// comes after it.
func TestTheUnansweredMarkTakesTheSendLock(t *testing.T) {
	_, req, prov := registeredPair(t)
	m := textMsg("hold on", "", "m-lock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := req.TaskSeam().Send(ctx, prov.AID(), module.TaskSend{Message: m})
		done <- err
	}()
	var id string
	waitUntil(t, "the message is recorded", func() bool {
		c, err := req.ix.FindByClientKey(interactions.ClientKeyQuery{Key: clientKey(m.ID), PeerAID: prov.AID()})
		if err == nil {
			id = c.Interaction.ID
		}
		return err == nil
	})
	time.Sleep(50 * time.Millisecond) // the send has let go of the lock and waits
	unlock := lockSend(req.AID()+"\x00"+prov.AID(), "", m.ID)
	cancel()
	time.Sleep(200 * time.Millisecond)
	marked := unanswered(t, req, id, m.ID)
	unlock()
	if err := <-done; err == nil {
		t.Fatal("the cut-off send returned no error")
	}
	if marked {
		t.Fatal("the mark was written while another holder had the message's send lock")
	}
	if !unanswered(t, req, id, m.ID) {
		t.Fatal("no mark once the lock was free")
	}
}

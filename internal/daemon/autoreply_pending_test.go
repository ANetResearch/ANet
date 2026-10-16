//go:build mod4_pending

package daemon

// Tests that drive the auto-reply module through its unexported API or through the daemon fixtures of the
// old package. They are not compiled until they are ported to module/autoreply (see the MOD-4 notes).

// from wire_edges_test.go
// A2A-DESIGN §5.1: the deny list is read on every auto-reply turn whatever
// the backend. The OpenAI-compatible backend does not answer a peer denied
// since its message landed, before any revocation sweep.
func TestTheOpenAIBackendReadsTheDenyListEveryTurn(t *testing.T) {
	api := &fakeOpenAI{reply: "hello"}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "first question", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times for an allowed peer, want 1", n)
	}
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.req.SendMessage(f.ctx, id, "second question", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	denyPeers(t, f.prov, f.req.AID()) // an edit outside the CLI; no sweep has run
	f.prov.autoReplyOnce(f.ctx, f.cfg, f.replier)
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times after the deny, want still 1", n)
	}
}

func (r denyDuring) Reply(ctx context.Context, rc replyContext, turns []chatTurn) (string, error) {
	r.deny()
	return r.inner.Reply(ctx, rc, turns)
}

// §5.1: a peer denied while the backend was producing its reply is not
// sent that reply.
func TestAReplyIsNotSentToAPeerDeniedWhileTheBackendWorked(t *testing.T) {
	api := &fakeOpenAI{reply: "hello"}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "a question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	replier := denyDuring{inner: f.replier, deny: func() { denyPeers(t, f.prov, f.req.AID()) }}
	f.prov.autoReplyOnce(f.ctx, f.cfg, replier)
	if n := api.calls.Load(); n != 1 {
		t.Fatalf("backend called %d times, want 1", n)
	}
	msgs, err := f.prov.ix.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.SenderAID == f.prov.AID() {
			t.Fatalf("a reply went to the peer denied during the call: %+v", m)
		}
	}
}

// from inbound_tasks_test.go
// A2A-DESIGN §11.6 and an internal note item, on the daemon's side: only a trusted
// peer's text task reaches a module's backend — a peer on the allow list
// alone, and a stranger under policy open, do not; a task the backend has
// is not also given to the auto-reply agent, while the others still are;
// the backend sees one context per (peer, context), so two peers naming the
// same context never share a conversation; and a task comes again with the
// requester's next message, in the same context.
func TestInboundTasksReachTheBackendOnlyFromTrustedPeers(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	peerA := registered(t, srv.URL, "a")
	peerB := registered(t, srv.URL, "b")
	allowed := registered(t, srv.URL, "allowed")
	stranger := registered(t, srv.URL, "stranger")
	trustPeers(t, prov, peerA.AID(), peerB.AID())
	allowPeers(t, prov, allowed.AID())
	if err := prov.SetInboundPolicy(PolicyOpen); err != nil {
		t.Fatal(err)
	}
	api := &fakeOpenAI{reply: "an auto-reply"}
	apiSrv := httptest.NewServer(api.handler())
	defer apiSrv.Close()
	arCfg := AutoReplyConfig{Model: "m", APIBase: apiSrv.URL + "/v1"}
	replier, err := newAutoReplier(arCfg, testEnv(prov.layout))
	if err != nil {
		t.Fatal(err)
	}

	fctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	host := a2aHost(prov)
	tasks, err := host.InboundTasks(fctx)
	if err != nil {
		t.Fatal(err)
	}

	delegate := func(from *Daemon, goal, contextID string) string {
		t.Helper()
		id, err := from.DelegateIn(ctx, prov.AID(), goal, nil, contextID)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a1 := delegate(peerA, "a first", "shared")
	a2 := delegate(peerA, "ask a second", "shared")
	b1 := delegate(peerB, "b first", "shared")
	al := delegate(allowed, "from an allowed peer", "shared")
	st := delegate(stranger, "from a stranger", "shared")
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a1, a2, b1, al, st} {
		if _, err := prov.ix.Get(id); err != nil {
			t.Fatalf("%s was not accepted: %v", id, err)
		}
	}

	// The auto-reply agent answers the two tasks no backend is given, and
	// leaves the three a backend is, although nothing has answered them yet.
	prov.autoReplyOnce(ctx, arCfg, replier)
	if n := api.calls.Load(); n != 2 {
		t.Fatalf("auto-reply called %d times, want 2 (allowed peer, stranger)", n)
	}
	for _, id := range []string{al, st} {
		if got := lastFromMe(t, prov, id); got != "an auto-reply" {
			t.Fatalf("%s: last answer %q", id, got)
		}
	}
	for _, id := range []string{a1, a2, b1} {
		if got := lastFromMe(t, prov, id); got != "" {
			t.Fatalf("auto-reply answered %s, a task the backend has: %q", id, got)
		}
	}

	backend := newFakeA2ABackend(t)
	errs := forwardInbound(t, fctx, host, tasks, backend)
	waitUntil(t, "the three trusted tasks to be answered", func() bool {
		return stateOf(t, prov, a1) == interactions.StateCompleted && stateOf(t, prov, b1) == interactions.StateCompleted &&
			stateOf(t, prov, a2) == interactions.StateInputRequired
	})
	if len(*errs) != 0 {
		t.Fatalf("forwarding: %v", *errs)
	}
	if got := lastFromMe(t, prov, a1); got != "backend: a first" {
		t.Fatalf("a1 answered %q", got)
	}
	calls := backend.snapshot()
	if len(calls) != 3 {
		t.Fatalf("the backend received %d tasks: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if strings.Contains(c.text, "allowed") || strings.Contains(c.text, "stranger") {
			t.Fatalf("an untrusted peer's task reached the backend: %q", c.text)
		}
		if c.meta[a2ashape.KeyTrusted] != true {
			t.Fatalf("%q: anet.trusted = %v", c.text, c.meta[a2ashape.KeyTrusted])
		}
	}
	ca1, ca2, cb1 := backend.byText(t, "a first"), backend.byText(t, "ask a second"), backend.byText(t, "b first")
	if ca1.meta[a2ashape.KeyPeerAID] != peerA.AID() || cb1.meta[a2ashape.KeyPeerAID] != peerB.AID() {
		t.Fatalf("anet.peer_aid: %v, %v", ca1.meta[a2ashape.KeyPeerAID], cb1.meta[a2ashape.KeyPeerAID])
	}
	if ca1.contextID != ca2.contextID || ca1.contextID == cb1.contextID || ca1.contextID == "shared" || cb1.contextID == "shared" {
		t.Fatalf("backend contexts: a %s, %s; b %s — want one per peer, not the requester's", ca1.contextID, ca2.contextID, cb1.contextID)
	}

	// The requester reads the question and answers it: the task comes
	// again, in the same context, and the auto-reply agent still leaves it
	// alone.
	if err := peerA.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := peerA.SendMessage(ctx, a2, "ask no more: the red one", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	prov.autoReplyOnce(ctx, arCfg, replier)
	if n := api.calls.Load(); n != 2 {
		t.Fatalf("auto-reply called for a follow-up the backend has (%d calls)", n)
	}
	waitUntil(t, "the follow-up to reach the backend", func() bool { return len(backend.snapshot()) == 4 })
	if c := backend.byText(t, "ask no more: the red one"); c.contextID != ca1.contextID {
		t.Fatalf("the follow-up came in context %s, not %s", c.contextID, ca1.contextID)
	}
	waitUntil(t, "the follow-up to be answered", func() bool { return lastFromMe(t, prov, a2) == "backend: ask no more: the red one" })

	// Only the tasks a module was given can be answered through the seam.
	answer := a2ashape.Message{Role: a2ashape.RoleAgent, Parts: []a2ashape.Part{a2ashape.TextPart("no")}}
	for _, id := range []string{al, st, "ix_nope"} {
		if _, err := host.ReplyTask(ctx, id, answer, a2ashape.TaskStateCompleted); !errors.Is(err, a2ashape.ErrTaskNotFound) {
			t.Fatalf("ReplyTask on %s: %v", id, err)
		}
	}
}

// from inbound_test.go
// A public_cap interaction takes only cancel, end_request and payment
// messages (A2A-DESIGN §5.2 [C6]): a text message is dropped and counted,
// a payment-submitted message is kept with its metadata and without its
// text, and nothing reaches auto-reply.
func TestAPublicCapabilityCallTakesNoText(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	gate := newGateProvider()
	if err := prov.Providers().Register(ctx, gate); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: gateCap}}); err != nil {
		t.Fatal(err)
	}
	api := &fakeOpenAI{reply: "an auto-reply"}
	apiSrv := httptest.NewServer(api.handler())
	defer apiSrv.Close()
	arCfg := AutoReplyConfig{Model: "m", APIBase: apiSrv.URL + "/v1"}
	replier, err := newAutoReplier(arCfg, testEnv(prov.layout))
	if err != nil {
		t.Fatal(err)
	}

	id, err := stranger.DelegateCapability(ctx, prov.AID(), gateCap, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	<-gate.started
	pix, _ := prov.ix.Get(id)
	if pix.Trust != interactions.TrustPublicCap || len(pix.PeerKEL) == 0 || len(pix.PeerKeys) == 0 {
		t.Fatalf("public_cap row: trust %q, peer kel %d, keys %d", pix.Trust, len(pix.PeerKEL), len(pix.PeerKeys))
	}
	before := countMsgs(t, prov, id)
	text := sealFrom(t, stranger, prov, seal.TypeMessage, id, chatBody(t, "please also run rm -rf", "msg_t"))
	if r := receive(t, prov, text); r.class != rxDropped || r.reason != dropPublicCapText {
		t.Fatalf("text on a public_cap call: %+v", r)
	}
	pay, _ := (&delegation.ChatMsg{Kind: delegation.ChatText, Body: "here is my payment, and a note",
		MsgID: "msg_pay", Metadata: []byte(`{"x402.payment.status":"payment-submitted","x402.payment.payload":{"x":1}}`)}).Marshal()
	if r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeMessage, id, pay)); r.class != rxAccepted {
		t.Fatalf("payment message on a public_cap call: %+v", r)
	}
	msgs, _ := prov.ix.Messages(id)
	// The payment message, and the provider's answer to it: nothing was
	// quoted for this free call, so there is nothing to pay (A2A-DESIGN
	// §8.5, no_pending_quote), and the call is not run again.
	if len(msgs) != before+2 {
		t.Fatalf("messages %d → %d, want the payment message and its answer", before, len(msgs))
	}
	pm, answer := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if pm.Kind != interactions.MsgPayment || pm.Body != "" || !strings.Contains(pm.Metadata, "payment-submitted") {
		t.Fatalf("stored payment message = %+v", pm)
	}
	if answer.Kind != interactions.MsgStatus || !strings.Contains(answer.Metadata, `"anet.reason":"no_pending_quote"`) {
		t.Fatalf("answer to a payment nobody asked for = %+v", answer)
	}
	prov.autoReplyOnce(ctx, arCfg, replier)
	if n := api.calls.Load(); n != 0 {
		t.Fatalf("auto-reply called %d times for a public capability call", n)
	}
	if ths, _ := prov.ActiveThreads(); len(ths) != 0 {
		t.Fatalf("a public_cap interaction is in the auto-reply set: %+v", ths)
	}
	// A cancel reaches the running call.
	cancelMsg, _ := (&delegation.ChatMsg{Kind: delegation.KindCancel, MsgID: "msg_c"}).Marshal()
	if r := receive(t, prov, sealFrom(t, stranger, prov, seal.TypeMessage, id, cancelMsg)); r.class != rxAccepted {
		t.Fatalf("cancel: %+v", r)
	}
	waitUntil(t, "the running call to be canceled", gate.wasCanceled)
	waitUntil(t, "the public_cap interaction to end", func() bool {
		pix, _ := prov.ix.Get(id)
		return pix.IsTerminal()
	})
	// Its answers to the caller are queued: the caller's keys are kept, as
	// they may have to be sealed again (engine ENG-S7).
	if pix, _ := prov.ix.Get(id); len(outboxRows(t, prov, id)) == 0 || len(pix.PeerKEL) == 0 || len(pix.PeerKeys) == 0 {
		t.Fatalf("ended with its answers queued, the caller's keys: kel %d, keys %d", len(pix.PeerKEL), len(pix.PeerKeys))
	}
	// The caller takes them, and the provider hears so: the keys go with
	// the last row.
	waitUntil(t, "the caller's answers to be confirmed and its keys dropped", func() bool {
		settleDelivery(t, prov, stranger)
		pix, _ := prov.ix.Get(id)
		return len(outboxRows(t, prov, id)) == 0 && len(pix.PeerKEL) == 0 && len(pix.PeerKeys) == 0
	})
	if _, err := prov.ix.PeerIdentity(stranger.AID()); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("a public_cap caller was recorded in peer_identity")
	}
}

// from modules_mod1_test.go
// K-5: the sensitive paths hold the kernel's fixed part, the default C1
// socket whatever the availability of anetlink, and the paths available
// modules declare.
func TestSensitivePaths(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, without := range [][]string{nil, {"anetlink"}} {
		d := startWith(t, map[string]any{"without": without}, nil)
		d.modules = append(d.modules, fakeLocalPaths{"/var/run/svc-test.sock"})
		got := d.sensitivePaths()
		for _, want := range []string{d.layout.Root, d.layout.ControlTokenPath(), "/var/run/svc-test.sock"} {
			if !slices.Contains(got, want) {
				t.Errorf("without=%v: %q missing from %v", without, want, got)
			}
		}
		// The sandbox's list holds the default C1 socket whether or not the
		// anetlink module is available (TestNeverBindIndependentOfModules).
		if never := d.sandboxNeverBind(); !slices.ContainsFunc(never, func(p string) bool { return strings.HasSuffix(p, "anetlink/c1.sock") }) {
			t.Errorf("without=%v: the sandbox never-bind list lacks the default C1 socket: %v", without, never)
		}
		if s := os.Getenv("XDG_RUNTIME_DIR"); !slices.Contains(got, s) {
			t.Errorf("without=%v: %q missing", without, s)
		}
		found := false
		for _, p := range got {
			if strings.HasSuffix(p, "anetlink/c1.sock") {
				found = true
			}
		}
		if !found {
			t.Errorf("without=%v: the default C1 socket is not in the list: %v", without, got)
		}
	}
}

// from preview_source_test.go
// CUX-P5: the streamed and the non-streamed answer are the same bytes,
// and the accumulated text and reasoning are what the coalescer is offered.
func TestStreamedAnswerEqualsTheWholeOne(t *testing.T) {
	for _, f := range sseFixtures {
		t.Run(f.name, func(t *testing.T) {
			var offers [][2]string
			got, err := readChatStream(strings.NewReader(f.stream), func(text, reasoning string) {
				offers = append(offers, [2]string{text, reasoning})
			})
			if err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSpace(f.whole)
			if got != want {
				t.Fatalf("streamed %q, whole %q", got, want)
			}
			a, _ := stripDoneSentinel(got)
			b, _ := stripDoneSentinel(want)
			if a != b {
				t.Fatalf("after the sentinel: %q and %q", a, b)
			}
			if len(offers) == 0 || offers[len(offers)-1][0] != f.whole && strings.TrimSpace(offers[len(offers)-1][0]) != want {
				t.Fatalf("the last offer is not the whole text: %q", offers)
			}
		})
	}
}

func TestOpenAIReplierStreamsOnlyWhenAskedAndAgrees(t *testing.T) {
	for _, f := range sseFixtures {
		t.Run(f.name, func(t *testing.T) {
			srv, plain, streamed := openAIStreamServer(t, f.stream, f.whole, 0)
			r := &openAIReplier{cfg: AutoReplyConfig{APIBase: srv.URL, Model: "m"}}
			turns := []chatTurn{{Role: "user", Text: "hi"}}
			a, err := r.Reply(context.Background(), replyContext{}, turns)
			if err != nil || plain.Load() != 1 || streamed.Load() != 0 {
				t.Fatalf("plain: %q %v (%d plain, %d streamed)", a, err, plain.Load(), streamed.Load())
			}
			var last string
			b, err := r.Reply(context.Background(), replyContext{Preview: func(text, _ string) { last = text }}, turns)
			if err != nil || streamed.Load() != 1 {
				t.Fatalf("streamed: %q %v", b, err)
			}
			if a != b {
				t.Fatalf("plain %q, streamed %q: not the same bytes", a, b)
			}
			if strings.TrimSpace(last) != b {
				t.Fatalf("the last offer %q is not the answer %q", last, b)
			}
		})
	}
}

// CUX-P5, TestStreamFailureNoRecall: a failed stream is one failed call.
// There is no second call without stream, and the requester gets
// backend_failed.
func TestStreamFailureNoRecall(t *testing.T) {
	for name, status := range map[string]int{"HTTP error": 500, "broken stream": 0} {
		t.Run(name, func(t *testing.T) {
			// A stream that ends before it finished.
			srv, plain, streamed := openAIStreamServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n", "whole", status)
			f := newAutoReplyFixture(t, AutoReplyConfig{Model: "m"}, &fakeOpenAI{})
			cfg := f.cfg
			cfg.APIBase = srv.URL
			f.cfg = cfg
			f.replier = &openAIReplier{cfg: cfg}
			if _, err := f.prov.SetStreamPreview(streamPreviewOn); err != nil {
				t.Fatal(err)
			}
			id := previewDelegation(t, f.req, f.prov, previewModeOn)
			f.tick(t)
			f.tick(t)
			if plain.Load() != 0 || streamed.Load() > 1 {
				t.Fatalf("%d plain calls, %d streamed: the failed stream was called again", plain.Load(), streamed.Load())
			}
			if err := f.req.pollOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			ix, err := f.req.ix.Get(id)
			if err != nil || ix.State != interactions.StateInputRequired {
				t.Fatalf("%+v %v", ix, err)
			}
		})
	}
}

// A round that asked for previews is answered by a streamed call, and the
// answer sent is the one the non-streamed call gives.
func TestAutoReplyStreamsAPreviewedRound(t *testing.T) {
	f0 := sseFixtures[0]
	srv, plain, streamed := openAIStreamServer(t, f0.stream, f0.whole, 0)
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "m"}, &fakeOpenAI{})
	cfg := f.cfg
	cfg.APIBase = srv.URL
	f.cfg, f.replier = cfg, &openAIReplier{cfg: cfg}
	if _, err := f.prov.SetStreamPreview(streamPreviewWR); err != nil {
		t.Fatal(err)
	}
	clk := newPreviewClock()
	co := manualPreviews(f.prov, clk)

	// A round that asked: one streamed call, frames offered, the answer as the plain one.
	id := previewDelegation(t, f.req, f.prov, previewModeWithReasoning)
	f.tick(t)
	if streamed.Load() != 1 || plain.Load() != 0 {
		t.Fatalf("%d streamed, %d plain", streamed.Load(), plain.Load())
	}
	if got := f.lastPeerMsg(t, id); got.Body != strings.TrimSpace(f0.whole) {
		t.Fatalf("answer %q", got.Body)
	}
	_ = co
	// A round that asked for nothing: the call is as it ever was.
	id2 := previewDelegation(t, f.req, f.prov, "")
	f.tick(t)
	if streamed.Load() != 1 || plain.Load() != 1 {
		t.Fatalf("%d streamed, %d plain after a round that asked for nothing", streamed.Load(), plain.Load())
	}
	_ = id2
	// Previews off on this node: plain again, whatever the round asked.
	if _, err := f.prov.SetStreamPreview(streamPreviewOff); err != nil {
		t.Fatal(err)
	}
	previewDelegation(t, f.req, f.prov, previewModeOn)
	f.tick(t)
	if streamed.Load() != 1 || plain.Load() != 2 {
		t.Fatalf("%d streamed, %d plain with previews off", streamed.Load(), plain.Load())
	}
}

func FuzzReadChatStream(f *testing.F) {
	for _, s := range sseFixtures {
		f.Add(s.stream)
	}
	f.Add("data: [DONE]\n")
	f.Add("data: {\"choices\":[{\"delta\":{\"content\":\"\\ud800\"},\"finish_reason\":\"x\"}]}\n")
	f.Fuzz(func(t *testing.T, s string) {
		var buf bytes.Buffer
		out, err := readChatStream(strings.NewReader(s), func(text, reasoning string) { buf.Reset() })
		if err == nil && strings.TrimSpace(out) == "" {
			t.Fatalf("an empty answer is no error: %q", s)
		}
	})
}

// from reply_result_test.go
// An auto-reply that judges the task done completes it with its reply in
// the result; a reply that does not complete the task is an ordinary turn.
func TestAnAutoReplyThatCompletesFoldsItsReplyIntoTheResult(t *testing.T) {
	api := &fakeOpenAI{reply: "here is your function: def f(): pass\n" + autoReplyDoneSentinel}
	f := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "write f()", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	pix := mustIX(t, f.prov, id)
	if pix.State != interactions.StateCompleted {
		t.Fatalf("the provider's task is %s after the done reply", pix.State)
	}
	rows, err := f.prov.ix.OutboxRows(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Type == seal.TypeMessage {
			t.Fatalf("the done reply went as a message of its own: %+v", r)
		}
	}
	if err := f.req.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	task, err := f.req.taskView(mustIX(t, f.req, id), viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != a2ashape.TaskStateCompleted || replyArtifact(task) != "here is your function: def f(): pass" {
		t.Fatalf("state %s, reply %q", task.Status.State, replyArtifact(task))
	}
	noHistoryState(t, task)

	api2 := &fakeOpenAI{reply: "which language?"}
	g := newAutoReplyFixture(t, AutoReplyConfig{Model: "test"}, api2)
	id2, err := g.req.Delegate(g.ctx, g.prov.AID(), "write f()", nil)
	if err != nil {
		t.Fatal(err)
	}
	g.tick(t)
	if ix := mustIX(t, g.prov, id2); ix.IsTerminal() {
		t.Fatalf("a question completed the task: %s", ix.State)
	}
	if m := msgWithBody(t, g.prov, id2, "which language?"); m == nil || m.Metadata != "" {
		t.Fatalf("the question: %+v", m)
	}
}

// from turnhost_test.go
// MOD-3 (2), TestConfigRewritePreservesModuleBlocks: a write the kernel makes of config.json (a policy
// write) carries the auto_reply block and `without` byte for byte, and so does the module's own write the
// other keys.
func TestConfigRewritePreservesModuleBlocks(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": []string{"x402"},
		"auto_reply": map[string]any{"backend": "openai", "api_base": "http://127.0.0.1:1/v1", "model": "m", "api_key": "k"}})
	d, err := New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	read := func() map[string]json.RawMessage {
		b, err := os.ReadFile(filepath.Join(root, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	before := read()
	if err := d.SetInboundPolicy(PolicyApprove); err != nil {
		t.Fatal(err)
	}
	after := read()
	for _, k := range []string{"auto_reply", "without"} {
		if !bytes.Equal(before[k], after[k]) {
			t.Errorf("%s changed under a policy write:\n%s\n%s", k, before[k], after[k])
		}
	}
	// The module's write replaces only auto_reply.
	if _, err := d.ar().apply(func(cur *AutoReplyConfig) (*AutoReplyConfig, error) {
		c := *cur
		c.Model = "m2"
		return &c, nil
	}); err != nil {
		t.Fatal(err)
	}
	final := read()
	if !bytes.Equal(after["without"], final["without"]) || !bytes.Equal(after["inbound"], final["inbound"]) {
		t.Errorf("the module's write changed another key")
	}
	if !strings.Contains(string(final["auto_reply"]), `"m2"`) {
		t.Errorf("auto_reply = %s", final["auto_reply"])
	}
}

// MOD §7.1, TestOnlyAutoReplyAssertsTurnHost: no file of the kernel asserts a Host to module.TurnHost; the
// module does, and the grants are what hands the interface to it alone.
func TestOnlyAutoReplyAssertsTurnHost(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	exec := map[string]bool{}
	for _, f := range execFiles {
		exec[f] = true
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ta, ok := n.(*ast.TypeAssertExpr)
			if !ok || ta.Type == nil {
				return true
			}
			if se, ok := ta.Type.(*ast.SelectorExpr); ok && se.Sel.Name == "TurnHost" && !exec[name] {
				t.Errorf("%s asserts module.TurnHost; only the auto-reply module may", name)
			}
			return true
		})
	}
}

// MOD-3 (3), TestRetentionPrunesExecDirsInEveryBuild: with auto-reply turned off the work directories of
// ended tasks and the session bindings are still removed by the maintenance period.
func TestRetentionPrunesExecDirsInEveryBuild(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	d := startWith(t, map[string]any{"without": []string{"autoreply"}}, nil)
	if d.ar() != nil {
		t.Fatal("the auto-reply module runs although it is in without")
	}
	aid := d.AID()
	direct := filepath.Join(execwork.WorkRoot(), execwork.AidShort(aid), execwork.IxDirName("ix-gone"))
	sandboxed := filepath.Join(execwork.SandboxWorkRoot(), execwork.AidShort(aid), execwork.IxDirName("ix-gone"))
	for _, p := range []string{direct, sandboxed} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sess := execwork.Sessions(anethome.ModuleDir(d.layout.Root, "autoreply"))
	sess.Set("ix-gone", "s")
	d.pruneEndedWorkDirs()
	for _, p := range []string{direct, sandboxed} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was not removed (%v)", p, err)
		}
	}
	if sess.Get("ix-gone") != "" {
		t.Error("the session binding of an ended task stayed")
	}
	if _, err := os.Stat(filepath.Join(d.layout.Root, "exec_sessions.json")); err == nil {
		t.Error("the old session file exists")
	}
}

// MOD-3 (4): without the module nothing answers: its routes say module_absent, /status has no auto_reply
// member, no loop runs, and the task stays in the inbox under each inbound policy.
func TestWithoutAutoReplyIsAbsent(t *testing.T) {
	d := startWith(t, map[string]any{"without": []string{"autoreply"}}, nil)
	if d.ar() != nil {
		t.Fatal("module started")
	}
	for _, route := range []string{"POST /autoreply", "POST /autoreply-test"} {
		rec := httptest.NewRecorder()
		h := d.moduleRouteGuard(route, d.moduleRoute("autoreply", route))
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader(`{"off":true}`)))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 409 || body["reason"] != "module_absent" && !strings.Contains(rec.Body.String(), "module_absent") {
			t.Errorf("%s: %d %s", route, rec.Code, rec.Body)
		}
	}
	st := statusJSON(t, d)
	if _, ok := st["auto_reply"]; ok {
		t.Error("/status has an auto_reply member")
	}
	if got, _ := st["build"].(map[string]any)["without"].([]any); len(got) != 1 || got[0] != "autoreply" {
		t.Errorf("/status.build.without = %v", got)
	}
	// A block with the module turned off is the module_absent.configured refusal at start.
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": []string{"autoreply"},
		"auto_reply": map[string]any{"backend": "openai", "api_base": "http://127.0.0.1:1/v1", "model": "m"}})
	_, err := New(NewLayout(root))
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Key != "module_absent.configured" || ref.Args["key"] != "auto_reply" {
		t.Errorf("New = %v, want module_absent.configured for auto_reply", err)
	}
}

// MOD-3 (4), TestNoAutoReplyLeavesTasksInInbox: with the module turned off a delegated task is kept and
// nothing is sent, whatever the inbound policy, and no local program is started (a canary on PATH).
func TestNoAutoReplyLeavesTasksInInbox(t *testing.T) {
	canary := filepath.Join(t.TempDir(), "claude")
	mark := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(canary, []byte("#!/bin/sh\ntouch "+mark+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(canary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, policy := range []string{PolicyOpen, PolicyApprove, PolicyClosed} {
		srv := newFakeHub(t)
		ctx := context.Background()
		prov := registered(t, srv.URL, "prov")
		req := registered(t, srv.URL, "req")
		allowPeers(t, prov, req.AID())
		trustPeers(t, prov, req.AID())
		if err := prov.SetInboundPolicy(policy); err != nil {
			t.Fatal(err)
		}
		prov = restartWithout(t, prov, "autoreply")
		if prov.ar() != nil {
			t.Fatal("the module started although it is in without")
		}
		id, err := req.Delegate(ctx, prov.AID(), "please run", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = prov.pollOnce(ctx)
		prov.kickTurns()
		time.Sleep(100 * time.Millisecond)
		if got := lastFromMe(t, prov, id); got != "" {
			t.Errorf("policy %s: the node answered %q", policy, got)
		}
		if st := stateOf(t, prov, id); policy != PolicyClosed && st != interactions.StateSubmitted {
			t.Errorf("policy %s: state %s", policy, st)
		}
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a local program was started")
	}
}

// MOD-3 (6), TestDarwinPendingFailsClosed: with the macOS sandbox not part of the build
// (seatbeltEnabled false, stood in for on this host), a turn that would run in the sandbox is turned away
// before the agent starts with sandbox_unavailable and the local reason platform_unsupported.pending; the
// direct and openai paths are not touched; and the constant is true.
func TestDarwinPendingFailsClosed(t *testing.T) {
	if !seatbeltEnabled {
		t.Fatal("seatbeltEnabled must default to true")
	}
	prev := sandboxPrepareForTest
	sandboxPrepareForTest = func(*sandboxPlan) error { return seatbeltGate(false) }
	t.Cleanup(func() { sandboxPrepareForTest = prev })
	srv, req, prov := quietPair(t)
	_ = srv
	allowPeers(t, prov, req.AID())
	cfg := AutoReplyConfig{Backend: "exec", Agent: "claude", Untrusted: UntrustedSandbox,
		APIKey: "k", APIBase: "https://api.anthropic.com"}
	if err := prov.SetAutoReply(&cfg); err != nil {
		t.Fatal(err)
	}
	prov.ar().haltLoop()
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "needs the sandbox", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	replier, err := newAutoReplier(cfg, testEnv(prov.layout))
	if err != nil {
		t.Fatal(err)
	}
	prov.autoReplyOnce(ctx, cfg, replier)
	state, meta := lastStatusMeta(t, req, id)
	if state != interactions.StateRejected || meta[a2ashape.KeyReason] != reasonSandboxUnavailble {
		t.Fatalf("state %s, meta %v", state, meta)
	}
	// The local reason is the one a status or doctor says.
	err = seatbeltGate(false)
	var pe *sandboxProbeError
	if !errors.As(err, &pe) || pe.Code != "platform_unsupported.pending" || !errors.Is(err, errSandboxUnavailable) {
		t.Fatalf("gate = %v", err)
	}
	if s := a2ashape.Sentence(pe.Code, a2ashape.English, nil); !strings.Contains(s, "v0.3.x") {
		t.Errorf("sentence %q", s)
	}
	// The openai backend and a direct run are not affected by the switch.
	if g := gateFor(AutoReplyConfig{Backend: "exec", Agent: "claude"}, true); g.mode != modeDirect {
		t.Errorf("a trusted peer's turn: %+v", g)
	}
	if g := gateFor(AutoReplyConfig{Model: "m", APIBase: "http://x"}, false); g.mode != modeOpenAI {
		t.Errorf("openai turn: %+v", g)
	}
}

// MOD-3 (3), TestModuleStopWaitsForRuns: Stop returns only when the turns under way have ended and
// recorded, which is before the evidence ledger closes, and no run starts after it.
func TestModuleStopWaitsForRuns(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once, runs atomic.Int64
	sandboxRunnerForTest = func(ctx context.Context, req sandboxRequest, workDir string) (string, *egressSummary, error) {
		runs.Add(1)
		if once.Add(1) == 1 {
			close(started)
		}
		<-release
		return "a late answer", &egressSummary{}, nil
	}
	t.Cleanup(func() { sandboxRunnerForTest = nil })
	f := newAutoReplyFixture(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox}, nil)
	cfg := f.cfg
	if err := f.prov.SetAutoReply(&cfg); err != nil { // the real loop
		t.Fatal(err)
	}
	if _, err := f.req.Delegate(f.ctx, f.prov.AID(), "a long task", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.prov.kickTurns()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not start")
	}
	done := make(chan struct{})
	go func() { _ = f.prov.ar().Stop(context.Background()); close(done) }()
	select {
	case <-done:
		t.Fatal("Stop returned while a run was under way")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the run ended")
	}
	if got := withExit(invoked(t, f.prov), exitOK); len(got) != 1 {
		t.Fatalf("the run was not recorded before Stop returned: %v", invoked(t, f.prov))
	}
	// Nothing starts after Stop.
	if _, err := f.req.Delegate(f.ctx, f.prov.AID(), "a second task", nil); err != nil {
		t.Fatal(err)
	}
	_ = f.prov.pollOnce(f.ctx)
	f.prov.kickTurns()
	time.Sleep(200 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d runs; one started after Stop", n)
	}
	if err := f.prov.ar().startLoop(cfg); err != nil {
		t.Fatal(err)
	}
	f.prov.kickTurns()
	time.Sleep(200 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("a stopped module started a loop again (%d runs)", n)
	}
}

// from x402task_more_test.go
// §17 C6/C29: both sides run the exec auto-reply backend and trust each
// other; a quoted, paid, completed capability call never reaches it.
func TestExecAutoReplyOnBothSidesStaysOutOfAPaidCall(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	stub := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho called >> "+calls+"\necho a reply\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	setExecCommand(t, stub)
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	trustPeers(t, prov, req.AID())
	trustPeers(t, req, prov.AID())
	cfg := AutoReplyConfig{Backend: "exec", Agent: "cursor"}
	ctx := context.Background()
	repliers := map[*Daemon]autoReplier{}
	for _, d := range []*Daemon{req, prov} {
		r, err := newAutoReplier(cfg, testEnv(d.layout))
		if err != nil {
			t.Fatal(err)
		}
		repliers[d] = r
	}
	step := func(ds ...*Daemon) {
		for _, d := range ds {
			poll(t, d)
			d.autoReplyOnce(ctx, cfg, repliers[d])
		}
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	step(prov, req, prov, req, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted || work.invoked.Load() != 1 {
		t.Fatalf("the paid call: %s, ran %d", rix.State, work.invoked.Load())
	}
	if b, err := os.ReadFile(calls); err == nil && len(b) > 0 {
		t.Fatalf("the exec backend ran %d times during a paid call", strings.Count(string(b), "\n"))
	}
}

// from wire_edges_test.go
// denyDuring is a replier through which the peer is denied while the
// backend works.
type denyDuring struct {
	inner autoReplier
	deny  func()
}

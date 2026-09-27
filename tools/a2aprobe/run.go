package main

// run.go is the scenario (`a2aprobe run`) and the restart check
// (`a2aprobe again`): what a client of the local A2A interface does with a
// remote agent, each step checked against A2A-DESIGN §11 and §8.7.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// runState is what run leaves for again and for the script: the contexts
// the client named and the tasks it made.
type runState struct {
	Agent     string            `json:"agent"`
	Nonce     string            `json:"nonce"`
	Contexts  map[string]string `json:"contexts"`  // binding → the contextId the client gave
	Blocking  map[string]string `json:"blocking"`  // binding → task of the blocking send
	Immediate map[string]string `json:"immediate"` // binding → task of the returnImmediately send
	Stream    map[string]string `json:"stream"`    // binding → task of the streamed send
	Cancel    string            `json:"cancel_task,omitempty"`
	Paid      string            `json:"paid_task,omitempty"`
	PaidText  string            `json:"paid_text,omitempty"`
	Over      string            `json:"over_task,omitempty"`
}

// probe is one run against one remote agent.
type probe struct {
	rep     *report
	agent   string
	base    string // the agent's base URL on the local interface
	token   string
	nonce   string
	timeout time.Duration
	st      *runState
	rec     *hermesRecord
}

// bindings are the two the local interface offers (§11.2), with the names
// the report uses for them.
var bindings = []struct {
	tag string
	tp  a2a.TransportProtocol
}{
	{"jsonrpc", a2a.TransportProtocolJSONRPC},
	{"rest", a2a.TransportProtocolHTTPJSON},
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	addr := fs.String("a2a-addr", "", "the requester's local A2A interface, host:port (its a2a_addr.txt)")
	tokenFile := fs.String("token-file", "", "the requester's a2a_token.txt")
	agent := fs.String("agent", "", "AID of the remote agent (the provider)")
	nonce := fs.String("nonce", "", "text put in every message, for the script to look for (default: random)")
	paid := fs.String("paid", "", "a priced public skill of the agent, within the requester's agent tier")
	pricey := fs.String("pricey", "", "a priced public skill of the agent, above the requester's agent tier")
	registry := fs.Bool("registry", false, "the hub serves the A2A registry: the agent's card must verify and declare a2a-x402")
	clientOut := fs.String("client-out", "", "write the client configuration (URL and token, 0600) here, for `again`")
	stateOut := fs.String("state", "", "write the ids of the tasks made here")
	record := fs.String("record", "", "add the raw JSON-RPC responses of blocking sends to this Hermes record")
	timeout := fs.Duration("timeout", 90*time.Second, "bound of one operation that waits for the remote agent")
	_ = fs.Parse(args)
	if *addr == "" || *tokenFile == "" || *agent == "" {
		fmt.Fprintln(os.Stderr, "a2aprobe run: --a2a-addr, --token-file and --agent are required")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "a2aprobe run: %v\n", err)
		return 2
	}
	if *nonce == "" {
		*nonce = "n" + randHex(8)
	}
	p := &probe{
		rep: newReport(os.Stdout), agent: *agent, token: tok, nonce: *nonce, timeout: *timeout,
		base: "http://" + *addr + agentsPath + "/" + *agent,
		st: &runState{Agent: *agent, Nonce: *nonce, Contexts: map[string]string{}, Blocking: map[string]string{},
			Immediate: map[string]string{}, Stream: map[string]string{}},
		rec: loadRecord(*record),
	}
	if !p.run(context.Background(), runOpts{registry: *registry, paid: *paid, pricey: *pricey, clientOut: *clientOut}) {
		return 2
	}
	p.save(*stateOut, *record)
	return p.rep.exitCode()
}

// runOpts are run's choices beyond the agent.
type runOpts struct {
	registry     bool
	paid, pricey string
	clientOut    string
}

// run is the scenario. It returns false when there was nothing to run it
// against (no card).
func (p *probe) run(ctx context.Context, o runOpts) bool {
	card := p.cardChecks(ctx, o.registry)
	if card == nil {
		return false
	}
	if o.clientOut != "" {
		var cc clientConfig
		cc.URL, cc.Auth.Type, cc.Auth.Token = p.base, "bearer", p.token
		if err := writePrivateJSON(o.clientOut, cc); err != nil {
			p.rep.fail("client-config", "%v", err)
		}
	}
	for _, b := range bindings {
		p.textFlow(ctx, card, b.tag, b.tp)
	}
	p.cancelFlow(ctx, card)
	p.noSecurityRequirements(ctx, card)
	p.otherAgent(ctx)
	if o.paid != "" {
		p.payFlow(ctx, card, o.paid, o.pricey)
	} else {
		p.rep.note("x402", "no --paid skill: the payment flow was not run")
	}
	return true
}

func cmdAgain(args []string) int {
	fs := flag.NewFlagSet("again", flag.ExitOnError)
	clientFile := fs.String("client", "", "the client configuration `run --client-out` wrote")
	stateFile := fs.String("state", "", "the task ids `run --state` wrote")
	record := fs.String("record", "", "add the raw JSON-RPC response of the blocking send to this Hermes record")
	timeout := fs.Duration("timeout", 90*time.Second, "bound of one operation that waits for the remote agent")
	_ = fs.Parse(args)
	if *clientFile == "" || *stateFile == "" {
		fmt.Fprintln(os.Stderr, "a2aprobe again: --client and --state are required")
		return 2
	}
	var cc clientConfig
	var st runState
	if err := readJSONFile(*clientFile, &cc); err != nil {
		fmt.Fprintf(os.Stderr, "a2aprobe again: %v\n", err)
		return 2
	}
	if err := readJSONFile(*stateFile, &st); err != nil {
		fmt.Fprintf(os.Stderr, "a2aprobe again: %v\n", err)
		return 2
	}
	p := &probe{rep: newReport(os.Stdout), agent: st.Agent, base: cc.URL, token: cc.Auth.Token,
		nonce: st.Nonce, timeout: *timeout, st: &st, rec: loadRecord(*record)}
	p.again(context.Background())
	p.save("", *record)
	return p.rep.exitCode()
}

// again checks, after the requester restarted, that the client
// configuration run wrote still reaches the agent and its tasks.
func (p *probe) again(ctx context.Context) {
	st := p.st

	// The same URL and token, as the client kept them: nothing re-read from
	// the node's files.
	cctx, cancel := opCtx(ctx, 30*time.Second)
	card, err := resolveCard(cctx, cardURL(p.base), p.token)
	cancel()
	if !p.rep.check(err == nil, "restart-card", "the saved URL and token still fetch the card (%v)", errText(err)) {
		return
	}
	cl, cctx2, err := newClient(ctx, card, p.token, a2a.TransportProtocolJSONRPC, false)
	if !p.rep.check(err == nil, "restart-client", "client from the card (%v)", errText(err)) {
		return
	}
	if id := st.Blocking["jsonrpc"]; id != "" {
		gctx, cancel := opCtx(cctx2, 30*time.Second)
		t, err := cl.GetTask(gctx, &a2a.GetTaskRequest{ID: a2a.TaskID(id)})
		cancel()
		p.rep.check(err == nil && t.Status.State == a2a.TaskStateCompleted, "restart-get",
			"a task from before the restart: %s (%v)", stateOf(t), errText(err))
	}
	ctxID := st.Contexts["jsonrpc"]
	text := fmt.Sprintf("joint-a2a after restart %s", p.nonce)
	t := p.blockingSend(cctx2, cl, "restart-send", text, ctxID, "text-blocking-after-restart")
	if t != nil {
		done := p.waitTerminal(cctx2, cl, t.ID, "restart-send-done")
		if done != nil {
			p.checkDoneText("restart-send-done", done, echoPrefix+text)
		}
	}
	if ctxID != "" {
		lctx, cancel := opCtx(cctx2, 30*time.Second)
		l, err := cl.ListTasks(lctx, &a2a.ListTasksRequest{ContextID: ctxID})
		cancel()
		want := []string{st.Blocking["jsonrpc"], st.Immediate["jsonrpc"]}
		if t != nil {
			want = append(want, string(t.ID))
		}
		p.rep.check(err == nil && sameIDs(taskIDs(l), want), "restart-list",
			"ListTasks(contextId) after the restart: %v, want %v (%v)", taskIDs(l), want, errText(err))
	}
}

// save writes the state and the Hermes record.
func (p *probe) save(stateFile, recordFile string) {
	if stateFile != "" {
		if err := writePrivateJSON(stateFile, p.st); err != nil {
			p.rep.fail("state", "%v", err)
		}
	}
	if recordFile != "" {
		if err := writePrivateJSON(recordFile, p.rec); err != nil {
			p.rep.fail("record", "%v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// The card.

// cardChecks fetches the proxy card with and without the bearer and checks
// what a client needs from it (§11.3). It returns the card, or nil when
// there is nothing to go on.
func (p *probe) cardChecks(ctx context.Context, registry bool) *a2a.AgentCard {
	url := cardURL(p.base)
	// Without a token: 401, with a Bearer challenge.
	cctx, cancel := opCtx(ctx, 30*time.Second)
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	cancel()
	if err != nil {
		p.rep.fail("card-no-token", "%v", err)
		return nil
	}
	resp.Body.Close()
	p.rep.check(resp.StatusCode == http.StatusUnauthorized && strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer"),
		"card-no-token", "card without a token: HTTP %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	cctx, cancel = opCtx(ctx, 30*time.Second)
	_, err = resolveCard(cctx, url, "")
	cancel()
	var sne *agentcard.ErrStatusNotOK
	p.rep.check(errors.As(err, &sne) && sne.StatusCode == http.StatusUnauthorized, "card-resolver-no-token",
		"a2a-go's resolver without a token is refused: %v", errText(err))

	// With it: the card, at the well-known URL and at the base URL (the
	// resolver fetches a base URL with a path as it is).
	cctx, cancel = opCtx(ctx, 30*time.Second)
	card, err := resolveCard(cctx, url, p.token)
	cancel()
	if !p.rep.check(err == nil, "card", "proxy card with the bearer (%v)", errText(err)) {
		return nil
	}
	cctx, cancel = opCtx(ctx, 30*time.Second)
	alias, err := resolveCard(cctx, p.base, p.token)
	cancel()
	p.rep.check(err == nil && alias.Name == card.Name, "card-base-url", "the base URL serves the same card (%v)", errText(err))

	schemes := bearerSchemes(card)
	p.rep.check(len(schemes) > 0 && len(card.SecurityRequirements) > 0, "card-security",
		"bearer schemes %v, securityRequirements %d", schemes, len(card.SecurityRequirements))
	var jsonrpc, rest string
	for _, in := range card.SupportedInterfaces {
		if in == nil {
			continue
		}
		switch in.ProtocolBinding {
		case a2a.TransportProtocolJSONRPC:
			jsonrpc = in.URL
		case a2a.TransportProtocolHTTPJSON:
			rest = in.URL
		}
	}
	local := func(u string) bool { return strings.HasPrefix(u, strings.TrimRight(p.base, "/")+"/") }
	p.rep.check(local(jsonrpc) && local(rest), "card-interfaces", "JSON-RPC %q, HTTP+JSON %q on this interface", jsonrpc, rest)
	p.rep.check(card.Capabilities.Streaming && !card.Capabilities.PushNotifications, "card-capabilities",
		"streaming %v, pushNotifications %v", card.Capabilities.Streaming, card.Capabilities.PushNotifications)

	verification, x402 := "", false
	for _, e := range card.Capabilities.Extensions {
		switch e.URI {
		case originURI:
			verification = str(e.Params["originVerification"])
		case x402URI:
			x402 = true
			p.rep.check(!e.Required, "card-x402-optional", "the proxy card does not make a2a-x402 required (§8.7)")
		}
	}
	if registry {
		p.rep.check(verification == "VERIFIED", "card-origin", "the agent's network card, verified by this node: %q", verification)
		p.rep.check(x402, "card-x402", "the proxy card declares a2a-x402 for an agent with a priced public skill")
	} else {
		p.rep.note("card-origin", "origin %q, a2a-x402 declared %v (the hub serves no A2A registry: the card is made from the AID)",
			verification, x402)
	}
	return card
}

// ---------------------------------------------------------------------------
// Text tasks.

// textFlow runs the text-task operations over one binding: a blocking send
// and one that returns at once, in a context the client names; a streamed
// send; GetTask; ListTasks by that context.
func (p *probe) textFlow(ctx context.Context, card *a2a.AgentCard, tag string, binding a2a.TransportProtocol) {
	cl, ctx, err := newClient(ctx, card, p.token, binding, false)
	if !p.rep.check(err == nil, tag+"-client", "a2a-go client over %s (%v)", binding, errText(err)) {
		return
	}
	ctxID := "ctx-joint-" + tag + "-" + randHex(6)
	p.st.Contexts[tag] = ctxID

	// Blocking: the answer comes back in the one call (§11.5 [C22]).
	text := fmt.Sprintf("joint-a2a %s blocking %s", tag, p.nonce)
	record := ""
	if binding == a2a.TransportProtocolJSONRPC {
		record = "text-blocking"
	}
	if t := p.blockingSend(ctx, cl, tag+"-send-blocking", text, ctxID, record); t != nil {
		p.st.Blocking[tag] = string(t.ID)
		if done := p.waitTerminal(ctx, cl, t.ID, tag+"-send-blocking-done"); done != nil {
			p.checkDoneText(tag+"-send-blocking-done", done, echoPrefix+text)
		}
	}

	// returnImmediately: the task at once, the answer by GetTask.
	text = fmt.Sprintf("joint-a2a %s immediate %s", tag, p.nonce)
	sctx, cancel := opCtx(ctx, 30*time.Second)
	began := time.Now()
	res, err := cl.SendMessage(sctx, &a2a.SendMessageRequest{Message: textMessage(text, ctxID),
		Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	took := time.Since(began)
	cancel()
	if t, terr := asTask(res); err == nil && terr == nil {
		p.st.Immediate[tag] = string(t.ID)
		p.rep.check(!t.Status.State.Terminal() && t.ContextID == ctxID, tag+"-send-immediate",
			"answered in %s at %s, contextId kept %v", took.Round(time.Millisecond), t.Status.State, t.ContextID == ctxID)
		if done := p.waitTerminal(ctx, cl, t.ID, tag+"-send-immediate-done"); done != nil {
			p.checkDoneText(tag+"-send-immediate-done", done, echoPrefix+text)
		}
	} else {
		p.rep.fail(tag+"-send-immediate", "%v", errText(errors.Join(err, terr)))
	}

	p.streamSend(ctx, cl, tag)

	// GetTask with history: the question and the answer, requester=user.
	if id := p.st.Blocking[tag]; id != "" {
		gctx, cancel := opCtx(ctx, 30*time.Second)
		n := 10
		t, err := cl.GetTask(gctx, &a2a.GetTaskRequest{ID: a2a.TaskID(id), HistoryLength: &n})
		cancel()
		if p.rep.check(err == nil, tag+"-get", "GetTask (%v)", errText(err)) {
			var roles []string
			var sawQ, sawA bool
			for _, m := range t.History {
				if m == nil {
					continue
				}
				roles = append(roles, string(m.Role))
				txt := partsText(m.Parts)
				sawQ = sawQ || (m.Role == a2a.MessageRoleUser && strings.Contains(txt, "blocking "+p.nonce))
				sawA = sawA || (m.Role == a2a.MessageRoleAgent && strings.HasPrefix(txt, echoPrefix))
			}
			p.rep.check(t.Status.State == a2a.TaskStateCompleted && sawQ && sawA && str(t.Metadata[keyPeerAID]) == p.agent,
				tag+"-get-history", "%s, history roles %v, question %v, answer %v, anet.peer_aid is the agent %v",
				t.Status.State, roles, sawQ, sawA, str(t.Metadata[keyPeerAID]) == p.agent)
		}
	}

	// ListTasks by the client's contextId finds exactly the two tasks sent
	// in it (C22), without artifacts unless asked.
	want := []string{p.st.Blocking[tag], p.st.Immediate[tag]}
	lctx, cancel := opCtx(ctx, 30*time.Second)
	l, err := cl.ListTasks(lctx, &a2a.ListTasksRequest{ContextID: ctxID})
	cancel()
	if p.rep.check(err == nil && sameIDs(taskIDs(l), want), tag+"-list-context",
		"ListTasks(contextId=%s): %v, want %v (%v)", ctxID, taskIDs(l), want, errText(err)) {
		arts := 0
		for _, t := range l.Tasks {
			arts += len(t.Artifacts)
		}
		p.rep.check(arts == 0, tag+"-list-no-artifacts", "without includeArtifacts: %d artifacts", arts)
	}
	lctx, cancel = opCtx(ctx, 30*time.Second)
	l, err = cl.ListTasks(lctx, &a2a.ListTasksRequest{ContextID: ctxID, IncludeArtifacts: true})
	cancel()
	withReply := 0
	if err == nil {
		for _, t := range l.Tasks {
			if _, ok := replyText(t); ok {
				withReply++
			}
		}
	}
	p.rep.check(err == nil && withReply == len(want), tag+"-list-artifacts",
		"with includeArtifacts: %d of %d carry anet.reply (%v)", withReply, len(want), errText(err))
	lctx, cancel = opCtx(ctx, 30*time.Second)
	l, err = cl.ListTasks(lctx, &a2a.ListTasksRequest{})
	cancel()
	all := taskIDs(l)
	mine := nonEmpty(append(want, p.st.Stream[tag]))
	p.rep.check(err == nil && len(mine) > 0 && containsAll(all, mine), tag+"-list-all",
		"ListTasks without a filter lists this client's tasks (%d; %v)", len(all), errText(err))
}

// blockingSend sends text without configuration — a blocking SendMessage,
// what Hermes sends — and judges what came back. It records the raw
// JSON-RPC response for the Hermes contract when record names a case.
func (p *probe) blockingSend(ctx context.Context, cl *a2aclient.Client, id, text, ctxID, record string) *a2a.Task {
	var body []byte
	sctx, cancel := opCtx(ctx, p.timeout)
	defer cancel()
	if record != "" {
		sctx = recordInto(sctx, &body)
	}
	began := time.Now()
	res, err := cl.SendMessage(sctx, &a2a.SendMessageRequest{Message: textMessage(text, ctxID)})
	took := time.Since(began).Round(time.Millisecond)
	t, terr := asTask(res)
	if err != nil || terr != nil {
		p.rep.fail(id, "after %s: %v", took, errText(errors.Join(err, terr)))
		return nil
	}
	if ctxID != "" && t.ContextID != ctxID {
		p.rep.fail(id+"-context", "the task is in context %q, not the %q the client gave", t.ContextID, ctxID)
	}
	want := echoPrefix + text
	p.judgeAnswer(id, t, want, took)
	if record != "" && len(body) > 0 {
		p.rec.add(hermesCase{Name: record, Agent: p.agent, ContextID: ctxID, Response: string(body),
			WantState: short(t.Status.State), WantReplyContains: want})
	}
	return t
}

// judgeAnswer checks that a blocking send came back with the answer. A
// completed task has it as anet.reply. The provider completes a text task
// with a message and then its result (reply_task(state=completed)), and the
// wait ends at the first state that is terminal or asks the client, so the
// call may come back at input-required with the answer as the status
// message: the client has the answer either way, and the NOTE records that
// a client reading only the state (Hermes) is told more input is needed.
func (p *probe) judgeAnswer(id string, t *a2a.Task, want string, took time.Duration) {
	switch t.Status.State {
	case a2a.TaskStateCompleted:
		got, _ := replyText(t)
		p.rep.check(got == want, id, "completed after %s with anet.reply %q", took, got)
	case a2a.TaskStateInputRequired:
		got := statusText(t)
		if p.rep.check(got == want, id, "after %s at input-required with the answer %q as the status message", took, got) {
			p.rep.note(id+"-state", "the blocking call ended at the provider's final message, before its result: "+
				"a client that reads the state (Hermes) shows input-required and 'needs more input' for a finished answer")
		}
	default:
		p.rep.fail(id, "after %s at %s: %q", took, t.Status.State, statusText(t))
	}
}

// waitTerminal reads the task until it is terminal.
func (p *probe) waitTerminal(ctx context.Context, cl *a2aclient.Client, id a2a.TaskID, check string) *a2a.Task {
	deadline := time.Now().Add(p.timeout)
	var last *a2a.Task
	var err error
	for {
		gctx, cancel := opCtx(ctx, 30*time.Second)
		var t *a2a.Task
		t, err = cl.GetTask(gctx, &a2a.GetTaskRequest{ID: id})
		cancel()
		if err == nil {
			last = t
			if t.Status.State.Terminal() {
				return t
			}
		}
		if time.Now().After(deadline) {
			p.rep.fail(check, "task %s not terminal after %s: %s (%v)", id, p.timeout, stateOf(last), errText(err))
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// checkDoneText checks a finished text task: completed, the answer as
// anet.reply, the provider's receipt checked (SI-6: a completed text task
// always says whether), and the right agent.
func (p *probe) checkDoneText(id string, t *a2a.Task, want string) {
	got, ok := replyText(t)
	rv, has := t.Metadata[keyReceiptOK]
	p.rep.check(t.Status.State == a2a.TaskStateCompleted && ok && got == want && has,
		id, "%s, anet.reply %q, anet.receipt_verified %v", t.Status.State, got, rv)
	if has && rv != "verified" {
		p.rep.fail(id+"-receipt", "the provider's receipt is %v, not verified", rv)
	}
	if peer := str(t.Metadata[keyPeerAID]); peer != p.agent {
		p.rep.fail(id+"-peer", "anet.peer_aid %q, not the agent %q", peer, p.agent)
	}
}

// streamSend streams a new text task: the Task first, then its updates,
// the anet.reply artifact before the terminal status (§11.5).
func (p *probe) streamSend(ctx context.Context, cl *a2aclient.Client, tag string) {
	id := tag + "-stream"
	text := fmt.Sprintf("joint-a2a %s stream %s", tag, p.nonce)
	want := echoPrefix + text
	sctx, cancel := opCtx(ctx, p.timeout)
	defer cancel()
	var kinds []string
	var taskID a2a.TaskID
	var endState a2a.TaskState
	var endMsg, reply string
	replyAt, endAt, n := -1, -1, 0
	var serr error
	for ev, err := range cl.SendStreamingMessage(sctx, &a2a.SendMessageRequest{Message: textMessage(text, "")}) {
		if err != nil {
			serr = err
			break
		}
		switch v := ev.(type) {
		case *a2a.Task:
			kinds = append(kinds, "task")
			taskID, endState = v.ID, v.Status.State
		case *a2a.TaskStatusUpdateEvent:
			kinds = append(kinds, short(v.Status.State))
			taskID, endState = v.TaskID, v.Status.State
			if v.Status.Message != nil {
				endMsg = partsText(v.Status.Message.Parts)
			}
			endAt = n
		case *a2a.TaskArtifactUpdateEvent:
			kinds = append(kinds, "artifact:"+string(v.Artifact.ID))
			if v.Artifact != nil && v.Artifact.ID == artifactReply {
				replyAt, reply = n, partsText(v.Artifact.Parts)
			}
		case *a2a.Message:
			kinds = append(kinds, "message")
		default:
			kinds = append(kinds, fmt.Sprintf("%T", ev))
		}
		n++
	}
	if taskID != "" {
		p.st.Stream[tag] = string(taskID)
	}
	if serr != nil || len(kinds) == 0 {
		p.rep.fail(id, "events %v: %v", kinds, errText(serr))
		return
	}
	p.rep.check(kinds[0] == "task", id+"-first", "the first event is the Task: %v", kinds)
	switch endState {
	case a2a.TaskStateCompleted:
		p.rep.check(replyAt >= 0 && replyAt < endAt && reply == want, id,
			"events %v: anet.reply %q before the terminal status", kinds, reply)
	case a2a.TaskStateInputRequired:
		if endMsg == "" {
			// A status update without its message: the task has it.
			gctx, cancel := opCtx(ctx, 30*time.Second)
			if g, err := cl.GetTask(gctx, &a2a.GetTaskRequest{ID: taskID}); err == nil {
				endMsg = statusText(g)
				if r, ok := replyText(g); ok && endMsg == "" {
					endMsg = r
				}
			}
			cancel()
		}
		if p.rep.check(endMsg == want, id, "events %v: ended at input-required with the answer %q", kinds, endMsg) {
			p.rep.note(id+"-state", "the stream ended at the provider's final message, before its result")
		}
		if done := p.waitTerminal(ctx, cl, taskID, id+"-done"); done != nil {
			p.checkDoneText(id+"-done", done, want)
		}
	default:
		p.rep.fail(id, "events %v: ended at %s", kinds, endState)
	}
}

// ---------------------------------------------------------------------------
// CancelTask, the scope of a path, and a client without credentials.

// cancelFlow cancels a task the provider holds open (§4.2).
func (p *probe) cancelFlow(ctx context.Context, card *a2a.AgentCard) {
	cl, ctx, err := newClient(ctx, card, p.token, a2a.TransportProtocolJSONRPC, false)
	if !p.rep.check(err == nil, "cancel-client", "(%v)", errText(err)) {
		return
	}
	text := fmt.Sprintf("joint-a2a %s cancel %s", holdMark, p.nonce)
	sctx, cancel := opCtx(ctx, 30*time.Second)
	res, err := cl.SendMessage(sctx, &a2a.SendMessageRequest{Message: textMessage(text, ""),
		Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	cancel()
	t, terr := asTask(res)
	if err != nil || terr != nil {
		p.rep.fail("cancel-send", "%v", errText(errors.Join(err, terr)))
		return
	}
	p.st.Cancel = string(t.ID)
	cctx, cancel := opCtx(ctx, 30*time.Second)
	c, err := cl.CancelTask(cctx, &a2a.CancelTaskRequest{ID: t.ID})
	cancel()
	p.rep.check(err == nil && c.Status.State == a2a.TaskStateCanceled, "cancel", "CancelTask: %s (%v)", stateOf(c), errText(err))
	gctx, cancel := opCtx(ctx, 30*time.Second)
	g, err := cl.GetTask(gctx, &a2a.GetTaskRequest{ID: t.ID})
	cancel()
	p.rep.check(err == nil && g.Status.State == a2a.TaskStateCanceled, "cancel-get", "GetTask after the cancel: %s (%v)", stateOf(g), errText(err))
	// A canceled task takes no new input (§4.2, C35).
	m := textMessage("joint-a2a after the cancel "+p.nonce, t.ContextID)
	m.TaskID = t.ID
	sctx, cancel = opCtx(ctx, 30*time.Second)
	_, err = cl.SendMessage(sctx, &a2a.SendMessageRequest{Message: m, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	cancel()
	p.rep.check(errIs(err, a2a.ErrUnsupportedOperation, ""), "cancel-then-send",
		"a message on the canceled task: %v", errText(err))
}

// noSecurityRequirements is the client whose card says nothing about
// authentication: AuthInterceptor then sends no token, whatever the
// CredentialsService holds, and the interface answers 401 — the reason the
// proxy card must carry securityRequirements (§11.3 [C18]).
func (p *probe) noSecurityRequirements(ctx context.Context, card *a2a.AgentCard) {
	bare := *card
	bare.SecurityRequirements = nil
	cl, cctx, err := newClient(ctx, &bare, p.token, a2a.TransportProtocolJSONRPC, false)
	if !p.rep.check(err == nil, "no-security-requirements-client", "(%v)", errText(err)) {
		return
	}
	before := p.countTasks(ctx, card)
	sctx, cancel := opCtx(cctx, 30*time.Second)
	_, err = cl.SendMessage(sctx, &a2a.SendMessageRequest{Message: textMessage("joint-a2a unauthenticated "+p.nonce, ""),
		Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	cancel()
	after := p.countTasks(ctx, card)
	p.rep.check(err != nil && strings.Contains(err.Error(), "401") && before >= 0 && before == after,
		"no-security-requirements", "a card without securityRequirements: %v; tasks %d → %d", errText(err), before, after)
}

// countTasks is how many tasks the agent's path lists, -1 when unknown.
func (p *probe) countTasks(ctx context.Context, card *a2a.AgentCard) int {
	cl, cctx, err := newClient(ctx, card, p.token, a2a.TransportProtocolJSONRPC, false)
	if err != nil {
		return -1
	}
	lctx, cancel := opCtx(cctx, 30*time.Second)
	defer cancel()
	l, err := cl.ListTasks(lctx, &a2a.ListTasksRequest{PageSize: 1})
	if err != nil {
		return -1
	}
	return l.TotalSize
}

// otherAgent reads a task of this agent through another agent's path: the
// token authorizes the node's own tasks with each agent, one path per
// agent, and a task outside the path does not exist there (§11.1 [C17]).
func (p *probe) otherAgent(ctx context.Context) {
	id := p.st.Blocking["jsonrpc"]
	if id == "" {
		p.rep.note("other-agent", "no task to look for")
		return
	}
	other := otherAID(p.agent)
	base := strings.TrimSuffix(p.base, "/"+p.agent) + "/" + other
	cctx, cancel := opCtx(ctx, 30*time.Second)
	card, err := resolveCard(cctx, cardURL(base), p.token)
	cancel()
	if !p.rep.check(err == nil, "other-agent-card", "the card of an agent with no network card (%v)", errText(err)) {
		return
	}
	cl, cctx2, err := newClient(ctx, card, p.token, a2a.TransportProtocolJSONRPC, false)
	if err != nil {
		p.rep.fail("other-agent", "%v", err)
		return
	}
	gctx, cancel := opCtx(cctx2, 30*time.Second)
	_, err = cl.GetTask(gctx, &a2a.GetTaskRequest{ID: a2a.TaskID(id)})
	cancel()
	p.rep.check(errIs(err, a2a.ErrTaskNotFound, ""), "other-agent", "GetTask of this agent's task through %s: %v", other, errText(err))
}

// otherAID is an AID-shaped id that is not aid: its last character changed.
func otherAID(aid string) string {
	if aid == "" {
		return "joint"
	}
	last := aid[len(aid)-1]
	c := byte('a')
	if last == 'a' {
		c = 'b'
	}
	return aid[:len(aid)-1] + string(c)
}

// ---------------------------------------------------------------------------
// Small readers.

func taskIDs(l *a2a.ListTasksResponse) []string {
	if l == nil {
		return nil
	}
	var out []string
	for _, t := range l.Tasks {
		if t != nil {
			out = append(out, string(t.ID))
		}
	}
	return out
}

// sameIDs compares two sets of ids; an empty want id fails it.
func sameIDs(got, want []string) bool {
	if slices.Contains(want, "") || len(got) != len(want) {
		return false
	}
	return containsAll(got, want)
}

func nonEmpty(ids []string) []string {
	var out []string
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func containsAll(got, want []string) bool {
	for _, w := range want {
		if w == "" || !slices.Contains(got, w) {
			return false
		}
	}
	return true
}

func stateOf(t *a2a.Task) string {
	if t == nil {
		return "no task"
	}
	return string(t.Status.State)
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// The Hermes record.

// hermesCase is one blocking SendMessage response as it came over the wire,
// with what Hermes' model must read from it (internal/a2ashape
// TestHermesReadsJointRecord reads the file).
type hermesCase struct {
	Name              string `json:"name"`
	Agent             string `json:"agent"`
	ContextID         string `json:"context_id"`
	Response          string `json:"response"`
	WantState         string `json:"want_state"`
	WantReplyContains string `json:"want_reply_contains,omitempty"`
	// Soft marks an expectation the projection is known not to meet yet
	// (0017 Q21): a miss is logged as a gap, not failed.
	Soft bool   `json:"soft,omitempty"`
	Why  string `json:"why,omitempty"`
}

type hermesRecord struct {
	Cases []hermesCase `json:"cases"`
}

// loadRecord reads an existing record (again adds to run's), or starts one.
func loadRecord(path string) *hermesRecord {
	r := &hermesRecord{}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, r)
		}
	}
	return r
}

// add adds c, replacing a case of the same name.
func (r *hermesRecord) add(c hermesCase) {
	for i := range r.Cases {
		if r.Cases[i].Name == c.Name {
			r.Cases[i] = c
			return
		}
	}
	r.Cases = append(r.Cases, c)
}

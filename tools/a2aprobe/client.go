package main

// client.go is the client half every scenario shares: the report, the card,
// an a2a-go client built from it the way any A2A client builds one, and the
// reading of what comes back.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2aext"
)

// What the probe reads, spelled out from the design rather than imported
// from the daemon (see the package comment).
const (
	// agentsPath is the root of the local interface's routes (§11.2).
	agentsPath = "/a2a/v1/agents"

	// x402URI is the a2a-x402 v0.2 extension (§8.1).
	x402URI = "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"
	// originURI is the proxy card's anet-origin extension (§11.3).
	originURI = "https://agentnetwork.org.cn/a2a/ext/anet-origin/v1"

	keyX402Status   = "x402.payment.status"
	keyX402Required = "x402.payment.required"
	keyX402Payload  = "x402.payment.payload"
	keyX402Receipts = "x402.payment.receipts"
	keyX402Error    = "x402.payment.error"
	keyAccept       = "anet.payment.accept"
	keyReason       = "anet.reason"
	keyEffect       = "anet.effect_status"
	keyReceiptOK    = "anet.receipt_verified"
	keyPeerAID      = "anet.peer_aid"
	keySkill        = "anet.skill"

	payRequired  = "payment-required"
	paySubmitted = "payment-submitted"
	payRejected  = "payment-rejected"
	payCompleted = "payment-completed"
	payFailed    = "payment-failed"

	// artifactReply is the text answer of a completed text task (§11.5).
	artifactReply = "anet.reply"

	// echoPrefix is what responder puts before the text it answers.
	echoPrefix = "echo: "
	// holdMark in a task's text tells responder not to answer it: the
	// task stays open for CancelTask.
	holdMark = "[hold]"
)

// ---------------------------------------------------------------------------
// The report.

// report prints one line per check and counts the failures.
type report struct {
	mu    sync.Mutex
	w     io.Writer
	fails int
}

func newReport(w io.Writer) *report { return &report{w: w} }

func (r *report) line(kind, id, format string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	msg := strings.Join(strings.Fields(fmt.Sprintf(format, a...)), " ")
	fmt.Fprintf(r.w, "%s %s: %s\n", kind, id, msg)
	if kind == "FAIL" {
		r.fails++
	}
}

func (r *report) pass(id, format string, a ...any) { r.line("PASS", id, format, a...) }
func (r *report) fail(id, format string, a ...any) { r.line("FAIL", id, format, a...) }
func (r *report) note(id, format string, a ...any) { r.line("NOTE", id, format, a...) }

// check is pass when ok, else fail, with the same detail.
func (r *report) check(ok bool, id, format string, a ...any) bool {
	if ok {
		r.pass(id, format, a...)
	} else {
		r.fail(id, format, a...)
	}
	return ok
}

func (r *report) exitCode() int {
	if r.fails > 0 {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Configuration.

// clientConfig is what a client keeps to reach one remote agent through the
// local interface: the agent's base URL and the bearer token — the shape of
// the entry `anet agents wire --a2a` writes into Hermes' a2a_agents
// (A2A-DESIGN §13.1). The restart check reuses it as it was written.
type clientConfig struct {
	URL  string `json:"url"`
	Auth struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	} `json:"auth"`
}

// readToken reads a bearer token from a file: the first line, trimmed.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok, _, _ := strings.Cut(string(b), "\n")
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", fmt.Errorf("%s holds no token", path)
	}
	return tok, nil
}

// writePrivateJSON writes v to path, 0600.
func writePrivateJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// The card and the client.

// cardURL is the proxy card of the agent at base, where Hermes and the
// specification look for it.
func cardURL(base string) string {
	return strings.TrimRight(base, "/") + "/.well-known/agent-card.json"
}

// resolveCard fetches a card with a2a-go's resolver, with the bearer when
// token is not empty.
func resolveCard(ctx context.Context, url, token string) (*a2a.AgentCard, error) {
	var opts []agentcard.ResolveOption
	if token != "" {
		opts = append(opts, agentcard.WithRequestHeader("Authorization", "Bearer "+token))
	}
	return agentcard.DefaultResolver.Resolve(ctx, url, opts...)
}

// session is the session id the probe's credentials are stored under.
const session a2aclient.SessionID = "joint-a2a"

// credentials is a CredentialsService holding token for every HTTP bearer
// scheme the card declares: what a client does that reads the card to
// learn how to authenticate, rather than knowing anet's scheme name.
func credentials(card *a2a.AgentCard, token string) *a2aclient.InMemoryCredentialsStore {
	creds := a2aclient.NewInMemoryCredentialsStore()
	for name, s := range card.SecuritySchemes {
		if h, ok := s.(a2a.HTTPAuthSecurityScheme); ok && strings.EqualFold(h.Scheme, "bearer") {
			creds.Set(session, name, a2aclient.AuthCredential(token))
		}
	}
	return creds
}

// bearerSchemes names the card's HTTP bearer schemes.
func bearerSchemes(card *a2a.AgentCard) []string {
	var out []string
	for name, s := range card.SecuritySchemes {
		if h, ok := s.(a2a.HTTPAuthSecurityScheme); ok && strings.EqualFold(h.Scheme, "bearer") {
			out = append(out, string(name))
		}
	}
	return out
}

// newClient is an unmodified a2a-go client for card over one binding:
// AuthInterceptor with the token in a CredentialsService and, when
// activate, a2aext's activator asking for the a2a-x402 extension. The HTTP
// client has no timeout — a blocking send waits for the remote agent — and
// records response bodies for the calls whose context asks (recordInto).
// The context returned carries the session the credentials are under.
func newClient(ctx context.Context, card *a2a.AgentCard, token string, binding a2a.TransportProtocol, activate bool) (*a2aclient.Client, context.Context, error) {
	hc := &http.Client{Transport: recorder{next: http.DefaultTransport}}
	icpt := []a2aclient.CallInterceptor{&a2aclient.AuthInterceptor{Service: credentials(card, token)}}
	if activate {
		icpt = append(icpt, a2aext.NewActivator(x402URI))
	}
	cl, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithCallInterceptors(icpt...),
		a2aclient.WithConfig(a2aclient.Config{PreferredTransports: []a2a.TransportProtocol{binding}}),
		a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		return nil, nil, err
	}
	return cl, a2aclient.AttachSessionID(ctx, session), nil
}

// recorder copies the body of a response whose request context asks for
// it. It sits under a2a-go's transport as the http.Client's RoundTripper:
// the client itself is not touched, and the bytes are exactly what came
// over the wire — what Hermes would have parsed.
type recorder struct{ next http.RoundTripper }

type recordKey struct{}

// recordInto asks for the next response body of calls made with ctx.
func recordInto(ctx context.Context, dst *[]byte) context.Context {
	return context.WithValue(ctx, recordKey{}, dst)
}

func (r recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	dst, _ := req.Context().Value(recordKey{}).(*[]byte)
	if err != nil || dst == nil {
		return resp, err
	}
	b, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	*dst = b
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if rerr != nil {
		return nil, rerr
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Reading tasks.

// textMessage is a user message of one text part.
func textMessage(text, contextID string) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.ContextID = contextID
	return m
}

// asTask is the Task a SendMessage answered with.
func asTask(res a2a.SendMessageResult) (*a2a.Task, error) {
	t, ok := res.(*a2a.Task)
	if !ok || t == nil {
		return nil, fmt.Errorf("SendMessage answered with %T, not a Task", res)
	}
	return t, nil
}

// partsText joins the text parts of parts.
func partsText(parts a2a.ContentParts) string {
	var out []string
	for _, p := range parts {
		if p == nil {
			continue
		}
		if t, ok := p.Content.(a2a.Text); ok {
			out = append(out, string(t))
		}
	}
	return strings.Join(out, "\n")
}

// replyText is the text of the task's anet.reply artifact.
func replyText(t *a2a.Task) (string, bool) {
	for _, a := range t.Artifacts {
		if a != nil && a.ID == artifactReply {
			return partsText(a.Parts), true
		}
	}
	return "", false
}

// statusText is the text of the task's status message.
func statusText(t *a2a.Task) string {
	if t.Status.Message == nil {
		return ""
	}
	return partsText(t.Status.Message.Parts)
}

// statusMeta is key on the status message.
func statusMeta(t *a2a.Task, key string) any {
	if t.Status.Message == nil {
		return nil
	}
	return t.Status.Message.Metadata[key]
}

// taskMeta is key on the task, else on its status message.
func taskMeta(t *a2a.Task, key string) any {
	if v, ok := t.Metadata[key]; ok {
		return v
	}
	return statusMeta(t, key)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// short is a state as Hermes shows it: TASK_STATE_INPUT_REQUIRED is
// input-required.
func short(s a2a.TaskState) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(string(s), "TASK_STATE_"), "_", "-"))
}

// paymentRequired is x402.payment.required of a task: on its status
// message, else in its metadata.
func paymentRequired(t *a2a.Task) map[string]any {
	if m, ok := statusMeta(t, keyX402Required).(map[string]any); ok {
		return m
	}
	m, _ := t.Metadata[keyX402Required].(map[string]any)
	return m
}

// accepts are the options of a payment requirement.
func accepts(req map[string]any) []map[string]any {
	var out []map[string]any
	list, _ := req["accepts"].([]any)
	for _, a := range list {
		if m, ok := a.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// cloneJSON is a deep copy of a JSON value.
func cloneJSON(v map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// successReceipts counts the receipts in x402.payment.receipts that say
// success.
func successReceipts(t *a2a.Task) int {
	list, _ := taskMeta(t, keyX402Receipts).([]any)
	n := 0
	for _, r := range list {
		if m, ok := r.(map[string]any); ok && m["success"] == true {
			n++
		}
	}
	return n
}

// errIs reports whether err is the A2A error want, or mentions text.
func errIs(err, want error, text string) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, want) || (text != "" && strings.Contains(err.Error(), text))
}

// opCtx bounds one operation.
func opCtx(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

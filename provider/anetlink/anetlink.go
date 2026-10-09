// Package anetlink is the C1 client for a separately deployed ANetLink runtime: it implements
// provider.CapabilityProvider by speaking C1 v1, CoreDet-CBOR over HTTP on the Unix socket anetlinkd
// serves (c1.socket, by default $XDG_RUNTIME_DIR/anetlink/c1.sock).
//
// The daemon stays behind the red line: capability ids arriving from this wire (form
// "<cap>@<adapter>/<device>") are opaque strings here, and nothing in this package or its callers
// interprets what stands behind them.
//
// Three rules carry its security:
//
//   - Every connection is dialled through internal/backendconn (Rules.DialUnix): the socket's path
//     is walked and judged, and the kernel says which account listens, before a byte is written. A
//     process that took the socket's place while ANetLink was down is told nothing.
//   - The caller ANetLink is told of is the one the daemon verified and nothing else: a call from
//     the relay names provider.Call.VerifiedCaller as caller.aid (via 1); a local surface sends via 0
//     and no aid; a call through any other door is not dialled at all, so the retired via 2 is never
//     sent. This package never reads Call.CallerAID (TestTheClientNeverReadsCallerAID).
//   - A refusal is ANetLink's code, mapped to the user's (a2ashape.C1Reason) and handed to the daemon
//     as a provider.RefusalError; the C1 code itself stays in this node's log.
package anetlink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
	"github.com/ANetResearch/ANet/provider"
)

// clientTimeout bounds one exchange with ANetLink; the daemon's own deadline around an invoke is
// usually shorter.
const clientTimeout = 30 * time.Second

// The user's codes this client gives a call it ends on this side of C1 (a2ashape's reason table).
const (
	// reasonUnavailable: nothing was sent, because ANetLink could not be reached or what listens on
	// its socket failed this node's checks. The far side certainly did not run it, and may later.
	reasonUnavailable = "unavailable"
	// reasonBackendFailed: something went wrong that the requester can do nothing about, and that
	// does not say whether the device acted.
	reasonBackendFailed = "backend_failed"
)

// DefaultSocket is the socket ANetLink listens on when c1.socket is not set, and so the one
// modules.anetlink.socket defaults to: neither side needs configuring for the two to meet. It is an
// error when XDG_RUNTIME_DIR is not set, as it is for anetlinkd.
func DefaultSocket() (string, error) {
	rd := os.Getenv("XDG_RUNTIME_DIR")
	if rd == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set, so there is no default C1 socket; " +
			"set modules.anetlink.socket to the path anetlinkd serves (its --c1-socket)")
	}
	return filepath.Join(rd, "anetlink", "c1.sock"), nil
}

// Provider speaks C1 v1 to one anetlinkd. Create with New.
type Provider struct {
	id   string
	sock string
	cli  *http.Client
}

var (
	_ provider.CapabilityProvider = (*Provider)(nil)
	_ provider.RemotePolicy       = (*Provider)(nil)
)

// New returns a provider for the anetlinkd socket at socketPath (CC-1). rules check every
// connection to it: by default the account listening must own the socket file; the module's
// expected_user, socket_group and peer_check say otherwise.
func New(id, socketPath string, rules backendconn.Rules) (*Provider, error) {
	t, err := backendconn.Parse(backendconn.SchemeUnix + "://" + socketPath)
	if err != nil || t.Socket != socketPath {
		return nil, fmt.Errorf("anetlink: socket %q is not a clean absolute path short enough for a Unix socket, "+
			"without a colon, space or control character", socketPath)
	}
	return &Provider{id: id, sock: socketPath, cli: rules.Client(t, clientTimeout)}, nil
}

func (p *Provider) ID() string { return p.id }

// Socket is the socket this provider dials.
func (p *Provider) Socket() string { return p.sock }

// Capabilities lists the ids ANetLink serves, remote or not: a local surface may call any of them.
func (p *Provider) Capabilities(ctx context.Context) ([]string, error) {
	l, err := p.capList(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(l.Caps))
	for _, c := range l.Caps {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

// RemoteAllowed reports which ids ANetLink lets a remote caller use: remote = 1 (CC-5). Any other
// value refuses, whatever a later C1 may mean by it.
func (p *Provider) RemoteAllowed(ctx context.Context) (map[string]bool, error) {
	l, err := p.capList(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(l.Caps))
	for _, c := range l.Caps {
		out[c.ID] = *c.Remote == viaRelay
	}
	return out, nil
}

func (p *Provider) capList(ctx context.Context) (capList, error) {
	b, err := p.get(ctx, "/v1/capabilities")
	if err != nil {
		return capList{}, err
	}
	l, err := decodeCapList(b)
	if err != nil {
		return capList{}, &C1Error{Path: "/v1/capabilities", Status: http.StatusOK, Err: err}
	}
	return l, nil
}

// Describe returns the CID of ANetLink's description object, "" when it has none.
func (p *Provider) Describe(ctx context.Context) (string, error) {
	b, err := p.get(ctx, "/v1/describe")
	if err != nil {
		return "", err
	}
	d, err := decodeDescribe(b)
	if err != nil {
		return "", &C1Error{Path: "/v1/describe", Status: http.StatusOK, Err: err}
	}
	return *d.CID, nil
}

// Health is GET /v1/health: 204 from a C1 v1 anetlinkd.
func (p *Provider) Health(ctx context.Context) error {
	resp, err := p.do(ctx, http.MethodGet, "/v1/health", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return refusalOf(resp, "/v1/health")
	}
	return nil
}

// SpeaksC1v1 checks that the far side is a C1 v1 anetlinkd and not one of another version: GET
// /v1/health answers 204, and GET /v0/health, the C1 v0 path, answers 426 c1_version as C1 v1 does
// for every /v0 path. Anything else is a *C1Error that NotC1v1 reports. doctor's anetlink.socket.
func (p *Provider) SpeaksC1v1(ctx context.Context) error {
	if err := p.Health(ctx); err != nil {
		return err
	}
	resp, err := p.do(ctx, http.MethodGet, "/v0/health", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if ref := refusalOf(resp, "/v0/health"); ref.Reason != "c1_version" {
		if ref.Reason != "" {
			// Another C1 v1 refusal where c1_version belongs: not the C1 v1 this client speaks.
			ref.Reason = ""
		}
		return ref
	}
	return nil
}

// Invoke runs one call (CC-2). Only a local surface (Via "") and the relay (Via ViaRelay, with the
// caller the daemon verified) are dialled; a call through any other door is refused here,
// backend_failed, and nothing is sent.
func (p *Provider) Invoke(ctx context.Context, call provider.Call) (effect.Effect, error) {
	who, err := callerOf(call)
	if err != nil {
		return effect.Effect{}, failed(call.Capability, err)
	}
	req := invokeReq{Capability: &call.Capability, Args: call.Args, Caller: &who}
	if call.CallID != "" {
		req.CallID = &call.CallID
	}
	body, err := encodeInvokeReq(req)
	if err != nil {
		return effect.Effect{}, failed(call.Capability, err)
	}
	if len(body) > maxBody {
		// What ANetLink would answer (413 too_large), without sending a megabyte to hear it.
		return effect.Effect{}, &provider.RefusalError{Reason: a2ashape.C1Reason("too_large"),
			Err: fmt.Errorf("anetlink: invoke %s: the request is %d bytes, over C1's %d; not sent", call.Capability, len(body), maxBody)}
	}
	hreq, err := p.request(ctx, http.MethodPost, "/v1/invoke", body)
	if err != nil {
		return effect.Effect{}, failed(call.Capability, err)
	}
	// A device command that may have gone out and lost its answer may have acted: the lamp may be
	// on, the door open. That is an outcome nobody knows (provider.OutcomeUnknownError), not an
	// error that says it failed: a requester told it failed could send the command again (redteam
	// F10, the same rule as the service module). "May have gone out" starts when the transport has a
	// connection for it (provider.TrackSent), which is after backendconn's checks passed.
	hreq, sent := provider.TrackSent(hreq)
	resp, err := p.cli.Do(hreq)
	if err != nil {
		if sent() {
			return effect.Effect{}, fmt.Errorf("anetlink: invoke %s: %w", call.Capability, provider.AnswerLost(err))
		}
		// Never sent: anetlinkd is not there, or what listens on its socket failed this node's
		// checks (backendconn.ErrRefused). Nothing was attempted at the far end, so the task is
		// rejected as unavailable and may be sent again later; which path and which account are
		// this node's to read, in its log (and doctor's anetlink.socket).
		return effect.Effect{}, &provider.RefusalError{Reason: reasonUnavailable,
			Err: fmt.Errorf("anetlink: invoke %s: nothing was sent: %w", call.Capability, err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ref := refusalOf(resp, "/v1/invoke")
		return effect.Effect{}, &provider.RefusalError{Reason: a2ashape.C1Reason(ref.Reason),
			Err: fmt.Errorf("invoke %s: %w", call.Capability, ref)}
	}
	raw, err := readBody(resp)
	switch {
	case errors.Is(err, errTooLong), errors.Is(err, errNotCBOR):
		return effect.Effect{}, failed(call.Capability, fmt.Errorf("the answer: %w", err))
	case err != nil:
		// Accepted (200) and the answer broke off: as above.
		return effect.Effect{}, fmt.Errorf("anetlink: invoke %s: %w", call.Capability, provider.AnswerLost(err))
	}
	out, err := decodeInvokeResp(raw)
	if err != nil {
		return effect.Effect{}, failed(call.Capability, fmt.Errorf("the answer: %w", err))
	}
	return effectOf(out, call.Capability)
}

// callerOf is the Caller of a call: who ANetLink is told is asking.
func callerOf(call provider.Call) (caller, error) {
	switch call.Via {
	case "":
		via := viaLocal
		return caller{Via: &via}, nil
	case provider.ViaRelay:
		aid := call.VerifiedCaller()
		if aid == "" {
			return caller{}, errors.New("a call from the relay with no verified caller; not sent")
		}
		via := viaRelay
		return caller{Via: &via, AID: &aid}, nil
	}
	return caller{}, fmt.Errorf("a call through %q cannot cross C1 (only a local surface or the relay can); not sent", call.Via)
}

// effectStatuses are the statuses an effect from ANetLink can have. Any other is a far side that
// does not speak C1 v1.
var effectStatuses = map[effect.Status]bool{effect.OK: true, effect.Unverified: true, effect.Failed: true, effect.Unavailable: true}

// effectOf turns an invoke answer into an effect, provenance included.
func effectOf(out invokeResp, capability string) (effect.Effect, error) {
	st := effect.Status(out.Status)
	if !effectStatuses[st] {
		return effect.Effect{}, failed(capability, fmt.Errorf("the answer has status %q, which is not an effect status", out.Status))
	}
	for k, v := range out.Metrics {
		// CoreDet has no NaN or infinity. The decoder takes them, so they are refused here: the
		// result could not be recorded or sent on with one in it.
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return effect.Effect{}, failed(capability, fmt.Errorf("the answer's metric %q is %v, which CoreDet cannot carry", k, v))
		}
	}
	e := effect.Effect{Status: st, Message: out.Message}
	if len(out.Metrics) > 0 {
		e.Record = &tsir.EffectRecord{Metrics: out.Metrics}
	}
	// Evidence is how ANetLink knows what it is telling us: which protocol carried the call, whether
	// the device itself acknowledged, what a readback actually reported, and which vendor
	// correction changed the number. Without it a corrected reading would reach the requester as a
	// bare value with nothing saying it had been corrected.
	if out.Evidence != nil {
		ev := *out.Evidence
		e.Evidence = &ev
	}
	return e, nil
}

// failed is backend_failed for a call that went wrong on this side of C1, or whose answer this side
// cannot read: the requester is told the service failed, and this node's log says why.
func failed(capability string, err error) error {
	return &provider.RefusalError{Reason: reasonBackendFailed, Err: fmt.Errorf("anetlink: invoke %s: %w", capability, err)}
}

// get is a GET whose 200 answer is CBOR; any other answer is a *C1Error.
func (p *Provider) get(ctx context.Context, path string) ([]byte, error) {
	resp, err := p.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, refusalOf(resp, path)
	}
	b, err := readBody(resp)
	if err != nil {
		return nil, &C1Error{Path: path, Status: resp.StatusCode, Err: err}
	}
	return b, nil
}

func (p *Provider) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := p.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := p.cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anetlink: %s %s on %s: %w", method, path, p.sock, err)
	}
	return resp, nil
}

// request is one C1 request: Host anetlink, and a CBOR body when there is one.
func (p *Provider) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://anetlink"+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentCBOR)
	}
	return req, nil
}

var (
	errTooLong = errors.New("the answer is over C1's 1 MiB")
	errNotCBOR = errors.New("the answer is not application/cbor")
)

// readBody reads a CBOR answer of at most maxBody bytes.
func readBody(resp *http.Response) ([]byte, error) {
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != contentCBOR {
		return nil, errNotCBOR
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, errTooLong
	}
	return b, nil
}

// C1Error is a C1 answer that is not a success, or a 200 whose body is not the object it should be:
// the path, the HTTP status and ANetLink's reason, or "" when the answer was no C1 v1 refusal at all.
type C1Error struct {
	Path   string
	Status int
	Reason string
	// Err is why a 200 answer could not be read; nil for a refusal.
	Err error
}

func (e *C1Error) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("anetlink: %s: the answer is not C1 v1: %v", e.Path, e.Err)
	case e.Reason == "":
		return fmt.Sprintf("anetlink: %s: HTTP %d, not a C1 v1 refusal", e.Path, e.Status)
	}
	return fmt.Sprintf("anetlink: %s: C1 %s (HTTP %d)", e.Path, e.Reason, e.Status)
}

func (e *C1Error) Unwrap() error { return e.Err }

// NotC1v1 reports a far side that answers but does not speak C1 v1: ANetLink's own c1_version
// refusal (an anetlinkd newer than this client), or an answer that is not C1 v1 at all (an older
// one, C1 v0). The two programs are on different releases (ux §1.4 J, c1_version), which is what
// the daemon's start and doctor's anetlink.socket say.
func NotC1v1(err error) bool {
	var e *C1Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Reason == "c1_version" || e.Reason == "" && e.Status != http.StatusOK || e.Err != nil
}

// refusalOf reads an answer that is not a success, once: a CBOR Refusal whose reason is a C1 v1 code
// sent with that code's status gives its Reason; anything else gives none, which the daemon reports
// as backend_failed.
func refusalOf(resp *http.Response, path string) *C1Error {
	e := &C1Error{Path: path, Status: resp.StatusCode}
	b, err := readBody(resp)
	if err != nil {
		return e
	}
	if r, err := decodeRefusal(b); err == nil && c1Status[r.Reason] == resp.StatusCode {
		e.Reason = r.Reason
	}
	return e
}

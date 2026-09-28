package daemon

// hub_client.go is the daemon's HTTP client to the official Hub (wire types in internal/hubapi; the Hub
// itself is a separate service, ANetResearch/ANetHub): registry publish,
// verifiable-review upload, and the shared request helpers the relay client (relay.go) builds on. v0.1
// is centralized, so the Hub is the single service every daemon talks to.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// maxHubResponse bounds a decoded Hub response body. It is generous because `find` / `GET /agents/{aid}`
// can return many agents or reviews carrying full interaction transcripts, and a relay poll may now
// return messages carrying inline binary ATTACHMENTS (base64). The daemon polls every few seconds and
// acks, so the undelivered backlog stays small in practice; this caps a single poll response.
const maxHubResponse = 256 << 20 // 256 MiB

// RegisterWithHub publishes this agent's AgentCard, KEL and encryption key set to the Hub so it can be
// discovered, reached and reviewed. The Hub derives the AID from the KEL and rejects a mismatch, and
// verifies the relayauth v2 signature against that KEL — so this cannot claim (or overwrite) another
// agent's AID.
// invite is an admission token, sent only when the operator supplied one.
// A hub that admits openly ignores it; a hub that requires one and does not
// already know this AID refuses without it. It is deliberately NOT persisted:
// it is spent on arrival, and a token sitting in config.json is a credential
// kept long after the thing it bought.
func (d *Daemon) RegisterWithHub(ctx context.Context, hubURL, name string, caps []string, invite string) error {
	// One registration at a time — see Daemon.regMu.
	d.regMu.Lock()
	defer d.regMu.Unlock()
	return d.registerWithHubLocked(ctx, hubURL, name, caps, invite)
}

// registerWithHubLocked is RegisterWithHub with regMu already held.
//
// Split out so the startup refresh can read the config and register under
// the same lock. Reading it outside left a window: the refresh could load
// the capability list, an explicit `hub-register` could then write a new
// one and publish it, and the refresh's request — still carrying the old
// list — would land afterwards and undo it.
func (d *Daemon) registerWithHubLocked(ctx context.Context, hubURL, name string, caps []string, invite string) error {
	kelB, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return err
	}
	// Only public capabilities are published (0017 Q14, published_caps.go).
	caps = d.publicOnly(caps)
	body := hubapi.RegisterRequest{
		AID:    d.AID(),
		Name:   name,
		Caps:   caps,
		KEL:    base64.StdEncoding.EncodeToString(kelB),
		Invite: invite,
	}
	// The key set is restated on every registration, so a hub that lost
	// it, or a registration after a restart, carries the current one. The
	// hub answers an identical set at the same seq as unchanged.
	keysSeq := uint64(0)
	if d.enc != nil {
		if ks := d.enc.SignedSet(); len(ks) > 0 {
			body.EncKeys = base64.StdEncoding.EncodeToString(ks)
			keysSeq = d.enc.Seq()
		}
	}
	// The card carries the same claims, signed by the node making them.
	// The request signature proves who is calling; only this proves what
	// they said. See card.go.
	if card, cerr := d.signedCard(name, caps); cerr == nil {
		body.Card = json.RawMessage(card)
	} else {
		log.Printf("anet: registering without a signed card: %v", cerr)
	}
	// The A2A network card (a2a_card.go), when this node has a public
	// skill. The hub reports what it did with it in card_status.
	a2aCard, a2aSeq := d.cardForRegistration(hubURL, name, false)
	body.A2ACard = a2aCard
	if err := d.screenPublication("this node's registration", body); err != nil {
		return err
	}
	var out hubapi.RegisterResponse
	if err := d.hubSigned(ctx, hubURL, http.MethodPost, "/register", relayauth.ActionRegister, body, &out); err != nil {
		return err
	}
	d.afterCardAnswer(ctx, hubURL, body, a2aSeq, out)
	// A refused key set does not fail the registration (the hub reports it
	// per field). The registration still stands, so it is logged and the
	// set is published again through POST /agents/{aid}/keys, whose
	// answer is unambiguous.
	if keysSeq != 0 {
		if keysAccepted(out.KeysStatus) {
			d.publishedKeySeq.Store(keysSeq)
		} else {
			log.Printf("anet: %s did not take this node's encryption keys at registration (keys_status %q: %s); publishing them separately",
				hubURL, out.KeysStatus, out.KeysError)
			if err := d.publishKeys(ctx, hubURL); err != nil {
				log.Printf("anet: publish encryption keys to %s: %v", hubURL, err)
			}
		}
	}
	return nil
}

// keysAccepted reads the per-field status a wire-2 hub returns for the
// enc_keys of a registration: stored, or identical to what it holds.
func keysAccepted(status string) bool {
	return status == hubapi.KeysStatusOK || status == hubapi.KeysStatusUnchanged
}

// publishKeys sends the current signed key set to the hub (POST
// /agents/{aid}/keys). 200 means the hub holds this set; 409 means it holds
// a set this one does not supersede, which after a lost key ring file is
// expected until the clock passes the old seq (seal.NextSeq).
func (d *Daemon) publishKeys(ctx context.Context, hubURL string) error {
	if d.enc == nil {
		return fmt.Errorf("anet: no key ring")
	}
	ks, seq := d.enc.SignedSet(), d.enc.Seq()
	if len(ks) == 0 {
		return fmt.Errorf("anet: key ring has no signed set")
	}
	body := hubapi.KeysPublishRequest{KeySet: base64.StdEncoding.EncodeToString(ks)}
	if err := d.hubSigned(ctx, hubURL, http.MethodPost, "/agents/"+url.PathEscape(d.AID())+"/keys",
		relayauth.ActionKeys, body, nil); err != nil {
		return err
	}
	d.publishedKeySeq.Store(seq)
	return nil
}

// PublishProfile uploads this agent's self-authored profile (summary/readme/pricing) to the Hub,
// authenticated with relayauth v2 against the registered KEL. Pricing is display-only text.
func (d *Daemon) PublishProfile(ctx context.Context, hubURL, summary, readme, pricing string) error {
	body := map[string]any{
		"aid":     d.AID(),
		"summary": summary,
		"readme":  readme,
		"pricing": pricing,
	}
	if err := d.screenPublication("this node's profile", body); err != nil {
		return err
	}
	return d.hubSigned(ctx, hubURL, http.MethodPost, "/profile", relayauth.ActionProfile, body, nil)
}

// UploadReview sends the provider-signed receipt and this agent's signed review for a completed
// outbound interaction to the Hub, which verifies both signatures and that the review is anchored to
// the receipt. No task content is sent: the request TaskDoc and the deliverable stay with the two
// parties (A2A-DESIGN §9, row 评价), and the hub reports the content binding of every review as
// UNVERIFIED.
func (d *Daemon) UploadReview(ctx context.Context, hubURL, interactionID string) error {
	if d.ix == nil {
		return fmt.Errorf("anet: interactions store unavailable")
	}
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return err
	}
	if len(ix.Receipt) == 0 {
		return fmt.Errorf("anet: no receipt for %s (run `results` first)", interactionID)
	}
	if len(ix.Review) == 0 {
		return fmt.Errorf("anet: no review for %s (run `review` first)", interactionID)
	}
	body := hubapi.UploadReviewRequest{
		Receipt: base64.StdEncoding.EncodeToString(ix.Receipt),
		Review:  base64.StdEncoding.EncodeToString(ix.Review),
	}
	return d.hubPost(ctx, hubURL, "/reviews", body, nil)
}

// hubPost POSTs a JSON body to hubURL+path without authentication, treats
// a non-2xx as an error (surfacing the Hub's message), and decodes a 2xx
// body into out when out != nil.
func (d *Daemon) hubPost(ctx context.Context, hubURL, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	target := strings.TrimRight(hubURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return d.hubDo(req, path, out)
}

// hubGet GETs hubURL+path (with optional query) and decodes a 2xx body into out.
func (d *Daemon) hubGet(ctx context.Context, hubURL, path string, query url.Values, out any) error {
	target := strings.TrimRight(hubURL, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	return d.hubDo(req, path, out)
}

// hubSigned sends one request authenticated with relayauth v2
// (A2A-DESIGN §3.7): the signature covers the action, this node's AID,
// the hub's AID, the time, and a hash of the method, the request target as
// sent and the exact body bytes. It travels in the X-ANet-* headers, so the
// body is signed as sent and never contains its own signature.
//
// The hub's AID is part of the preimage, so a signature captured by one hub
// cannot be replayed at another; it comes from GET /hub/identity and is
// fetched before the first signed call to that hub.
func (d *Daemon) hubSigned(ctx context.Context, hubURL, method, path, action string, body, out any) error {
	hubAID, _, err := d.hubIdentity(ctx, hubURL)
	if err != nil {
		return err
	}
	var raw []byte
	if body != nil {
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	target := strings.TrimRight(hubURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Wall clock, not d.nowMS: the hub checks the time against its own
	// clock, and the test clock only moves the sealed wire.
	//
	// Strictly increasing per process. Ed25519 signatures are
	// deterministic, so two identical requests signed in the same
	// millisecond would carry the same signature, and the hub's replay
	// cache would refuse the second one as a replay.
	ts := uint64(time.Now().UnixMilli())
	for {
		last := d.lastSignTS.Load()
		if ts <= last {
			ts = last + 1
		}
		if d.lastSignTS.CompareAndSwap(last, ts) {
			break
		}
	}
	sig, seq := d.self.Sign(relayauth.PreimageV2(action, d.AID(), hubAID, ts, method, req.URL.RequestURI(), raw))
	req.Header.Set(hubapi.HeaderAID, d.AID())
	req.Header.Set(hubapi.HeaderTS, strconv.FormatUint(ts, 10))
	req.Header.Set(hubapi.HeaderSeq, strconv.FormatUint(seq, 10))
	req.Header.Set(hubapi.HeaderSig, relayauth.EncodeSig(sig))
	return d.hubDo(req, path, out)
}

// errHubWire is returned for every call to a hub that speaks a wire
// contract below 2. Such a hub stores and forwards plaintext, and this
// daemon only speaks sealed envelopes; there is no fallback.
var errHubWire = errors.New("hub does not speak wire 2")

// hubDo sends the request with this daemon's wire version, refuses a hub
// below wire 2, and decodes a 2xx body into out.
func (d *Daemon) hubDo(req *http.Request, path string, out any) error {
	// Every request states the C2 contract version this daemon speaks.
	req.Header.Set(hubapi.WireVersionHeader, strconv.Itoa(hubapi.WireVersion))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("anet: hub %s: %w", path, err)
	}
	defer resp.Body.Close()
	// Cap the response to bound memory against a hostile/broken Hub. It must comfortably exceed the
	// largest legitimate body — `find`/`GET /agents/{aid}` can list many agents or reviews carrying full
	// interaction transcripts — so it is generous; a truncated body would fail to JSON-decode.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxHubResponse))
	if werr := d.checkHubWire(req.URL.Host, resp.StatusCode, resp.Header.Get(hubapi.WireVersionHeader), respBody); werr != nil {
		return werr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBody, &e)
		if e.Error != "" {
			return &hubError{path: path, code: resp.StatusCode, msg: e.Error, retryAfter: resp.Header.Get("Retry-After")}
		}
		return &hubError{path: path, code: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
	}
	if out != nil {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

// hubError is a non-2xx hub answer. The status code is kept so a caller can
// tell "recipient unknown" (404) from "try later" (429, 507).
type hubError struct {
	path       string
	code       int
	msg        string
	retryAfter string
}

func (e *hubError) Error() string {
	s := fmt.Sprintf("anet: hub %s returned %d", e.path, e.code)
	if e.msg != "" {
		s = fmt.Sprintf("anet: hub %s rejected (%d): %s", e.path, e.code, e.msg)
	}
	if e.retryAfter != "" {
		s += " (retry after " + e.retryAfter + "s)"
	}
	return s
}

// hubStatus returns the HTTP status of a hub error, or 0.
func hubStatus(err error) int {
	var he *hubError
	if errors.As(err, &he) {
		return he.code
	}
	return 0
}

// checkHubWire refuses a hub below wire 2 and reports a newer one once.
//
// A 426 is the hub refusing this daemon's version; a stated version below 2
// is this daemon refusing the hub. Both end the call with errHubWire. A
// response without the header (a pre-versioning hub, or an endpoint the hub
// serves outside its relay mux such as /hub/identity) is not refused here:
// a hub that old rejects the wire-2 request bodies on every relay call, so
// nothing is delivered through it either way.
func (d *Daemon) checkHubWire(host string, code int, v string, body []byte) error {
	if code == http.StatusUpgradeRequired {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		d.noteWireRefusal(host, fmt.Sprintf("the hub answered 426: %s", strings.TrimSpace(e.Error+" "+string(body))))
		return fmt.Errorf("anet: hub %s refused this daemon's wire version %d (426): %w", host, hubapi.WireVersion, errHubWire)
	}
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 2 {
		d.noteWireRefusal(host, "the hub states wire "+v)
		return fmt.Errorf("anet: hub %s speaks wire %s; this daemon requires wire 2 (sealed envelopes) and does not fall back to plaintext: %w",
			host, v, errHubWire)
	}
	if n > hubapi.WireVersion {
		d.wireWarnOnce.Do(func() {
			log.Printf("anet: hub %s speaks wire %d, this daemon speaks %d — upgrade this daemon", host, n, hubapi.WireVersion)
		})
	}
	return nil
}

// noteWireRefusal logs a refused hub once per process, so a relay loop
// polling a wire-1 hub every second does not repeat the line.
func (d *Daemon) noteWireRefusal(host, why string) {
	d.wireRefuseOnce.Do(func() {
		log.Printf("anet: refusing to operate against hub %s: %s; this daemon requires ANetHub wire 2 "+
			"and will not send or accept plaintext relay messages", host, why)
	})
}

// hubIdent is one hub's identity as this node pinned it.
type hubIdent struct {
	aid string
	kel []identity.SignedEvent
}

// hubIdentity returns the AID and KEL of the hub at hubURL, fetching GET
// /hub/identity on first use and pinning the KEL in peer_identity
// (pinned_reason "hub"). A later fetch that serves a KEL forking from the
// pinned one is refused; one that serves an older prefix keeps the pinned
// KEL (A2A-DESIGN §3.8).
func (d *Daemon) hubIdentity(ctx context.Context, hubURL string) (string, []identity.SignedEvent, error) {
	key := strings.TrimRight(hubURL, "/")
	d.hubIDMu.Lock()
	if h, ok := d.hubIDs[key]; ok {
		d.hubIDMu.Unlock()
		return h.aid, h.kel, nil
	}
	d.hubIDMu.Unlock()
	var out hubapi.HubIdentity
	cctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	if err := d.hubGet(cctx, key, "/hub/identity", nil, &out); err != nil {
		return "", nil, fmt.Errorf("anet: learn the hub's identity: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(out.KEL)
	if err != nil || out.AID == "" {
		return "", nil, fmt.Errorf("anet: hub %s served an unreadable identity", key)
	}
	kel, err := seal.ParseKEL(raw)
	if err != nil {
		return "", nil, fmt.Errorf("anet: hub %s identity KEL: %w", key, err)
	}
	resolved, err := d.pinPeerKEL(out.AID, kel, interactions.PinHub)
	if err != nil {
		return "", nil, fmt.Errorf("anet: hub %s identity: %w", key, err)
	}
	d.hubIDMu.Lock()
	if d.hubIDs == nil {
		d.hubIDs = map[string]hubIdent{}
	}
	d.hubIDs[key] = hubIdent{aid: out.AID, kel: resolved}
	d.hubIDMu.Unlock()
	return out.AID, resolved, nil
}

// hubCallTimeoutShort bounds hub calls made on a send path, where the hub
// is a fallback and must not stall delivery.
const hubCallTimeoutShort = 10 * time.Second

// SetVisibility tells the hub how far this node is willing to be
// published: hub-local, federated or public.
//
// The agent decides and the agent signs, because this is the setting
// that decides whether other hubs learn it exists — one that anyone else
// could change is not a setting. Default is hub-local, and the
// conservative default is the point: a card cannot be recalled from a hub
// that already has it.
func (d *Daemon) SetVisibility(ctx context.Context, visibility string) error {
	hub := d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no hub configured")
	}
	body := map[string]any{"visibility": visibility}
	return d.hubSigned(ctx, hub, http.MethodPost, "/agents/"+url.PathEscape(d.AID())+"/visibility",
		relayauth.ActionVisibility, body, nil)
}

// LeaveHub stops this node being deliverable at a hub.
//
// The other half of registering, and it was missing. A node repointed at
// a second hub left a live registration behind: the old hub went on
// listing it and went on accepting delegations for it into a mailbox that
// would never be polled again. Work addressed there was accepted, queued,
// and silently swallowed — found in production, by a cross-hub call that
// was relayed into a dead mailbox instead of crossing.
//
// hubURL is given rather than read from the config, because by the time
// you want to leave a hub you have usually already pointed the config at
// the new one. Leaving is something you do to a hub you are no longer
// configured for.
//
// The evidence stays where it is. What goes is the routing.
func (d *Daemon) LeaveHub(ctx context.Context, hubURL string) (map[string]any, error) {
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if hubURL == "" {
		return nil, fmt.Errorf("anet: name the hub to leave")
	}
	var out map[string]any
	if err := d.hubSigned(ctx, hubURL, http.MethodPost, "/agents/"+url.PathEscape(d.AID())+"/deregister",
		relayauth.ActionDeregister, map[string]any{}, &out); err != nil {
		return out, err
	}
	d.forgetHubIfCurrent(hubURL)
	return out, nil
}

// forgetHubIfCurrent stops this node treating a hub it just left as its own.
//
// Deregistering removed the routing at the hub. Locally, hub_url stayed —
// so the relay loop went on polling the hub that had just refused this
// node, once a second, forever: every poll rejected, every rejection a
// log line, about 7.9 MB of daemon.log a day with no rotation, surviving
// restarts because the address was still in config.json. `anet status`
// still reported the hub and the opening banner still said "Registered
// with Hub", because both read hub_url and nothing else.
//
// Conditional on purpose. The documented way to move house is to register
// with the new hub and THEN leave the old one, and in that order hub_url
// already names the new hub — clearing it unconditionally would unregister
// the node from the hub it had just joined. So this only forgets the hub
// the node is actually pointed at.
//
// Found by the release matrix, on min and paid, on Ubuntu and Debian.
func (d *Daemon) forgetHubIfCurrent(left string) {
	left = strings.TrimRight(strings.TrimSpace(left), "/")
	// Under the config write lock (cfgWrite), so a concurrent write can
	// neither save a copy that still names the hub nor be lost. In force
	// even if the save fails: the node has left that hub, whatever the
	// file says.
	d.cfgWrite.Lock()
	defer d.cfgWrite.Unlock()
	d.mu.Lock()
	current := strings.TrimRight(strings.TrimSpace(d.cfg.HubURL), "/")
	if current == "" || current != left {
		d.mu.Unlock()
		return
	}
	d.cfg.HubURL = ""
	cfg := d.cfg
	d.mu.Unlock()

	// Stop polling before persisting: the loop is what produces the log
	// flood, and a failure to write the config should not leave it running.
	d.stopRelayLoop()
	if err := SaveConfig(d.layout, cfg); err != nil {
		log.Printf("anet: left %s but could not clear hub_url: %v", left, err)
	}
}

// AdvertisePeerAddress publishes where this node can be dialled directly,
// so peers on other machines can find it.
//
// The daemon does this rather than the peer process, because publishing
// is a signed statement about this node's identity and the peer process
// holds no key. It learns the address it is reachable at from its
// operator and tells nobody; the daemon says so under its own signature.
// Keeping it this way means the peer process — the one carrying other
// people's traffic — never needs anything that could speak as this node.
//
// In the kernel and untagged, alongside hub-leave and the profile
// publish, because it is the same kind of thing: a signed statement
// about this node, sent to its own hub. It names no transport and does
// not reach the p2p module — a build with -tags no_p2p can still publish
// an address, and one without a hub is told there is nowhere to publish
// it. What consumes the address is a module; saying it is not.
//
// An empty address withdraws the entry. Withdrawing and deregistering are
// different decisions: a node can stop accepting direct connections and
// go on receiving work through the hub.
func (d *Daemon) AdvertisePeerAddress(ctx context.Context, addr string) (map[string]any, error) {
	hubURL := strings.TrimRight(strings.TrimSpace(d.config().HubURL), "/")
	if hubURL == "" {
		return nil, fmt.Errorf("anet: this node has no hub, so there is nowhere to publish an address")
	}
	body := map[string]any{"addr": strings.TrimSpace(addr)}
	var out map[string]any
	if err := d.hubSigned(ctx, hubURL, http.MethodPost, "/agents/"+url.PathEscape(d.AID())+"/p2p",
		relayauth.ActionP2P, body, &out); err != nil {
		return out, err
	}
	return out, nil
}

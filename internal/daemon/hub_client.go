package daemon

// hub_client.go is the daemon's HTTP client to the official Hub (wire types in internal/hubapi; the Hub
// itself is a separate service, ANetResearch/ANetHub): registry publish,
// verifiable-review upload, and the shared request helpers the relay client (relay.go) builds on. v0.1
// is centralized, so the Hub is the single service every daemon talks to.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// registerWithKEL sends one POST /register carrying kel (identity.MarshalKEL
// bytes, base64url on the wire, core G2) and, when the hub asked for one, a
// proof of work, and returns the hub's answer as it came (the join loop
// reads it, hubjoin.go). It publishes this agent's encryption key set and
// A2A network card with it, so it can be discovered, reached and reviewed.
// The hub derives the AID from the KEL, refuses a mismatch and verifies the
// relayauth v2 signature against that KEL — so this cannot claim (or
// overwrite) another agent's AID. There is no invitation in it: a hub
// admits by AID.
//
// An accepted registration is followed through here: the card's answer
// (afterCardAnswer) and the key set's (publishKeys when the hub did not take
// it). A body this node refuses to publish (screenPublication) is a local
// failure, not an answer of the hub.
//
// The caller holds d.regMu (see Daemon.regMu).
func (d *Daemon) registerWithKEL(ctx context.Context, hubURL, name string, caps []string, kel []byte, pow *powProof) hubReply {
	// Only public capabilities are published (an internal note item, published_caps.go).
	caps = d.publicOnly(caps)
	body := hubapi.RegisterRequest{
		AID:  d.AID(),
		Name: name,
		Caps: caps,
		KEL:  hubapi.B64(kel),
		PoW:  wirePow(pow),
	}
	// The key set is restated on every registration, so a hub that lost
	// it, or a registration after a restart, carries the current one. The
	// hub answers an identical set at the same seq as unchanged. It goes
	// in base64url with its prev_seq (FS-K5), and not at all while key
	// sets are held (keysHeld): the registration then restates nothing.
	keysSeq := uint64(0)
	var keySet []byte
	withKeys := func() {
		keysSeq, keySet, body.EncKeys, body.PrevSeq = 0, nil, "", nil
		if d.enc == nil || d.keysHeld() {
			return
		}
		if ks, seq, prev := d.enc.published(); len(ks) > 0 {
			body.EncKeys, body.PrevSeq = hubapi.B64(ks), &prev
			keysSeq, keySet = seq, ks
		}
	}
	withKeys()
	// The A2A network card (a2a_card.go), when this node has a public
	// skill: the node's claims, signed by the node making them. The request
	// signature proves who is calling; only the card proves what they said.
	// The hub reports what it did with it in card_status.
	a2aCard, a2aSeq := d.cardForRegistration(hubURL, name, false)
	body.A2ACard = a2aCard
	if err := d.screenPublication("this node's registration", body); err != nil {
		return hubReply{Local: true, Err: err}
	}
	reply := d.hubSignedRaw(ctx, hubURL, http.MethodPost, "/register", relayauth.ActionRegister, body)
	if stored, ok := keysRollbackOf(reply); ok && keySet != nil {
		// The hub holds a newer set of this identity than the one sent, and
		// wrote nothing: the registration goes again once with a set above
		// the hub's (adoptKeysSeq), or not at all.
		if d.adoptKeysSeq(hubURL, stored) {
			withKeys()
			if err := d.screenPublication("this node's registration", body); err != nil {
				return hubReply{Local: true, Err: err}
			}
			reply = d.hubSignedRaw(ctx, hubURL, http.MethodPost, "/register", relayauth.ActionRegister, body)
		}
	}
	if reply.Status != http.StatusOK {
		return reply
	}
	var out hubapi.RegisterResponse
	if json.Unmarshal(reply.Body, &out) != nil || out.Status != "registered" {
		return reply // the join loop reads it as the hub misbehaving
	}
	d.afterCardAnswer(ctx, hubURL, body, a2aSeq, out)
	// The hub's count of this node's withdrawn credentials: fewer than were
	// pushed means it lost some (SSR-D21).
	d.noteRegisterRevocations(out.Revocations)
	// A refused key set does not fail the registration (the hub reports it
	// per field). The registration still stands, so it is logged and the
	// set is published again through POST /agents/{aid}/keys, whose
	// answer is unambiguous. Either answer is checked for the mailbox
	// signing key (SSR-D4, noteKeysAtHub).
	switch {
	case keysSeq == 0:
		// No key set yet (the mailbox key waits for the hub, mbox.go).
		d.noteKeysAtHub(nil, false)
	case keysAccepted(out.KeysStatus):
		d.publishedKeySeq.Store(keysSeq)
		d.noteKeysAtHub(keySet, true)
		if err := d.enc.markRemoved(keySet, d.nowMS()); err != nil {
			log.Printf("anet: record the keys %s no longer holds: %v", hubURL, err)
		}
	default:
		log.Printf("anet: %s did not take this node's encryption keys at registration (keys_status %q: %s); publishing them separately",
			hubURL, out.KeysStatus, out.KeysError)
		if err := d.publishKeys(ctx, hubURL); err != nil {
			log.Printf("anet: publish encryption keys to %s: %v", hubURL, err)
		}
	}
	return reply
}

// keysAccepted reads the per-field status a wire-2 hub returns for the
// enc_keys of a registration: stored, or identical to what it holds.
func keysAccepted(status string) bool {
	return status == hubapi.KeysStatusOK || status == hubapi.KeysStatusUnchanged
}

// errKeysHeld is a key set publication while key sets are held (keysHeld):
// nothing is sent.
var errKeysHeld = errors.New("anet: key sets are held until the emergency rotation publishes its new keys")

// publishKeys sends the current signed key set to the hub (POST
// /agents/{aid}/keys): base64url, with the prev_seq it follows (FS-K5).
// 200 with keys_status ok or unchanged means the hub holds this set, and
// the keys it does not list are no longer acceptable there (FS-K6). 409
// keys_rollback means the hub holds a newer set of this identity than this
// ring signed (a lost or restored key ring): the set is signed again above
// it and sent once more (adoptKeysSeq). 400 bad_request means the hub
// refused the set: when its mailbox key is behind the hub's, a new one is
// made above it and the set sent once more (healMboxEpoch, SSR-W10a).
// Every answer is checked for the mailbox signing key (SSR-D4,
// noteKeysAtHub). Nothing is sent while key sets are held (keysHeld).
func (d *Daemon) publishKeys(ctx context.Context, hubURL string) error {
	if d.enc == nil {
		return fmt.Errorf("anet: no key ring")
	}
	if d.keysHeld() {
		return errKeysHeld
	}
	err := d.publishKeysOnce(ctx, hubURL)
	if stored, ok := keysRollbackSeq(err); ok && d.adoptKeysSeq(hubURL, stored) {
		err = d.publishKeysOnce(ctx, hubURL)
	}
	// The hub refused the set as invalid: the mailbox key may be one it
	// cannot take (SSR-W9), which a new key above the hub's heals, once
	// (SSR-W10a).
	if keySetRefused(err) && d.healMboxEpoch(ctx, hubURL) {
		if rerr := d.resignKeyRing(); rerr != nil {
			return rerr
		}
		err = d.publishKeysOnce(ctx, hubURL)
	}
	return err
}

// keySetRefused reports whether err is the hub refusing a key set as
// invalid: POST /agents/{aid}/keys answers 400 bad_request, saying why
// (forward-secrecy FS-H4, sealed-sender SSR-W9).
func keySetRefused(err error) bool {
	return hubStatus(err) == http.StatusBadRequest && hubReason(err) == "bad_request"
}

// publishKeysOnce is one POST /agents/{aid}/keys of the ring's current set.
func (d *Daemon) publishKeysOnce(ctx context.Context, hubURL string) error {
	ks, seq, prev := d.enc.published()
	if len(ks) == 0 {
		d.noteKeysAtHub(nil, false)
		return fmt.Errorf("anet: key ring has no signed set")
	}
	body := hubapi.KeysPublishRequest{Keys: hubapi.B64(ks), PrevSeq: prev}
	var out hubapi.KeysPublishResponse
	if err := d.hubSigned(ctx, hubURL, http.MethodPost, "/agents/"+url.PathEscape(d.AID())+"/keys",
		relayauth.ActionKeys, body, &out); err != nil {
		d.noteKeysAtHub(ks, false)
		return err
	}
	if !keysAccepted(out.KeysStatus) {
		d.noteKeysAtHub(ks, false)
		return fmt.Errorf("anet: %s answered keys_status %q to this node's key set", hubURL, out.KeysStatus)
	}
	d.publishedKeySeq.Store(seq)
	d.noteKeysAtHub(ks, true)
	if err := d.enc.markRemoved(ks, d.nowMS()); err != nil {
		log.Printf("anet: record the keys %s no longer holds: %v", hubURL, err)
	}
	return nil
}

// counterKeysRollbackUnverified counts the hub's keys_rollback answers this
// node followed without finding that its own data went back (FS-B2).
const counterKeysRollbackUnverified = "keys-rollback-unverified"

// adoptKeysSeq answers the hub's 409 keys_rollback with stored_seq (FS-B2,
// the branch that ends nothing): the ring signs its set again above
// stored_seq, with prev_seq stored_seq, so the next publication follows the
// hub's set. It reports whether there is a new set to send. Counted and
// logged; no task ends and nothing is shown, since only a set this identity
// signed would say this node's data went back.
func (d *Daemon) adoptKeysSeq(hubURL string, storedSeq uint64) bool {
	in := d.ringInputsAt(d.nowMS())
	in.hold = d.keysHeld()
	adopted, err := d.enc.adoptStoredSeq(storedSeq, in)
	if err != nil {
		log.Printf("anet: sign a key set above the one %s holds: %v", hubURL, err)
		return false
	}
	if adopted {
		d.countErr(counterKeysRollbackUnverified, fmt.Errorf("%s holds key set seq %d of this identity, newer than this node signed; the next set follows it",
			hubURL, storedSeq))
	}
	return adopted
}

// keysRollbackSeq reads a hub's 409 keys_rollback refusal of a key set:
// the stored_seq it names.
func keysRollbackSeq(err error) (uint64, bool) {
	var he *hubError
	if !errors.As(err, &he) || he.code != http.StatusConflict || he.reason != hubapi.KeysRollbackReason || he.storedSeq == nil {
		return 0, false
	}
	return *he.storedSeq, true
}

// keysRollbackOf is keysRollbackSeq for a reply read as it came
// (/register).
func keysRollbackOf(r hubReply) (uint64, bool) {
	if r.Status != http.StatusConflict {
		return 0, false
	}
	return keysRollbackSeq(newHubError("/register", r))
}

// PublishProfile uploads this agent's self-authored profile (summary/readme) to the Hub,
// authenticated with relayauth v2 against the registered KEL.
func (d *Daemon) PublishProfile(ctx context.Context, hubURL, summary, readme string) error {
	body := map[string]any{
		"aid":     d.AID(),
		"summary": summary,
		"readme":  readme,
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
	return d.hubDo(req, hubURL, path, out)
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
	return d.hubDo(req, hubURL, path, out)
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
	if rep, ok := out.(*hubReply); ok {
		// The caller reads the hub's answer itself (hubSignedRaw).
		*rep = d.hubExchange(req, hubURL, path, -1)
		return nil
	}
	return d.hubDo(req, hubURL, path, out)
}

// hubSignedRaw is hubSigned answering with the hub's reply as it came, for a
// caller that reads the hub's codes itself (the join loop). Status 0: no
// HTTP answer, Err says why.
func (d *Daemon) hubSignedRaw(ctx context.Context, hubURL, method, path, action string, body any) hubReply {
	var rep hubReply
	if err := d.hubSigned(ctx, hubURL, method, path, action, body, &rep); err != nil {
		return hubReply{Err: err}
	}
	return rep
}

// errHubWire is returned for every call to a hub that does not speak this
// daemon's wire (core CORE-V1): it answered 426, or it states another
// version. There is no fallback to another wire.
var errHubWire = errors.New("hub does not speak wire 3")

// hubWireError is a call refused over the wire contract (errors.Is
// errHubWire): hubOlder when the hub states a wire below this daemon's
// (wire_too_new to the user), otherwise the hub is newer — it answered 426,
// or states a higher wire — and version is what it needs (wire_too_old's
// {version}): the anet release a 426 names, else the wire it states.
type hubWireError struct {
	hubOlder bool
	version  string
	msg      string
}

func (e *hubWireError) Error() string        { return e.msg }
func (e *hubWireError) Is(target error) bool { return target == errHubWire }

// wireVersionText is {version} for a hub that states only its wire: "wire 4".
func wireVersionText(n int) string { return "wire " + strconv.Itoa(n) }

// wireVersionTextRe is the form of wireVersionText.
var wireVersionTextRe = regexp.MustCompile(`^wire [0-9]{1,4}$`)

// validWireTooOldVersion reports whether v is a wire_too_old {version}: the
// anet release a 426 names (required_anet), or the wire a hub states.
func validWireTooOldVersion(v string) bool {
	return anetVersionRe.MatchString(v) || wireVersionTextRe.MatchString(v)
}

// hubWireCheck is CORE-V1 on one answer: a 426 (the hub refuses this
// daemon's wire), or an X-ANet-Wire other than this daemon's (the hub
// speaks another wire), ends the call. An answer without the header is
// not refused here: a hub of wire 3 stamps it on every response, so its
// absence is something in front of the hub (a proxy's error page) or not a
// hub at all, and the request fails on its status or its body.
func hubWireCheck(host string, rep hubReply) *hubWireError {
	if rep.Status == http.StatusUpgradeRequired {
		var b struct {
			RequiredAnet string `json:"required_anet"`
			RequiredWire *int   `json:"required_wire"`
		}
		_ = json.Unmarshal(rep.Body, &b)
		v := ""
		switch {
		case anetVersionRe.MatchString(b.RequiredAnet):
			v = b.RequiredAnet
		case b.RequiredWire != nil && *b.RequiredWire > hubapi.WireVersion && *b.RequiredWire < 10000:
			v = wireVersionText(*b.RequiredWire)
		}
		return &hubWireError{version: v, msg: fmt.Sprintf("anet: hub %s refused this daemon's wire version %d (426): %v",
			host, hubapi.WireVersion, errHubWire)}
	}
	if rep.Wire == "" {
		return nil
	}
	n, err := strconv.Atoi(rep.Wire)
	switch {
	case err != nil || n < hubapi.WireVersion:
		return &hubWireError{hubOlder: true, msg: fmt.Sprintf("anet: hub %s speaks wire %q; this daemon speaks wire %d only: %v",
			host, rep.Wire, hubapi.WireVersion, errHubWire)}
	case n > hubapi.WireVersion:
		v := ""
		if n < 10000 {
			v = wireVersionText(n)
		}
		return &hubWireError{version: v, msg: fmt.Sprintf("anet: hub %s speaks wire %d; this daemon speaks wire %d only: %v",
			host, n, hubapi.WireVersion, errHubWire)}
	}
	return nil
}

// hubUserAgent is the User-Agent of every hub request, on both sides: one
// fixed string, so that nothing in a request but its route tells requests
// apart (SSR-D25).
const hubUserAgent = "anet/" + Version

// counterHubRouteUnmapped counts the hub requests refused before they were
// sent because hubapi.Routes gives their route no side (SSR-D26), or the
// side it gives cannot carry them (a signature on the anonymous side).
const counterHubRouteUnmapped = "hub-route-unmapped"

// hubClient is the daemon's HTTP client of its hub (sealed-sender §4.3.11):
// two transports that never share a connection, a TLS session ticket or a
// cookie — the signed side for the requests that name this node, the
// anonymous side for the others (hubapi.Routes) — and the first-contact
// sessions (session_client.go). Its zero value is ready; the transports are
// built on first use.
type hubClient struct {
	mu    sync.Mutex
	sides [3]*http.Client // indexed by hubapi.SideSigned, hubapi.SideAnonymous
	// roots, when a test set it (setHubRoots), replaces the system's
	// certificate roots on both sides.
	roots *x509.CertPool
	// wireNoted holds the hubs a wire refusal was logged for, so a loop
	// polling every second logs it once; wireNeeds holds, per hub, the
	// anet release its last 426 named (wire_too_old's {version}).
	wireNoted sync.Map
	wireNeeds sync.Map
	sessions  sessionTable
}

// client is the HTTP client of one side, built on first use.
func (c *hubClient) client(side hubapi.Side) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sides[side] == nil {
		c.sides[hubapi.SideSigned] = newHubHTTPClient(hubapi.SideSigned, c.roots)
		c.sides[hubapi.SideAnonymous] = newHubHTTPClient(hubapi.SideAnonymous, c.roots)
	}
	return c.sides[side]
}

// newHubHTTPClient is one side's client: a transport of its own (its own
// connection pool), a TLS configuration of its own, no cookie jar. Only the
// signed side keeps TLS session tickets: a ticket resumed on the anonymous
// side would tie its connection to the signed one that earned it (SSR-D25).
//
// Neither side follows a redirect: the hub's 3xx is the answer, and the
// call fails on it. A redirect would send the request — its signature or
// session MAC, its envelope — to a route hubapi.Routes was never asked
// about, or to another host, which this node never contacts (SSR-D13,
// SSR-D26). A hub address that only redirects (http to https, say) is
// fixed by configuring the address it redirects to.
func newHubHTTPClient(side hubapi.Side, roots *x509.CertPool) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	tc := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	if side == hubapi.SideSigned {
		tc.ClientSessionCache = tls.NewLRUClientSessionCache(16)
	}
	t.TLSClientConfig = tc
	return &http.Client{Transport: t, CheckRedirect: noHubRedirect}
}

// noHubRedirect is the hub clients' CheckRedirect: never follow.
func noHubRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// hubExchange sends req to the hub at hubURL and returns its answer as it
// came: status, Retry-After, the hub's X-ANet-Wire and the body (bounded).
// route is the request's path below hubURL; kind is the access kind of a
// /relay/send (0 or 1) and -1 for every other route. The request goes on
// the side hubapi.SideOf gives it, with this daemon's wire version and its
// fixed User-Agent; a route without a side is not sent at all (Status 0,
// Local).
func (d *Daemon) hubExchange(req *http.Request, hubURL, route string, kind int) hubReply {
	resp, err := d.hubTransmit(req, hubURL, route, kind)
	if err != nil {
		var unmapped *hubRouteError
		return hubReply{Err: err, Local: errors.As(err, &unmapped)}
	}
	defer resp.Body.Close()
	// Cap the response to bound memory against a hostile/broken Hub. It must comfortably exceed the
	// largest legitimate body — a relay poll can carry envelopes with inline attachments — so it is
	// generous; a truncated body would fail to JSON-decode.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHubResponse))
	rep := hubReply{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After"),
		Wire: resp.Header.Get(hubapi.WireVersionHeader), Body: body}
	if we := hubWireCheck(req.URL.Host, rep); we != nil {
		d.noteHubWire(hubURL, we)
	}
	return rep
}

// hubRouteError is a request refused before it was sent: hubapi.Routes
// gives its route no side (SSR-D26), or its route is on the anonymous side
// and it carries naming, a header that names this node.
type hubRouteError struct{ method, route, naming string }

func (e *hubRouteError) Error() string {
	if e.naming != "" {
		return fmt.Sprintf("anet: %s %s goes on the anonymous side, which carries no %s; nothing was sent", e.method, e.route, e.naming)
	}
	return fmt.Sprintf("anet: %s %s is no hub route this node sends; nothing was sent", e.method, e.route)
}

// namingHeaders are the request headers that name this node: a relayauth
// signature and a first contact's session (core §2.12.1). None of them
// rides the anonymous side.
var namingHeaders = []string{hubapi.HeaderAID, hubapi.HeaderSeq, hubapi.HeaderSig, hubapi.HeaderSession, hubapi.HeaderMAC}

// hubTransmit sends req on the side hubapi.SideOf gives its route, with
// this daemon's wire version and its fixed User-Agent and without cookies,
// and returns the response unread. A route without a side is not sent
// (hubRouteError, counted hub-route-unmapped), and neither is a request for
// the anonymous side that carries a signature or a session: it would tie
// the anonymous connection to this node (SI-13), so it is refused before
// anything is sent, however it was built.
func (d *Daemon) hubTransmit(req *http.Request, hubURL, route string, kind int) (*http.Response, error) {
	_, side, ok := hubapi.SideOf(req.Method, route, d.AID(), d.knownHubAID(hubURL), kind)
	if !ok {
		err := &hubRouteError{method: req.Method, route: route}
		d.countErr(counterHubRouteUnmapped, err)
		return nil, err
	}
	if side == hubapi.SideAnonymous {
		for _, h := range namingHeaders {
			if len(req.Header.Values(h)) > 0 {
				err := &hubRouteError{method: req.Method, route: route, naming: h}
				d.countErr(counterHubRouteUnmapped, err)
				return nil, err
			}
		}
	}
	// Every request states the C2 contract version this daemon speaks.
	req.Header.Set(hubapi.WireVersionHeader, strconv.Itoa(hubapi.WireVersion))
	req.Header.Set("User-Agent", hubUserAgent)
	req.Header.Del("Cookie")
	resp, err := d.hubc.client(side).Do(req)
	if err == nil {
		// Every answer of the configured hub checks this node's clock
		// against the hub's (FS-K14).
		d.noteHubDate(hubURL, resp.Header.Get("Date"))
	}
	return resp, err
}

// moduleHubDo is module.PaymentSeam.HubDo and module.HubSeam.HubDo: a
// module's request to this node's hub goes out the way the daemon's own do
// (hubTransmit), on the side its route belongs to. It must be addressed to
// the configured hub. The caller closes the response body.
func (d *Daemon) moduleHubDo(req *http.Request) (*http.Response, error) {
	hub := strings.TrimRight(d.config().HubURL, "/")
	if hub == "" {
		return nil, errors.New("anet: no hub configured")
	}
	base, err := url.Parse(hub)
	if err != nil {
		return nil, fmt.Errorf("anet: the configured hub URL: %w", err)
	}
	prefix := strings.TrimRight(base.EscapedPath(), "/")
	path := req.URL.EscapedPath()
	if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host || !strings.HasPrefix(path, prefix+"/") {
		return nil, fmt.Errorf("anet: a module's hub request goes to this node's hub %s only", hub)
	}
	resp, err := d.hubTransmit(req, hub, strings.TrimPrefix(path, prefix), -1)
	if err != nil {
		return nil, err
	}
	// A hub of another wire is refused here as hubDo refuses it (CORE-V1):
	// the module gets the refusal, never an answer read under another wire.
	if we := hubWireCheck(req.URL.Host, hubReply{Status: resp.StatusCode, Wire: resp.Header.Get(hubapi.WireVersionHeader)}); we != nil {
		resp.Body.Close()
		d.noteHubWire(hub, we)
		return nil, we
	}
	return resp, nil
}

// hubDo sends the request (hubExchange), refuses a hub of another wire, and
// decodes a 2xx body into out. route is the path below hubURL.
func (d *Daemon) hubDo(req *http.Request, hubURL, route string, out any) error {
	rep := d.hubExchange(req, hubURL, route, -1)
	if rep.Status == 0 {
		if rep.Local {
			return rep.Err
		}
		return fmt.Errorf("anet: hub %s: %w", route, rep.Err)
	}
	if werr := hubWireCheck(req.URL.Host, rep); werr != nil {
		return werr
	}
	if rep.Status < 200 || rep.Status >= 300 {
		return newHubError(route, rep)
	}
	if out != nil {
		return json.Unmarshal(rep.Body, out)
	}
	return nil
}

// newHubError is a non-2xx answer as an error: its status, its reason code
// and sentence (core G1), and its Retry-After.
func newHubError(path string, rep hubReply) *hubError {
	reason, msg := hubapi.ParseErrorBody(rep.Body)
	e := &hubError{path: path, code: rep.Status, reason: reason, msg: msg, retryAfter: rep.RetryAfter}
	if reason == hubapi.KeysRollbackReason {
		var extra struct {
			StoredSeq *uint64 `json:"stored_seq"`
		}
		if json.Unmarshal(rep.Body, &extra) == nil {
			e.storedSeq = extra.StoredSeq
		}
	}
	return e
}

// hubError is a non-2xx hub answer. The status code is kept so a caller can
// tell "recipient unknown" (404) from "try later" (429, 507), and the
// reason code is what a caller branches on (sealed-sender §2.11): the
// sentence is the hub's, shown only for a code this node does not know.
type hubError struct {
	path       string
	code       int
	reason     string
	msg        string
	retryAfter string
	// storedSeq is the stored_seq of a 409 keys_rollback (FS-H4).
	storedSeq *uint64
}

func (e *hubError) Error() string {
	s := fmt.Sprintf("anet: hub %s returned %d", e.path, e.code)
	switch {
	case e.reason != "" && e.msg != "":
		s = fmt.Sprintf("anet: hub %s refused (%d %s): %s", e.path, e.code, e.reason, e.msg)
	case e.reason != "":
		s = fmt.Sprintf("anet: hub %s refused (%d %s)", e.path, e.code, e.reason)
	case e.msg != "":
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

// hubReason returns the reason code of a hub error, or "".
func hubReason(err error) string {
	var he *hubError
	if errors.As(err, &he) {
		return he.reason
	}
	return ""
}

// noteHubWire handles a hub that answered over another wire (CORE-V1): the
// daemon stops talking to it. The hub's identity is forgotten, so the next
// call asks GET /hub/identity again and meets the same refusal before it
// signs anything; a node registered there has its join loop make an
// attempt now, which records wire_too_old or wire_too_new — `anet status`
// says so in one sentence, and the relay loop stops polling (it polls only
// while registered). Logged once per hub per process.
func (d *Daemon) noteHubWire(hubURL string, we *hubWireError) {
	key := strings.TrimRight(hubURL, "/")
	d.hubIDMu.Lock()
	delete(d.hubIDs, key)
	d.hubIDMu.Unlock()
	if anetVersionRe.MatchString(we.version) {
		d.hubc.wireNeeds.Store(key, we.version)
	}
	if _, logged := d.hubc.wireNoted.LoadOrStore(key, true); !logged {
		log.Printf("anet: stopping all requests to hub %s: %v", key, we)
	}
	if d.joinRegistered() && sameHub(d.config().HubURL, key) {
		d.kickJoin()
	}
}

// wireTooOldVersion is wire_too_old's {version} for the hub at hubURL
// given the refusal we: the release its last 426 named, which says more to
// the person who has to install it than the wire it states.
func (d *Daemon) wireTooOldVersion(hubURL string, we *hubWireError) string {
	if v, ok := d.hubc.wireNeeds.Load(strings.TrimRight(hubURL, "/")); ok {
		return v.(string)
	}
	return we.version
}

// knownHubAID is the AID of the hub at hubURL when this node has pinned
// it, without asking; "" otherwise.
func (d *Daemon) knownHubAID(hubURL string) string {
	key := strings.TrimRight(hubURL, "/")
	d.hubIDMu.Lock()
	defer d.hubIDMu.Unlock()
	return d.hubIDs[key].aid
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
	return d.pinHubIdentity(key, out)
}

// pinHubIdentity checks a hub's GET /hub/identity answer, pins its KEL in
// peer_identity (pinned_reason "hub") and caches it for hubURL.
func (d *Daemon) pinHubIdentity(hubURL string, out hubapi.HubIdentity) (string, []identity.SignedEvent, error) {
	key := strings.TrimRight(hubURL, "/")
	raw, err := hubapi.DecodeB64(out.KEL, -1)
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
		actionP2PRetired, body, &out); err != nil {
		return out, err
	}
	return out, nil
}

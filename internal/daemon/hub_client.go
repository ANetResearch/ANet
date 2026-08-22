package daemon

// hub_client.go is the daemon's HTTP client to the official Hub (wire types in internal/hubapi; the Hub
// itself is a separate closed-source service): registry publish,
// verifiable-review upload, and the shared request helpers the relay client (relay.go) builds on. v0.1
// is centralized, so the Hub is the single service every daemon talks to.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// maxHubResponse bounds a decoded Hub response body. It is generous because `find` / `GET /agents/{aid}`
// can return many agents or reviews carrying full interaction transcripts, and a relay poll may now
// return messages carrying inline binary ATTACHMENTS (base64). The daemon polls every few seconds and
// acks, so the undelivered backlog stays small in practice; this caps a single poll response.
const maxHubResponse = 256 << 20 // 256 MiB

// RegisterWithHub publishes this agent's AgentCard + KEL to the Hub so it can be discovered and reviewed.
// The Hub derives the AID from the KEL and rejects a mismatch, and verifies a signed challenge proving we
// hold the key — so this cannot claim (or overwrite) another agent's AID. guestMessages is the guest-mode
// trial quota this agent accepts (0 = opt out); it is always sent so the Hub row stays in sync.
func (d *Daemon) RegisterWithHub(ctx context.Context, hubURL, name string, caps []string, guestMessages int) error {
	kelB, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return err
	}
	ts, seq, sig := d.signRelayAuth(relayauth.ActionRegister)
	body := map[string]any{
		"aid":            d.AID(),
		"name":           name,
		"caps":           caps,
		"guest_messages": guestMessages,
		"kel":            base64.StdEncoding.EncodeToString(kelB),
		"ts":             ts,
		"key_state_seq":  seq,
		"sig":            sig,
	}
	// The card carries the same claims, signed by the node making them.
	// The challenge above proves who is calling; only this proves what
	// they said. See card.go.
	if card, cerr := d.signedCard(name, caps); cerr == nil {
		body["card"] = json.RawMessage(card)
	} else {
		log.Printf("anet: registering without a signed card: %v", cerr)
	}
	if err := d.screenPublication("this node's registration", body); err != nil {
		return err
	}
	return d.hubPost(ctx, hubURL, "/register", body, nil)
}

// PublishProfile uploads this agent's self-authored profile (summary/readme/pricing) to the Hub,
// authenticated by a signed challenge verified against the registered KEL. Pricing is display-only text.
func (d *Daemon) PublishProfile(ctx context.Context, hubURL, summary, readme, pricing string) error {
	ts, seq, sig := d.signRelayAuth(relayauth.ActionProfile)
	body := map[string]any{
		"aid":           d.AID(),
		"summary":       summary,
		"readme":        readme,
		"pricing":       pricing,
		"ts":            ts,
		"key_state_seq": seq,
		"sig":           sig,
	}
	if err := d.screenPublication("this node's profile", body); err != nil {
		return err
	}
	return d.hubPost(ctx, hubURL, "/profile", body, nil)
}

// UploadReview sends the provider-signed receipt + this agent's signed review + the verified interaction
// content (request TaskDoc + deliverable) for a completed outbound interaction to the Hub, which
// re-verifies both signatures AND re-hashes the content against the receipt's anchors before storing.
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
	if len(ix.RequestDoc) == 0 || len(ix.Result) == 0 {
		return fmt.Errorf("anet: interaction %s is missing its request/deliverable content", interactionID)
	}
	body := map[string]any{
		"receipt":     base64.StdEncoding.EncodeToString(ix.Receipt),
		"review":      base64.StdEncoding.EncodeToString(ix.Review),
		"request_doc": base64.StdEncoding.EncodeToString(ix.RequestDoc),
		"deliverable": base64.StdEncoding.EncodeToString(ix.Result),
	}
	return d.hubPost(ctx, hubURL, "/reviews", body, nil)
}

// hubPost POSTs a JSON body to hubURL+path, treats a non-2xx as an error (surfacing the Hub's message),
// and decodes a 2xx body into out when out != nil.
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

func (d *Daemon) hubDo(req *http.Request, path string, out any) error {
	// Every request states the C2 contract version this daemon speaks.
	req.Header.Set(hubapi.WireVersionHeader, strconv.Itoa(hubapi.WireVersion))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("anet: hub %s: %w", path, err)
	}
	defer resp.Body.Close()
	d.noteHubWire(resp.Header.Get(hubapi.WireVersionHeader))
	// Cap the response to bound memory against a hostile/broken Hub. It must comfortably exceed the
	// largest legitimate body — `find`/`GET /agents/{aid}` can list many agents or reviews carrying full
	// interaction transcripts — so it is generous; a truncated body would fail to JSON-decode.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxHubResponse))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBody, &e)
		if e.Error != "" {
			return fmt.Errorf("anet: hub %s rejected: %s", path, e.Error)
		}
		return fmt.Errorf("anet: hub %s returned %d", path, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

// noteHubWire reports a hub speaking a different C2 contract, once.
//
// Not fatal: a version gap usually degrades rather than breaks, and
// stranding a working deployment over a header would be worse than the
// drift it warns about. But it must be said out loud — the failure this
// exists to catch is the silent one, where both sides carry on and quietly
// disagree about what a field means.
func (d *Daemon) noteHubWire(v string) {
	if v == "" || v == strconv.Itoa(hubapi.WireVersion) {
		return
	}
	d.wireWarnOnce.Do(func() {
		log.Printf("anet: hub speaks wire contract %s, this daemon speaks %d — "+
			"fields either side does not know are ignored; upgrade the older one",
			v, hubapi.WireVersion)
	})
}

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
	ts, seq, sig := d.signRelayAuth(relayauth.ActionProfile)
	body := map[string]any{
		"visibility": visibility, "ts": ts, "key_state_seq": seq, "sig": sig,
	}
	return d.hubPost(ctx, hub, "/agents/"+url.PathEscape(d.AID())+"/visibility", body, nil)
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
	ts, seq, sig := d.signRelayAuth(relayauth.ActionProfile)
	body := map[string]any{"ts": ts, "key_state_seq": seq, "sig": sig}
	var out map[string]any
	if err := d.hubPost(ctx, hubURL, "/agents/"+url.PathEscape(d.AID())+"/deregister", body, &out); err != nil {
		return out, err
	}
	return out, nil
}

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
	"net/http"
	"net/url"
	"strings"

	"github.com/ANetResearch/ANet/internal/protocol/identity"
	"github.com/ANetResearch/ANet/internal/protocol/relayauth"
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("anet: hub %s: %w", path, err)
	}
	defer resp.Body.Close()
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

// Package anetlink is the C1 shim for a separately-deployed ANetLink
// runtime: it implements provider.CapabilityProvider by speaking the
// HTTP-over-UDS wire anetlinkd serves with --c1-socket.
//
// The daemon stays behind the red line: capability ids arriving from this
// wire (form "<cap>@<adapter>/<device>") are opaque strings here — nothing
// in this package or its callers interprets what stands behind them.
package anetlink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/provider"
)

// Provider speaks the ANetLink C1 wire. Create with New.
type Provider struct {
	id  string
	cli *http.Client
}

var _ provider.CapabilityProvider = (*Provider)(nil)

// New returns a provider backed by the anetlinkd socket at socketPath.
func New(id, socketPath string) *Provider {
	return &Provider{
		id: id,
		cli: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

func (p *Provider) ID() string { return p.id }

func (p *Provider) Capabilities(ctx context.Context) ([]string, error) {
	var out struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := p.get(ctx, "/v0/capabilities", &out); err != nil {
		return nil, err
	}
	return out.Capabilities, nil
}

func (p *Provider) Describe(ctx context.Context) (string, error) {
	var out struct {
		CID string `json:"cid"`
	}
	if err := p.get(ctx, "/v0/describe", &out); err != nil {
		return "", err
	}
	return out.CID, nil
}

func (p *Provider) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://anetlink/v0/health", nil)
	if err != nil {
		return err
	}
	resp, err := p.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("anetlink: health %d", resp.StatusCode)
	}
	return nil
}

func (p *Provider) Invoke(ctx context.Context, call provider.Call) (effect.Effect, error) {
	body, err := json.Marshal(map[string]any{
		"capability": call.Capability,
		"args":       call.Args,
		"call_id":    call.CallID,
		"caller_aid": call.CallerAID,
	})
	if err != nil {
		return effect.Effect{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://anetlink/v0/invoke", bytes.NewReader(body))
	if err != nil {
		return effect.Effect{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.cli.Do(req)
	if err != nil {
		return effect.Effect{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return effect.Effect{}, fmt.Errorf("anetlink: invoke %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out struct {
		Status  string             `json:"status"`
		Metrics map[string]float64 `json:"metrics"`
		Message string             `json:"message"`
		// Evidence is how ANetLink knows what it is telling us: which
		// protocol carried the call, whether the device itself acknowledged,
		// what a readback actually reported, and which vendor correction was
		// applied to the number.
		//
		// It was on the wire and this decoder ignored it, so every device
		// effect reached the daemon stripped of its provenance — a corrected
		// reading arriving as a bare value with nothing saying it had been
		// corrected, and a V4 readback indistinguishable from a V1 "I sent
		// it". The daemon then honestly recorded and forwarded the nothing
		// it had been given.
		Evidence *struct {
			Requested     string `json:"requested"`
			Protocol      string `json:"protocol"`
			NativeAck     bool   `json:"native_ack"`
			ObservedState string `json:"observed_state"`
			LatencyMS     int64  `json:"latency_ms"`
			VerifyTrust   uint8  `json:"verify_trust"`
			AuthTrust     uint8  `json:"auth_trust"`
			Quirk         string `json:"quirk"`
		} `json:"evidence"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return effect.Effect{}, err
	}
	e := effect.Effect{Status: effect.Status(out.Status), Message: out.Message}
	if len(out.Metrics) > 0 {
		e.Record = &tsir.EffectRecord{Metrics: out.Metrics}
	}
	if ev := out.Evidence; ev != nil {
		e.Evidence = &effect.Evidence{
			Requested: ev.Requested, Protocol: ev.Protocol, NativeAck: ev.NativeAck,
			ObservedState: ev.ObservedState, LatencyMS: ev.LatencyMS,
			VerifyTrust: ev.VerifyTrust, AuthTrust: ev.AuthTrust, Quirk: ev.Quirk,
		}
	}
	return e, nil
}

func (p *Provider) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://anetlink"+path, nil)
	if err != nil {
		return err
	}
	resp, err := p.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("anetlink: GET %s: %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

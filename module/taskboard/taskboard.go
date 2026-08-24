//go:build !no_taskboard

// Package taskboard is this node's client for the hub's shared task board.
//
// The board shipped with the hub and nothing in this suite could speak to
// it. Nine mutating endpoints, each gated behind a challenge signed with
// the caller's key, and the only thing that could produce such a
// signature was the hub package's own tests — so the board could be read
// and not used, and no live run had ever touched it. A surface with a
// server and no client is a claim.
//
// Three actions, not nine. Reading the board, putting work on it and
// taking work off it are what an agent needs to participate; moving,
// blocking and rejecting are coordination a human does in a UI, and
// adding them here would be building a product surface to make a
// checklist longer. They remain available over HTTP to anything that
// signs.
//
// Optional, like everything else that is not the kernel: `-tags
// no_taskboard` leaves it out, and a node that never touches a board
// should not carry a client for one.
package taskboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

const name = "taskboard"

// Capability ids this module offers.
const (
	CapBoard  = "task.board"
	CapCreate = "task.create"
	CapClaim  = "task.claim"
)

// callTimeout bounds one call to the hub. The board is a small SQLite
// table behind an HTTP handler; anything slower than this is a network
// problem and the caller should hear about it rather than wait.
const callTimeout = 30 * time.Second

func init() {
	module.Register(name, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil // compiled in, not configured
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		return &Module{cfg: cfg}, nil
	})
}

// Config is the module's block. Everything in it is optional: the board
// lives on the node's own hub, which the daemon already knows.
type Config struct {
	// URL overrides which board to use. Empty means this node's own hub,
	// which is the case worth optimising for — an agent's board is
	// normally the one its hub keeps.
	URL string `json:"url,omitempty"`
}

// Module offers the board as capabilities.
type Module struct {
	cfg  Config
	seam module.HubSeam
	aid  string
	http *http.Client
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	seam, ok := h.HubSeam()
	if !ok {
		// The board is the hub's. A node without one has nothing to talk
		// to, and starting anyway would register capabilities that answer
		// an error to every call.
		return fmt.Errorf("taskboard: this node has no hub, so there is no board")
	}
	m.seam = seam
	m.aid = h.AID()
	m.http = &http.Client{Timeout: callTimeout}
	return h.Providers().Register(ctx, &boardProvider{m: m})
}

func (m *Module) Stop(context.Context) error { return nil }

// base is the board's address.
func (m *Module) base() string {
	if m.cfg.URL != "" {
		return strings.TrimSuffix(m.cfg.URL, "/")
	}
	return strings.TrimSuffix(m.seam.HubURL(), "/")
}

// sign builds the challenge fields the hub requires for one action.
//
// The hub verifies the signature against the key history it holds for
// this AID, so a caller cannot act as anyone else and this node cannot
// deny having acted. Actions are namespaced task.* precisely so a
// signature made for one purpose cannot be replayed against another.
func (m *Module) sign(action string) map[string]any {
	ts := uint64(time.Now().UnixMilli())
	sig, seq := m.seam.Sign(relayauth.Preimage(action, m.aid, ts))
	return map[string]any{
		"aid": m.aid, "ts": ts, "key_state_seq": seq,
		"sig": base64.StdEncoding.EncodeToString(sig),
	}
}

// post sends one signed mutation and returns the card the hub reports.
func (m *Module) post(ctx context.Context, action string, fields map[string]any) (map[string]any, error) {
	body := m.sign("task." + action)
	for k, v := range fields {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.base()+"/tasks/"+action, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var reply struct {
		Card  map[string]any `json:"card"`
		Error string         `json:"error"`
	}
	if json.Unmarshal(out, &reply) != nil {
		return nil, fmt.Errorf("taskboard: %s answered %s", m.base(), resp.Status)
	}
	// The hub's own words, not a status code translated into ours.
	//
	// Its refusals say which rule was broken — only ready cards are
	// claimable, only the assignee submits — and that is exactly what a
	// caller needs. Replacing them with "400 bad request" would throw
	// away the only useful part.
	if reply.Error != "" {
		return nil, fmt.Errorf("taskboard: %s", reply.Error)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("taskboard: %s answered %s", m.base(), resp.Status)
	}
	return reply.Card, nil
}

type boardProvider struct{ m *Module }

func (p *boardProvider) ID() string { return name }

func (p *boardProvider) Capabilities(context.Context) ([]string, error) {
	return []string{CapBoard, CapCreate, CapClaim}, nil
}

func (p *boardProvider) Describe(context.Context) (string, error) { return "", nil }

func (p *boardProvider) Health(context.Context) error {
	if p.m.seam == nil {
		return fmt.Errorf("taskboard: not started")
	}
	return nil
}

func (p *boardProvider) Invoke(ctx context.Context, call provider.Call) (effect.Effect, error) {
	switch call.Capability {
	case CapBoard:
		return p.board(ctx)
	case CapCreate:
		return p.create(ctx, call)
	case CapClaim:
		return p.claim(ctx, call)
	}
	return effect.Effect{}, fmt.Errorf("taskboard: unknown capability %q", call.Capability)
}

// board reads the columns and their cards.
//
// Unsigned, because reading is: the board is a shared surface and a
// signature would only say who was looking.
func (p *boardProvider) board(ctx context.Context) (effect.Effect, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.m.base()+"/tasks/board", nil)
	if err != nil {
		return effect.Effect{}, err
	}
	resp, err := p.m.http.Do(req)
	if err != nil {
		return effect.Effect{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return effect.Effect{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return effect.Effect{}, fmt.Errorf("taskboard: board answered %s", resp.Status)
	}
	var out struct {
		Columns []struct {
			Key   string           `json:"key"`
			Name  string           `json:"name"`
			Cards []map[string]any `json:"cards"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return effect.Effect{}, fmt.Errorf("taskboard: board reply: %w", err)
	}
	metrics := map[string]float64{}
	total := 0
	for _, c := range out.Columns {
		metrics[c.Key] = float64(len(c.Cards))
		total += len(c.Cards)
	}
	metrics["cards"] = float64(total)
	// V2: read back from the board itself, which is the thing being
	// described. Nothing here is inferred.
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: metrics},
		Evidence: &effect.Evidence{Protocol: name, Requested: CapBoard,
			ObservedState: string(raw), VerifyTrust: 2},
		Message: fmt.Sprintf("%d cards across %d columns", total, len(out.Columns)),
	}, nil
}

// create puts work on the board.
//
// taskdoc_cid is required by the hub and that is the right requirement: a
// board of titles is a board of intentions, and a card that names no
// document describes work nobody can pick up and do.
func (p *boardProvider) create(ctx context.Context, call provider.Call) (effect.Effect, error) {
	title, _ := call.Args["title"].(string)
	doc, _ := call.Args["taskdoc_cid"].(string)
	if title == "" || doc == "" {
		return effect.Effect{}, fmt.Errorf(
			"taskboard: create needs args.title and args.taskdoc_cid " +
				"(the CID of the document describing the work)")
	}
	col, _ := call.Args["column"].(string)
	if col == "" {
		col = "backlog"
	}
	card, err := p.m.post(ctx, "create", map[string]any{
		"title": title, "taskdoc_cid": doc, "column": col})
	if err != nil {
		return effect.Effect{}, err
	}
	return cardEffect(card, "created")
}

// claim takes a card off the board.
func (p *boardProvider) claim(ctx context.Context, call provider.Call) (effect.Effect, error) {
	id, _ := call.Args["card_id"].(string)
	if id == "" {
		return effect.Effect{}, fmt.Errorf("taskboard: claim needs args.card_id")
	}
	card, err := p.m.post(ctx, "claim", map[string]any{"card_id": id})
	if err != nil {
		return effect.Effect{}, err
	}
	return cardEffect(card, "claimed")
}

// cardEffect reports what the board says the card is now.
//
// The state comes back from the hub rather than being assumed from the
// action: a claim that the board turned into something else — because
// somebody else got there first, or the card moved — must be reported as
// what happened, not as what was asked for.
func cardEffect(card map[string]any, did string) (effect.Effect, error) {
	if card == nil {
		return effect.Effect{Status: effect.Unverified,
			Message: "the board accepted the change and returned no card"}, nil
	}
	raw, err := json.Marshal(card)
	if err != nil {
		return effect.Effect{}, err
	}
	id, _ := card["id"].(string)
	state, _ := card["state"].(string)
	return effect.Effect{
		Status: effect.OK,
		// The card the hub returned, verbatim. What the board says the
		// card is now is the evidence; what we asked for is not.
		Evidence: &effect.Evidence{Protocol: name, ObservedState: string(raw), VerifyTrust: 2},
		Message:  fmt.Sprintf("%s %s — now %s", did, id, state),
	}, nil
}

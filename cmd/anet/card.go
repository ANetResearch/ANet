package main

// card.go is `anet card <aid> [--json]` (HPP §4.5, §5.2 text C; ux §3.1):
// one agent's card as this node verified it, with the prices the agent
// signed. The daemon's /agents/card does the work — the same answer MCP
// get_agent_card gives, which --json prints as it is — and asks the agent
// for its extended card only when the agent charges or this node is on its
// list (core CORE-K23 rule 2), waiting up to 10 seconds.
//
//	Translator  bafyrei…6kx2wxyz (verified, key confirmed)  partly paid
//	  translate   3 credits
//	  summarize   5 credits
//	  chat        free
//	Prices are signed by the other side, taken 10-01 14:05; at payment the
//	quote in the task governs, and a price above these is not paid automatically.
//
// The lines are ux M4's card.header, card.price or card.price_free (or
// card.price_unknown when the prices should have been had and were not),
// card.preview when the agent sends previews, and card.footer — or
// card.footer_free for a card that charges nothing, or the daemon's
// priceNote (extended_card_unavailable), the same sentence MCP and the
// local A2A interface give.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// previewExtURI is the extension a card declares when its agent sends
// previews while it writes (CUX-P30).
const previewExtURI = module.ExtPreviewURI

// cardAnswer is what `anet card` reads of /agents/card.
type cardAnswer struct {
	AID          string          `json:"aid"`
	Card         json.RawMessage `json:"card"`
	Verification string          `json:"verification"`
	KeyState     string          `json:"key_state"`
	Charge       string          `json:"charge"`
	Prices       []struct {
		Skill  string `json:"skill"`
		Amount string `json:"amount"`
	} `json:"prices"`
	PricesFetchedAt string `json:"pricesFetchedAt"`
	PriceNote       string `json:"priceNote"`
}

// cardCommand is `anet card <aid> [--json]`.
func cardCommand(c *client, rest []string) error {
	const use = "anet card <aid> [--json]"
	pos, flags := splitFlags(rest)
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		return usageError{use}
	}
	body := map[string]any{"aid": strings.TrimSpace(pos[0])}
	if flags["json"] == "true" {
		return c.do("/agents/card", body)
	}
	out, code, err := c.fetch("/agents/card", body)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return answerError(code, out)
	}
	var ra cardAnswer
	if err := json.Unmarshal(out, &ra); err != nil {
		return answerError(code, out)
	}
	// The preview line is a statement about what this node can do with the
	// agent's previews: none while the preview module is not available
	// (11-modularity §5.4).
	writeCard(outW(), ra, a2ashape.Lang(), previewHere(c))
	return nil
}

// writeCard writes text C for ra.
func writeCard(w io.Writer, ra cardAnswer, lang a2ashape.Language, preview bool) {
	verified := ra.Verification == "VERIFIED"
	header := a2ashape.Args{"state": "unverified"}
	if identity.ValidAID(ra.AID) {
		header["peer"] = ra.AID
	}
	var card struct {
		Name   string `json:"name"`
		Skills []struct {
			ID string `json:"id"`
		} `json:"skills"`
		Capabilities struct {
			Extensions []struct {
				URI string `json:"uri"`
			} `json:"extensions"`
		} `json:"capabilities"`
	}
	if verified {
		_ = json.Unmarshal(ra.Card, &card)
		// The key column of KEL M-3: confirmed only when the daemon says so.
		header["state"] = "verified_pending"
		if ra.KeyState == "fresh" {
			header["state"] = "verified"
		}
		if n := printable(card.Name, 80); n != "" {
			header["name"] = n
		}
		if ra.Charge != "" {
			header["charge"] = ra.Charge
		}
	} else {
		header["name"] = a2ashape.Label("find.unverified", lang)
	}
	fmt.Fprintln(w, a2ashape.Sentence("card.header", lang, header))
	if !verified {
		return
	}

	type line struct{ skill, key, price string }
	var lines []line
	switch {
	case len(ra.Prices) > 0:
		for _, p := range ra.Prices {
			if p.Amount == "0" {
				lines = append(lines, line{printable(p.Skill, 64), "card.price_free", ""})
			} else {
				lines = append(lines, line{printable(p.Skill, 64), "card.price", printable(p.Amount, 24)})
			}
		}
	default:
		// The public card's skills: free when it charges nothing, and
		// otherwise at a price this node does not have.
		key := "card.price_unknown"
		if ra.Charge == "free" {
			key = "card.price_free"
		}
		for _, sk := range card.Skills {
			if id := printable(sk.ID, 64); id != "" {
				lines = append(lines, line{id, key, ""})
			}
		}
	}
	width := 0
	for _, l := range lines {
		width = max(width, displayWidth(l.skill))
	}
	for _, l := range lines {
		skill := l.skill + strings.Repeat(" ", width-displayWidth(l.skill))
		fmt.Fprintln(w, "  "+a2ashape.Sentence(l.key, lang, a2ashape.Args{"skill": skill, "price": l.price}))
	}
	for _, e := range card.Capabilities.Extensions {
		if preview && e.URI == previewExtURI {
			fmt.Fprintln(w, a2ashape.Sentence("card.preview", lang, nil))
			break
		}
	}
	// The last line follows the card shown (CORE-K23 rule 4): a card that
	// declares no a2a-x402 — the public card, or an extended card whose
	// skills are all free (a free list-only skill, J-PAY-4's face node) —
	// ends with card.footer_free; signed prices end with card.footer; and
	// prices that should have been had and were not, with the daemon's note.
	switch {
	case len(ra.Prices) > 0 && ra.Charge == "free":
		fmt.Fprintln(w, a2ashape.Sentence("card.footer_free", lang, nil))
	case len(ra.Prices) > 0:
		args := a2ashape.Args{}
		if t, err := time.Parse(time.RFC3339Nano, ra.PricesFetchedAt); err == nil {
			args["time"] = t
		}
		fmt.Fprintln(w, a2ashape.Sentence("card.footer", lang, args))
	case ra.PriceNote != "":
		fmt.Fprintln(w, printable(ra.PriceNote, 400))
	case ra.Charge == "free":
		fmt.Fprintln(w, a2ashape.Sentence("card.footer_free", lang, nil))
	}
}

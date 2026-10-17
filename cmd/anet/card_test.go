package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// cardServer answers /agents/card with answer and records the bodies asked.
func cardServer(t *testing.T, answer string) (*client, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if r.URL.Path == "/agents/card" {
			bodies = append(bodies, m)
		}
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "t", timeout: 5 * time.Second}, &bodies
}

func setLang(t *testing.T, lang string) a2ashape.Language {
	t.Helper()
	t.Setenv("ANET_LANG", lang)
	return a2ashape.Lang()
}

const cardJSON = `{"name":"翻译助手","skills":[{"id":"translate"},{"id":"summarize"},{"id":"chat"}],` +
	`"capabilities":{"extensions":[{"uri":"https://agentnetwork.org.cn/a2a/ext/anet-card/v1"}]}}`

// HPP §5.2 text C (ux §3.1): the header with the whole AID, the state and
// the charge; each skill with its signed price or free, aligned; the footer
// with when the prices were fetched.
func TestCardPrintsTextC(t *testing.T) {
	answer := `{"aid":"` + wholeAID + `","verification":"VERIFIED","key_state":"fresh","charge":"partial","card":` + cardJSON + `,` +
		`"prices":[{"skill":"translate","amount":"3","unit":"credits"},{"skill":"summarize","amount":"5","unit":"credits"},{"skill":"chat","amount":"0","unit":"credits"}],` +
		`"pricesFetchedAt":"2026-10-01T06:05:00.000Z"}`
	c, bodies := cardServer(t, answer)
	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		l := setLang(t, lang)
		out, errOut := captureCLI(t, func() {
			if err := cardCommand(c, []string{wholeAID}); err != nil {
				t.Fatal(err)
			}
		})
		at, _ := time.Parse(time.RFC3339, "2026-10-01T06:05:00Z")
		want := []string{
			a2ashape.Sentence("card.header", l, a2ashape.Args{"name": "翻译助手", "peer": wholeAID, "state": "verified", "charge": "partial"}),
			"  " + a2ashape.Sentence("card.price", l, a2ashape.Args{"skill": "translate", "price": "3"}),
			"  " + a2ashape.Sentence("card.price", l, a2ashape.Args{"skill": "summarize", "price": "5"}),
			"  " + a2ashape.Sentence("card.price_free", l, a2ashape.Args{"skill": "chat     "}),
			a2ashape.Sentence("card.footer", l, a2ashape.Args{"time": at}),
		}
		if got := strings.TrimRight(out, "\n"); got != strings.Join(want, "\n") || errOut != "" {
			t.Errorf("%s:\n%s\nwant\n%s", lang, got, strings.Join(want, "\n"))
		}
		if !strings.Contains(out, wholeAID) {
			t.Errorf("%s: the AID is not whole", lang)
		}
	}
	if len(*bodies) != 2 || (*bodies)[0]["aid"] != wholeAID || len((*bodies)[0]) != 1 {
		t.Errorf("asked %v", *bodies)
	}
}

// The other endings (CORE-K23 rule 4): a card that charges nothing ends
// with card.footer_free, the extended card of a free list included; one whose prices should have been had and were not
// lists its public skills at "—" and ends with the daemon's priceNote, word
// for word (what MCP's priceNote and the local A2A error say); a key not
// confirmed lately says so; an unverified card shows nothing of its own.
func TestCardOtherEndings(t *testing.T) {
	l := setLang(t, "C")
	note := a2ashape.Sentence("extended_card_unavailable.bad_signature", l, nil)
	for _, c := range []struct {
		name, answer string
		want         []string
	}{
		{"free", `{"aid":"` + wholeAID + `","verification":"VERIFIED","key_state":"stale","charge":"free","card":` + cardJSON + `}`,
			[]string{
				a2ashape.Sentence("card.header", l, a2ashape.Args{"name": "翻译助手", "peer": wholeAID, "state": "verified_pending", "charge": "free"}),
				"  " + a2ashape.Sentence("card.price_free", l, a2ashape.Args{"skill": "translate"}),
				"  " + a2ashape.Sentence("card.price_free", l, a2ashape.Args{"skill": "summarize"}),
				"  " + a2ashape.Sentence("card.price_free", l, a2ashape.Args{"skill": "chat     "}),
				a2ashape.Sentence("card.footer_free", l, nil),
			}},
		// J-PAY-4's face node seen from its list: the extended card adds a
		// free list-only skill and declares no a2a-x402, so every skill is
		// listed free and the card ends as a free card does, not with the
		// signed-price footer.
		{"free extended", `{"aid":"` + wholeAID + `","verification":"VERIFIED","key_state":"fresh","charge":"free","card":` + cardJSON +
			`,"prices":[{"skill":"face.detect","amount":"0","unit":"credits"},{"skill":"face.compare","amount":"0","unit":"credits"}],"pricesFetchedAt":"2026-10-01T06:05:00.000Z"}`,
			[]string{
				a2ashape.Sentence("card.header", l, a2ashape.Args{"name": "翻译助手", "peer": wholeAID, "state": "verified", "charge": "free"}),
				"  " + a2ashape.Sentence("card.price_free", l, a2ashape.Args{"skill": "face.detect "}),
				"  " + a2ashape.Sentence("card.price_free", l, a2ashape.Args{"skill": "face.compare"}),
				a2ashape.Sentence("card.footer_free", l, nil),
			}},
		{"unavailable", `{"aid":"` + wholeAID + `","verification":"VERIFIED","key_state":"fresh","charge":"paid","card":` + cardJSON +
			`,"priceNote":` + jsonString(note) + `}`,
			[]string{
				a2ashape.Sentence("card.header", l, a2ashape.Args{"name": "翻译助手", "peer": wholeAID, "state": "verified", "charge": "paid"}),
				"  " + a2ashape.Sentence("card.price_unknown", l, a2ashape.Args{"skill": "translate"}),
				"  " + a2ashape.Sentence("card.price_unknown", l, a2ashape.Args{"skill": "summarize"}),
				"  " + a2ashape.Sentence("card.price_unknown", l, a2ashape.Args{"skill": "chat     "}),
				note,
			}},
		{"unverified", `{"aid":"` + wholeAID + `","verification":"UNVERIFIED","verificationError":"INVALID_SIGNATURE"}`,
			[]string{a2ashape.Sentence("card.header", l, a2ashape.Args{"name": a2ashape.Label("find.unverified", l), "peer": wholeAID, "state": "unverified"})}},
	} {
		cl, _ := cardServer(t, c.answer)
		out, _ := captureCLI(t, func() {
			if err := cardCommand(cl, []string{wholeAID}); err != nil {
				t.Fatal(err)
			}
		})
		if got := strings.TrimRight(out, "\n"); got != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, strings.Join(c.want, "\n"))
		}
	}
}

// --json prints the daemon's answer, the same object MCP get_agent_card
// gives.
func TestCardJSON(t *testing.T) {
	answer := `{"aid":"` + wholeAID + `","verification":"VERIFIED","charge":"free"}`
	c, _ := cardServer(t, answer)
	out, _ := captureCLI(t, func() {
		if err := cardCommand(c, []string{wholeAID, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil || m["charge"] != "free" {
		t.Fatalf("%v %s", err, out)
	}
	if err := checkFlags("card", []string{wholeAID, "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := cardCommand(c, nil); err == nil {
		t.Error("anet card without an AID")
	}
}

// ux §3.1 text B: the charge and key columns, from the public card's
// charge and key_state, "—" for what is not known and for an unverified
// agent.
func TestFindShowsChargeAndKey(t *testing.T) {
	const a2 = "bafyreiezxp2ekudenn7w2fa4eigj3vrgi2imeroc2e5v6l2cwliogscawa"
	answer := `{"agents":[` +
		`{"aid":"` + wholeAID + `","verification":"VERIFIED","charge":"partial","key_state":"fresh","card":{"name":"翻译助手","skills":[{"id":"translate"}]}},` +
		`{"aid":"` + a2 + `","verification":"VERIFIED","charge":"free","key_state":"stale","card":{"name":"人脸检测","skills":[{"id":"face.detect"}]}},` +
		`{"aid":"bafyreifqxkwovompzom7wbbr6qmw4wxfogo6mvn6a6wfhym6l35tz6mnop","verification":"UNVERIFIED","charge":"paid","key_state":"fresh"}` +
		`],"nextCursor":""}`
	c, _ := cardServer(t, answer)
	l := setLang(t, "zh_CN.UTF-8")
	out, _ := captureCLI(t, func() {
		if err := findCommand(c, nil); err != nil {
			t.Fatal(err)
		}
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("%s", out)
	}
	head := strings.Fields(lines[0])
	if head[1] != a2ashape.Label("find.charge", l) || head[2] != a2ashape.Label("find.key", l) {
		t.Errorf("header %q", lines[0])
	}
	for i, want := range [][]string{
		{"翻译助手", "部分收费", "已确认", "translate"},
		{"人脸检测", "免费", "暂未确认", "face.detect"},
		{"（未验证）", "—", "—", "—"},
	} {
		if f := strings.Fields(lines[i+1]); strings.Join(f[:4], " ") != strings.Join(want, " ") {
			t.Errorf("row %d: %q, want %v", i+1, lines[i+1], want)
		}
	}
}

// ux §3.2 text E, the second line: the signed price, higher, or none.
func TestPayPromptSignedPrice(t *testing.T) {
	l := setLang(t, "C")
	var ra cardAnswer
	if err := json.Unmarshal([]byte(`{"aid":"`+wholeAID+`","verification":"VERIFIED","prices":[{"skill":"translate","amount":"3"},{"skill":"chat","amount":"0"}]}`), &ra); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		have          bool
		skill, amount string
		want          string
	}{
		{true, "translate", "3", a2ashape.Sentence("pay.prompt_price", l, a2ashape.Args{"price": 3})},
		{true, "translate", "2", a2ashape.Sentence("pay.prompt_price", l, a2ashape.Args{"price": 3})},
		{true, "translate", "4", a2ashape.Sentence("pay.prompt_price_higher", l, a2ashape.Args{"price": 3})},
		{true, "other", "1", a2ashape.Sentence("pay.prompt_price_higher", l, a2ashape.Args{"price": 0})},
		{true, "", "1", a2ashape.Sentence("pay.prompt_no_price", l, nil)},
		{false, "translate", "3", a2ashape.Sentence("pay.prompt_no_price", l, nil)},
	} {
		if got := signedPriceLine(ra, c.have, c.skill, c.amount, l); got != c.want {
			t.Errorf("%+v: %q, want %q", c, got, c.want)
		}
	}
	if got := signedPriceLine(cardAnswer{}, true, "translate", "3", l); got != a2ashape.Sentence("pay.prompt_no_price", l, nil) {
		t.Errorf("no prices: %q", got)
	}
}

// A confirmation prompt asks for a name only: the daemon is told not to
// contact the agent for its prices.
func TestPeerNameAsksNoPrices(t *testing.T) {
	c, bodies := cardServer(t, `{"aid":"`+wholeAID+`","verification":"VERIFIED","card":{"name":"x"}}`)
	if got := peerName(c, wholeAID); got != "x" {
		t.Fatalf("name %q", got)
	}
	if len(*bodies) != 1 || (*bodies)[0]["prices"] != false {
		t.Fatalf("asked %v", *bodies)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

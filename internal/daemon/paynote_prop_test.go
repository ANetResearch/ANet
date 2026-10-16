package daemon

// P-PAY-6 and P-PAY-12 (HPP §7.2): the payment decision over random inputs.

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// payModel is the state a decision reads, as limits and totals: what
// `anet payments set` changes and `anet pay` is decided on.
type payModel struct {
	auto, agent, agentDaily, explicit, daily uint64
	spent, agentSpent                        uint64
}

// input is the decision on amount at purpose in m, with the rest given.
func (m payModel) input(purpose string, amount uint64, capID string, price cardPrice, readable, listed bool) PayInput {
	left := leftOf(m.daily, m.spent)
	in := PayInput{Purpose: purpose, Amount: amount, CapID: capID, Price: price, ManualSingle: m.explicit,
		ManualLeft: left, SpentToday: m.spent, PayeesReadable: readable, PayeeListed: readable && listed,
		Peer: payNotePeer, At: payNoteAt}
	// What is left today is the tier's as the daemon counts it
	// (countsAgentDaily): the agent tier's two remainders, or daily_max's.
	in.Left = left
	if countsAgentDaily(purpose) {
		in.Left = min(leftOf(m.agentDaily, m.agentSpent), left)
	}
	switch purpose {
	case module.PurposeTaskAuto:
		in.Single = m.auto
	case module.PurposeTaskAgent:
		in.Single = m.agent
	}
	return in
}

// set is `anet payments set k=v ...` on m; false for a key it does not take.
func (m *payModel) set(kv string) bool {
	k, v, ok := strings.Cut(kv, "=")
	n, err := strconv.ParseUint(v, 10, 64)
	if !ok || err != nil {
		return false
	}
	switch k {
	case "auto_max":
		m.auto = n
	case "agent_max":
		m.agent = n
	case "agent_daily_max":
		m.agentDaily = n
	case "explicit_max":
		m.explicit = n
	case "daily_max":
		m.daily = n
	default:
		return false
	}
	return true
}

func randAmount(r *rand.Rand) uint64 {
	switch r.IntN(10) {
	case 0:
		return 1 + r.Uint64N(1<<62)
	case 1:
		return 0
	}
	return r.Uint64N(40)
}

func randModel(r *rand.Rand) payModel {
	return payModel{auto: randAmount(r), agent: randAmount(r), agentDaily: randAmount(r), explicit: randAmount(r),
		daily: randAmount(r), spent: randAmount(r), agentSpent: randAmount(r)}
}

// P-PAY-6: over random quotes, signed prices or none, limits, payee lists,
// tiers and tasks with or without a capability: ① the automatic and the
// agent tier sign if and only if the quote is at most the signed price and
// the tier's limits and the list let it through; ② every command a
// sentence that waits names, run in order on the same state, makes
// `anet pay` succeed; ③ the code is one of ux D1 and D2.
func TestPPay6(t *testing.T) {
	r := rand.New(rand.NewPCG(6, uint64(time.Now().UnixNano())))
	for i := 0; i < 200000; i++ {
		m := randModel(r)
		purpose := module.PurposeTaskAuto
		if r.IntN(2) == 0 {
			purpose = module.PurposeTaskAgent
		}
		amount := 1 + r.Uint64N(40)
		capID := "translate"
		if r.IntN(5) == 0 {
			capID = ""
		}
		var price cardPrice
		switch r.IntN(4) {
		case 0:
			price.Why = []string{priceWhyTimeout, priceWhyNotOffered, priceWhyBadSignature, "strangers_busy"}[r.IntN(4)]
		default:
			price = cardPrice{Have: true, Price: r.Uint64N(40)}
		}
		readable, listed := r.IntN(8) != 0, r.IntN(3) != 0
		in := m.input(purpose, amount, capID, price, readable, listed)
		n := decidePay(in)
		// ③
		if !a2ashape.PayNoteCode(n.Code) || !(a2ashape.PayNoteWaits(n.Code) || strings.HasPrefix(n.Code, "paid.")) {
			t.Fatalf("%+v: code %q is not of ux D1 or D2", in, n.Code)
		}
		// ①
		left := in.Left
		want := capID != "" && price.Have && amount <= price.Price && amount <= in.Single && amount <= left &&
			readable && listed
		if payNoteSigns(n) != want {
			t.Fatalf("%+v: signs %v (%s), want %v", in, payNoteSigns(n), n.Code, want)
		}
		if want {
			if n.Left != left-amount {
				t.Fatalf("%+v: paid, left %d, want %d", in, n.Left, left-amount)
			}
			continue
		}
		// ②: the commands of the sentence, then anet pay.
		n.Task = "t_9f3c"
		cmd := a2ashape.Next(n.Code, a2ashape.English, a2ashape.PayNoteArgs(n))
		mm := m
		switch {
		case strings.HasPrefix(cmd, "anet payments set "):
			for _, kv := range strings.Fields(strings.TrimPrefix(cmd, "anet payments set ")) {
				if !mm.set(kv) {
					t.Fatalf("%+v: %q is not a command anet payments set takes", in, cmd)
				}
			}
			if s := a2ashape.PaymentNote(n, a2ashape.English); !strings.Contains(s, "then anet pay t_9f3c") {
				t.Fatalf("%+v: %q does not go on to anet pay", in, s)
			}
		case cmd == "anet pay t_9f3c":
		default:
			t.Fatalf("%+v: %s's command %q", in, n.Code, cmd)
		}
		manual := decidePay(mm.input(module.PurposeTaskManual, amount, capID, price, readable, listed))
		if manual.Code != "paid.manual" {
			t.Fatalf("%+v: after %q, anet pay gives %s", in, cmd, manual.Code)
		}
		if n.Code == "needs_operator_approval.manual_limit" {
			// Only when anet pay alone would have been refused.
			if decidePay(m.input(module.PurposeTaskManual, amount, capID, price, readable, listed)).Code == "paid.manual" {
				t.Fatalf("%+v: manual_limit, and anet pay alone would have paid", in)
			}
		} else if decidePay(m.input(module.PurposeTaskManual, amount, capID, price, readable, listed)).Code != "paid.manual" {
			t.Fatalf("%+v: %s names anet pay, which would be refused", in, n.Code)
		}
	}
}

// P-PAY-12: a node that set only auto_max and its payee list (every other
// limit its default, agent_daily_max 0) pays automatically every quote at
// most min(auto_max, the signed price) to a listed payee that fits in what
// daily_max leaves today; agent_daily_max changes nothing at the automatic
// tier; the {left} of the sentence is daily_max − spent_today.
func TestPPay12(t *testing.T) {
	r := rand.New(rand.NewPCG(12, uint64(time.Now().UnixNano())))
	for i := 0; i < 2000; i++ {
		m := payModel{auto: 1 + r.Uint64N(20), explicit: defaultExplicitMax, daily: defaultDailyMax}
		for q := 0; q < 30; q++ {
			amount := 1 + r.Uint64N(25)
			price := cardPrice{Have: true, Price: r.Uint64N(25)}
			listed := r.IntN(4) != 0
			in := m.input(module.PurposeTaskAuto, amount, "translate", price, true, listed)
			n := decidePay(in)
			left := leftOf(m.daily, m.spent)
			want := amount <= min(m.auto, price.Price) && listed && amount <= left
			if payNoteSigns(n) != want || (want && n.Code != "paid.auto") {
				t.Fatalf("%+v: %s, want paid.auto %v", in, n.Code, want)
			}
			for _, ad := range []uint64{0, 1, amount, 1 << 40} {
				m2 := m
				m2.agentDaily = ad
				if got := decidePay(m2.input(module.PurposeTaskAuto, amount, "translate", price, true, listed)); got.Code != n.Code {
					t.Fatalf("agent_daily_max %d changes the automatic decision: %s, not %s", ad, got.Code, n.Code)
				}
			}
			if n.Code != "needs_operator_approval.manual_limit" && n.Code != "paid.auto" && n.Left != left {
				t.Fatalf("%+v: {left} %d, want daily_max − spent_today = %d", in, n.Left, left)
			}
			if want {
				if n.Left != left-amount {
					t.Fatalf("%+v: paid, {left} %d, want %d", in, n.Left, left-amount)
				}
				m.spent += amount
			}
		}
	}
}

// FuzzDecidePay: P-PAY-6 ① and ③ over any numbers — a non-manual tier signs
// exactly when the quote is within the signed price, its limit, what is left
// and the payee list, and every decision is a sentence of ux D1 or D2 whose
// command names the setting anet pay needs.
func FuzzDecidePay(f *testing.F) {
	f.Add(uint64(3), uint64(3), true, uint64(5), uint64(6), uint64(10), uint64(6), uint64(44), true, true, true, false)
	f.Add(uint64(30), uint64(3), true, uint64(5), uint64(6), uint64(10), uint64(6), uint64(44), true, true, true, true)
	f.Add(uint64(1<<63), uint64(0), false, uint64(0), uint64(0), uint64(0), uint64(0), uint64(1<<63), false, false, false, false)
	f.Fuzz(func(t *testing.T, amount, price uint64, have bool, single, left, mSingle, mLeft, spent uint64,
		capability, readable, listed, agent bool) {
		purpose := module.PurposeTaskAuto
		if agent {
			purpose = module.PurposeTaskAgent
		}
		capID := ""
		if capability {
			capID = "translate"
		}
		in := PayInput{Purpose: purpose, Amount: amount, CapID: capID, Price: cardPrice{Have: have, Price: price, Why: priceWhyTimeout},
			Single: single, Left: left, ManualSingle: mSingle, ManualLeft: mLeft, SpentToday: spent,
			PayeesReadable: readable, PayeeListed: readable && listed, Peer: payNotePeer, At: payNoteAt}
		n := decidePay(in)
		if amount == 0 {
			if n.Code != "" {
				t.Fatalf("a quote of 0: %+v", n)
			}
			return
		}
		want := capability && have && amount <= price && amount <= single && amount <= left && readable && listed
		if payNoteSigns(n) != want {
			t.Fatalf("%+v: %s", in, n.Code)
		}
		if !a2ashape.PayNoteCode(n.Code) || !(a2ashape.PayNoteWaits(n.Code) || strings.HasPrefix(n.Code, "paid.")) {
			t.Fatalf("%+v: code %q", in, n.Code)
		}
		if n.Code == "needs_operator_approval.manual_limit" {
			if amount <= mSingle && amount <= mLeft {
				t.Fatalf("%+v: manual_limit while anet pay would pay", in)
			}
			if n.Value == nil || n.Key == "" {
				t.Fatalf("%+v: manual_limit without its setting", in)
			}
		}
	})
}

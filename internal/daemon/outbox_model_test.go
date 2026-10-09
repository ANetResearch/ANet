package daemon

// P-ENG-1 (engine §8.1): the outbound state machine under random event
// sequences. The engine's own code runs — attemptOutboxAt with outboxStep
// deciding, the store, the status queries, the scan, CancelTask — against a
// direct transport and a hub whose every answer is drawn at random: p2p with
// a valid token, with none, failing before or after the write; the hub with
// every row of §2.5, or a reset after it read the request, or not reachable
// at all; status answers queued, done with a valid or a forged token,
// unknown, or an error. Between the attempts come new delegations,
// follow-ups, cancels, a peer's message (implicit confirmation), the clock
// moving minutes or days (deadlines), the gate opening and closing, a crash
// inside an attempt, and the no-response scan.
//
// The invariants are checked where they can be seen: at every byte that
// leaves (in the transport and in the hub), and after every event.
//
//	I1  a row is deleted only by its confirmation or its abandonment (or,
//	    never handed over, by a withdrawal);
//	I2  undeliverable or UNAVAILABLE ⇒ no version was handed over;
//	    withdrawn ⇒ never handed over;
//	I3  a text or end_request leaves ⇒ its task's delegation is confirmed;
//	I4  a row leaves ⇒ every row queued before it was handed over;
//	I5  a frozen row never leaves; frozen and never handed over, it is
//	    gone once an attempt at it ends;
//	I6  no version the hub last said it holds (queued) is sent to the hub
//	    again, nor another version sealed for the hub;
//	I7  every version kept has its ack_key, the current one included;
//	I8  a crash inside an attempt ⇒ the row is handed over after the start;
//	I9  a confirmation takes effect once;
//	I10 only the current version's bytes leave, and only once it and
//	    attempting are on disk;
//	I11 while the gate is closed nothing leaves and nothing is sealed;
//	I12 every version of a row carries the same (mid, ts, exp, ix, type,
//	    body);
//	I13 no queued message's envelope carries outer key 8.
//
// ANET_MODEL_SEED replays one sequence.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// modelRow is what the model saw of one outbox row.
type modelRow struct {
	// row is the row as it was when it last left.
	row interactions.OutboxRow
	// sent: a byte of it left this node (a transport took it, the hub read
	// it); sentVers are the versions that did.
	sent     bool
	sentVers map[uint64]bool
	// inner is the first version that left, opened: I12 compares the rest
	// with it.
	inner *seal.SealedInner
}

// outboxModel is one sequence: the requester whose engine runs, the
// provider it sends to (never polled: its keys are what the requester seals
// to), and what was seen.
type outboxModel struct {
	t         *testing.T
	ctx       context.Context
	req, prov *Daemon
	hubURL    string
	clk       *sharedClock

	mu       sync.Mutex // rng and everything below; the hooks run on other goroutines
	rng      *rand.Rand
	gateOpen bool
	p2pOn    bool
	rows     map[int64]*modelRow
	touched  map[int64]bool // rows that left during the current event
	tasks    []string
	trace    []string
	broken   []string
}

func (m *outboxModel) fail(format string, args ...any) {
	m.broken = append(m.broken, fmt.Sprintf(format, args...))
}

func (m *outboxModel) intn(n int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rng.Intn(n)
}

// gate is the identity gate the model holds open or closed.
func (m *outboxModel) gate() GateState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gateOpen {
		return GateState{true, IdentityOK, reasonIdentityOK}
	}
	return GateState{false, IdentityNoticeStale, reasonNoticeStale}
}

// modelP2P is the direct transport, every outcome drawn.
type modelP2P struct{ m *outboxModel }

func (p modelP2P) Name() string { return "p2p-model" }
func (p modelP2P) Reachable(context.Context, string) bool {
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	return p.m.p2pOn
}
func (p modelP2P) Send(_ context.Context, _ string, env []byte) ([]byte, bool, error) {
	m := p.m
	switch r := m.intn(10); {
	case r < 2: // nothing left: refused before the write
		m.observe("p2p", env, false)
		return nil, false, errors.New("p2p: dial failed")
	case r < 5: // written, the answer lost
		m.observe("p2p", env, true)
		return nil, true, errors.New("p2p: reset after the write")
	case r < 6: // written, an answer without a token
		m.observe("p2p", env, true)
		return nil, true, nil
	case r < 7: // written, a forged token
		m.observe("p2p", env, true)
		junk := make([]byte, seal.DAckLen)
		m.mu.Lock()
		m.rng.Read(junk)
		m.mu.Unlock()
		return junk, true, nil
	default: // written and taken: the recipient's token
		key, ok := m.observe("p2p", env, true)
		if !ok {
			return nil, true, nil
		}
		o := seal.OutcomeProcessed
		if m.intn(20) == 0 {
			o = seal.OutcomeRefused
		}
		dack, err := key.MakeDAck(o, seal.Note{})
		if err != nil {
			m.fail("make a token: %v", err)
			return nil, true, nil
		}
		return dack[:], true, nil
	}
}

// hubAnswer is the hub's answer to a /relay/send, drawn.
func (m *outboxModel) hubAnswer(env []byte) fakeSendAnswer {
	m.observe("hub", env, true)
	refuse := func(status int, code, ra string) fakeSendAnswer {
		return fakeSendAnswer{kind: fakeSendRefuse, status: status, code: code, retryAfter: ra}
	}
	switch r := m.intn(100); {
	case r < 40:
		return fakeSendAnswer{kind: fakeSendAccept}
	case r < 44:
		return fakeSendAnswer{kind: fakeSendBadRID}
	case r < 54:
		m.seen("kid_stale answered")
		return refuse(409, "kid_stale", "")
	case r < 57:
		return refuse(403, "access_invalid", "")
	case r < 59:
		return refuse(404, "recipient_unknown", "")
	case r < 60:
		return refuse(413, "too_large", "")
	case r < 61:
		return refuse(400, "bad_envelope", "")
	case r < 65:
		return refuse(429, "rate_limited", "3")
	case r < 69:
		return refuse(429, "strangers_busy", "30")
	case r < 72:
		return refuse(507, "mailbox_full", "")
	case r < 75:
		return refuse(502, "home_unreachable", "")
	case r < 87:
		return refuse(503, "internal", "")
	default:
		return fakeSendAnswer{kind: fakeSendReset}
	}
}

// statusAnswer answers one status item, drawn: a token only for a version
// of the requester's that the hub holds by that handle.
func (m *outboxModel) statusAnswer(it hubapi.RelayStatusItem) (hubapi.RelayStatusResult, bool) {
	switch r := m.intn(10); {
	case r < 3:
		return hubapi.RelayStatusResult{State: hubapi.RelayStatusQueued}, true
	case r < 5:
		return hubapi.RelayStatusResult{State: hubapi.RelayStatusUnknown}, true
	case r < 6:
		junk := make([]byte, seal.DAckLen)
		m.mu.Lock()
		m.rng.Read(junk)
		m.mu.Unlock()
		return hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: hubapi.B64(junk)}, true
	}
	key, ok := m.keyByHandle(it.Handle)
	if !ok {
		return hubapi.RelayStatusResult{State: hubapi.RelayStatusUnknown}, true
	}
	o := seal.OutcomeProcessed
	if m.intn(20) == 0 {
		o = seal.OutcomeRefused
	}
	dack, err := key.MakeDAck(o, seal.Note{})
	if err != nil {
		return hubapi.RelayStatusResult{}, false
	}
	return hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: hubapi.B64(dack[:])}, true
}

// keyByHandle is the ack_key of the requester's version whose handle h is.
func (m *outboxModel) keyByHandle(h string) (seal.AckKey, bool) {
	for _, r := range m.allRows() {
		_, vers, err := m.req.ix.OutboxState(r.ID)
		if err != nil {
			continue
		}
		for _, v := range vers {
			var k seal.AckKey
			copy(k[:], v.AckKey)
			hd := k.Handle()
			if hubapi.B64(hd[:]) == h {
				return k, true
			}
		}
	}
	return seal.AckKey{}, false
}

func (m *outboxModel) allRows() []interactions.OutboxRow {
	var rows []interactions.OutboxRow
	_ = m.req.ix.Update(func(tx *interactions.Tx) error {
		var err error
		rows, err = tx.AllOutboxRows()
		return err
	})
	return rows
}

// observe checks the bytes env as they leave by path (I3–I6, I10–I13),
// handed saying whether they did leave. It returns the ack_key of the
// version they are.
func (m *outboxModel) observe(path string, env []byte, handed bool) (seal.AckKey, bool) {
	d := m.req
	outer, err := seal.ParseOuter(env)
	if err != nil {
		m.lockedFail("%s: an envelope that does not parse left: %v", path, err)
		return seal.AckKey{}, false
	}
	if outer.Exp != nil {
		m.lockedFail("I13: %s: a queued message's envelope carries outer key 8", path)
	}
	digest, err := seal.EnvDigest(env)
	if err != nil {
		m.lockedFail("%s: %v", path, err)
		return seal.AckKey{}, false
	}
	var hit *interactions.OutboxRow
	rows := m.allRows()
	for i := range rows {
		_, cur, err := d.ix.OutboxContent(rows[i].ID)
		if err != nil || cur == nil {
			continue
		}
		if dg, err := seal.EnvDigest(cur); err == nil && dg == digest {
			hit = &rows[i]
			break
		}
	}
	if hit == nil {
		m.lockedFail("I10: %s: bytes that are no row's current version left", path)
		return seal.AckKey{}, false
	}
	_, vers, err := d.ix.OutboxState(hit.ID)
	if err != nil {
		m.lockedFail("%s: row %d: %v", path, hit.ID, err)
		return seal.AckKey{}, false
	}
	var key seal.AckKey
	found := false
	for _, v := range vers {
		if v.Ver == hit.CurVer && len(v.AckKey) == len(key) {
			copy(key[:], v.AckKey)
			found = true
		}
	}
	if !found || !hit.Attempting {
		m.lockedFail("I10: %s: row %d left before its version (%v) and attempting (%v) were on disk",
			path, hit.ID, found, hit.Attempting)
	}
	if !m.gate().Open {
		m.lockedFail("I11: %s: row %d left while the gate was closed", path, hit.ID)
	}
	if hit.Frozen {
		m.lockedFail("I5: %s: frozen row %d left", path, hit.ID)
	}
	queue, err := d.ix.OutboxQueue(hit.IX, hit.ToAID)
	if err == nil {
		for _, a := range queue {
			if a.ID >= hit.ID {
				continue
			}
			if a.HandedAt == 0 {
				m.lockedFail("I4: %s: row %d left before row %d queued ahead of it was handed over", path, hit.ID, a.ID)
			}
			if hit.Type == seal.TypeMessage && (hit.Kind == rowKindText || hit.Kind == rowKindEndRequest) &&
				a.Type == seal.TypeDelegate {
				m.lockedFail("I3: %s: %s row %d left while its delegation row %d is queued", path, hit.Kind, hit.ID, a.ID)
			}
		}
	}
	if path == "hub" {
		for _, v := range d.verViews(hit.ID, vers, d.configuredHub(), d.nowMS()) {
			if hubStateOf(v) == interactions.OutboxQueued {
				m.lockedFail("I6: hub: row %d (version %d) sent to the hub while its version %d is queued there",
					hit.ID, hit.CurVer, v.Ver)
			}
		}
	}
	if hit.Type == seal.TypeMessage && (hit.Kind == rowKindText || hit.Kind == rowKindEndRequest) && !m.delegationConfirmed(hit.IX) {
		m.lockedFail("I3: %s: %s row %d left before its delegation was confirmed", path, hit.Kind, hit.ID)
	}
	op, err := seal.Open(env, m.prov.AID(), m.prov.enc, nil)
	m.mu.Lock()
	defer m.mu.Unlock()
	mr := m.rows[hit.ID]
	if mr == nil {
		mr = &modelRow{}
		m.rows[hit.ID] = mr
	}
	mr.row = *hit
	m.touched[hit.ID] = true
	if err != nil {
		m.fail("%s: row %d: the recipient cannot open it: %v", path, hit.ID, err)
	} else if mr.inner == nil {
		in := op.Inner
		mr.inner = &in
	} else if a, b := mr.inner, &op.Inner; !bytes.Equal(a.MID, b.MID) || a.TS != b.TS || a.Exp != b.Exp ||
		a.IX != b.IX || a.Type != b.Type || !bytes.Equal(a.Body, b.Body) {
		m.fail("I12: %s: row %d version %d is another message than the first that left", path, hit.ID, hit.CurVer)
	}
	if handed {
		mr.sent = true
		if mr.sentVers == nil {
			mr.sentVers = map[uint64]bool{}
		}
		mr.sentVers[hit.CurVer] = true
		m.seen(path + " sent")
	}
	return key, found
}

// delegationConfirmed reports whether the delegation of task ix is
// confirmed: its message, the task's first from the requester, says so.
func (m *outboxModel) delegationConfirmed(ix string) bool {
	msgs, err := m.req.ix.Messages(ix)
	if err != nil {
		return false
	}
	for _, msg := range msgs {
		if msg.SenderAID != m.req.AID() {
			continue
		}
		var meta map[string]any
		_ = json.Unmarshal([]byte(msg.Metadata), &meta)
		return meta[metaDelivery] == a2ashape.DeliveryConfirmed
	}
	return false
}

// tally counts, over a run, what the sequences made happen, so a model
// that stopped reaching a path is seen.
var (
	tallyMu sync.Mutex
	tally   = map[string]int{}
)

func (m *outboxModel) seen(what string) {
	tallyMu.Lock()
	defer tallyMu.Unlock()
	tally[what]++
}

func (m *outboxModel) lockedFail(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail(format, args...)
}

// snapshot is the rows, their versions and the confirmations counted.
type modelSnap struct {
	rows      map[int64]interactions.OutboxRow
	vers      map[int64][]interactions.OutboxVer
	confirmed uint64
}

func (m *outboxModel) snap() modelSnap {
	s := modelSnap{rows: map[int64]interactions.OutboxRow{}, vers: map[int64][]interactions.OutboxVer{}}
	for _, r := range m.allRows() {
		s.rows[r.ID] = r
		_, vers, err := m.req.ix.OutboxState(r.ID)
		if err == nil {
			s.vers[r.ID] = vers
		}
	}
	s.confirmed = counter(m.req, cntDAckConfirmedP2P) + counter(m.req, cntDAckConfirmedHub) +
		counter(m.req, cntDAckConfirmedImplicit)
	return s
}

// after checks what an event changed (I1, I2, I5, I7, I9, I11), attempted
// naming the row an attempt was made at (0: none).
func (m *outboxModel) after(before, now modelSnap, gateClosed bool, attempted int64) {
	vanished := 0
	confirmedRows := 0
	gone := map[int64]interactions.OutboxRow{}
	for id, r := range before.rows {
		gone[id] = r
	}
	m.mu.Lock()
	for id := range m.touched {
		if _, ok := gone[id]; !ok {
			gone[id] = m.rows[id].row // queued and gone within the event
		}
	}
	clear(m.touched)
	m.mu.Unlock()
	for id, r := range gone {
		if _, ok := now.rows[id]; ok {
			continue
		}
		vanished++
		m.mu.Lock()
		mr := m.rows[id]
		m.mu.Unlock()
		sent := mr != nil && mr.sent
		msg, err := m.req.ix.MessageByMsgID(r.IX, hex.EncodeToString(r.MID))
		if err != nil {
			continue // a row with no message of its own (a cancel the scan queued)
		}
		var meta map[string]any
		_ = json.Unmarshal([]byte(msg.Metadata), &meta)
		switch meta[metaDelivery] {
		case a2ashape.DeliveryConfirmed:
			confirmedRows++
		case a2ashape.DeliveryUnconfirmed:
			m.seen("abandoned " + fmt.Sprint(meta[metaDeliveryDetail]))
			if meta[metaDeliveryDetail] == "not_sent" && sent {
				m.fail("I2: row %d was given up as never sent, and it left", id)
			}
		default:
			// Withdrawn, or a follow-up a cancel or an abandonment took with
			// it: never handed over.
			m.seen("gone unsent")
			if sent || r.HandedAt != 0 {
				m.fail("I1: row %d is gone without a confirmation or an abandonment, and it was handed over", id)
			}
		}
		// Given up (not refused by the provider's token, which says the
		// call did not run), a capability call is UNAVAILABLE only when
		// none of its delegation left.
		if r.Type == seal.TypeDelegate && meta[metaDelivery] == a2ashape.DeliveryUnconfirmed {
			if ix, err := m.req.ix.Get(r.IX); err == nil && strings.Contains(ix.ResultMeta, `"UNAVAILABLE"`) && sent {
				m.fail("I2: task %s is UNAVAILABLE, and its delegation left", r.IX)
			}
		}
	}
	for range now.confirmed - before.confirmed {
		m.seen("confirmed")
	}
	if n := now.confirmed - before.confirmed; n > uint64(vanished) || n < uint64(confirmedRows) {
		m.fail("I9: %d confirmations counted, %d rows gone, %d of them confirmed", n, vanished, confirmedRows)
	}
	for id, r := range now.rows {
		vers := now.vers[id]
		if r.Frozen {
			m.seen("frozen")
		}
		if len(vers) > 1 {
			m.seen("versions")
		}
		m.mu.Lock()
		var sentVers map[uint64]bool
		if mr := m.rows[id]; mr != nil {
			sentVers = mr.sentVers
		}
		kept := map[uint64]bool{}
		for _, v := range vers {
			kept[v.Ver] = true
			if sentVers[v.Ver] && v.HandedAt == 0 && !r.Attempting {
				m.fail("I10: row %d version %d left and is not recorded as handed over", id, v.Ver)
			}
		}
		for v := range sentVers {
			if !kept[v] && len(vers) < interactions.MaxHandedVersions {
				m.fail("I7: row %d version %d left and its ack_key is gone", id, v)
			}
		}
		m.mu.Unlock()
		curKept := r.CurVer == 0
		for _, v := range vers {
			if len(v.AckKey) == 0 {
				m.fail("I7: row %d version %d has no ack_key", id, v.Ver)
			}
			if v.Ver == r.CurVer {
				curKept = true
			}
		}
		if !curKept || len(vers) > interactions.MaxHandedVersions+1 {
			m.fail("I7: row %d keeps %d versions, the current one (%d) %v", id, len(vers), r.CurVer, curKept)
		}
		if gateClosed {
			if b, ok := before.rows[id]; ok && b.CurVer != r.CurVer || !ok && r.CurVer != 0 {
				m.fail("I11: row %d was sealed while the gate was closed", id)
			}
		}
		if id == attempted && r.Frozen && r.HandedAt == 0 && !r.Attempting && !versionsHanded(rowView(&r), viewsOf(vers)) {
			m.fail("I5: frozen row %d, never handed over, is still queued after an attempt at it", id)
		}
	}
}

func (m *outboxModel) pickTask() string {
	if len(m.tasks) == 0 {
		return ""
	}
	return m.tasks[m.intn(len(m.tasks))]
}

func (m *outboxModel) pickRow() int64 {
	rows := m.allRows()
	if len(rows) == 0 {
		return 0
	}
	return rows[m.intn(len(rows))].ID
}

// step makes one event happen and checks it.
func (m *outboxModel) step() {
	d, ctx := m.req, m.ctx
	before := m.snap()
	gateClosed := !m.gate().Open
	var attempted int64
	var ev string
	switch r := m.intn(100); {
	case r < 8:
		if len(m.tasks) >= 5 {
			return
		}
		ev = "delegate"
		var id string
		var err error
		if m.intn(2) == 0 {
			id, err = d.Delegate(ctx, m.prov.AID(), "summarise the notes", nil)
		} else {
			ev = "delegate a capability call"
			id, err = d.DelegateCapability(ctx, m.prov.AID(), lampCap, map[string]any{"on": true})
		}
		if err == nil {
			m.tasks = append(m.tasks, id)
		}
	case r < 14:
		ev = "follow-up"
		if id := m.pickTask(); id != "" {
			_ = d.SendMessage(ctx, id, "and keep it short", nil)
		}
	case r < 18:
		ev = "cancel"
		if id := m.pickTask(); id != "" {
			_, _ = d.CancelTask(ctx, id)
		}
	case r < 46:
		attempted = m.pickRow()
		ev = fmt.Sprintf("attempt %d", attempted)
		if attempted != 0 {
			_ = d.attemptOutboxAt(ctx, attempted, m.intn(5) == 0)
		}
	case r < 54:
		ev = "flush"
		d.flushOutbox(ctx)
	case r < 72:
		var add uint64
		switch k := m.intn(20); {
		case k < 6:
			add = 1_000
		case k < 12:
			add = uint64(10_000 + m.intn(110_000))
		case k < 17:
			add = uint64(5+m.intn(25)) * 60_000
		case k < 19:
			add = uint64(1+m.intn(6)) * 3_600_000
		default:
			add = uint64(3+m.intn(12)) * 86_400_000
		}
		ev = fmt.Sprintf("clock +%s", time.Duration(add)*time.Millisecond)
		m.clk.add(add)
	case r < 77:
		m.mu.Lock()
		m.gateOpen = !m.gateOpen
		ev = fmt.Sprintf("gate open=%v", m.gateOpen)
		m.mu.Unlock()
		if !gateClosed {
			gateClosed = true // closed during part of the event
		}
		d.kickOutbox()
	case r < 81:
		m.mu.Lock()
		m.p2pOn = !m.p2pOn
		ev = fmt.Sprintf("p2p reachable=%v", m.p2pOn)
		m.mu.Unlock()
	case r < 85:
		if d.configuredHub() == deadHubURL {
			ev = "the hub is back"
			setHubURL(d, m.hubURL)
		} else {
			ev = "the hub cannot be reached"
			setHubURL(d, deadHubURL)
		}
	case r < 88:
		ev = "status queries fail"
		setFake(m.t, m.hubURL, func(h *fakeHub) { h.statusDown = !h.statusDown })
	case r < 92:
		ev = "crash inside an attempt"
		id := m.pickRow()
		row, _, err := d.ix.OutboxState(id)
		if err != nil || row.CurVer == 0 || row.Frozen {
			return
		}
		if err := d.ix.Update(func(tx *interactions.Tx) error { return tx.BeginOutboxAttempt(id) }); err != nil {
			m.fail("crash: %v", err)
			return
		}
		for _, r := range m.allRows() {
			d.outboxMem.forgetRow(r.ID)
		}
		d.recoverOutboxAttempts()
		if row, _, err := d.ix.OutboxState(id); err == nil && row.HandedAt == 0 {
			m.fail("I8: row %d was being attempted when the node stopped, and is not handed over after the start", id)
		}
	case r < 96:
		ev = "the provider answers"
		id := m.pickTask()
		if id == "" {
			return
		}
		m.mu.Lock()
		reached := false
		for rid, mr := range m.rows {
			if row, ok := before.rows[rid]; ok && row.IX == id && row.Type == seal.TypeDelegate && mr.sent {
				reached = true
			}
		}
		m.mu.Unlock()
		if !reached {
			return // a provider answers only a delegation it may have got
		}
		var n int
		if err := d.ix.Update(func(tx *interactions.Tx) error {
			var err error
			n, err = d.confirmImplicitTx(tx, id, m.prov.AID())
			return err
		}); err != nil {
			m.fail("implicit confirmation: %v", err)
		}
		d.implicitConfirmed(id, n)
	default:
		ev = "no-response scan"
		d.scanUnconfirmed(int64(m.clk.now()), 15*time.Minute)
	}
	m.trace = append(m.trace, ev)
	if !m.gate().Open {
		gateClosed = true
	}
	m.after(before, m.snap(), gateClosed, attempted)
}

// TestOutboxStateMachineModel is P-ENG-1.
func TestOutboxStateMachineModel(t *testing.T) {
	seqs, steps := 24, 90
	if testing.Short() {
		seqs = 6
	}
	seed := time.Now().UnixNano()
	if s := os.Getenv("ANET_MODEL_SEED"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		seed, seqs = v, 1
	}
	tallyMu.Lock()
	clear(tally)
	tallyMu.Unlock()
	for i := 0; i < seqs; i++ {
		s := seed + int64(i)
		t.Run(strconv.FormatInt(s, 10), func(t *testing.T) { runOutboxModel(t, s, steps) })
	}
	tallyMu.Lock()
	defer tallyMu.Unlock()
	t.Logf("what the sequences made happen: %v", tally)
	if seqs < 24 || t.Failed() {
		return
	}
	for _, what := range []string{"p2p sent", "hub sent", "confirmed", "abandoned sent", "gone unsent", "frozen"} {
		if tally[what] == 0 {
			t.Errorf("no sequence reached %q", what)
		}
	}
	if tally["kid_stale answered"] > 0 && tally["versions"] == 0 {
		t.Error("kid_stale was answered and no row ever held two versions")
	}
}

func runOutboxModel(t *testing.T, seed int64, steps int) {
	srv, req, prov := quietPair(t)
	m := &outboxModel{t: t, ctx: context.Background(), req: req, prov: prov, hubURL: srv.URL,
		clk: newSharedClock(uint64(time.Now().UnixMilli())), rng: rand.New(rand.NewSource(seed)),
		gateOpen: true, rows: map[int64]*modelRow{}, touched: map[int64]bool{}}
	req.setOutboxHold(true)
	req.setClock(m.clk.fn())
	prov.setClock(m.clk.fn())
	req.setIdentityGate(m.gate)
	req.RegisterTransport(modelP2P{m})
	setFake(t, srv.URL, func(h *fakeHub) {
		h.sendHook = m.hubAnswer
		h.statusHook = m.statusAnswer
	})
	for i := 0; i < steps; i++ {
		m.step()
		m.mu.Lock()
		broken := append([]string(nil), m.broken...)
		m.mu.Unlock()
		if len(broken) > 0 {
			t.Fatalf("seed %d, after %q:\n  %s\nevents: %s", seed, m.trace[len(m.trace)-1],
				strings.Join(broken, "\n  "), strings.Join(m.trace, "; "))
		}
	}
}

package daemon

// dack_status.go asks the hub what became of the versions it took (engine
// §2.6, DLV §2.4.3): POST /relay/status on the anonymous side, with no
// relayauth header and over no connection the signed side uses (ENG-D2,
// SI-13). For each version the hub says queued (still in the recipient's
// mailbox), done with the recipient's delivery token, or unknown. "Ask
// before you send again": a version the hub holds is never sent through
// it again (ENG-D6, SI-26); one it no longer knows is (ENG-D8).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// statusQueryTimeout bounds one status query.
const statusQueryTimeout = hubCallTimeoutShort

// statusQuery is one /relay/status request: the versions of one row that
// went to one hub and were queued at one hub (ENG-D3).
type statusQuery struct {
	viaHub string
	vers   []interactions.OutboxVer
}

// statusGroups splits the versions of a row the hub at hub can be asked
// about by the hub they were queued at (ENG-D3): a request asks one hub
// about one row, which is one interaction and one recipient (ENG-A7,
// DLV-S-9).
func statusGroups(vers []VerView, stored []interactions.OutboxVer) []statusQuery {
	var out []statusQuery
	for i, v := range vers {
		if !queryable(v) {
			continue
		}
		j := 0
		for j < len(out) && out[j].viaHub != v.ViaHub {
			j++
		}
		if j == len(out) {
			out = append(out, statusQuery{viaHub: v.ViaHub})
		}
		out[j].vers = append(out[j].vers, stored[i])
	}
	return out
}

// statusResult is what one request concluded: confirmed by a valid token
// (outcome and note), the states the hub gave each version, or failed (no
// usable answer: when to ask again, at the latest).
type statusResult struct {
	confirmed bool
	outcome   seal.Outcome
	note      seal.Note
	states    map[uint64]string
	failed    bool
	retry     time.Duration
}

// queryOutbox is step A4 for row: the hub at hub is asked about every
// version it took (ENG-D1–D8), and what it says is recorded. It reports
// whether the row was confirmed (then it is gone).
//
//   - a version done whose token checks under any version's ack_key
//     confirms the row (ENG-D4); a token that does not check, or is not 80
//     bytes, counts as unknown (dack-invalid);
//   - else a version queued: the next query follows the schedule (§0.4),
//     and the hub holds the message — it is not sent again through it;
//   - else every version is unknown: the row is to be sent again, after
//     its backoff (ENG-D8, dack-retransmit).
//
// A request that gets no usable answer (429, 502, a network failure, a
// malformed 200) is asked again at its Retry-After or the current interval
// and changes nothing else: not check_n, not last_error, not the wait the
// task shows (ENG-D1, DLV-W-21c). Versions whose queries fail for an hour
// count as unknown (ENG-D5, verViews).
func (d *Daemon) queryOutbox(ctx context.Context, row *interactions.OutboxRow, stored []interactions.OutboxVer, hub string) bool {
	now := d.nowMS()
	views := d.verViews(row.ID, stored, hub, now)
	keys := ackKeysOf(stored)
	defer clearAckKeys(keys)
	groups := statusGroups(views, stored)
	if len(groups) == 0 {
		return false // nothing the hub can be asked about (no rid, or stale)
	}
	states := map[uint64]string{}
	failed := false
	var retry time.Duration
	for _, q := range groups {
		res := d.askStatus(ctx, hub, q, keys)
		if res.confirmed {
			if err := d.confirmOutbox(row.ID, res.outcome, res.note, cntDAckConfirmedHub); err != nil {
				d.countErr(cntDAckStatusError, err)
				return false
			}
			return true
		}
		if res.failed {
			failed = true
			retry = max(retry, res.retry)
			for _, v := range q.vers {
				d.outboxMem.noteQuery(row.ID, v.Ver, "", true, now)
			}
			continue
		}
		for ver, st := range res.states {
			states[ver] = st
			d.outboxMem.noteQuery(row.ID, ver, st, false, now)
		}
	}
	// The conclusion over every version (ENG-D4), stale ones as unknown.
	queued := false
	for _, v := range d.verViews(row.ID, stored, hub, now) {
		if v.RID == nil {
			continue
		}
		st, asked := states[v.Ver]
		if !asked {
			st = hubStateOf(v)
		}
		if st == interactions.OutboxQueued || st == "" {
			queued = true
		}
	}
	c := interactions.OutboxCheck{States: states, CheckN: row.CheckN}
	switch {
	case queued && !failed:
		d.count(cntDAckStatusQueued)
		c.CheckN = row.CheckN + 1
		c.CheckAt = int64(now + statusDelay(c.CheckN))
	case queued || failed:
		// No answer about at least one version the hub may hold: ask again
		// at the Retry-After, or at the current interval.
		c.CheckAt = int64(now + max(uint64(retry.Milliseconds()), statusDelay(max(row.CheckN, 1))))
	default:
		// Gone from the hub, and no token: sent again after the backoff.
		d.count(cntDAckStatusUnknown)
		d.count(cntDAckRetransmit)
		c.SetNext = true
		c.Attempts = row.Attempts + 1
		c.NextAt = int64(min(now+uint64(retryDelay(c.Attempts).Milliseconds()), row.Exp+1))
	}
	err := d.ix.Update(func(tx *interactions.Tx) error { return tx.NoteOutboxCheck(row.ID, c) })
	if err != nil && !errors.Is(err, interactions.ErrNotFound) {
		d.countErr(cntDAckStatusError, err)
		return false
	}
	if len(states) > 0 {
		d.noteDelivery(row.IX)
	}
	return false
}

// askStatus makes one /relay/status request about q's versions of the hub
// at hub. keys are the ack_keys of every version of the row (ENG-I4).
func (d *Daemon) askStatus(ctx context.Context, hub string, q statusQuery, keys []seal.AckKey) statusResult {
	items := make([]hubapi.RelayStatusItem, 0, len(q.vers))
	for _, v := range q.vers {
		var k seal.AckKey
		copy(k[:], v.AckKey)
		h := k.Handle()
		clear(k[:])
		it := hubapi.RelayStatusItem{RID: hubapi.B64(v.RID), Handle: hubapi.B64(h[:])}
		if v.ViaHub != "" {
			it.ViaHub = v.ViaHub
		}
		items = append(items, it)
	}
	failed := func(err error, retry time.Duration) statusResult {
		if err != nil {
			d.countErr(cntDAckStatusError, err)
		}
		return statusResult{failed: true, retry: retry}
	}
	body, err := json.Marshal(hubapi.RelayStatusRequest{Items: items})
	if err != nil {
		return failed(err, 0)
	}
	cctx, cancel := context.WithTimeout(ctx, statusQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, strings.TrimRight(hub, "/")+hubapi.RelayStatusPath, bytes.NewReader(body))
	if err != nil {
		return failed(err, 0)
	}
	req.Header.Set("Content-Type", "application/json")
	rep := d.hubExchange(req, hub, hubapi.RelayStatusPath, -1)
	if rep.Status == 0 {
		return failed(nil, 0)
	}
	if we := hubWireCheck(hub, rep); we != nil {
		return failed(nil, 0)
	}
	if rep.Status != http.StatusOK {
		retry, _ := parseRetryAfterHeader(rep.RetryAfter)
		return failed(nil, retry)
	}
	res, invalid, noteInvalid, err := decodeStatusAnswer(rep.Body, q.vers, keys)
	for range invalid {
		d.count(cntDAckInvalid)
	}
	if noteInvalid {
		d.count(cntDAckNoteInvalid)
	}
	if err != nil {
		return failed(err, 0)
	}
	return res
}

// decodeStatusAnswer reads the body of a 200 answer to a /relay/status
// request about vers (in that order), keys being the ack_keys of every
// version of the row (ENG-D1, D4, DLV-W-18a). An answer that is no JSON of
// the shape, whose items do not match the request one for one, a state
// outside queued, done and unknown, or a done without its token, is a
// failed query (err). A done whose token checks confirms the row with its
// outcome and note (noteInvalid when the note does not open); one whose
// token does not check — not base64url, not 80 bytes, not made with any
// of keys — counts in invalid and the version is unknown, never delivered
// (SI-20).
func decodeStatusAnswer(body []byte, vers []interactions.OutboxVer, keys []seal.AckKey) (res statusResult, invalid int, noteInvalid bool, err error) {
	var out hubapi.RelayStatusResponse
	if err := json.Unmarshal(body, &out); err != nil || len(out.Items) != len(vers) {
		return statusResult{}, 0, false, errors.New("the status answer does not match the request")
	}
	res = statusResult{states: map[uint64]string{}}
	for i, it := range out.Items {
		ver := vers[i].Ver
		switch it.State {
		case hubapi.RelayStatusQueued:
			res.states[ver] = interactions.OutboxQueued
		case hubapi.RelayStatusUnknown:
			res.states[ver] = interactions.OutboxUnknown
		case hubapi.RelayStatusDone:
			if it.DAck == "" {
				return statusResult{}, invalid, false, errors.New("a done without its token")
			}
			dack, derr := hubapi.DecodeB64(it.DAck, -1)
			if derr == nil && len(dack) == seal.DAckLen {
				if _, o, n, noteOK, ok := seal.VerifyDAck(keys, dack); ok {
					return statusResult{confirmed: true, outcome: o, note: n}, invalid, !noteOK, nil
				}
			}
			invalid++
			res.states[ver] = interactions.OutboxUnknown
		default:
			return statusResult{}, invalid, false, errors.New("a status outside queued, done and unknown")
		}
	}
	return res, invalid, false, nil
}

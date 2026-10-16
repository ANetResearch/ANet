package daemon

// tasks_pay.go holds the control-plane routes of a payment decision
// (A2A-DESIGN §8.6, §12) and of the spending limits.
//
//	POST /tasks/pay          {task_id, decision: submit|reject, accept?}   purpose task-agent
//	POST /tasks/pay-manual   {task_id, decision: submit|reject, accept?}   purpose task-manual
//	POST /payments/status    {hub?}                                        limits and today's totals
//	POST /payments/limits    {set: {key: n}}                               change the limits
//	POST /payees/list        {}                                            the payee list
//	POST /payees/add         {aid}                                         put a payee on it
//	POST /payees/remove      {aid}                                         take one off
//
// All are bearer only: none is on the console session list, because a
// console session cannot authorize a payment (§8.6). The route decides
// the purpose, never the body. /tasks/pay-manual is what `anet pay` calls
// after a confirmation typed on a terminal, /payments/limits what `anet
// payments set` calls after one, and /payees/add what `anet payees add`
// calls after one; the daemon only sees the control token and cannot tell
// whether a terminal was involved (§21 item 13).
//
// A /tasks/pay above the agent tier is not an error (§8.3): nothing is
// signed, the task waits for the operator, and the answer (200) says so —
// anet.reason needs_operator_approval, the policy's spend_refusal code and
// a message naming `anet pay <task>`.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// payTaskBody is the body of /tasks/pay and /tasks/pay-manual.
type payTaskBody struct {
	TaskID   string          `json:"task_id"`
	Decision string          `json:"decision"`
	Accept   json.RawMessage `json:"accept,omitempty"`
	// Payload is refused (§8.7): present so that a client that sends one is
	// told so rather than having it silently ignored.
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (d *Daemon) hTasksPay(w http.ResponseWriter, r *http.Request) {
	d.handlePay(w, r, module.PurposeTaskAgent)
}

func (d *Daemon) hTasksPayManual(w http.ResponseWriter, r *http.Request) {
	d.handlePay(w, r, module.PurposeTaskManual)
}

func (d *Daemon) handlePay(w http.ResponseWriter, r *http.Request, purpose string) {
	var req payTaskBody
	if err := readJSON(r, &req); err != nil || req.TaskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id and decision (submit or reject) required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), relayCallTimeout)
	defer cancel()
	out, err := d.PayTask(ctx, PayRequest{TaskID: req.TaskID, Decision: req.Decision,
		Accept: req.Accept, Payload: req.Payload, Purpose: purpose})
	var hold *PayHold
	if errors.As(err, &hold) {
		// A decision left to a person (HPP PAY-D3): the task waits for
		// the operator, and the answer says why in its sentence (note).
		writeJSON(w, http.StatusOK, hold.Outcome)
		return
	}
	if err != nil {
		writePayError(w, req.TaskID, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writePayError answers a refused decision with a status that says whose
// move it is, and the machine-readable reason. A refusal of the spending
// policy is its sentence of ux §1.4 D4 with the numbers of the check that
// failed (the control plane's error body, UX-R9).
func writePayError(w http.ResponseWriter, taskID string, err error) {
	if sr, ok := isSpendRefusal(err); ok {
		writeSpendRefusal(w, sr, taskID, err)
		return
	}
	body := map[string]any{"error": err.Error()}
	code := http.StatusBadRequest
	var pr *PayRefusal
	switch {
	case errors.As(err, &pr):
		// §8.7: the caller is told payment-failed with the code; nothing
		// was signed or sent.
		code = http.StatusUnprocessableEntity
		body["result"] = pr.Outcome
		body["reason"] = pr.Outcome.Reason
	case errors.Is(err, interactions.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrPaymentPending), errors.Is(err, ErrNoQuote), errors.Is(err, ErrTaskTerminal),
		errors.Is(err, ErrQuoteExpired), errors.Is(err, ErrNotRequester):
		code = http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded):
		code = http.StatusGatewayTimeout
	}
	writeJSON(w, code, body)
}

// hPaymentsStatus reports the limits and the 24-hour totals. With
// {"hub": true} it adds the hub this node settles on and the hub's AID —
// the payee of a redemption, which `anet redeem` shows before it asks —
// at the cost of learning the hub's identity if it is not known yet.
func (d *Daemon) hPaymentsStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hub bool `json:"hub"`
	}
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	st := d.SpendStatus()
	summary := st.LimitsSummary(a2ashape.Lang())
	if !req.Hub {
		writeJSON(w, http.StatusOK, struct {
			SpendStatus
			Summary string `json:"summary"`
		}{st, summary})
		return
	}
	out := struct {
		SpendStatus
		Summary  string `json:"summary"`
		Hub      string `json:"hub"`
		HubAID   string `json:"hub_aid"`
		HubError string `json:"hub_error,omitempty"`
	}{SpendStatus: st, Summary: summary, Hub: d.config().HubURL}
	if out.Hub != "" {
		ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
		defer cancel()
		aid, _, err := d.hubIdentity(ctx, out.Hub)
		if err != nil {
			out.HubError = err.Error()
		}
		out.HubAID = aid
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Daemon) hPaymentsLimits(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Set map[string]uint64 `json:"set"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Set) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "set: {limit: amount} required"})
		return
	}
	if err := d.SetSpendLimits(req.Set); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, d.SpendStatus())
}

// spendRefusalSentence is a spending-policy refusal as ux §1.4 D4 says it:
// the per-payment limit and the setting that raises it, or what was paid
// today against which daily limit and the limit that would let it through.
func spendRefusalSentence(sr *SpendRefusal, taskID string) *a2ashape.Refusal {
	args := a2ashape.Args{"amount": sr.Amount, "single": sr.Single, "key": sr.Key, "spent": sr.Spent,
		"daily": sr.Daily, "need": sr.Need}
	if taskID != "" {
		args["task"] = taskID
	}
	if sr.PayTo != "" {
		args["peer"] = sr.PayTo
	}
	return a2ashape.Refuse(sr.Code, args)
}

// writeSpendRefusal answers a spending-policy refusal: 403, with the
// sentence of its code (ux §1.4 D4) when the reason table has one, and
// otherwise (zero_amount, unknown_purpose: no tier pays these) the error
// and the code.
func writeSpendRefusal(w http.ResponseWriter, sr *SpendRefusal, taskID string, err error) {
	if _, ok := a2ashape.Lookup(sr.Code); ok {
		writeRefusal(w, http.StatusForbidden, spendRefusalSentence(sr, taskID))
		return
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error(), "reason": sr.Code})
}

package daemon

// tasks_pay.go holds the control-plane routes of a payment decision
// (A2A-DESIGN §8.6, §12) and of the spending limits.
//
//	POST /tasks/pay          {task_id, decision: submit|reject, accept?}   purpose task-agent
//	POST /tasks/pay-manual   {task_id, decision: submit|reject, accept?}   purpose task-manual
//	POST /payments/status    {hub?}                                        limits and 24 h totals
//	POST /payments/limits    {set: {key: n}, payees_file?}                 change the limits
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
		// The agent tier's answer above its limits: the task waits for
		// the operator (§8.3).
		writeJSON(w, http.StatusOK, hold.Outcome)
		return
	}
	if err != nil {
		writePayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writePayError answers a refused decision with a status that says whose
// move it is, and the machine-readable reason.
func writePayError(w http.ResponseWriter, err error) {
	body := map[string]any{"error": err.Error()}
	code := http.StatusBadRequest
	var pr *PayRefusal
	var sr *SpendRefusal
	switch {
	case errors.As(err, &pr):
		// §8.7: the caller is told payment-failed with the code; nothing
		// was signed or sent.
		code = http.StatusUnprocessableEntity
		body["result"] = pr.Outcome
		body["reason"] = pr.Outcome.Reason
	case errors.As(err, &sr):
		code = http.StatusForbidden
		body["reason"] = sr.Code
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
	if !req.Hub {
		writeJSON(w, http.StatusOK, d.SpendStatus())
		return
	}
	out := struct {
		SpendStatus
		Hub      string `json:"hub"`
		HubAID   string `json:"hub_aid"`
		HubError string `json:"hub_error,omitempty"`
	}{SpendStatus: d.SpendStatus(), Hub: d.config().HubURL}
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
		Set        map[string]uint64 `json:"set"`
		PayeesFile *string           `json:"payees_file,omitempty"`
	}
	if err := readJSON(r, &req); err != nil || (len(req.Set) == 0 && req.PayeesFile == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "set: {limit: amount} or payees_file required"})
		return
	}
	if err := d.SetSpendLimits(req.Set, req.PayeesFile); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, d.SpendStatus())
}

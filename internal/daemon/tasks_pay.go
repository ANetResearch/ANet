package daemon

// tasks_pay.go holds the control-plane routes of a payment decision
// (A2A-DESIGN §8.6, §12) and of the spending limits.
//
//	POST /tasks/pay          {task_id, decision: submit|reject, accept?}   purpose task-agent
//	POST /tasks/pay-manual   {task_id, decision: submit|reject, accept?}   purpose task-manual
//	POST /payments/status    {}                                            limits and 24 h totals
//	POST /payments/limits    {set: {key: n}, payees_file?}                 change the limits
//
// All four are bearer only: none is on the console session list, because
// a console session cannot authorize a payment (§8.6). The route decides
// the purpose, never the body. /tasks/pay-manual is what `anet pay` calls
// after a confirmation typed on a terminal, and /payments/limits what
// `anet payments set` calls after one; the daemon only sees the control
// token and cannot tell whether a terminal was involved (§21 item 13).

import (
	"context"
	"encoding/json"
	"errors"
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

func (d *Daemon) hPaymentsStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.SpendStatus())
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

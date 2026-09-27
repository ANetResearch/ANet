package main

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/ANetResearch/ANetCore/payment"
)

// a2a.x402.check: a checker for a2a-x402 v0.2 payment data.
//
// The rules are those of the a2a-x402 v0.2 specification (metadata keys,
// statuses, error codes, the state diagram of §7.1) and of the x402 object
// definitions it defers to: x402 v2 (resource, accepts, amount, accepted),
// and v1 (maxAmountRequired, top-level scheme/network) where an object
// declares x402Version 1, since the a2a-x402 v0.2 examples are v1.
// Where anet deviates on purpose (hub:<AID> networks, no resource in
// quotes sent to a hub) the finding says so rather than hiding it.

const x402ExtensionURI = "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"

const (
	keyStatus   = "x402.payment.status"
	keyRequired = "x402.payment.required"
	keyPayload  = "x402.payment.payload"
	keyReceipts = "x402.payment.receipts"
	keyError    = "x402.payment.error"
)

const (
	stRequired  = "payment-required"
	stSubmitted = "payment-submitted"
	stRejected  = "payment-rejected"
	stVerified  = "payment-verified"
	stCompleted = "payment-completed"
	stFailed    = "payment-failed"
)

var x402Statuses = map[string]bool{stRequired: true, stSubmitted: true, stRejected: true,
	stVerified: true, stCompleted: true, stFailed: true}

// x402ErrorCodes are the common codes of a2a-x402 v0.2 §9.1.
var x402ErrorCodes = map[string]bool{"INSUFFICIENT_FUNDS": true, "INVALID_SIGNATURE": true,
	"EXPIRED_PAYMENT": true, "DUPLICATE_NONCE": true, "NETWORK_MISMATCH": true,
	"INVALID_AMOUNT": true, "SETTLEMENT_FAILED": true}

// x402Transitions is the state diagram of a2a-x402 v0.2 §7.1, with two
// additions the text of the spec allows: a merchant may settle without
// sending payment-verified (the §5.5 example goes from submitted to
// completed), and may ask again after a failure (§9).
var x402Transitions = map[string]map[string]string{
	"":          {stRequired: ""},
	stRequired:  {stRejected: "", stSubmitted: ""},
	stSubmitted: {stVerified: "", stCompleted: "skips payment-verified (allowed by the §5.5 example, not by the §7.1 diagram)", stFailed: "skips payment-verified"},
	stVerified:  {stCompleted: "", stFailed: ""},
	stFailed:    {stRequired: "asks for payment again after a failure (§9)"},
}

var (
	atomicAmount = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	caip2        = regexp.MustCompile(`^[-a-z0-9]{3,8}:[-_a-zA-Z0-9]{1,32}$`)
)

type x402Args struct {
	Metadata json.RawMessage `json:"metadata"`
	Sequence json.RawMessage `json:"sequence"`
	Object   json.RawMessage `json:"object"`
	// Kind names what Object is: payment_required, payment_requirements,
	// payment_payload or settlement_response; empty or "auto" guesses.
	Kind string `json:"kind"`
}

type x402Result struct {
	Valid     bool     `json:"valid"`
	Kind      string   `json:"kind"`
	Statuses  []string `json:"statuses,omitempty"`
	Errors    int      `json:"errors"`
	Warnings  int      `json:"warnings"`
	Issues    []issue  `json:"issues"`
	Truncated bool     `json:"truncated,omitempty"`
}

func handleX402Check(_ context.Context, _ *env, body []byte) (any, error) {
	var a x402Args
	if err := decodeArgs(body, &a); err != nil {
		return nil, err
	}
	given := 0
	for _, r := range []json.RawMessage{a.Metadata, a.Sequence, a.Object} {
		if len(r) > 0 {
			given++
		}
	}
	if given != 1 {
		return nil, badArgs("give exactly one of \"metadata\", \"sequence\" or \"object\"")
	}
	res := &x402Result{}
	var is issues
	parse := func(name string, raw json.RawMessage) (any, error) {
		v, _, serr := parseStrictJSON(string(raw))
		if serr != nil {
			return nil, badArgs("%q: %s", name, serr.msg)
		}
		return v, nil
	}
	switch {
	case len(a.Metadata) > 0:
		v, err := parse("metadata", a.Metadata)
		if err != nil {
			return nil, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, badArgs("\"metadata\" must be an object")
		}
		res.Kind = "metadata"
		if st := checkX402Metadata(m, "", &is); st != "" {
			res.Statuses = []string{st}
		}
	case len(a.Sequence) > 0:
		v, err := parse("sequence", a.Sequence)
		if err != nil {
			return nil, err
		}
		seq, ok := v.([]any)
		if !ok || len(seq) == 0 {
			return nil, badArgs("\"sequence\" must be a non-empty array of metadata objects")
		}
		res.Kind = "sequence"
		res.Statuses = checkX402Sequence(seq, &is)
	default:
		v, err := parse("object", a.Object)
		if err != nil {
			return nil, err
		}
		kind := a.Kind
		if kind == "" || kind == "auto" {
			kind = guessX402Kind(v)
			if kind == "" {
				return nil, badArgs("cannot tell what \"object\" is; name it with \"kind\"")
			}
			is.add(sevInfo, "", "kind_guessed", "checked as %s", kind)
		}
		res.Kind = kind
		switch kind {
		case "payment_required":
			checkPaymentRequired(v, "", &is)
		case "payment_requirements":
			checkRequirements(v, "", 2, &is)
		case "payment_payload":
			checkPaymentPayload(v, "", nil, &is)
		case "settlement_response":
			checkSettlement(v, "", &is)
		default:
			return nil, badArgs("unknown kind %q (payment_required, payment_requirements, payment_payload, settlement_response)", kind)
		}
	}
	res.Issues, res.Truncated = is.out(), is.truncated
	for _, i := range res.Issues {
		switch i.Severity {
		case sevError:
			res.Errors++
		case sevWarning:
			res.Warnings++
		}
	}
	res.Valid = res.Errors == 0
	return res, nil
}

func guessX402Kind(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	has := func(k string) bool { _, ok := m[k]; return ok }
	switch {
	case has("accepts"):
		return "payment_required"
	case has("accepted") || (has("payload") && has("x402Version")):
		return "payment_payload"
	case has("success"):
		return "settlement_response"
	case has("payTo") || has("maxAmountRequired") || has("amount"):
		return "payment_requirements"
	}
	return ""
}

// checkX402Metadata checks one message's metadata and returns its status.
func checkX402Metadata(m map[string]any, path string, is *issues) string {
	var x402Keys []string
	for _, k := range sortedKeys(m) {
		if strings.HasPrefix(k, "x402.") {
			x402Keys = append(x402Keys, k)
			switch k {
			case keyStatus, keyRequired, keyPayload, keyReceipts, keyError:
			default:
				is.add(sevWarning, ptr(path, k), "unknown_key", "%s is not a metadata key of a2a-x402 v0.2", k)
			}
		}
	}
	raw, present := m[keyStatus]
	if !present {
		if len(x402Keys) > 0 {
			is.add(sevError, ptr(path, keyStatus), "status_missing",
				"%s must be present in every x402-related message (a2a-x402 v0.2 §7)", keyStatus)
		} else {
			is.add(sevError, path, "no_x402", "no x402.payment.* metadata")
		}
		return ""
	}
	st, ok := raw.(string)
	if !ok || !x402Statuses[st] {
		is.add(sevError, ptr(path, keyStatus), "status_value", "%s is %v; allowed: %s", keyStatus, raw,
			strings.Join(sortedKeys(x402Statuses), ", "))
		return ""
	}

	req, hasReq := m[keyRequired]
	pl, hasPayload := m[keyPayload]
	rc, hasReceipts := m[keyReceipts]
	ec, hasError := m[keyError]

	if hasReq && st != stRequired {
		is.add(sevWarning, ptr(path, keyRequired), "misplaced_key", "%s belongs with status %s, not %s", keyRequired, stRequired, st)
	}
	if hasPayload && st != stSubmitted {
		is.add(sevWarning, ptr(path, keyPayload), "misplaced_key", "%s belongs with status %s, not %s", keyPayload, stSubmitted, st)
	}
	if hasError && st != stFailed {
		is.add(sevWarning, ptr(path, keyError), "misplaced_key", "%s belongs with status %s, not %s", keyError, stFailed, st)
	}
	var accepts []any
	switch st {
	case stRequired:
		if hasReq {
			accepts = checkPaymentRequired(req, ptr(path, keyRequired), is)
		} else {
			is.add(sevInfo, path, "embedded_flow",
				"no %s: this is the embedded flow, and the PaymentRequired must be inside an artifact (a2a-x402 v0.2 §4.2)", keyRequired)
		}
	case stSubmitted:
		switch {
		case hasPayload:
			checkPaymentPayload(pl, ptr(path, keyPayload), accepts, is)
		case m["anet.payment.accept"] != nil:
			is.add(sevInfo, path, "anet_signer",
				"no %s but anet.payment.accept: a local client asking its anet daemon to sign (A2A-DESIGN §8.7)", keyPayload)
		default:
			is.add(sevInfo, path, "embedded_flow",
				"no %s: this is the embedded flow, and the PaymentPayload must be inside a message part (a2a-x402 v0.2 §4)", keyPayload)
		}
	case stCompleted:
		if !hasReceipts {
			is.add(sevError, ptr(path, keyReceipts), "receipts_missing", "%s is required in the final message (a2a-x402 v0.2 §7)", keyReceipts)
		} else if last := checkReceipts(rc, ptr(path, keyReceipts), is); last != nil && last["success"] != true {
			is.add(sevError, ptr(path, keyReceipts), "receipt_not_success", "status is %s but the last receipt is not a success", st)
		}
	case stFailed:
		if !hasError {
			is.add(sevError, ptr(path, keyError), "error_missing", "a failed payment must carry %s (a2a-x402 v0.2 §9)", keyError)
		} else if code, ok := ec.(string); !ok || code == "" {
			is.add(sevError, ptr(path, keyError), "error_value", "%s must be a short error code string", keyError)
		} else if !x402ErrorCodes[code] {
			is.add(sevWarning, ptr(path, keyError), "error_code", "%q is not one of the common codes of a2a-x402 v0.2 §9.1 (%s)",
				code, strings.Join(sortedKeys(x402ErrorCodes), ", "))
		}
		if !hasReceipts {
			is.add(sevWarning, ptr(path, keyReceipts), "receipts_missing",
				"%s should carry the failed settlement ({success:false, errorReason, network, transaction:\"\"}) when this is the final message", keyReceipts)
		} else if last := checkReceipts(rc, ptr(path, keyReceipts), is); last != nil && last["success"] == true {
			is.add(sevWarning, ptr(path, keyReceipts), "receipt_success", "status is %s but the last receipt is a success", st)
		}
	}
	if hasReceipts && st != stCompleted && st != stFailed {
		checkReceipts(rc, ptr(path, keyReceipts), is)
	}
	return st
}

func checkReceipts(v any, path string, is *issues) map[string]any {
	arr, ok := v.([]any)
	if !ok {
		is.add(sevError, path, "type", "%s must be an array of SettlementResponse", keyReceipts)
		return nil
	}
	if len(arr) == 0 {
		is.add(sevError, path, "empty", "%s is empty", keyReceipts)
		return nil
	}
	for i, r := range arr {
		checkSettlement(r, ptr(path, i), is)
	}
	last, _ := arr[len(arr)-1].(map[string]any)
	return last
}

// checkX402Sequence checks each message and the transitions between their
// statuses, and that receipts only grow.
func checkX402Sequence(seq []any, is *issues) []string {
	var statuses []string
	prev := ""
	var prevReceipts []any
	for i, e := range seq {
		p := ptr("", i)
		m, ok := e.(map[string]any)
		if !ok {
			is.add(sevError, p, "type", "sequence[%d] must be a metadata object", i)
			continue
		}
		st := checkX402Metadata(m, p, is)
		statuses = append(statuses, st)
		if st == "" {
			prev = ""
			continue
		}
		next, known := x402Transitions[prev]
		if prev == stCompleted || prev == stRejected {
			is.add(sevError, ptr(p, keyStatus), "after_terminal", "%s after %s, which ends the payment", st, prev)
		} else if note, ok := next[st]; !known || !ok {
			if !(prev == "" && i > 0) {
				is.add(sevError, ptr(p, keyStatus), "transition", "%s cannot follow %s (a2a-x402 v0.2 §7.1)", st, orStart(prev))
			}
		} else if note != "" {
			is.add(sevWarning, ptr(p, keyStatus), "transition", "%s after %s: %s", st, orStart(prev), note)
		}
		if rc, ok := m[keyReceipts].([]any); ok {
			if len(rc) < len(prevReceipts) || canon(rc[:len(prevReceipts)]) != canon(prevReceipts) {
				is.add(sevWarning, ptr(p, keyReceipts), "receipts_history",
					"%s is the complete history and should extend the previous message's list", keyReceipts)
			}
			prevReceipts = rc
		}
		prev = st
	}
	return statuses
}

func orStart(s string) string {
	if s == "" {
		return "the start"
	}
	return s
}

func x402Version(m map[string]any, path string, is *issues) int {
	raw, ok := m["x402Version"]
	if !ok {
		is.add(sevError, ptr(path, "x402Version"), "required", "x402Version is required")
		return 0
	}
	n, ok := raw.(json.Number)
	if !ok {
		is.add(sevError, ptr(path, "x402Version"), "type", "x402Version must be a number")
		return 0
	}
	switch string(n) {
	case "1":
		return 1
	case "2":
		return 2
	}
	is.add(sevError, ptr(path, "x402Version"), "version", "x402Version %s is neither 1 nor 2", n)
	return 0
}

func objectAt(v any, path, what string, is *issues) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		is.add(sevError, path, "type", "%s must be an object", what)
	}
	return m
}

func reqString(m map[string]any, k, path string, is *issues) string {
	v, ok := m[k]
	if !ok {
		is.add(sevError, ptr(path, k), "required", "%s is required", k)
		return ""
	}
	s, ok := v.(string)
	if !ok {
		is.add(sevError, ptr(path, k), "type", "%s must be a string", k)
		return ""
	}
	if s == "" {
		is.add(sevError, ptr(path, k), "empty", "%s is empty", k)
	}
	return s
}

func optType(m map[string]any, k, path, want string, is *issues) {
	v, ok := m[k]
	if !ok || v == nil {
		return
	}
	var good bool
	switch want {
	case "object":
		_, good = v.(map[string]any)
	case "string":
		_, good = v.(string)
	}
	if !good {
		is.add(sevError, ptr(path, k), "type", "%s must be a%s %s", k, map[bool]string{true: "n", false: ""}[want == "object"], want)
	}
}

func unknownMembers(m map[string]any, known []string, path, what string, is *issues) {
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	for _, k := range sortedKeys(m) {
		if !set[k] {
			is.add(sevWarning, ptr(path, k), "unknown_field", "%s has no member %q", what, k)
		}
	}
}

// checkPaymentRequired checks a PaymentRequired (x402PaymentRequiredResponse)
// and returns its accepts list.
func checkPaymentRequired(v any, path string, is *issues) []any {
	m := objectAt(v, path, "PaymentRequired", is)
	if m == nil {
		return nil
	}
	ver := x402Version(m, path, is)
	optType(m, "error", path, "string", is)
	switch ver {
	case 2:
		unknownMembers(m, []string{"x402Version", "error", "resource", "accepts", "extensions"}, path, "PaymentRequired (x402 v2)", is)
		if r, ok := m["resource"]; !ok {
			is.add(sevWarning, ptr(path, "resource"), "resource_missing",
				"x402 v2 requires resource ({url, description, mimeType}); anet leaves it out of quotes that reach a hub on purpose (A2A-DESIGN X4), "+
					"and a strict v2 client may refuse the quote")
		} else if rm := objectAt(r, ptr(path, "resource"), "resource", is); rm != nil {
			reqString(rm, "url", ptr(path, "resource"), is)
			optType(rm, "description", ptr(path, "resource"), "string", is)
			optType(rm, "mimeType", ptr(path, "resource"), "string", is)
		}
		optType(m, "extensions", path, "object", is)
	case 1:
		unknownMembers(m, []string{"x402Version", "error", "accepts"}, path, "x402PaymentRequiredResponse (x402 v1)", is)
	}
	acc, ok := m["accepts"].([]any)
	switch {
	case !ok:
		is.add(sevError, ptr(path, "accepts"), "required", "accepts must be an array of PaymentRequirements")
		return nil
	case len(acc) == 0:
		is.add(sevError, ptr(path, "accepts"), "empty", "accepts is empty: nothing can be paid")
	}
	for i, r := range acc {
		checkRequirements(r, ptr(ptr(path, "accepts"), i), max(ver, 1), is)
	}
	return acc
}

// checkRequirements checks one PaymentRequirements of the given x402
// version.
func checkRequirements(v any, path string, ver int, is *issues) {
	m := objectAt(v, path, "PaymentRequirements", is)
	if m == nil {
		return
	}
	scheme := reqString(m, "scheme", path, is)
	network := reqString(m, "network", path, is)
	reqString(m, "asset", path, is)
	reqString(m, "payTo", path, is)
	amountKey := "amount"
	if ver == 1 {
		amountKey = "maxAmountRequired"
		unknownMembers(m, []string{"scheme", "network", "maxAmountRequired", "resource", "description", "mimeType",
			"outputSchema", "payTo", "maxTimeoutSeconds", "asset", "extra"}, path, "PaymentRequirements (x402 v1)", is)
		reqString(m, "resource", path, is)
		if _, ok := m["description"].(string); !ok {
			is.add(sevError, ptr(path, "description"), "required", "description is required (x402 v1)")
		}
		if _, ok := m["mimeType"].(string); !ok {
			is.add(sevError, ptr(path, "mimeType"), "required", "mimeType is required (x402 v1)")
		}
	} else {
		unknownMembers(m, []string{"scheme", "network", "amount", "asset", "payTo", "maxTimeoutSeconds", "extra"},
			path, "PaymentRequirements (x402 v2)", is)
		if _, ok := m["maxAmountRequired"]; ok {
			is.add(sevError, ptr(path, "maxAmountRequired"), "v1_field", "maxAmountRequired is x402 v1; v2 uses amount")
		}
	}
	if amt := reqString(m, amountKey, path, is); amt != "" && !atomicAmount.MatchString(amt) {
		is.add(sevError, ptr(path, amountKey), "amount", "%s %q must be a decimal string of atomic units, without sign, point or leading zeros", amountKey, amt)
	}
	if t, ok := m["maxTimeoutSeconds"]; !ok {
		is.add(sevError, ptr(path, "maxTimeoutSeconds"), "required", "maxTimeoutSeconds is required")
	} else if r, ok := ratOf(t); !ok || !r.IsInt() || r.Sign() <= 0 {
		is.add(sevError, ptr(path, "maxTimeoutSeconds"), "type", "maxTimeoutSeconds must be a positive integer")
	}
	optType(m, "extra", path, "object", is)
	if ver == 2 && network != "" && !caip2.MatchString(network) {
		if strings.HasPrefix(network, "hub:") {
			is.add(sevWarning, ptr(path, "network"), "network_caip2",
				"network %q is anet's hub:<AID> form, which exceeds the CAIP-2 reference length; a documented deviation (A2A-DESIGN §21 #7)", network)
		} else {
			is.add(sevWarning, ptr(path, "network"), "network_caip2", "network %q is not a CAIP-2 identifier (namespace:reference), as x402 v2 asks", network)
		}
	}
	if scheme == payment.SchemeCredit {
		if a, _ := m["asset"].(string); a != "" && a != payment.AssetCredit {
			is.add(sevWarning, ptr(path, "asset"), "anet_credit", "the anet-credit scheme settles in asset %q, not %q", payment.AssetCredit, a)
		}
		if network != "" && !strings.HasPrefix(network, "hub:") {
			is.add(sevWarning, ptr(path, "network"), "anet_credit", "the anet-credit scheme names its ledger as hub:<AID>, not %q", network)
		}
	}
}

// checkPaymentPayload checks a PaymentPayload. accepts, when known, is the
// list the payer chose from.
func checkPaymentPayload(v any, path string, accepts []any, is *issues) {
	m := objectAt(v, path, "PaymentPayload", is)
	if m == nil {
		return
	}
	ver := x402Version(m, path, is)
	pl, ok := m["payload"].(map[string]any)
	if !ok {
		is.add(sevError, ptr(path, "payload"), "required", "payload must be an object (the scheme's signed authorization)")
	}
	switch ver {
	case 2:
		unknownMembers(m, []string{"x402Version", "resource", "accepted", "payload", "extensions"}, path, "PaymentPayload (x402 v2)", is)
		acc, ok := m["accepted"]
		if !ok {
			is.add(sevError, ptr(path, "accepted"), "required", "accepted (the PaymentRequirements chosen) is required in x402 v2")
		} else {
			checkRequirements(acc, ptr(path, "accepted"), 2, is)
		}
		if _, ok := m["resource"]; ok {
			is.add(sevInfo, ptr(path, "resource"), "resource_present",
				"resource describes what is bought; anet does not send it to a hub facilitator (A2A-DESIGN X4)")
		}
		optType(m, "extensions", path, "object", is)
		if am, ok := acc.(map[string]any); ok && am["scheme"] == payment.SchemeCredit && pl != nil {
			checkCreditAuthorization(pl, am, ptr(path, "payload"), is)
		}
	case 1:
		unknownMembers(m, []string{"x402Version", "scheme", "network", "payload"}, path, "PaymentPayload (x402 v1)", is)
		reqString(m, "scheme", path, is)
		reqString(m, "network", path, is)
	}
}

// checkCreditAuthorization decodes an anet-credit authorization and checks
// that it says what the accepted option says. The signature is not
// checked: that needs the payer's KEL, and the hub checks it at settlement.
func checkCreditAuthorization(pl, accepted map[string]any, path string, is *issues) {
	s, ok := pl["authorization"].(string)
	if !ok {
		is.add(sevError, ptr(path, "authorization"), "anet_credit", "an anet-credit payload carries authorization (base64 CBOR)")
		return
	}
	b, err := decodeBase64(s)
	if err != nil {
		is.add(sevError, ptr(path, "authorization"), "anet_credit", "authorization is not base64")
		return
	}
	auth, err := payment.UnmarshalAuthorization(b)
	if err != nil {
		is.add(sevError, ptr(path, "authorization"), "anet_credit", "authorization does not decode: %v", err)
		return
	}
	if p, _ := accepted["payTo"].(string); p != "" && auth.PayTo != p {
		is.add(sevError, ptr(path, "authorization"), "payee_mismatch", "the authorization pays %s, the accepted option asks for %s", auth.PayTo, p)
	}
	if n, _ := accepted["network"].(string); n != "" && auth.Network != n {
		is.add(sevError, ptr(path, "authorization"), "network_mismatch", "the authorization is for network %s, the accepted option is %s", auth.Network, n)
	}
	if a, _ := accepted["amount"].(string); a != "" {
		if want, err := payment.ParseAmount(a); err == nil && auth.Amount < want {
			is.add(sevError, ptr(path, "authorization"), "invalid_amount", "the authorization is for %d, less than the %d asked", auth.Amount, want)
		}
	}
	if auth.NotAfter <= auth.IssuedAt {
		is.add(sevError, ptr(path, "authorization"), "window", "the authorization expires before it is issued")
	}
	if auth.InteractionID == "" {
		is.add(sevWarning, ptr(path, "authorization"), "unbound",
			"the authorization is not bound to an interaction: whoever holds it can spend it on other work")
	}
	is.add(sevInfo, ptr(path, "authorization"), "anet_credit",
		"anet-credit authorization from %s; its signature is checked by the hub at settlement, not here", auth.Payer)
}

// checkSettlement checks one SettlementResponse (x402SettleResponse).
func checkSettlement(v any, path string, is *issues) {
	m := objectAt(v, path, "SettlementResponse", is)
	if m == nil {
		return
	}
	unknownMembers(m, []string{"success", "errorReason", "payer", "transaction", "network", "amount", "extensions"},
		path, "SettlementResponse", is)
	succ, ok := m["success"].(bool)
	if !ok {
		is.add(sevError, ptr(path, "success"), "required", "success must be a boolean")
	}
	if t, ok := m["transaction"]; !ok {
		is.add(sevError, ptr(path, "transaction"), "required", "transaction is required (\"\" when nothing settled)")
	} else if ts, ok := t.(string); !ok {
		is.add(sevError, ptr(path, "transaction"), "type", "transaction must be a string")
	} else if succ && ts == "" {
		is.add(sevError, ptr(path, "transaction"), "empty", "a successful settlement names its transaction")
	}
	reqString(m, "network", path, is)
	optType(m, "errorReason", path, "string", is)
	optType(m, "payer", path, "string", is)
	optType(m, "extensions", path, "object", is)
	if ok && !succ {
		if r, _ := m["errorReason"].(string); r == "" {
			is.add(sevWarning, ptr(path, "errorReason"), "error_reason", "a failed settlement should say why (errorReason)")
		}
	}
	if a, ok := m["amount"].(string); ok && !atomicAmount.MatchString(a) {
		is.add(sevError, ptr(path, "amount"), "amount", "amount %q must be a decimal string of atomic units", a)
	}
	ext, _ := m["extensions"].(map[string]any)
	if rv, ok := ext[payment.ExtReceipt]; ok {
		checkAnetReceipt(rv, m, ptr(ptr(path, "extensions"), payment.ExtReceipt), is)
	}
}

// checkAnetReceipt decodes an anet hub-signed settlement receipt and
// checks it against the response carrying it. The hub's signature is not
// checked here (it needs the hub's KEL).
func checkAnetReceipt(v any, resp map[string]any, path string, is *issues) {
	s, ok := v.(string)
	if !ok {
		is.add(sevError, path, "anet_receipt", "the anet settlement receipt is a base64 string")
		return
	}
	b, err := decodeBase64(s)
	if err != nil {
		is.add(sevError, path, "anet_receipt", "the anet settlement receipt is not base64")
		return
	}
	rc, err := payment.UnmarshalReceipt(b)
	if err != nil {
		is.add(sevError, path, "anet_receipt", "the anet settlement receipt does not decode: %v", err)
		return
	}
	if n, _ := resp["network"].(string); n != "" && rc.Network != n {
		is.add(sevError, path, "anet_receipt", "the receipt settled on %s, the response says %s", rc.Network, n)
	}
	if a, _ := resp["amount"].(string); a != "" && a != payment.Amount(rc.Amount) {
		is.add(sevError, path, "anet_receipt", "the receipt settled %d, the response says %s", rc.Amount, a)
	}
	if p, _ := resp["payer"].(string); p != "" && rc.Payer != p {
		is.add(sevError, path, "anet_receipt", "the receipt's payer is %s, the response says %s", rc.Payer, p)
	}
	is.add(sevInfo, path, "anet_receipt", "hub-signed receipt for authorization %s (%d on %s); verify it with the hub's KEL (anet verify)",
		rc.AuthID, rc.Amount, rc.Network)
}

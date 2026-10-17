package a2ashape

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Error is an A2A error as a task operation reports it (A2A §3.3.2): Name
// is the error's name in the specification, and a caller that answers a
// client — module/a2a, the control plane — maps it to its binding's code.
//
// Here rather than in module/a2a because the kernel is where the decision
// is made. "No such task" and "a task that is not yours" must be the same
// answer (A2A-DESIGN §11.1 [C17]), and that holds only if the kernel says
// TaskNotFound in both cases, in a vocabulary the interface can read
// without guessing from an error string.
type Error struct {
	Name   string
	Detail string
}

// Error returns the name and the detail.
func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Name
	}
	return e.Name + ": " + e.Detail
}

// Is matches any Error with the same name, so errors.Is(err,
// ErrTaskNotFound) holds for an error made with Errorf(ErrTaskNotFound, …).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Name == e.Name
}

// The A2A errors a task operation reports.
var (
	ErrTaskNotFound                 = &Error{Name: "TaskNotFoundError"}
	ErrTaskNotCancelable            = &Error{Name: "TaskNotCancelableError"}
	ErrUnsupportedOperation         = &Error{Name: "UnsupportedOperationError"}
	ErrContentTypeNotSupported      = &Error{Name: "ContentTypeNotSupportedError"}
	ErrVersionNotSupported          = &Error{Name: "VersionNotSupportedError"}
	ErrExtensionSupportRequired     = &Error{Name: "ExtensionSupportRequiredError"}
	ErrPushNotificationNotSupported = &Error{Name: "PushNotificationNotSupportedError"}
	// ErrInvalidParams is JSON-RPC's invalid-params error, which A2A uses
	// for a request that is well formed but not acceptable (a url part, a
	// page size out of range).
	ErrInvalidParams = &Error{Name: "InvalidParamsError"}
	// ErrUnavailable is not an A2A error: the network could not be reached
	// (no hub, hub down) and the same call may succeed later. A binding
	// reports it as a server error that invites a retry.
	ErrUnavailable = &Error{Name: "UnavailableError"}
)

// ErrorName is the name of the Error in err's chain, or "InternalError"
// for any other error.
func ErrorName(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Name
	}
	return "InternalError"
}

// Errorf returns an error with kind's name and a formatted detail.
func Errorf(kind *Error, format string, args ...any) error {
	return &Error{Name: kind.Name, Detail: fmt.Sprintf(format, args...)}
}

// Refusals: how a reason code leaves this node (core G1, ux UX-R9, UX-R10).

// ErrorBody is the control plane's error body (core §2.18, UX-R9): the
// sentence, its code and the command to copy. Every control-plane refusal
// has this form; MCP's tool error text is Error (UX-M4).
type ErrorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
	Next   string `json:"next,omitempty"`
	// HubCode is the hub's own code, on hub_refused only: the one reason
	// outside the table a body may carry.
	HubCode string `json:"hub_code,omitempty"`
}

// ErrorInfo is a google.rpc.ErrorInfo, as the local A2A interface puts it in
// a JSON-RPC error's data and a REST error's details. Metadata values are
// strings (map<string, string> in the proto).
type ErrorInfo struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// The two ErrorInfo domains (core G1) and the type URL of the message.
const (
	ErrorInfoType = "type.googleapis.com/google.rpc.ErrorInfo"
	DomainA2A     = "a2a-protocol.org"
	DomainANet    = "agentnetwork.org.cn"
)

// A2AError is a refusal as the local A2A interface reports it (UX-R10): the
// A2A error, its JSON-RPC code and REST status, the sentence as message and
// two ErrorInfo — A2A's first, anet's second. RetryAfter is the
// Retry-After a passing state carries, in seconds; 0 for the rest.
type A2AError struct {
	Name       string
	RPCCode    int
	HTTPStatus int
	// Status is the google.rpc code name of the REST error.
	Status     string
	Message    string
	Details    []ErrorInfo
	RetryAfter int
}

// Refusal is an error that carries a reason: a key of the reason table and
// its arguments. A handler returns it and the surface renders it in the
// language of the process (UX-G3): the control plane as an ErrorBody, the
// local A2A interface as an A2AError.
type Refusal struct {
	Key  string
	Args Args
	// HubCode is the hub's code behind hub_refused (UX-R9).
	HubCode string
	// RetryAfter is when a passing state may have passed (A2ATransient).
	RetryAfter time.Duration
	// Task is the task a refusal is about, for a client to go on with
	// (message_id_reused, correctness-ux CUX-A4): the control plane adds
	// task_id and state to its body, the local A2A interface taskId,
	// contextId and state to the anet ErrorInfo's metadata.
	Task *RefusalTask
}

// RefusalTask is the task a refusal names (Refusal.Task).
type RefusalTask struct {
	ID        string
	ContextID string
	State     TaskState
}

// TaskBody is the control plane's error body of a refusal that names a task
// (Refusal.Task): ErrorBody and the task's id and state.
type TaskBody struct {
	ErrorBody
	TaskID string `json:"task_id"`
	State  string `json:"state"`
}

// ControlBody is what the control plane writes for r in a language: Body,
// with the task's id and state when r names one (CUX-A4).
func (r *Refusal) ControlBody(lang Language) any {
	b := r.Body(lang)
	if r.Task == nil {
		return b
	}
	return TaskBody{ErrorBody: b, TaskID: r.Task.ID, State: r.Task.State.String()}
}

// Refuse returns a refusal with key and args.
func Refuse(key string, args Args) *Refusal { return &Refusal{Key: key, Args: args} }

// Is matches an A2A Error of the name the refusal's row maps to, so that
// errors.Is(no_local_task, ErrTaskNotFound) holds for the callers that ask
// for a task that is not there.
func (r *Refusal) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	shown, _ := shownKey(r.Key, nil)
	row, _ := Lookup(shown)
	return row.Class&ClassE != 0 && row.A2A.Err != A2ANone && a2aErrorOf(row.A2A.Err).Name == t.Name
}

// Error is the English sentence, for logs.
func (r *Refusal) Error() string { return Sentence(r.Key, English, r.Args) }

// Code is the reason code (the key without its variant). A key the table
// does not have is shown as unknown_code or refused (Sentence), and that is
// its code: a control-plane body never carries a reason outside the table
// (UX-R9), and the code always goes with the sentence shown (UX-G3).
func (r *Refusal) Code() string {
	shown, _ := shownKey(r.Key, nil)
	return ReasonCode(shown)
}

// Body is the control plane's error body in a language: the sentence, the
// code and the command (UX-R2, R9).
func (r *Refusal) Body(lang Language) ErrorBody {
	b := ErrorBody{Error: Sentence(r.Key, lang, r.Args), Reason: r.Code(), Next: Next(r.Key, lang, r.Args)}
	if b.Reason == "hub_refused" {
		b.HubCode = r.HubCode
	}
	return b
}

// CodeSuffix is what a command line or a tool error shows after a sentence so
// that a person can name the event and a script can match it: " [<reason>]",
// or " [hub_refused:<hub code>]" for a hub's refusal with a code of its own.
// It is empty when reason is not in the form of a code, so a body from another
// program shows its sentence alone. The JSON of a body is never changed.
func CodeSuffix(reason, hubCode string) string {
	if !codeRe.MatchString(reason) {
		return ""
	}
	if reason == "hub_refused" && codeRe.MatchString(hubCode) {
		return " [" + reason + ":" + hubCode + "]"
	}
	return " [" + reason + "]"
}

// defaultRetryAfter is the Retry-After of a passing state whose refusal did
// not say when (seconds): the shorter of the two waits HPP §4.7 gives.
const defaultRetryAfter = 15

// maxRetryAfter bounds the Retry-After of a passing state (seconds): a day.
// The longest automatic wait anet has, the hub join backoff, is 10 minutes
// (ux §2.6); a longer value is a mistake, not a reason to make a client
// wait for years.
const maxRetryAfter = 24 * 60 * 60

// A2A is the local A2A error in a language (UX-R10, core G1): the error the
// code's row names, the sentence as its message, and two ErrorInfo — the A2A
// error's reason in the A2A domain, then the code in anet's, with the code,
// the command and the variant in its metadata.
func (r *Refusal) A2A(lang Language) A2AError {
	shown, _ := shownKey(r.Key, nil)
	row, _ := Lookup(shown)
	e := a2aErrorOf(row.A2A.Err)
	e.Message = Sentence(r.Key, lang, r.Args)
	code := r.Code()
	md := map[string]string{"reason": code}
	if n := Next(r.Key, lang, r.Args); n != "" {
		md["next"] = n
	}
	if _, v, ok := strings.Cut(shown, "."); ok && code != "" {
		md["variant"] = v
	}
	if t := r.Task; t != nil {
		// The task the client can go on with; ErrorInfo metadata values
		// are strings (core G1).
		md["taskId"], md["contextId"], md["state"] = t.ID, t.ContextID, t.State.String()
	}
	e.Details = []ErrorInfo{
		{Type: ErrorInfoType, Reason: e.reason, Domain: DomainA2A},
		{Type: ErrorInfoType, Reason: strings.ToUpper(code), Domain: DomainANet, Metadata: md},
	}
	if row.A2A.Err == A2ATransient {
		// Whole seconds, rounded up, without adding to the duration (no
		// wrap-around near its largest value).
		secs := r.RetryAfter / time.Second
		if r.RetryAfter%time.Second > 0 {
			secs++
		}
		switch {
		case secs < 1:
			secs = defaultRetryAfter
		case secs > maxRetryAfter:
			secs = maxRetryAfter
		}
		e.RetryAfter = int(secs)
	}
	return e.A2AError
}

// a2aError is an A2AError without its message, details and wait, and the
// reason of its first ErrorInfo.
type a2aError struct {
	A2AError
	reason string
}

// a2aErrorOf is the A2A error of a kind (the rows of A2A v1.0.1 §5.4 that
// module/a2a's errorTable copies from a2a-go). A refusal of a row that is not
// class E is an internal error: it should not have reached A2A.
func a2aErrorOf(k A2AKind) a2aError {
	mk := func(name string, rpc, status int, st, reason string) a2aError {
		return a2aError{A2AError{Name: name, RPCCode: rpc, HTTPStatus: status, Status: st}, reason}
	}
	switch k {
	case A2AInvalidParams:
		return mk("InvalidParamsError", -32602, 400, "INVALID_ARGUMENT", "INVALID_PARAMS")
	case A2AFailedPrecondition:
		return mk("InvalidParamsError", -32602, 400, "FAILED_PRECONDITION", "INVALID_PARAMS")
	case A2ATransient:
		return mk("InternalError", -32603, 503, "UNAVAILABLE", "INTERNAL_ERROR")
	case A2ATaskNotFound:
		return mk("TaskNotFoundError", -32001, 404, "NOT_FOUND", "TASK_NOT_FOUND")
	case A2AUnsupportedOperation:
		return mk("UnsupportedOperationError", -32004, 400, "FAILED_PRECONDITION", "UNSUPPORTED_OPERATION")
	case A2AExtendedCardNotConfigured:
		return mk("ExtendedAgentCardNotConfiguredError", -32007, 400, "FAILED_PRECONDITION", "EXTENDED_AGENT_CARD_NOT_CONFIGURED")
	case A2AInvalidAgentResponse:
		return mk("InvalidAgentResponseError", -32006, 500, "INTERNAL", "INVALID_AGENT_RESPONSE")
	}
	return mk("InternalError", -32603, 500, "INTERNAL", "INTERNAL_ERROR")
}

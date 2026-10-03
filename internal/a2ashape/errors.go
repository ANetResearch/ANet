package a2ashape

import (
	"errors"
	"fmt"
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
}

// Refuse returns a refusal with key and args.
func Refuse(key string, args Args) *Refusal { return &Refusal{Key: key, Args: args} }

// Error is the English sentence, for logs.
func (r *Refusal) Error() string { return "" } // v030-todo(UX-5)

// Code is the reason code (the key without its variant).
func (r *Refusal) Code() string { return ReasonCode(r.Key) }

// Body is the control plane's error body in a language.
func (r *Refusal) Body(lang Language) ErrorBody { return ErrorBody{} } // v030-todo(UX-5)

// A2A is the local A2A error in a language.
func (r *Refusal) A2A(lang Language) A2AError { return A2AError{} } // v030-todo(UX-5)

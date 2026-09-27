package a2ashape

import (
	"errors"
	"fmt"
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

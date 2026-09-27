//go:build !no_a2a

package a2a

// convert.go moves values between the kernel's A2A projection
// (internal/a2ashape, which does not import an SDK) and a2a-go's types. The
// only conversion is a JSON round trip: the projection's JSON is A2A's JSON
// by construction, and internal/a2ashape's contract tests pin it against
// a2a-go byte for byte, so a round trip is exact where a field-by-field
// copy would be one more mapping to keep in step.

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// convert re-reads v as a T through its JSON.
func convert[T any](v any) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

// sdkTask converts a projected task.
func sdkTask(t module.Task) (*a2a.Task, error) {
	out, err := convert[a2a.Task](t)
	if err != nil {
		log.Printf("anet: a2a: task %s does not convert: %v", t.ID, err)
		return nil, a2a.NewError(a2a.ErrInternalError, "the task could not be encoded")
	}
	return &out, nil
}

// sdkEvent converts one projected stream item.
func sdkEvent(e module.TaskEvent) (a2a.Event, error) {
	sr, err := convert[a2a.StreamResponse](e)
	if err != nil || sr.Event == nil {
		log.Printf("anet: a2a: stream event does not convert: %v", err)
		return nil, a2a.NewError(a2a.ErrInternalError, "a task event could not be encoded")
	}
	return sr.Event, nil
}

// shapeMessage converts the client's message to the projection's form.
func shapeMessage(m *a2a.Message) (a2ashape.Message, error) {
	if m == nil {
		return a2ashape.Message{}, a2a.NewError(a2a.ErrInvalidParams, "message is required")
	}
	out, err := convert[a2ashape.Message](m)
	if err != nil {
		return a2ashape.Message{}, a2a.NewError(a2a.ErrInvalidParams, "message: "+err.Error())
	}
	return out, nil
}

// toSDKError maps a kernel error to the A2A error the client is given.
//
// The kernel reports A2A errors by name (a2ashape.Error). Only the name
// crosses: the message is fixed per error, except where the detail is
// about the client's own input (InvalidParams, UnsupportedOperation),
// which is what the client needs to correct it. Anything else — a store
// error, a hub reply, a path — stays in the daemon's log, and TaskNotFound
// reads the same whether the task does not exist or belongs to another
// agent [C17].
func toSDKError(err error) error {
	if err == nil {
		return nil
	}
	var sdk *a2a.Error
	if errors.As(err, &sdk) {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	detail := ""
	var e *a2ashape.Error
	if errors.As(err, &e) {
		detail = e.Detail
	}
	switch a2ashape.ErrorName(err) {
	case a2ashape.ErrTaskNotFound.Name:
		return a2a.NewError(a2a.ErrTaskNotFound, "task not found")
	case a2ashape.ErrTaskNotCancelable.Name:
		return a2a.NewError(a2a.ErrTaskNotCancelable, "the task cannot be canceled in its current state")
	case a2ashape.ErrUnsupportedOperation.Name:
		return a2a.NewError(a2a.ErrUnsupportedOperation, orDefault(detail, "this operation is not supported"))
	case a2ashape.ErrInvalidParams.Name:
		return a2a.NewError(a2a.ErrInvalidParams, orDefault(detail, "invalid params"))
	case a2ashape.ErrContentTypeNotSupported.Name:
		return a2a.NewError(a2a.ErrUnsupportedContentType, orDefault(detail, "content type not supported"))
	case a2ashape.ErrVersionNotSupported.Name:
		return a2a.NewError(a2a.ErrVersionNotSupported, "this interface speaks A2A 1.0")
	case a2ashape.ErrExtensionSupportRequired.Name:
		return a2a.NewError(a2a.ErrExtensionSupportRequired, orDefault(detail, "an extension must be activated"))
	case a2ashape.ErrPushNotificationNotSupported.Name:
		return a2a.NewError(a2a.ErrPushNotificationNotSupported, "push notifications are not supported")
	case a2ashape.ErrUnavailable.Name:
		log.Printf("anet: a2a: network unavailable: %v", err)
		return a2a.NewError(a2a.ErrServerError, "the network cannot be reached from this node right now; try again later")
	}
	log.Printf("anet: a2a: %v", err)
	return a2a.NewError(a2a.ErrInternalError, "internal error")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

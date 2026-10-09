//go:build !no_a2a

package a2a

// precheck.go carries out every send before the binding, and answers a
// streaming call that cannot start with an ordinary error response instead
// of a stream (an internal note item; a2a-tck STREAM-SUB-003 and STREAM-SUB-004).
//
// A send — SendMessage or SendStreamingMessage, on either binding — is
// read here from the request's own bytes, so that the numbers in the
// message (its metadata, a capability call's args) reach the kernel as the
// client wrote them: a2a-go reads them as doubles, which would make
// 9007199254740993 and 9007199254740992 one number, and the kernel holds a
// messageId to the literal content of its message (correctness-ux CUX-D1).
// A refusal of the reason table that the send meets — a messageId already
// used for other content (MESSAGE_ID_REUSED), the send gate — is written
// here in the two-ErrorInfo form of core G1 (refusal.go), which a2a-go's
// own error encoding cannot carry. Any other outcome of a SendMessage is
// handed to the handler, which answers it through the binding as before.
//
// a2a-go writes a stream's headers — HTTP 200, text/event-stream — before
// it asks the handler for the first event (a2asrv jsonrpc.go and rest.go),
// so an error the handler returns first (the task does not exist, belongs
// to another agent, has ended; the message is refused) reaches the client
// as the stream's one event, and a client that looks at the response
// rather than into the stream takes the call for a success. So the checks
// run here, before the binding: SubscribeToTask is checked for existence,
// scope and state; SendStreamingMessage is carried out — the message is
// sent, which is the one way to learn every reason the kernel may refuse it
// — and the task it answered is handed to the handler, which streams it
// without sending again. A call that passes opens its stream as before.
//
// The error is written in the binding's own form: a JSON-RPC response with
// the A2A code (and the request's id), or the HTTP+JSON binding's
// google.rpc.Status. Either has the HTTP status the HTTP+JSON binding gives
// that error (a2a-go's table, restStatus), not 200: a2a-go's own JSON-RPC
// client reads a streaming call's 200 body as SSE and would see an empty
// stream and no error at all, and a status says what happened to any
// client that looks no further.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// JSON-RPC method names of the streaming calls (A2A §9).
const (
	methodSendStreaming = "SendStreamingMessage"
	methodSubscribe     = "SubscribeToTask"
)

// streamCall is a send or a streaming call read from its request.
type streamCall struct {
	// id is the JSON-RPC request id; nil for the HTTP+JSON binding.
	id any
	// Exactly one of send and sub is set. A request whose parameters do not
	// decode has neither, and err says why.
	send *a2a.SendMessageRequest
	sub  *a2a.SubscribeToTaskRequest
	err  error
	// blocking marks a SendMessage (not streamed).
	blocking bool
	// msg is send's message as the client wrote it, numbers included
	// (literalMessage); nil when it does not read so.
	msg *a2ashape.Message
}

// readStreamCall reads r as a send or a streaming call, and returns nil for
// any other request, or for one the binding answers itself before its
// handler runs (a body that is not a JSON-RPC request). The body is read and
// put back for the binding.
func readStreamCall(r *http.Request, aid string, jsonrpc bool) *streamCall {
	if jsonrpc {
		body, err := io.ReadAll(r.Body)
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{err}))
		if err != nil {
			return nil
		}
		// The envelope first, without the params: a SendMessage can carry
		// files, and only a streaming call's params are wanted here.
		var env struct {
			JSONRPC string `json:"jsonrpc"`
			ID      any    `json:"id"`
			Method  string `json:"method"`
		}
		if decodeFirst(body, &env) != nil || env.JSONRPC != "2.0" || !validRPCID(env.ID) ||
			(env.Method != methodSendStreaming && env.Method != methodSubscribe && env.Method != methodSend) {
			return nil
		}
		var params struct {
			Params json.RawMessage `json:"params"`
		}
		_ = decodeFirst(body, &params)
		c := &streamCall{id: env.ID, blocking: env.Method == methodSend}
		if env.Method != methodSubscribe {
			c.send = &a2a.SendMessageRequest{}
			if c.err = json.Unmarshal(params.Params, c.send); c.err != nil {
				if c.blocking {
					return nil // a SendMessage the binding refuses as ever, in its own form
				}
				c.send = nil
			} else {
				c.msg = literalMessage(params.Params)
			}
		} else {
			c.sub = &a2a.SubscribeToTaskRequest{}
			if c.err = json.Unmarshal(params.Params, c.sub); c.err != nil {
				c.sub = nil
			}
		}
		if c.err != nil {
			c.err = a2a.NewError(a2a.ErrInvalidParams, "params: "+c.err.Error())
		}
		return c
	}
	rest := strings.TrimPrefix(r.URL.Path, agentsPath+"/"+aid+"/rest")
	switch {
	case r.Method == http.MethodPost && (rest == "/message:stream" || rest == "/message:send"):
		body, err := io.ReadAll(r.Body)
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{err}))
		if err != nil {
			return nil
		}
		c := &streamCall{send: &a2a.SendMessageRequest{}, blocking: rest == "/message:send"}
		if decodeFirst(body, c.send) != nil {
			return nil // the binding answers a body that does not decode, before any stream
		}
		c.send.Tenant = aid
		c.msg = literalMessage(body)
		return c
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) &&
		strings.HasPrefix(rest, "/tasks/") && strings.HasSuffix(rest, ":subscribe"):
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "/tasks/"), ":subscribe")
		if id == "" || strings.Contains(id, "/") {
			return nil
		}
		return &streamCall{sub: &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(id), Tenant: aid}}
	}
	return nil
}

// literalMessage is the message member of a SendMessageRequest's JSON, read
// into the projection's form with every number as the client wrote it
// (json.Number): in the message's metadata, and in its data parts
// (a2ashape.Part keeps theirs). nil when it does not read so — the caller
// then has a2a-go's reading only.
func literalMessage(req []byte) *a2ashape.Message {
	var w struct {
		Message json.RawMessage `json:"message"`
	}
	if decodeFirst(req, &w) != nil || len(w.Message) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(w.Message))
	dec.UseNumber()
	var m a2ashape.Message
	if dec.Decode(&m) != nil {
		return nil
	}
	return &m
}

// decodeFirst decodes the first JSON value of body into v, as the bindings
// read a request (a json.Decoder, which leaves what follows the value
// unread). json.Unmarshal refuses what follows, so a body with trailing
// bytes was a request the binding served and the check passed over, and
// its refusal went back inside a stream again (an internal note item).
func decodeFirst(body []byte, v any) error {
	return json.NewDecoder(bytes.NewReader(body)).Decode(v)
}

// errReader returns err once the body before it is read (nil: EOF), so a
// body that could not be read fails for the binding as it did for us.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	return 0, io.EOF
}

// validRPCID is a JSON-RPC id a2a-go accepts: absent, a string or a number.
func validRPCID(id any) bool {
	switch id.(type) {
	case nil, string, float64:
		return true
	}
	return false
}

// precheckStream carries out a send and runs the checks of a streaming call
// (see the file comment), and reports whether it answered the request with
// an error. The request it returns is the one to serve: a send carries its
// outcome.
func (s *server) precheckStream(w http.ResponseWriter, r *http.Request, aid string, jsonrpc bool) (*http.Request, bool) {
	c := readStreamCall(r, aid, jsonrpc)
	if c == nil {
		return r, false
	}
	ctx := r.Context()
	err := c.err
	switch {
	case err != nil:
	case c.sub != nil:
		err = s.h.checkSubscribe(ctx, c.sub)
	case c.send != nil && c.blocking:
		// SendMessage: carried out here, answered by the binding through
		// the handler — unless it meets a refusal of the reason table.
		sent := s.h.sendBlocking(ctx, c.send, c.msg)
		var ref *a2ashape.Refusal
		if !errors.As(sent.err, &ref) {
			return r.WithContext(context.WithValue(ctx, sentKey{}, sent)), false
		}
		lang := a2ashape.Lang()
		if jsonrpc {
			writeRefusalRPC(w, c.id, ref, lang)
		} else {
			writeRefusalREST(w, ref, lang)
		}
		return r, true
	case c.send != nil:
		var sent *sentStream
		if sent, err = s.h.sendForStream(ctx, c.send, c.msg); err == nil {
			return r.WithContext(context.WithValue(ctx, sentKey{}, sent)), false
		}
	}
	if err == nil {
		return r, false
	}
	var ref *a2ashape.Refusal
	if errors.As(err, &ref) {
		// A stream's error goes with the status its binding gives it
		// (writeRefusalRPCStatus).
		lang := a2ashape.Lang()
		if jsonrpc {
			writeRefusalRPCStatus(w, c.id, ref, lang)
		} else {
			writeRefusalREST(w, ref, lang)
		}
		return r, true
	}
	err = toSDKError(err)
	if jsonrpc {
		writeRPCErrorFor(w, c.id, err)
	} else {
		writeError(w, restStatus(err), err, err.Error())
	}
	return r, true
}

// sentStream is a send already carried out by precheckStream: a streaming
// one, or a SendMessage with its outcome (err).
type sentStream struct {
	req  module.TaskSend
	task module.Task
	err  error
}

type sentKey struct{}

// sentFrom is the send precheckStream carried out for this request, if any.
func sentFrom(ctx context.Context) (*sentStream, bool) {
	s, ok := ctx.Value(sentKey{}).(*sentStream)
	return s, ok && s != nil
}

// errorTable is a2a-go's mapping of each A2A error to its JSON-RPC code and
// its HTTP+JSON status (a2a-go keeps both tables internal), so that an error
// written here reads the same as one the binding writes. The A2A errors'
// rows are those of A2A v1.0.1 §5.4, which moved TaskNotCancelable and
// ContentTypeNotSupported to 400 (v1.0.0 had 409 and 415;
// docs/a2a/issue-a2a-go.md A11, withdrawn).
var errorTable = []struct {
	err    error
	code   int
	status int
	grpc   string
}{
	{a2a.ErrParseError, -32700, http.StatusBadRequest, "INVALID_ARGUMENT"},
	{a2a.ErrInvalidRequest, -32600, http.StatusBadRequest, "INVALID_ARGUMENT"},
	{a2a.ErrMethodNotFound, -32601, http.StatusNotImplemented, "UNIMPLEMENTED"},
	{a2a.ErrInvalidParams, -32602, http.StatusBadRequest, "INVALID_ARGUMENT"},
	{a2a.ErrInternalError, -32603, http.StatusInternalServerError, "INTERNAL"},
	{a2a.ErrServerError, -32000, http.StatusInternalServerError, "INTERNAL"},
	{a2a.ErrTaskNotFound, -32001, http.StatusNotFound, "NOT_FOUND"},
	{a2a.ErrTaskNotCancelable, -32002, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrPushNotificationNotSupported, -32003, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrUnsupportedOperation, -32004, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrUnsupportedContentType, -32005, http.StatusBadRequest, "INVALID_ARGUMENT"},
	{a2a.ErrInvalidAgentResponse, -32006, http.StatusInternalServerError, "INTERNAL"},
	{a2a.ErrExtendedCardNotConfigured, -32007, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrExtensionSupportRequired, -32008, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrVersionNotSupported, -32009, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrUnauthenticated, -32603, http.StatusUnauthorized, "UNAUTHENTICATED"},
	{a2a.ErrUnauthorized, -32603, http.StatusForbidden, "PERMISSION_DENIED"},
	{context.Canceled, -32603, 499, "CANCELLED"},
	{context.DeadlineExceeded, -32603, http.StatusGatewayTimeout, "DEADLINE_EXCEEDED"},
}

// errorEntry is err's row of errorTable; an unknown error is internal.
func errorEntry(err error) (code, status int, grpc string) {
	for _, e := range errorTable {
		if errors.Is(err, e.err) {
			return e.code, e.status, e.grpc
		}
	}
	return -32603, http.StatusInternalServerError, "INTERNAL"
}

// restStatus is the HTTP status the HTTP+JSON binding gives err.
func restStatus(err error) int {
	_, status, _ := errorEntry(err)
	return status
}

// writeRPCErrorFor answers a JSON-RPC request with err, in the binding's
// error form, with the request's id and restStatus as the HTTP status.
func writeRPCErrorFor(w http.ResponseWriter, id any, err error) {
	code, status, _ := errorEntry(err)
	writeRPCResponse(w, status, id, code, err.Error(), a2a.ErrorReason(err))
}

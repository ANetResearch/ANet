//go:build !no_mcp

package agentwire

import (
	"errors"
	"io/fs"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// Everything wire says to a person is a Msg: a key of the reason table
// (internal/a2ashape/reasons.go) and the local values its template takes. The
// package holds no sentence of its own, so the CLI renders each one in the
// language of the process and the English a log or a test reads is the same
// row. A value is a path, a tool name, a line number or an operating-system
// error, never a line of a file the user wrote (UX-G2).

// Msg is a sentence of the reason table with its arguments. Detail is text
// printed after the sentence, one line each, that is not a sentence: the
// JSON an operator adds by hand to a file anet will not rewrite.
type Msg struct {
	Key    string
	Args   a2ashape.Args
	Detail string
}

// msg builds a Msg from alternating argument names and values.
func msg(key string, kv ...any) Msg {
	m := Msg{Key: key}
	if len(kv) > 0 {
		m.Args = a2ashape.Args{}
		for i := 0; i+1 < len(kv); i += 2 {
			m.Args[kv[i].(string)] = kv[i+1]
		}
	}
	return m
}

// Text is the sentence in a language.
func (m Msg) Text(lang a2ashape.Language) string { return a2ashape.Sentence(m.Key, lang, m.Args) }

// Next is the command the sentence's row asks for, or "".
func (m Msg) Next(lang a2ashape.Language) string { return a2ashape.Next(m.Key, lang, m.Args) }

// Code is the reason code of the key (the key without its variant), or ""
// for a status word, a result line or a note, which have none.
func (m Msg) Code() string { return a2ashape.ReasonCode(m.Key) }

// String is the English sentence.
func (m Msg) String() string { return m.Text(a2ashape.English) }

// Error is a Msg that ends an operation: a refusal or a failure, always a
// wire_ code. Its Error text is English; the CLI renders the sentence and its
// next step in the language of the process (Msg.Text, Msg.Next).
type Error struct{ Msg }

func (e *Error) Error() string { return e.Msg.String() }

// fail builds an *Error.
func fail(key string, kv ...any) *Error { return &Error{msg(key, kv...)} }

// ConflictError is a same-named entry, or a file shape, that anet did not
// write and cannot extend. The tool is left untouched; the operator changes
// the file and runs wire again.
type ConflictError struct{ Msg }

func (e *ConflictError) Error() string { return e.Msg.String() }

// conflict builds a *ConflictError.
func conflict(key string, kv ...any) *ConflictError {
	return &ConflictError{msg(key, kv...)}
}

// AsMsg is the Msg an error carries, when it is one of this package's: the
// caller renders it in a language. Any other error has none.
func AsMsg(err error) (Msg, bool) {
	var ce *ConflictError
	if errors.As(err, &ce) {
		return ce.Msg, true
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Msg, true
	}
	return Msg{}, false
}

// ioError wraps an operating-system error on path as wire_io_failed. An
// error of this package passes through; only the system's own words (such as
// "permission denied") are shown, not the call that failed.
func ioError(path string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := AsMsg(err); ok {
		return err
	}
	return fail("wire_io_failed", "path", path, "error", sysText(err))
}

// sysText is the operating system's words in err, without the call and the
// path around them.
func sysText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

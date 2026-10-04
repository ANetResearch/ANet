package hubapi

import (
	"context"
	"net/http/httptrace"
	"sync/atomic"
)

// TraceHanded returns ctx with an HTTP trace attached, and a function that
// reports whether a request made under it got a connection.
//
// That is the line between "the envelope never left this process" and "it
// may have". Until the client has a connection — DNS, the dial and the TLS
// handshake all done — no byte of the request body can have gone, and a
// failure there is one the caller can report as never sent. From the moment
// it has one the request may be on the wire, so the answer stays true
// whatever happens next: a connection that drops, a timeout, a 4xx or a
// 500. A hub that answered cannot be taken to have stored nothing.
//
// Traces already on ctx keep running.
func TraceHanded(ctx context.Context) (context.Context, func() bool) {
	var handed atomic.Bool
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { handed.Store(true) },
	}
	return httptrace.WithClientTrace(ctx, trace), handed.Load
}

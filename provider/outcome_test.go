package provider

// When a call may have reached its target (TrackSent), which decides whether
// an error is "could not reach it" (UNAVAILABLE) or an outcome nobody knows
// (SI-6: "not known" must never be reported as "did not happen").

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
)

// Over HTTP/2 the transport flushes the header block, which is when the
// server starts the handler, and only then reports WroteHeaders, on the
// goroutine that writes the request; Do returns as soon as the context
// ends. A call whose deadline passed in between reached its target, and
// TrackSent said it had not been sent. The outer trace below holds the
// WroteHeaders report back the way a slow scheduler would, so the window is
// open every run rather than once in a while under load.
func TestACallCutOffAfterItsHeadersWentOutMayHaveBeenSent(t *testing.T) {
	arrived := make(chan struct{})
	var once sync.Once
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("the request came over HTTP/%d.%d; this test is about HTTP/2", r.ProtoMajor, r.ProtoMinor)
		}
		once.Do(func() { close(arrived) }) // the call has reached the service: it may act now
		<-r.Context().Done()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader([]byte(`{"to":"x"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req, sent := TrackSent(req)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // runs before srv.Close
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteHeaders: func() { <-release },
	}))

	errc := make(chan error, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	select {
	case <-arrived:
	case err := <-errc:
		t.Fatalf("the call ended before it reached the service: %v", err)
	}
	cancel() // the call's deadline passes while the service has it
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want the call cut off", err)
	}
	if !sent() {
		t.Fatal("a call the service received is reported as never sent: UNAVAILABLE, where the effect may have happened")
	}
}

// A call whose deadline passes before the transport has a connection for it
// wrote nothing anywhere: that is still "not sent", the UNAVAILABLE that
// tells a requester trying elsewhere is safe.
func TestACallCutOffBeforeItHadAConnectionWasNotSent(t *testing.T) {
	dialing := make(chan struct{})
	cli := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(dialing)
			<-ctx.Done() // a far side that takes longer to reach than the call may wait
			return nil, ctx.Err()
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://backend.invalid/x", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req, sent := TrackSent(req)
	errc := make(chan error, 1)
	go func() {
		_, err := cli.Do(req)
		errc <- err
	}()
	<-dialing
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("no error")
	}
	if sent() {
		t.Fatal("a call that never had a connection is reported as possibly sent")
	}
}

// The transport sends a request again on a new connection only when nothing
// of it was written — for one that is not idempotent. An idempotent one it
// may send again after it went out, so "may have been sent" stays.
func TestARetryStartsUnsentOnlyForARequestNotRetriedAfterItWentOut(t *testing.T) {
	for _, c := range []struct {
		method string
		header string
		stays  bool
	}{
		{http.MethodPost, "", false},
		{http.MethodPatch, "", false},
		{http.MethodGet, "", true},
		{http.MethodPut, "Idempotency-Key", true},
		{http.MethodPost, "X-Idempotency-Key", true},
	} {
		req, err := http.NewRequest(c.method, "http://backend.invalid/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.header != "" {
			req.Header.Set(c.header, "k1")
		}
		traced, sent := TrackSent(req)
		tr := httptrace.ContextClientTrace(traced.Context())
		if tr == nil || tr.GetConn == nil || tr.GotConn == nil {
			t.Fatal("TrackSent does not follow the transport's connections (GetConn, GotConn)")
		}
		tr.GetConn("backend.invalid:80")
		if sent() {
			t.Fatalf("%s %s: sent before a connection", c.method, c.header)
		}
		tr.GotConn(httptrace.GotConnInfo{Reused: true})
		if !sent() {
			t.Fatalf("%s %s: not sent once it had a connection", c.method, c.header)
		}
		tr.GetConn("backend.invalid:80") // the transport tries again on a new connection
		if sent() != c.stays {
			t.Fatalf("%s %s: sent %v after a retry began, want %v", c.method, c.header, sent(), c.stays)
		}
	}
}

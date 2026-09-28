package localpeer

// F18 spot checks after the round-5b merge (docs/notes/0030): the token
// reaches only a listener verified on the very connection that carries it,
// also when the connection is a pooled one, the address is a name with two
// loopback families, or the challenge is relayed byte for byte.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// The client talked to this user's daemon and keeps the connection pooled.
// The daemon stops; another user binds the port. The next request with the
// same client does not reach the new holder: the pooled connection was the
// old daemon's, and a new one is verified before the token is written.
func TestSpotAPooledConnectionDoesNotCarryTheTokenToTheNextHolder(t *testing.T) {
	haveSocketTable(t)
	const token = "tok-pooled-spot"
	own := newCapture(t, nil)
	addr := own.addr()
	c := Client(token, 5*time.Second)
	do := func() error {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/status", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return err
	}
	if err := do(); err != nil {
		t.Fatalf("this user's daemon: %v", err)
	}
	own.srv.Close()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("the port could not be taken again: %v", err)
	}
	squat := &capture{ln: ln}
	squat.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			squat.mu.Lock()
			squat.auth = append(squat.auth, a)
			squat.mu.Unlock()
		}
	})}
	go squat.srv.Serve(ln)
	defer squat.srv.Close()
	defer TreatAsForeignForTest(addr)()
	for i := 0; i < 3; i++ {
		if err := do(); err == nil {
			t.Fatalf("request %d went through to the port's new holder", i)
		} else if !errors.Is(err, ErrNotOurs) {
			t.Logf("request %d: %v", i, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if s := squat.seen(); len(s) != 0 {
		t.Fatalf("the port's new holder received %v", s)
	}
}

// The daemon holds 127.0.0.1:P; another user holds [::1]:P. A client given
// "localhost:P" may dial either family. Whichever it dials, the token goes
// only to this user's listener.
func TestSpotASquatterOnTheOtherLoopbackFamilyGetsNothing(t *testing.T) {
	haveSocketTable(t)
	const token = "tok-family-spot"
	own := newCapture(t, nil)
	_, port, _ := net.SplitHostPort(own.addr())
	ln, err := net.Listen("tcp", "[::1]:"+port)
	if err != nil {
		t.Skipf("no IPv6 loopback with that port here: %v", err)
	}
	squat := &capture{ln: ln}
	squat.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			squat.mu.Lock()
			squat.auth = append(squat.auth, a)
			squat.mu.Unlock()
		}
	})}
	go squat.srv.Serve(ln)
	defer squat.srv.Close()
	defer TreatAsForeignForTest("[::1]:" + port)()
	p, _ := strconv.Atoi(port)
	reached := 0
	for i := 0; i < 6; i++ {
		// A fresh client each time, so the dial is repeated.
		c := &http.Client{Transport: Transport(token + strconv.Itoa(i)), Timeout: 5 * time.Second}
		req, _ := http.NewRequest(http.MethodPost, "http://localhost:"+strconv.Itoa(p)+"/status", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
			reached++
		}
	}
	// A client configured with the other family's address outright.
	req, _ := http.NewRequest(http.MethodPost, "http://[::1]:"+port+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := Client(token, 5*time.Second).Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("a request to the other user's [::1] listener went through")
	} else if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("[::1]: %v, want ErrNotOurs", err)
	}
	time.Sleep(50 * time.Millisecond)
	if s := squat.seen(); len(s) != 0 {
		t.Fatalf("the other family's listener received %v", s)
	}
	t.Logf("\"localhost\" reached this user's daemon %d of 6 times", reached)
}

// Where the challenge is used: a squatter that relays the connection byte
// for byte to the real daemon (another port) gets a proof bound to the
// real daemon's listener, not to the address the client dialled, and the
// check fails before the token is sent.
func TestSpotAByteRelayToTheRealDaemonFailsTheChallenge(t *testing.T) {
	const token = "tok-relay-spot"
	real := newCapture(t, func(w http.ResponseWriter, r *http.Request) {
		out := `{"anet":true}`
		if p, ok := Answer(r, token); ok {
			out = `{"anet":true,"proof":"` + p + `"}`
		}
		io.WriteString(w, out)
	})
	relay, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	go func() {
		for {
			in, err := relay.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", real.addr())
			if err != nil {
				in.Close()
				continue
			}
			go func() { io.Copy(out, in); out.Close() }()
			go func() { io.Copy(in, out); in.Close() }()
		}
	}()
	conn, err := net.Dial("tcp", relay.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	far, _ := addrPort(conn.RemoteAddr())
	if err := challenge(context.Background(), conn, token, far); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("challenge through a byte relay: %v, want refused", err)
	}
	// Control: the real daemon passes it.
	direct, err := net.Dial("tcp", real.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	dfar, _ := addrPort(direct.RemoteAddr())
	if err := challenge(context.Background(), direct, token, dfar); err != nil {
		t.Fatalf("control: the real daemon's own proof: %v", err)
	}
}

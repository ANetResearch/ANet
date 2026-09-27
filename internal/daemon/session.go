package daemon

// session.go implements browser console access without putting the control token in the page
// (A2A-DESIGN §7.2, §7.4, SI-7).
//
//  1. `anet console` holds the bearer token and calls POST /console/ticket. The daemon returns a random
//     single-use ticket valid for 60 seconds and the URL http://127.0.0.1:<port>/console#t=<ticket>.
//     The ticket travels in the URL fragment, which the browser never sends in a request line, a
//     Referer header or a server log.
//  2. The page removes the fragment from its URL and posts the ticket to POST /console/session. The
//     daemon consumes the ticket, sets the session cookie anet_s_<port> (HttpOnly; SameSite=Strict;
//     Path=/) and returns a CSRF value once, in the response body. The page keeps the CSRF value in
//     memory only. No endpoint returns it from the cookie, so reloading the page requires a new ticket.
//  3. Each session request carries the cookie and the X-Anet-CSRF header, and ctlsec.go restricts it to
//     the session route allowlist.
//
// The cookie name carries the listener port because cookies are not isolated by port: two daemons on
// 127.0.0.1 would otherwise overwrite each other's session cookie. Sessions live in memory; a daemon
// restart ends them.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	consoleTicketTTL   = 60 * time.Second
	consoleSessionTTL  = 12 * time.Hour
	maxConsoleTickets  = 32
	maxConsoleSessions = 32
	csrfHeader         = "X-Anet-CSRF"
	sessionCookiePref  = "anet_s_"
)

// consoleSessions holds outstanding tickets and live sessions. Tickets and session ids are stored by
// SHA-256 digest, so a map lookup's timing depends on the digest and not on how much of a guessed
// value matches.
type consoleSessions struct {
	mu       sync.Mutex
	tickets  map[string]time.Time // digest(ticket) -> expiry
	sessions map[string]*consoleSession
	now      func() time.Time
}

type consoleSession struct {
	csrf    string
	expires time.Time
}

func newConsoleSessions() *consoleSessions {
	return &consoleSessions{
		tickets:  map[string]time.Time{},
		sessions: map[string]*consoleSession{},
		now:      time.Now,
	}
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// purgeLocked drops expired tickets and sessions.
func (c *consoleSessions) purgeLocked(now time.Time) {
	for k, exp := range c.tickets {
		if !now.Before(exp) {
			delete(c.tickets, k)
		}
	}
	for k, s := range c.sessions {
		if !now.Before(s.expires) {
			delete(c.sessions, k)
		}
	}
}

// issueTicket mints a single-use ticket. When maxConsoleTickets are outstanding the one closest to
// expiry is dropped, which bounds memory without refusing the operator's newest request.
func (c *consoleSessions) issueTicket() (string, time.Time, error) {
	t, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeLocked(now)
	for len(c.tickets) >= maxConsoleTickets {
		var oldest string
		var oldestExp time.Time
		for k, exp := range c.tickets {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = k, exp
			}
		}
		delete(c.tickets, oldest)
	}
	exp := now.Add(consoleTicketTTL)
	c.tickets[digest(t)] = exp
	return t, exp, nil
}

// redeemTicket consumes a ticket. It reports false for an unknown, already used or expired ticket.
func (c *consoleSessions) redeemTicket(t string) bool {
	if t == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := digest(t)
	exp, ok := c.tickets[k]
	delete(c.tickets, k)
	return ok && c.now().Before(exp)
}

// createSession starts a session and returns its id and CSRF value. When maxConsoleSessions are live
// the one closest to expiry is ended.
func (c *consoleSessions) createSession() (id, csrf string, exp time.Time, err error) {
	if id, err = randomToken(32); err != nil {
		return "", "", time.Time{}, err
	}
	if csrf, err = randomToken(32); err != nil {
		return "", "", time.Time{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeLocked(now)
	for len(c.sessions) >= maxConsoleSessions {
		var oldest string
		var oldestExp time.Time
		for k, s := range c.sessions {
			if oldest == "" || s.expires.Before(oldestExp) {
				oldest, oldestExp = k, s.expires
			}
		}
		delete(c.sessions, oldest)
	}
	exp = now.Add(consoleSessionTTL)
	c.sessions[digest(id)] = &consoleSession{csrf: csrf, expires: exp}
	return id, csrf, exp, nil
}

// fromRequest returns the live session named by the request's session cookie, or nil.
func (c *consoleSessions) fromRequest(r *http.Request) *consoleSession {
	ck, err := r.Cookie(sessionCookieName(requestPort(r)))
	if err != nil || ck.Value == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[digest(ck.Value)]
	if !ok {
		return nil
	}
	if !c.now().Before(s.expires) {
		delete(c.sessions, digest(ck.Value))
		return nil
	}
	return s
}

func sessionCookieName(port string) string { return sessionCookiePref + port }

// consoleHost is the host part of URLs handed to a browser for this daemon: the configured loopback
// name, with 127.0.0.1 for anything else.
func consoleHost(controlAddr string) string {
	host, _, err := net.SplitHostPort(controlAddr)
	if err != nil {
		return "127.0.0.1"
	}
	switch strings.ToLower(host) {
	case "localhost":
		return "localhost"
	case "::1":
		return "[::1]"
	}
	return "127.0.0.1"
}

// hConsoleTicket issues a console ticket (bearer only: it is not on the session allowlist, so a session
// cannot extend itself). The answer carries the URL `anet console` opens.
func (d *Daemon) hConsoleTicket(cs *consoleSessions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, exp, err := cs.issueTicket()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ticket: " + err.Error()})
			return
		}
		u := "http://" + consoleHost(d.config().ControlAddr) + ":" + requestPort(r) + "/console#t=" + t
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"ticket": t, "url": u, "expires_in": int(consoleTicketTTL / time.Second), "expires_at": exp.UnixMilli(),
		})
	}
}

// hConsoleSession exchanges a ticket for a session. It is reachable without other credentials; the
// ticket is the credential. Only a same-origin browser request is accepted: the Origin header is
// required (hostGuard has already checked that it is this listener) and a JSON content type is required,
// which a cross-site form cannot send without a CORS preflight.
func (d *Daemon) hConsoleSession(cs *consoleSessions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Origin") == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "a console session is started by the console page"})
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "application/json required"})
			return
		}
		var req struct {
			Ticket string `json:"ticket"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		if !cs.redeemTicket(req.Ticket) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "the console ticket is unknown, used or expired; run `anet console` again"})
			return
		}
		id, csrf, exp, err := cs.createSession()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session: " + err.Error()})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookieName(requestPort(r)), Value: id, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
			MaxAge: int(consoleSessionTTL / time.Second),
		})
		cfg := d.config()
		writeJSON(w, http.StatusOK, map[string]any{
			"csrf": csrf, "aid": d.AID(), "name": cfg.Name, "hub": cfg.HubURL, "expires_at": exp.UnixMilli(),
		})
	}
}

// switchTimeout bounds the ticket request to another local daemon.
const switchTimeout = 5 * time.Second

// hConsoleSwitch implements the console identity switcher (A2A-DESIGN §7.4). The current daemon finds
// the target among the running local daemons, reads that identity's control token (same uid, from the
// target's own data dir), asks the target for a console ticket and returns the target's console URL.
// The page then navigates there; the target sets its own port-named session cookie.
//
// The token is sent only to a loopback control address, and only after checking that the data dir the
// registry names holds the requested AID, so a stale or edited registry entry cannot direct the token
// of one identity to a daemon of another.
func (d *Daemon) hConsoleSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AID string `json:"aid"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.AID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "aid required"})
		return
	}
	if req.AID == d.AID() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this console already is " + req.AID})
		return
	}
	for _, e := range RunningDaemons() {
		if e.AID != req.AID || e.DataDir == "" || checkLoopbackControlAddr(e.ControlAddr) != nil {
			continue
		}
		l := NewLayout(e.DataDir)
		if ReadIdentityAID(l) != req.AID {
			continue
		}
		tok, err := os.ReadFile(l.ControlTokenPath())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "read the target's control token: " + err.Error()})
			return
		}
		u, err := requestTicket(r.Context(), e.ControlAddr, strings.TrimSpace(string(tok)))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"url": u, "aid": e.AID, "name": e.Name})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no running local daemon has AID " + req.AID})
}

// requestTicket asks the daemon at controlAddr for a console ticket and returns its console URL. The
// URL must point at that same daemon.
func requestTicket(ctx context.Context, controlAddr, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, switchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+controlAddr+"/console/ticket", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ticket from %s: %w", controlAddr, err)
	}
	defer resp.Body.Close()
	var out struct {
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("ticket from %s: %s: %w", controlAddr, resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK || out.URL == "" {
		return "", fmt.Errorf("ticket from %s: %s %s", controlAddr, resp.Status, out.Error)
	}
	pu, err := url.Parse(out.URL)
	_, want, _ := net.SplitHostPort(controlAddr)
	if err != nil || pu.Scheme != "http" || pu.Port() != want || !loopbackName(pu.Hostname()) {
		return "", fmt.Errorf("ticket from %s: unexpected console URL", controlAddr)
	}
	return out.URL, nil
}

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The control plane is a LOCAL HTTP API (loopback by default) the CLI uses to drive a running daemon.
// Auth is a bearer token persisted at control_token.txt (0600); the CLI reads the same file. This is a
// control channel between the operator's shell and their own daemon — not a network-facing surface.

// loadOrGenControlToken loads (or generates + persists) the control bearer token.
func loadOrGenControlToken(l Layout) (string, error) {
	b, err := os.ReadFile(l.ControlTokenPath())
	if err == nil {
		t := strings.TrimSpace(string(b))
		if t != "" {
			return t, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw[:])
	if err := l.EnsureRoot(); err != nil {
		return "", err
	}
	if err := writeFileAtomic(l.ControlTokenPath(), []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// daemonPointer is the JSON written to DaemonPointerPath so a CLI in a different env can find the daemon.
type daemonPointer struct {
	ControlAddr string `json:"control_addr"`
	DataDir     string `json:"data_dir"`
}

// writeDaemonPointer records this daemon's control endpoint + data dir at the uid-scoped pointer path
// (best-effort; failures are silent — the pointer is a convenience fallback, not a requirement).
func writeDaemonPointer(controlAddr, dataDir string) {
	p := DaemonPointerPath()
	if err := ensurePrivateDir(filepath.Dir(p)); err != nil {
		return
	}
	b, err := json.Marshal(daemonPointer{ControlAddr: controlAddr, DataDir: dataDir})
	if err != nil {
		return
	}
	_ = writeFileAtomic(p, b, 0o600)
}

// ResolveControl returns the control base URL + bearer token for the running daemon. It prefers the
// caller's own data dir (layout); if that has no token (the daemon lives elsewhere — e.g. an agent tool
// whose HOME differs from the operator's), it falls back to the uid-scoped daemon pointer. This makes
// `anet <verb>` work from any process of the daemon's uid without threading ANET_DATA_DIR.
func ResolveControl(layout Layout) (baseURL, token string, err error) {
	cfg, cerr := LoadConfig(layout)
	if cerr == nil {
		if b, rerr := os.ReadFile(layout.ControlTokenPath()); rerr == nil {
			return "http://" + cfg.ControlAddr, strings.TrimSpace(string(b)), nil
		}
	}
	// Fallback: the uid-scoped pointer a running daemon published.
	pb, perr := readDaemonPointerFile()
	if perr != nil {
		return "", "", fmt.Errorf("read control token (is the daemon running?): %w", perr)
	}
	var dp daemonPointer
	if json.Unmarshal(pb, &dp) != nil || dp.ControlAddr == "" || dp.DataDir == "" {
		return "", "", fmt.Errorf("read control token (is the daemon running?): bad daemon pointer")
	}
	// The token is about to be sent to the pointer's address; only a loopback address qualifies.
	if err := checkLoopbackControlAddr(dp.ControlAddr); err != nil {
		return "", "", fmt.Errorf("daemon pointer: %w", err)
	}
	tb, terr := os.ReadFile(NewLayout(dp.DataDir).ControlTokenPath())
	if terr != nil {
		return "", "", fmt.Errorf("read control token (is the daemon running?): %w", terr)
	}
	return "http://" + dp.ControlAddr, strings.TrimSpace(string(tb)), nil
}

// ResolveControlStrict resolves the control plane for EXACTLY this data dir — no uid-pointer fallback. It
// is used whenever the operator pinned a specific identity (--id/ANET_ID/ANET_DATA_DIR/current), so with
// several daemons running the CLI can never silently talk to the wrong one: if this identity's own daemon
// isn't reachable, that's an error, not a hop to whichever daemon started last.
func ResolveControlStrict(l Layout) (baseURL, token string, err error) {
	cfg, ok := loadConfigNoCreate(l)
	if !ok {
		return "", "", fmt.Errorf("no daemon for this identity (data dir %s has no config yet)", l.Root)
	}
	b, rerr := os.ReadFile(l.ControlTokenPath())
	if rerr != nil {
		return "", "", fmt.Errorf("read control token (is this identity's daemon running?): %w", rerr)
	}
	return "http://" + cfg.ControlAddr, strings.TrimSpace(string(b)), nil
}

// ControlHandler returns the daemon's control-plane HTTP handler. Exposed so tests can drive it via
// httptest without binding a port. Every route registered on api below is authenticated (bearer token,
// or a console session for the routes in ctlsec.go's sessionRoutes); the Host/Origin guard, the console
// page and the console session routes are added by secureControlPlane (ctlsec.go).
func (d *Daemon) ControlHandler(token string) http.Handler {
	api := newRouteMux()
	api.HandleFunc("GET /status", d.hStatus)
	api.HandleFunc("POST /status", d.hStatus)
	api.HandleFunc("POST /hub-register", d.hHubRegister)
	api.HandleFunc("POST /hub-leave", d.hHubLeave)
	api.HandleFunc("POST /p2p-advertise", d.hP2PAdvertise)
	api.HandleFunc("POST /accept", d.hAccept)
	api.HandleFunc("POST /autoreply", d.hAutoReply)
	api.HandleFunc("POST /autoreply-test", d.hAutoReplyTest)
	api.HandleFunc("POST /shutdown", d.hShutdown)
	api.HandleFunc("POST /profile", d.hProfile)
	api.HandleFunc("POST /find", d.hFind)
	api.HandleFunc("POST /delegate", d.hDelegate)
	api.HandleFunc("POST /inbox", d.hInbox)
	api.HandleFunc("POST /message", d.hMessage)
	api.HandleFunc("POST /pull", d.hPull)
	api.HandleFunc("POST /end", d.hEnd)
	api.HandleFunc("POST /end-accept", d.hEndAccept)
	api.HandleFunc("POST /results", d.hResults)
	api.HandleFunc("POST /review", d.hReview)
	api.HandleFunc("POST /threads", d.hThreads)
	api.HandleFunc("POST /thread", d.hThread)
	api.HandleFunc("POST /identities", d.hIdentities)
	api.HandleFunc("POST /evidence", d.hEvidence)
	api.HandleFunc("POST /balance", d.hBalance)
	api.HandleFunc("POST /redeem", d.hRedeemCredit)
	api.HandleFunc("POST /x402-authorize", d.hX402Authorize)
	api.HandleFunc("POST /reconcile", d.hReconcile)
	api.HandleFunc("POST /audit-hub", d.hAuditHub)
	api.HandleFunc("POST /visibility", d.hVisibility)
	// Inbound policy and peer lists (A2A-DESIGN §5; handlers in
	// inbound_api.go). Bearer only: none of these is a console session
	// route. The TTY confirmation for allow, trust and approve is made by
	// the CLI before it calls them (§5.3, §21 item 13).
	api.HandleFunc("POST /peers/list", d.hPeersList)
	api.HandleFunc("POST /peers/allow", d.hPeersAllow)
	api.HandleFunc("POST /peers/trust", d.hPeersTrust)
	api.HandleFunc("POST /peers/deny", d.hPeersDeny)
	api.HandleFunc("POST /peers/remove", d.hPeersRemove)
	api.HandleFunc("POST /inbound/policy", d.hInboundPolicy)
	api.HandleFunc("POST /inbound/pending", d.hInboundPending)
	api.HandleFunc("POST /inbound/approve", d.hInboundApprove)
	api.HandleFunc("POST /inbound/reject", d.hInboundReject)
	// Spending limits (§8.6; payments_config.go). Bearer only; the CLI
	// asks for the TTY confirmation before it writes.
	api.HandleFunc("POST /payments/limits", d.hPaymentLimits)
	return d.secureControlPlane(token, api)
}

// maxControlBody caps a control-plane request body.
//
// A gigabyte, because 1 MiB was the wrong kind of limit: a capability
// call carries its arguments as JSON, so an image handed to an
// image-capability had about 750 KB of headroom once base64 inflated it.
// The cap was defending a local, token-gated socket against its own
// operator, and the cost was that the obvious first thing anyone tries
// did not work.
//
// It is a memory ceiling as much as a size one — the body is buffered to
// decode the JSON — so this is headroom, not a recommendation. And it is
// not the only limit on the path: a payload still has to cross the hub,
// which caps its own bodies, so raising this alone does not make an
// arbitrarily large call deliverable.
const maxControlBody = 1 << 30

// maxUploadBody caps a multipart upload (console attach). One attachment is bounded to maxAttachmentBytes
// (64 MiB); this leaves headroom for a couple of files plus multipart framing in a single request.
const maxUploadBody = 130 << 20 // 130 MiB

// readMultipartAttachments streams a multipart/form-data control request, collecting non-file fields into
// a map and each file part into a self-verified attachment. It reads parts incrementally (no on-disk temp
// files) and bounds every file to maxAttachmentBytes; the authentication gate already caps the total body.
func readMultipartAttachments(r *http.Request) (fields map[string]string, atts []delegation.Attachment, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, err
	}
	fields = map[string]string{}
	for {
		p, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return nil, nil, perr
		}
		if p.FileName() == "" { // ordinary form field (provider/goal/interaction_id/body)
			b, _ := io.ReadAll(io.LimitReader(p, 1<<20))
			fields[p.FormName()] = string(b)
			_ = p.Close()
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(p, maxAttachmentBytes+1))
		_ = p.Close()
		if rerr != nil {
			return nil, nil, rerr
		}
		att, aerr := attachmentFromBytes(p.FileName(), data)
		if aerr != nil {
			return nil, nil, aerr
		}
		atts = append(atts, att)
	}
	return fields, atts, nil
}

// listenControl binds the configured control address, and if that address is taken by another daemon
// AND this node never chose it deliberately, moves to a free port and records the move.
//
// The narrow case this exists for: two daemons under different homes starting in the same instant both
// allocate the same auto-assigned port, because allocation tested the port and let go of it before
// either bound. The loser used to exit with "address already in use", which for an auto-assigned port
// is a failure with no operator error behind it and no action for them to take.
//
// It is deliberately narrow. A port the operator wrote into config.json is a decision — other things
// point at it — so a collision there is reported and fatal, not silently worked around. "Auto-assigned"
// means the address is loopback and inside the allocator's own scan range; anything else is treated as
// deliberate. That test can misread a hand-written 127.0.0.1:39811 as automatic, which is the safe
// direction to be wrong in: the new address is persisted to config.json and logged before it is used,
// so the CLI follows and the operator can see what happened and pin it back.
func (d *Daemon) listenControl() (net.Listener, error) {
	addr := d.config().ControlAddr
	// The control plane is loopback-only (A2A-DESIGN §7.1). A non-loopback address is a configuration
	// error, reported before anything is bound; there is no switch that allows it.
	if err := checkLoopbackControlAddr(addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) || !autoAssignedControlAddr(addr) {
		return nil, err
	}
	moved, port, aerr := AllocControlListener()
	if aerr != nil {
		return nil, err // report the original bind failure, not the scan's
	}
	next := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	d.mu.Lock()
	d.cfg.ControlAddr = next
	cfg := d.cfg
	d.mu.Unlock()
	if serr := SaveConfig(d.layout, cfg); serr != nil {
		_ = moved.Close()
		return nil, fmt.Errorf("anet: %s was taken and the new control address could not be saved: %w", addr, serr)
	}
	log.Printf("anet: control port %s was taken by another daemon; moved to %s and updated config.json", addr, next)
	return moved, nil
}

// autoAssignedControlAddr reports whether an address looks like one this daemon allocated for itself
// rather than one an operator chose.
func autoAssignedControlAddr(addr string) bool {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		return false
	}
	p, err := strconv.Atoi(ps)
	return err == nil && p >= controlPortBase && p < controlPortBase+2000
}

// ServeControl binds the configured control address and serves the control plane until ctx is done.
func (d *Daemon) ServeControl(ctx context.Context) error {
	token, err := loadOrGenControlToken(d.layout)
	if err != nil {
		return err
	}
	ln, err := d.listenControl()
	if err != nil {
		return err
	}
	// Publish a uid-scoped pointer so a CLI whose env differs from ours (an agent tool sandbox) can find
	// this daemon without knowing ANET_DATA_DIR. Best-effort; removed on shutdown. See DaemonPointerPath.
	writeDaemonPointer(d.config().ControlAddr, d.layout.Root)
	defer os.Remove(DaemonPointerPath())
	// Register this identity in the uid-scoped registry so the console can offer an account-style
	// identity switcher across all locally-running daemons. Best-effort; removed on shutdown.
	d.writeRegistry()
	defer removeRegistryEntry(d.config().ControlAddr)
	// Whatever public faces the modules need. Fatal on failure rather
	// than logged and skipped: a node configured to sell work and
	// silently not listening would take payments at its hub that nobody
	// could ever redeem.
	if err := d.serveModuleFaces(ctx); err != nil {
		return err
	}
	srv := &http.Server{Handler: d.ControlHandler(token), ReadHeaderTimeout: 5 * time.Second}
	drained := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done(): // OS signal (Ctrl+C / SIGTERM)
		case <-d.stop: // graceful `anet stop`
		}
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		close(drained)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	<-drained
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// hubCallTimeout bounds a single Hub HTTP round-trip made on the operator's behalf.
const hubCallTimeout = 30 * time.Second

// relayError maps a Hub/relay failure to a status: a context deadline → 504, else 400.
func relayError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, context.DeadlineExceeded) {
		code = http.StatusGatewayTimeout
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// --- handlers ---

func (d *Daemon) hStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := d.config()
	out := map[string]any{
		"aid":            d.AID(),
		"version":        Version,
		"data_dir":       d.layout.Root,
		"hub_url":        cfg.HubURL,
		"name":           cfg.Name,
		"caps":           cfg.Caps,
		"summary":        cfg.Summary,
		"readme":         cfg.Readme,
		"pricing":        cfg.Pricing,
		"inbound_policy": cfg.inbound().Policy,
		"console_url":    consoleURL(cfg),
	}
	if ar := cfg.AutoReply; ar != nil {
		backend := ar.Backend
		if backend == "" {
			backend = "openai"
		}
		entry := map[string]any{"backend": backend, "untrusted": ar.UntrustedMode()}
		switch backend {
		case "exec":
			entry["agent"] = ar.Agent
			if ar.WorkDir != "" {
				entry["work_dir"] = ar.WorkDir
			}
		default:
			entry["model"] = ar.Model
			entry["api_base"] = ar.APIBase
		}
		out["auto_reply"] = entry
	}
	// Receive outcomes by reason since start (receive.go): every envelope
	// dropped or held back, and why.
	out["receive"] = d.ReceiveStats()
	writeJSON(w, http.StatusOK, out)
}

// hShutdown gracefully stops the daemon — this backs `anet stop`, so a resident node can be shut down
// without hunting for its PID / sending a signal. It replies FIRST, then signals shutdown asynchronously
// so the response is flushed before the control server drains and the process exits.
func (d *Daemon) hShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopping", "aid": d.AID()})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go d.RequestStop()
}

// consoleURL is the address of this daemon's console page. The page opens only with a single-use
// ticket in its fragment, which `anet console` obtains (session.go); opened without one, the page says
// to run that command. Empty until the control addr is known.
func consoleURL(cfg Config) string {
	_, port, err := net.SplitHostPort(cfg.ControlAddr)
	if err != nil || port == "" {
		return ""
	}
	return "http://" + consoleHost(cfg.ControlAddr) + ":" + port + "/console"
}

// hProfile sets this agent's self-authored profile (summary/readme/pricing). Fields omitted from the
// request keep their current value (partial update); provided fields overwrite. If a Hub is configured
// the new profile is published (signed). Meant to be called by the operator's agent (`anet profile set`).
func (d *Daemon) hProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Summary *string `json:"summary"`
		Readme  *string `json:"readme"`
		Pricing *string `json:"pricing"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	p := d.CurrentProfile()
	if req.Summary != nil {
		p.Summary = *req.Summary
	}
	if req.Readme != nil {
		p.Readme = *req.Readme
	}
	if req.Pricing != nil {
		p.Pricing = *req.Pricing
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	if err := d.SetProfile(ctx, p); err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "profile_set", "summary": p.Summary, "readme": p.Readme, "pricing": p.Pricing,
	})
}

// hHubRegister registers this agent with the official Hub, persists the Hub target + profile to config,
// and (re)starts the relay poll loop so delegations/results start flowing.
func (d *Daemon) hHubRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hub  string   `json:"hub"`
		Name string   `json:"name"`
		Caps []string `json:"caps"`
		// AcceptDelegations is the wire-1 switch. true is refused with the
		// same explanation as POST /accept; false asks for what the closed
		// policy already does (A2A-DESIGN §5.1).
		AcceptDelegations *bool `json:"accept_delegations"`
		// Token is an admission token for a hub that requires one. It is
		// passed through and not stored: it is spent on arrival, and a
		// spent credential kept on disk is a credential that can leak
		// long after it bought anything.
		Token string `json:"token"`
	}
	if err := readJSON(r, &req); err != nil || req.Hub == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hub URL required"})
		return
	}
	if req.AcceptDelegations != nil && *req.AcceptDelegations {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errAcceptOn.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	if err := d.HubRegister(ctx, req.Hub, req.Name, req.Caps, req.Token); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if req.AcceptDelegations != nil {
		if err := d.SetInboundPolicy(PolicyClosed); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
	}
	d.writeRegistry() // refresh the identity entry so the switcher shows the (possibly new) name
	writeJSON(w, http.StatusOK, map[string]any{
		"hub": req.Hub, "aid": d.AID(), "status": "registered",
		"inbound_policy": d.config().inbound().Policy,
	})
}

// hAccept is the wire-1 accept_delegations switch (A2A-DESIGN §5.1). "on" used to mean "anyone may
// delegate" and has no equivalent that is safe to pick for the operator, so it is refused with the
// three policies and the allow-list command; "off" sets policy closed.
func (d *Daemon) hAccept(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if req.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errAcceptOn.Error()})
		return
	}
	if err := d.SetInboundPolicy(PolicyClosed); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	d.writeRegistry()
	writeJSON(w, http.StatusOK, map[string]any{"inbound_policy": PolicyClosed, "status": "updated"})
}

// hAutoReply reconfigures the built-in auto-reply loop live (see autoreply.go): `{"off":true}` turns it
// off; otherwise the body is an AutoReplyConfig that is validated, persisted, and started immediately —
// no daemon restart, no hand-editing config.json. This is the switch behind `anet autoreply set|off`.
func (d *Daemon) hAutoReply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Off bool `json:"off"`
		AutoReplyConfig
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	var cfg *AutoReplyConfig
	if !req.Off {
		c := req.AutoReplyConfig
		cfg = &c
	}
	if err := d.SetAutoReply(cfg); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrPolicyConflict) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auto_reply": cfg, "status": "updated"})
}

// hAutoReplyTest runs the configured backend once on a synthetic prompt (no Hub, no identity) so an
// operator/agent can verify auto-reply works without registering a throwaway node. Behind `anet autoreply test`.
func (d *Daemon) hAutoReplyTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt string `json:"prompt"`
	}
	_ = readJSON(r, &req)
	reply, err := d.TestAutoReply(r.Context(), req.Prompt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reply": reply, "status": "ok"})
}

// hFind searches the Hub registry (substring over AID/name/caps).
func (d *Daemon) hFind(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
		// Capability asks the exact question instead: who serves this id.
		Capability string `json:"capability"`
	}
	_ = readJSON(r, &req)
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	var agents []hubapi.AgentView
	var err error
	if capID := strings.TrimSpace(req.Capability); capID != "" {
		agents, err = d.FindByCapability(ctx, capID)
	} else {
		agents, err = d.Find(ctx, req.Query)
	}
	if err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// hDelegate queues a task on a provider AID via the Hub relay and returns an interaction_id immediately.
func (d *Daemon) hDelegate(w http.ResponseWriter, r *http.Request) {
	// Web console: multipart upload (goal + uploaded files). The browser sends bytes, not paths.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		fields, atts, err := readMultipartAttachments(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		provider, goal := fields["provider"], strings.TrimSpace(fields["goal"])
		if provider == "" || goal == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider + goal required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), relayCallTimeout)
		defer cancel()
		id, err := d.DelegateAtts(ctx, provider, goal, atts)
		if err != nil {
			relayError(w, err)
			return
		}
		d.writeDelegated(w, provider, map[string]any{"interaction_id": id, "status": "delegated"})
		return
	}
	var req struct {
		Provider    string   `json:"provider"`
		Goal        string   `json:"goal"`
		Attachments []string `json:"attachments"` // local file paths the daemon reads (images/media/archives)
		// Capability turns this into a C1 capability call: the provider
		// resolves it against its registry and executes it deterministically
		// instead of handing it to an agent.
		Capability string         `json:"capability,omitempty"`
		Args       map[string]any `json:"args,omitempty"`
		// Pay makes this a paying delegation: a PAYMENT_REQUIRED answer is
		// paid for and the work delegated again, rather than handed back
		// to the caller as a quote to act on.
		Pay bool `json:"pay,omitempty"`
	}
	if err := readJSON(r, &req); err != nil || req.Provider == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider (AID) required"})
		return
	}
	// A capability call needs no goal: the capability id IS the request.
	if req.Capability == "" && req.Goal == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "goal or capability required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), relayCallTimeout)
	defer cancel()
	if req.Capability != "" {
		// This path existed on the Daemon and was reachable from nothing.
		// A joint run against a real hub, a real ANetLink and real mock
		// hardware is what surfaced it: the delegation arrived, the
		// provider could have executed it, and the capability id was
		// sitting in the goal text where no resolver looks. Both repos'
		// suites passed throughout, because each fakes the other and the
		// capability round-trip test calls DelegateCapability in-process.
		if req.Pay {
			id, quote, err := d.DelegateAndPay(ctx, req.Provider, req.Capability, req.Args)
			if err != nil {
				relayError(w, err)
				return
			}
			out := map[string]any{"interaction_id": id, "status": "queued", "capability": req.Capability}
			if quote != nil {
				out["paid"] = quote
			}
			d.writeDelegated(w, req.Provider, out)
			return
		}
		id, err := d.DelegateCapability(ctx, req.Provider, req.Capability, req.Args)
		if err != nil {
			relayError(w, err)
			return
		}
		d.writeDelegated(w, req.Provider, map[string]any{
			"interaction_id": id, "status": "queued", "capability": req.Capability})
		return
	}
	id, err := d.Delegate(ctx, req.Provider, req.Goal, req.Attachments)
	if err != nil {
		relayError(w, err)
		return
	}
	d.writeDelegated(w, req.Provider, map[string]any{"interaction_id": id, "status": "queued"})
}

// writeDelegated answers a successful delegation, carrying through what the hub said about the
// recipient when it said the recipient has stopped collecting its mail.
//
// The hub returns recipient_quiet + warning on /relay/send; the daemon used to consume them for its own
// log and answer the CLI with the interaction id alone, so the person who typed `anet delegate` was the
// one party who did not learn that the provider had been silent for days. The task is still queued —
// quiet is not dead, and one poll by the provider collects everything waiting — so this adds a field,
// never a refusal.
func (d *Daemon) writeDelegated(w http.ResponseWriter, providerAID string, out map[string]any) {
	if warn := d.QuietPeer(providerAID); warn != "" {
		out["recipient_quiet"] = true
		out["warning"] = warn
	}
	writeJSON(w, http.StatusOK, out)
}

// hInbox lists inbound (delegated-to-us) tasks; pending=true shows only the still-queued backlog. It
// best-effort pulls the relay first so a task delegated moments ago is visible immediately (matching
// /threads and /results), rather than waiting for the next background poll tick.
func (d *Daemon) hInbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pending bool `json:"pending"`
	}
	_ = readJSON(r, &req)
	d.pollFresh(r.Context())
	items, err := d.Inbox(req.Pending)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"inbox": items})
}

// hMessage appends a chat message to an active interaction (either side) and relays it to the peer. This
// is the multi-turn conversation primitive: a provider "delivering" is just sending message(s); wrapping
// up is the end negotiation (/end + /end-accept). anet runs no model — these bytes come from the operator
// or their external agent.
func (d *Daemon) hMessage(w http.ResponseWriter, r *http.Request) {
	// Web console: multipart upload (body + uploaded files). The browser sends bytes, not paths.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		fields, atts, err := readMultipartAttachments(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		ixID := strings.TrimSpace(fields["interaction_id"])
		if ixID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction_id required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), relayCallTimeout)
		defer cancel()
		if err := d.SendMessageAtts(ctx, ixID, fields["body"], atts); err != nil {
			relayError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"interaction_id": ixID, "status": "sent"})
		return
	}
	var req struct {
		InteractionID string   `json:"interaction_id"`
		Body          string   `json:"body"`
		Attachments   []string `json:"attachments"` // local file paths the daemon reads (images/media/archives)
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.InteractionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction_id + body required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), relayCallTimeout)
	defer cancel()
	if err := d.SendMessage(ctx, req.InteractionID, req.Body, req.Attachments); err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction_id": req.InteractionID, "status": "sent"})
}

// hPull writes an interaction's received attachments to a local directory (default cwd) and returns the
// files written. This is how a receiving agent lands delivered images/archives on disk.
func (d *Daemon) hPull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InteractionID string `json:"interaction_id"`
		OutDir        string `json:"out_dir"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.InteractionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction_id required"})
		return
	}
	files, err := d.Pull(req.InteractionID, req.OutDir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction_id": req.InteractionID, "files": files, "count": len(files)})
}

// attachmentHandler streams one stored attachment's bytes for the local web console to render (inline
// <img>) or download (A2A-DESIGN §7.6). It sits behind the authentication gate: the console's <img>/<a>
// loads reach it with the session cookie (a session route without CSRF, see ctlsec.go), the CLI with the
// bearer token.
//
// The peer chooses both the bytes and the declared type, so neither decides how the browser treats the
// response. The type is sniffed from the bytes (sniffMime), and only the four raster image types in
// inlineImageTypes are served inline; everything else, SVG and HTML included, is served as
// application/octet-stream with Content-Disposition: attachment. Every response also carries
// "Content-Security-Policy: default-src 'none'; sandbox" and nosniff, so even a response a browser does
// render gets an opaque origin without script. Before this, an attachment declared image/svg+xml or
// text/html rendered inline on the control-plane origin, where its script could call the control API.
// Responses are not cached: the previous one-year immutable caching would have kept serving any
// response cached under the old rules.
func (d *Daemon) attachmentHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		ixID := r.URL.Query().Get("interaction_id")
		cid := r.URL.Query().Get("cid")
		if ixID == "" || cid == "" {
			http.Error(w, "interaction_id + cid required", http.StatusBadRequest)
			return
		}
		name, _, data, err := d.AttachmentBytes(ixID, cid)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeAttachment(w, name, data)
	}
}

// writeAttachment sends attachment bytes with the type decided by sniffing (see attachmentHandler).
func writeAttachment(w http.ResponseWriter, name string, data []byte) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if sniffed := sniffMime(data); inlineImageTypes[sniffed] {
		h.Set("Content-Type", sniffed)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		cd := mime.FormatMediaType("attachment", map[string]string{"filename": safeName(name)})
		if cd == "" {
			cd = "attachment"
		}
		h.Set("Content-Disposition", cd)
	}
	_, _ = w.Write(data)
}

// hEnd ends a task from this side: the provider completes it (signs the receipt and delivers the
// result); the requester asks the provider to complete it (A2A-DESIGN §4.2).
func (d *Daemon) hEnd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InteractionID string `json:"interaction_id"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.InteractionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction_id required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	if err := d.RequestEnd(ctx, req.InteractionID); err != nil {
		relayError(w, err)
		return
	}
	status := "end_requested"
	if ix, err := d.ix.Get(req.InteractionID); err == nil && ix.Role == interactions.RoleInbound {
		status = "completed"
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction_id": req.InteractionID, "status": status})
}

// hEndAccept answered the second step of the wire-1 end negotiation. From wire 2 the provider
// completes on its own and a requester's `end` is enough (A2A-DESIGN §4.2), so there is nothing to
// accept; the route answers 410 with that explanation.
func (d *Daemon) hEndAccept(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusGone, map[string]string{"error": "end-accept was removed: the provider " +
		"completes a task itself, and the requester's `anet end` asks it to; use `anet end <interaction_id>`"})
}

// hThreads powers the console's chat view: it best-effort pulls any pending relay messages (so newly
// arrived inbound tasks / outbound results show up), then returns ALL interactions in both roles.
func (d *Daemon) hThreads(w http.ResponseWriter, r *http.Request) {
	d.pollFresh(r.Context()) // best-effort freshness; large inbound transfers are left to the background loop
	ts, err := d.Threads()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cfg := d.config()
	writeJSON(w, http.StatusOK, map[string]any{"threads": ts, "aid": d.AID(), "name": cfg.Name, "hub": cfg.HubURL})
}

// hThread returns ONE interaction's full conversation (the multi-turn message log + end-negotiation
// state), best-effort pulling the relay first so a CLI-driven agent can READ the ongoing conversation
// (e.g. a follow-up the requester just sent) before deciding what to reply / whether to end.
func (d *Daemon) hThread(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InteractionID string `json:"interaction_id"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.InteractionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction_id required"})
		return
	}
	d.pollFresh(r.Context()) // best-effort freshness; large inbound transfers are left to the background loop
	t, err := d.Thread(req.InteractionID)
	if errors.Is(err, interactions.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such interaction: " + req.InteractionID})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": t})
}

// hIdentities lists all locally-running anet identities (daemons) so the console can offer an
// account-style switcher. Each entry carries the control address its console lives at; "self" marks the
// identity serving this console. Reading the registry is same-uid + loopback, so no extra auth beyond the
// bearer wrapper this handler already sits behind.
func (d *Daemon) hIdentities(w http.ResponseWriter, _ *http.Request) {
	// Only offer daemons that are actually reachable right now — the registry can hold stale entries for
	// daemons that crashed without cleaning up, and navigating to a dead port just fails to load.
	list := RunningDaemons()
	self := d.AID()
	type ident struct {
		AID         string `json:"aid"`
		Name        string `json:"name"`
		ControlAddr string `json:"control_addr"`
		Self        bool   `json:"self"`
	}
	// displayName prefers the Hub profile name; if that's blank (common for the "default" identity, which
	// never had a name set), fall back to the local identity name derived from its data dir ("default" for
	// the home root, the <home>/ids/<name> folder name otherwise) so the switcher shows "default" not a raw AID.
	displayName := func(name, dataDir string) string {
		if strings.TrimSpace(name) != "" {
			return name
		}
		return IdentityNameForDir(dataDir)
	}
	out := make([]ident, 0, len(list)+1)
	seen := false
	for _, e := range list {
		out = append(out, ident{AID: e.AID, Name: displayName(e.Name, e.DataDir), ControlAddr: e.ControlAddr, Self: e.AID == self})
		if e.AID == self {
			seen = true
		}
	}
	if !seen { // registry write may lag a fresh start; always include ourselves
		cfg := d.config()
		out = append(out, ident{AID: self, Name: displayName(cfg.Name, d.layout.Root), ControlAddr: cfg.ControlAddr, Self: true})
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": out, "self": self})
}

// hResults polls the Hub relay for any pending deliverables, then lists completed outbound delegations.
func (d *Daemon) hResults(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	res, err := d.Results(ctx)
	if err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

// hReview signs a review of a completed delegation (anchored to the provider's receipt), stores it, and
// uploads the receipt + review + verified content to the configured Hub.
func (d *Daemon) hReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InteractionID string `json:"interaction_id"`
		Rating        int    `json:"rating"`
		Comment       string `json:"comment"`
	}
	if err := readJSON(r, &req); err != nil || req.InteractionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction_id + rating required"})
		return
	}
	res, err := d.SubmitReview(req.InteractionID, req.Rating, req.Comment)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out := map[string]any{"interaction_id": res.InteractionID, "subject": res.Subject, "rating": res.Rating}
	if hub := d.config().HubURL; hub != "" {
		ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
		defer cancel()
		if err := d.UploadReview(ctx, hub, req.InteractionID); err != nil {
			out["hub_error"] = err.Error()
		} else {
			out["hub"] = hub
			out["uploaded"] = true
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// hEvidence serves this node's own evidence chain.
//
// The chain was write-only for its whole existence: every capability
// effect, receipt and accepted result went onto it and nothing could read
// it back. That is a strange shape for an audit substrate — the operator
// accumulating the evidence was the one person who could not look at it,
// and "verify before use" was enforced against a file nobody ever saw.
func (d *Daemon) hEvidence(w http.ResponseWriter, r *http.Request) {
	var q EvidenceQuery
	if err := readJSON(r, &q); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if d.ledger == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"head": EvidenceHead{State: "ACTIVE"}, "records": []EvidenceRecord{}})
		return
	}
	head, recs := d.ledger.Evidence(q)
	if recs == nil {
		recs = []EvidenceRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"head": head, "records": recs})
}

// hHubLeave stops this node being deliverable at a hub it has left.
func (d *Daemon) hHubLeave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hub string `json:"hub"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Hub) == "" {
		req.Hub = d.config().HubURL
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	out, err := d.LeaveHub(ctx, req.Hub)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "result": out})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// hP2PAdvertise publishes this node's direct address on its hub.
func (d *Daemon) hP2PAdvertise(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addr string `json:"addr"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	out, err := d.AdvertisePeerAddress(ctx, req.Addr)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "result": out})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// hX402Authorize signs a payment for an x402 resource server.
//
// Exists because buying through a gateway is something a person does, not
// only something a test does. The gateway wants a PAYMENT-SIGNATURE
// header; producing one needs this node's key, which never leaves the
// daemon — so the daemon signs and hands back the header value.
//
// It does NOT settle anything. The signature authorises a payment the
// gateway may then present to its facilitator; until it does, nothing has
// moved. Handing out an authorization is closer to writing a cheque than
// to spending, and the window and nonce are what keep it that way.
func (d *Daemon) hX402Authorize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayTo         string `json:"pay_to"`
		Amount        uint64 `json:"amount"`
		Network       string `json:"network"`
		InteractionID string `json:"interaction_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.PayTo == "" || req.Amount == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "pay_to and amount are required"})
		return
	}
	p := d.payer()
	if p == nil {
		relayError(w, errNoPayments())
		return
	}
	network := strings.TrimSpace(req.Network)
	if network == "" {
		// Default to this node's own hub, which is the only ledger it can
		// actually draw on. Naming another one is allowed — a buyer may
		// hold credit somewhere else — but it has to be deliberate.
		network = payment.CreditNetwork(d.hubAID())
	}
	raw, err := p.Authorize(payment.PaymentOption{
		Scheme: payment.SchemeCredit, Network: network,
		Amount: payment.Amount(req.Amount), Asset: payment.AssetCredit, PayTo: req.PayTo,
	}, req.InteractionID)
	if err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"header":  payment.HeaderPaymentSignature,
		"value":   base64.StdEncoding.EncodeToString(raw),
		"pay_to":  req.PayTo,
		"amount":  req.Amount,
		"network": network,
	})
}

// hReconcile compares this node's payment history against the hub's
// ledger for this account.
func (d *Daemon) hReconcile(w http.ResponseWriter, r *http.Request) {
	p := d.payer()
	if p == nil {
		relayError(w, errNoPayments())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	out, err := p.Reconcile(ctx)
	if err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// hAuditHub verifies the hub's issuance chain against heads this node
// recorded earlier.
func (d *Daemon) hAuditHub(w http.ResponseWriter, r *http.Request) {
	p := d.payer()
	if p == nil {
		relayError(w, errNoPayments())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	out, err := p.AuditIssuance(ctx)
	if err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// hBalance reads this node's credit standing off its hub.
func (d *Daemon) hBalance(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	out, err := d.Balance(ctx)
	if err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// hRedeemCredit takes credit back out of the hub's ledger.
func (d *Daemon) hRedeemCredit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Amount    uint64 `json:"amount"`
		Reference string `json:"reference"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	out, err := d.RedeemCredit(ctx, req.Amount, req.Reference)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "result": out})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// hVisibility sets how far this node is published.
func (d *Daemon) hVisibility(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Visibility string `json:"visibility"`
	}
	if err := readJSON(r, &req); err != nil || req.Visibility == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "visibility required: hub-local, federated or public"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	if err := d.SetVisibility(ctx, req.Visibility); err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"visibility": req.Visibility, "status": "set"})
}

package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/protocol/delegation"
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
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
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
	pb, perr := os.ReadFile(DaemonPointerPath())
	if perr != nil {
		return "", "", fmt.Errorf("read control token (is the daemon running?): %w", perr)
	}
	var dp daemonPointer
	if json.Unmarshal(pb, &dp) != nil || dp.ControlAddr == "" || dp.DataDir == "" {
		return "", "", fmt.Errorf("read control token (is the daemon running?): bad daemon pointer")
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

// ControlHandler returns the daemon's control-plane HTTP handler (token-guarded). Exposed so tests can
// drive it via httptest without binding a port. v0.1 is centralized, so the surface is small: identity
// status, Hub registration + discovery, and the delegation lifecycle over the Hub relay.
func (d *Daemon) ControlHandler(token string) http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /status", d.hStatus)
	api.HandleFunc("POST /status", d.hStatus)
	api.HandleFunc("POST /hub-register", d.hHubRegister)
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
	// The local web console is served OUTSIDE the bearer wrapper (a browser navigation cannot send an
	// Authorization header); loopback-only makes this safe. The page then calls the token-guarded API
	// above with the injected token. Everything else stays bearer-gated.
	top := http.NewServeMux()
	top.HandleFunc("GET /console", d.consoleHandler(token))
	top.HandleFunc("GET /ping", d.pingHandler())
	top.HandleFunc("GET /attachment", d.attachmentHandler())
	// A browser landing on the bare root (typed URL, bookmark, or an identity-switcher hop from an older
	// tab) would otherwise hit the bearer-gated API and get a bare "unauthorized". Redirect GET / to the
	// console so any human navigation resolves to the web UI. ({$} matches ONLY "/", so the API keeps the
	// "/" catch-all for its POST endpoints.)
	top.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console", http.StatusFound)
	})
	top.Handle("/", bearer(token, api))
	return top
}

// maxControlBody caps a control-plane request body (defense-in-depth; the control plane is local +
// token-gated, so this only bounds a self-inflicted oversized request).
const maxControlBody = 1 << 20

// maxUploadBody caps a multipart upload (console attach). One attachment is bounded to maxAttachmentBytes
// (64 MiB); this leaves headroom for a couple of files plus multipart framing in a single request.
const maxUploadBody = 130 << 20 // 130 MiB

// readMultipartAttachments streams a multipart/form-data control request, collecting non-file fields into
// a map and each file part into a self-verified attachment. It reads parts incrementally (no on-disk temp
// files) and bounds every file to maxAttachmentBytes; the bearer wrapper already caps the total body.
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

// ServeControl binds the configured control address and serves the control plane until ctx is done.
func (d *Daemon) ServeControl(ctx context.Context) error {
	token, err := loadOrGenControlToken(d.layout)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", d.config().ControlAddr)
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

// bearer wraps h with a constant-time bearer-token check.
func bearer(token string, h http.Handler) http.Handler {
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Web-console uploads (delegate/message with files) arrive as multipart and carry real bytes, so
		// they need a much larger ceiling than the tiny JSON control calls; everything else stays at 1 MiB.
		cap := int64(maxControlBody)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			cap = maxUploadBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, cap)
		h.ServeHTTP(w, r)
	})
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
		"aid":                d.AID(),
		"version":            Version,
		"data_dir":           d.layout.Root,
		"hub_url":            cfg.HubURL,
		"name":               cfg.Name,
		"caps":               cfg.Caps,
		"summary":            cfg.Summary,
		"readme":             cfg.Readme,
		"pricing":            cfg.Pricing,
		"accept_delegations": cfg.AcceptsDelegations(),
		"guest_messages":     cfg.GuestQuota(),
		"console_url":        consoleURL(cfg),
	}
	if ar := cfg.AutoReply; ar != nil {
		backend := ar.Backend
		if backend == "" {
			backend = "openai"
		}
		entry := map[string]any{"backend": backend}
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

// consoleURL builds the loopback URL a human opens to drive THIS daemon in the browser (same web UI as
// the Hub, but bound to this identity). Host is forced to 127.0.0.1 (the control port may bind 0.0.0.0),
// and the configured Hub is passed through so the console connects to the right Hub. Empty until the
// control addr is known. This is the URL an onboarding agent hands back to its operator.
func consoleURL(cfg Config) string {
	_, port, err := net.SplitHostPort(cfg.ControlAddr)
	if err != nil || port == "" {
		return ""
	}
	u := "http://127.0.0.1:" + port + "/console"
	if cfg.HubURL != "" {
		u += "?hub=" + cfg.HubURL
	}
	return u
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
		Hub               string   `json:"hub"`
		Name              string   `json:"name"`
		Caps              []string `json:"caps"`
		GuestMessages     *int     `json:"guest_messages"`
		AcceptDelegations *bool    `json:"accept_delegations"`
	}
	if err := readJSON(r, &req); err != nil || req.Hub == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hub URL required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	if err := d.HubRegister(ctx, req.Hub, req.Name, req.Caps, req.GuestMessages, req.AcceptDelegations); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	d.writeRegistry() // refresh the identity entry so the switcher shows the (possibly new) name
	writeJSON(w, http.StatusOK, map[string]any{
		"hub": req.Hub, "aid": d.AID(), "status": "registered",
		"accept_delegations": d.config().AcceptsDelegations(),
	})
}

// hAccept toggles whether this daemon stores inbound delegated tasks (persisted; effective immediately).
// This is the CLI-accessible switch for accept_delegations — no need to hand-edit config.json.
func (d *Daemon) hAccept(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	v, err := d.SetAcceptDelegations(req.Enabled)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	d.writeRegistry()
	writeJSON(w, http.StatusOK, map[string]any{"accept_delegations": v, "status": "updated"})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
	}
	_ = readJSON(r, &req)
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	agents, err := d.Find(ctx, req.Query)
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
		writeJSON(w, http.StatusOK, map[string]any{"interaction_id": id, "status": "delegated"})
		return
	}
	var req struct {
		Provider    string   `json:"provider"`
		Goal        string   `json:"goal"`
		Attachments []string `json:"attachments"` // local file paths the daemon reads (images/media/archives)
	}
	if err := readJSON(r, &req); err != nil || req.Provider == "" || req.Goal == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider (AID) + goal required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), relayCallTimeout)
	defer cancel()
	id, err := d.Delegate(ctx, req.Provider, req.Goal, req.Attachments)
	if err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction_id": id, "status": "queued"})
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
// <img>) or download. Served OUTSIDE the bearer wrapper (like /console) because a browser <img>/<a> load
// cannot send an Authorization header; loopback-only makes this safe.
func (d *Daemon) attachmentHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ixID := r.URL.Query().Get("interaction_id")
		cid := r.URL.Query().Get("cid")
		if ixID == "" || cid == "" {
			http.Error(w, "interaction_id + cid required", http.StatusBadRequest)
			return
		}
		name, mimeType, data, err := d.AttachmentBytes(ixID, cid)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mimeType)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		// Non-inline-renderable types download with their original name.
		if !strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "text/") {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeName(name)))
		}
		_, _ = w.Write(data)
	}
}

// hEnd proposes ending a task (or accepts the peer's proposal if they already made one).
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
	writeJSON(w, http.StatusOK, map[string]any{"interaction_id": req.InteractionID, "status": "end_proposed"})
}

// hEndAccept accepts the peer's end proposal; mutual agreement makes the provider issue the signed receipt.
func (d *Daemon) hEndAccept(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InteractionID string `json:"interaction_id"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.InteractionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction_id required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	if err := d.AcceptEnd(ctx, req.InteractionID); err != nil {
		relayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction_id": req.InteractionID, "status": "end_accepted"})
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
	ts, err := d.Threads()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	for _, t := range ts {
		if t.InteractionID == req.InteractionID {
			writeJSON(w, http.StatusOK, map[string]any{"thread": t})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such interaction: " + req.InteractionID})
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

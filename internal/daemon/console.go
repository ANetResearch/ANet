package daemon

// console.go serves the LOCAL web console page and the liveness probe.
//
// The page carries no credential (A2A-DESIGN §7.5, SI-7). It is injected with window.__ANET = {aid,
// name, hub, nonce}; the page then exchanges the ticket from its URL fragment for a session (session.go)
// and calls the control API with the session cookie and the X-Anet-CSRF header. Earlier versions
// injected the control bearer token into this page, and /console was reachable without authentication,
// so anything able to read the page (a DNS-rebinding site, or a LAN peer when the control address was
// not loopback) obtained full control of the daemon, including local code execution through /autoreply
// (found in the 2026-09 A2A security survey of the control plane).
//
// The page reads public registry data (/graph, /stats, /agents/{aid}) cross-origin from the configured
// hub, whose CORS is open; the Content-Security-Policy allows exactly that origin in connect-src.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// hubOriginRe bounds what may appear as a hub origin inside the CSP header: scheme, host name or IP
// literal, optional port. Anything else is left out of the policy rather than escaped into it.
var hubOriginRe = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:[0-9]{1,5})?$|^https?://\[[0-9A-Fa-f:.]+\](:[0-9]{1,5})?$`)

// hubOrigin returns the scheme://host[:port] origin of a hub URL, or "" when it cannot be expressed
// safely in a CSP source list.
func hubOrigin(hubURL string) string {
	u, err := url.Parse(strings.TrimSpace(hubURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	o := u.Scheme + "://" + u.Host
	if !hubOriginRe.MatchString(o) {
		return ""
	}
	return o
}

// consoleCSP is the page's policy. Scripts run only with the per-response nonce; inline style
// attributes stay allowed because the page builds its markup with them and a style cannot execute code.
// connect-src admits this origin and the configured hub.
func consoleCSP(nonce, hub string) string {
	connect := "'self'"
	if hub != "" {
		connect += " " + hub
	}
	return "default-src 'none'; script-src 'nonce-" + nonce + "'; style-src 'unsafe-inline'; img-src 'self'; " +
		"connect-src " + connect + "; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}

// consoleHandler serves the embedded console page with a token-free bootstrap and a nonce-based CSP.
func (d *Daemon) consoleHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := ConsoleHTML()
		if err != nil {
			http.Error(w, "console unavailable", http.StatusInternalServerError)
			return
		}
		nonce, err := randomToken(18)
		if err != nil {
			http.Error(w, "console nonce", http.StatusInternalServerError)
			return
		}
		cfg := d.config()
		// json.Marshal escapes <, > and & inside strings, so a name containing "</script>" cannot end the
		// script element early.
		boot, err := json.Marshal(map[string]any{
			"aid":   d.AID(),
			"name":  cfg.Name,
			"hub":   cfg.HubURL,
			"nonce": nonce,
		})
		if err != nil {
			http.Error(w, "console bootstrap", http.StatusInternalServerError)
			return
		}
		open := `<script nonce="` + nonce + `">`
		out := bytes.ReplaceAll(page, []byte("<script>"), []byte(open))
		inject := open + "window.__ANET = " + string(boot) + ";</script>\n</head>"
		out = bytes.Replace(out, []byte("</head>"), []byte(inject), 1)
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", consoleCSP(nonce, hubOrigin(cfg.HubURL)))
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		_, _ = w.Write(out)
	}
}

// pingHandler answers an unauthenticated loopback liveness probe with the public AID and the build. It
// sends no Access-Control-Allow-Origin header: its consumers are curl in scripts and RunningDaemons,
// none of which needs CORS, and without the header a page on another site cannot read which identity
// and build answer on a local port.
func (d *Daemon) pingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The build is reported here as well as the identity. A check
		// asking "is the daemon I deployed the one answering" needs an
		// unauthenticated endpoint to ask, and this is the one everything
		// already uses to find out whether a node is up.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"anet": true, "aid": d.AID(), "name": d.config().Name,
			"version": Version, "commit": BuildCommit, "built_at": BuildAt, "tags": BuildTags,
		})
	}
}

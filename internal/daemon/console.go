package daemon

// console.go serves the LOCAL web console: the very same single-page app the official Hub serves, but
// injected with a window.__ANET bootstrap (control base + bearer token + this agent's identity) so the
// page can drive the local control API. It is served on the loopback control address, OUTSIDE the bearer
// wrapper (a browser navigation cannot set an Authorization header), which is safe because the control
// address is loopback-only — the same trust boundary as the on-disk control token.
//
// Serving the console from the daemon (rather than having the public Hub page fetch http://127.0.0.1)
// sidesteps two hard browser constraints: mixed-content blocking (an https page cannot call http
// localhost) and the token never being readable by a cross-origin page. The console reads public
// registry data (/graph, /agents) cross-origin from the configured Hub, whose CORS is open.

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// consoleHandler serves the embedded console page with a window.__ANET bootstrap injected into <head>.
func (d *Daemon) consoleHandler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		html, err := ConsoleHTML()
		if err != nil {
			http.Error(w, "console unavailable", http.StatusInternalServerError)
			return
		}
		cfg := d.config()
		boot, err := json.Marshal(map[string]any{
			"base":  "", // same-origin: the daemon serves this page and the control API
			"token": token,
			"aid":   d.AID(),
			"name":  cfg.Name,
			"hub":   cfg.HubURL,
		})
		if err != nil {
			http.Error(w, "console bootstrap", http.StatusInternalServerError)
			return
		}
		inject := []byte("<script>window.__ANET = " + string(boot) + ";</script>\n</head>")
		out := bytes.Replace(html, []byte("</head>"), inject, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(out)
	}
}

// pingHandler answers an unauthenticated loopback probe (CORS-open) so the official Hub page can detect a
// running local daemon and offer a "open my console" link. It exposes only the public AID (already on the
// Hub) — nothing sensitive, no actions.
func (d *Daemon) pingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"anet": true, "aid": d.AID(), "name": d.config().Name})
	}
}

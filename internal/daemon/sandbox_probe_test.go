package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sandboxProbeMarker introduces the probe request inside the prompt the sandbox integration test sends.
// The request is JSON terminated by sandboxProbeEnd.
const (
	sandboxProbeMarker = "ANET-SANDBOX-PROBE:"
	sandboxProbeEnd    = ":END-PROBE"
)

// sandboxProbeRequest names the host paths the probe tries to reach from inside the sandbox.
type sandboxProbeRequest struct {
	ControlToken string `json:"control_token"`
	A2AToken     string `json:"a2a_token"`
	Socket       string `json:"socket"`
	DBus         string `json:"dbus"`
	DataDir      string `json:"data_dir"`
	// HostTmpMarker is a file in the host's temp dir, which the sandbox's /tmp must not show.
	HostTmpMarker string `json:"host_tmp_marker"`
	// Egress asks for the egress checks instead: a model call on the injection route, the model
	// service's own address (HostURL) directly, a CONNECT, and the environment's key (OperatorKey is the
	// operator's, which must not be in it).
	Egress      bool   `json:"egress,omitempty"`
	HostURL     string `json:"host_url,omitempty"`
	OperatorKey string `json:"operator_key,omitempty"`
}

// sandboxProbeResult is what the probe could do. Every field except WorkDirWritable must come back
// false (or zero) for the sandbox to hold.
type sandboxProbeResult struct {
	ControlTokenRead bool   `json:"control_token_read"`
	A2ATokenRead     bool   `json:"a2a_token_read"`
	SocketConnected  bool   `json:"socket_connected"`
	DBusConnected    bool   `json:"dbus_connected"`
	DataDirVisible   bool   `json:"data_dir_visible"`
	HostTmpVisible   bool   `json:"host_tmp_visible"`
	HomeEntries      int    `json:"home_entries"`
	WorkDirWritable  bool   `json:"workdir_writable"`
	Cwd              string `json:"cwd"`
	Error            string `json:"error,omitempty"`

	KeyIsPlaceholder  bool   `json:"key_is_placeholder,omitempty"`
	EnvHasOperatorKey bool   `json:"env_has_operator_key,omitempty"`
	RouteStatus       int    `json:"route_status,omitempty"`
	RouteBody         string `json:"route_body,omitempty"`
	HostReached       bool   `json:"host_reached,omitempty"`
	ConnectStatus     int    `json:"connect_status,omitempty"`
}

// runEgressProbe is the egress part of the probe, inside the sandbox.
func runEgressProbe(req sandboxProbeRequest, res *sandboxProbeResult) {
	res.KeyIsPlaceholder = os.Getenv("ANTHROPIC_API_KEY") == egressPlaceholderKey
	res.EnvHasOperatorKey = req.OperatorKey != "" && strings.Contains(strings.Join(os.Environ(), "\n"), req.OperatorKey)
	noProxy := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
	hreq, err := http.NewRequest("POST", os.Getenv("ANTHROPIC_BASE_URL")+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		res.Error = err.Error()
		return
	}
	hreq.Header.Set("x-api-key", os.Getenv("ANTHROPIC_API_KEY"))
	hreq.Header.Set("anthropic-version", "2023-06-01")
	if resp, err := noProxy.Do(hreq); err != nil {
		res.RouteBody = err.Error()
	} else {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		res.RouteStatus, res.RouteBody = resp.StatusCode, string(b)
	}
	direct := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	if resp, err := direct.Get(req.HostURL); err == nil {
		resp.Body.Close()
		res.HostReached = true
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:3128", 5*time.Second); err == nil {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = io.WriteString(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
		if resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect}); err == nil {
			res.ConnectStatus = resp.StatusCode
		}
		c.Close()
	}
}

// runSandboxProbe runs inside the sandbox (the test binary started as the agent) and prints the result
// as one JSON line, which becomes the agent's reply.
func runSandboxProbe(arg string) int {
	var res sandboxProbeResult
	raw := arg
	if i := strings.Index(raw, sandboxProbeEnd); i >= 0 {
		raw = raw[:i]
	}
	var req sandboxProbeRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		res.Error = "bad probe request: " + err.Error()
	} else if req.Egress {
		runEgressProbe(req, &res)
	} else {
		if _, err := os.ReadFile(req.ControlToken); err == nil {
			res.ControlTokenRead = true
		}
		if _, err := os.ReadFile(req.A2AToken); err == nil {
			res.A2ATokenRead = true
		}
		if c, err := net.DialTimeout("unix", req.Socket, 2*time.Second); err == nil {
			res.SocketConnected = true
			_ = c.Close()
		}
		if req.DBus != "" {
			if c, err := net.DialTimeout("unix", req.DBus, 2*time.Second); err == nil {
				res.DBusConnected = true
				_ = c.Close()
			}
		}
		if _, err := os.Stat(req.DataDir); err == nil {
			res.DataDirVisible = true
		}
		if req.HostTmpMarker != "" {
			if _, err := os.Stat(req.HostTmpMarker); err == nil {
				res.HostTmpVisible = true
			}
		}
		if home, err := os.UserHomeDir(); err == nil {
			if ents, err := os.ReadDir(home); err == nil {
				res.HomeEntries = len(ents)
			}
		}
		res.Cwd, _ = os.Getwd()
		if err := os.WriteFile(filepath.Join(res.Cwd, "probe-wrote-this"), []byte("x"), 0o600); err == nil {
			res.WorkDirWritable = true
		}
	}
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	return 0
}

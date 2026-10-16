package autoreply

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
	// ConnectURL is an https address of an allowed host, fetched through the proxy variables the
	// sandbox gives, once as it is and once with another TLS name (runTunnelProbe).
	ConnectURL string `json:"connect_url,omitempty"`
}

// sandboxProbeResult is what the probe could do. Every field except WorkDirWritable must come back
// false (or zero) for the sandbox to hold.
type sandboxProbeResult struct {
	ControlTokenRead bool `json:"control_token_read"`
	A2ATokenRead     bool `json:"a2a_token_read"`
	SocketConnected  bool `json:"socket_connected"`
	DBusConnected    bool `json:"dbus_connected"`
	DataDirVisible   bool `json:"data_dir_visible"`
	HostTmpVisible   bool `json:"host_tmp_visible"`
	// DataDirListed and HostTmpRead are the same two with their contents: on macOS a path's metadata
	// stays visible (LEC MS-2), what is in it must not.
	DataDirListed   bool   `json:"data_dir_listed"`
	HostTmpRead     bool   `json:"host_tmp_read"`
	HomeEntries     int    `json:"home_entries"`
	WorkDirWritable bool   `json:"workdir_writable"`
	Cwd             string `json:"cwd"`
	Error           string `json:"error,omitempty"`

	// What the agent was started with (LEC SI-56, AV-1): its environment, its command line as the
	// kernel shows it, and how many bytes it read on stdin (fd 4).
	Environ    []string `json:"environ,omitempty"`
	Cmdline    string   `json:"cmdline,omitempty"`
	Argv       []string `json:"argv,omitempty"`
	StdinBytes int      `json:"stdin_bytes,omitempty"`

	KeyIsPlaceholder  bool   `json:"key_is_placeholder,omitempty"`
	EnvHasOperatorKey bool   `json:"env_has_operator_key,omitempty"`
	RouteStatus       int    `json:"route_status,omitempty"`
	RouteBody         string `json:"route_body,omitempty"`
	HostReached       bool   `json:"host_reached,omitempty"`
	ConnectStatus     int    `json:"connect_status,omitempty"`
	// ConnectNoKeyStatus is a CONNECT without the proxy variable's credentials, tried only when they hold
	// a key: 407 on a keyed endpoint.
	ConnectNoKeyStatus int    `json:"connect_no_key_status,omitempty"`
	TunnelStatus       int    `json:"tunnel_status,omitempty"`
	TunnelBody         string `json:"tunnel_body,omitempty"`
	WrongNameFailed    bool   `json:"wrong_name_failed,omitempty"`
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
	// CONNECT to the endpoint the proxy variables name, with their credentials (a keyed endpoint's
	// run key) and without.
	proxyAddr, proxyAuth := "127.0.0.1:3128", ""
	if u, err := url.Parse(os.Getenv("HTTPS_PROXY")); err == nil && u.Host != "" {
		proxyAddr = u.Host
		if u.User != nil {
			pw, _ := u.User.Password()
			proxyAuth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw)) + "\r\n"
		}
	}
	connect := func(auth string) int {
		c, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
		if err != nil {
			return 0
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = io.WriteString(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n"+auth+"\r\n")
		if resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect}); err == nil {
			return resp.StatusCode
		}
		return 0
	}
	res.ConnectStatus = connect(proxyAuth)
	if proxyAuth != "" { // a keyed endpoint (macOS): the same CONNECT without the key
		res.ConnectNoKeyStatus = connect("")
	}
	if req.ConnectURL != "" {
		runTunnelProbe(req.ConnectURL, res)
	}
}

// runTunnelProbe fetches url as an HTTPS client in the sandbox does, through the proxy its environment
// names, and then again with a TLS name other than the URL's host.
func runTunnelProbe(url string, res *sandboxProbeResult) {
	client := func(serverName string) *http.Client {
		return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: serverName}}}
	}
	if resp, err := client("").Get(url); err != nil {
		res.TunnelBody = err.Error()
	} else {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		res.TunnelStatus, res.TunnelBody = resp.StatusCode, string(b)
	}
	if resp, err := client("other.example").Get(url); err != nil {
		res.WrongNameFailed = true
	} else {
		resp.Body.Close()
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
		if _, err := os.ReadDir(req.DataDir); err == nil {
			res.DataDirListed = true
		}
		if req.HostTmpMarker != "" {
			if _, err := os.Stat(req.HostTmpMarker); err == nil {
				res.HostTmpVisible = true
			}
			if _, err := os.ReadFile(req.HostTmpMarker); err == nil {
				res.HostTmpRead = true
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
	res.Environ = os.Environ()
	res.Argv = os.Args
	if b, err := os.ReadFile("/proc/self/cmdline"); err == nil {
		res.Cmdline = strings.ReplaceAll(string(b), "\x00", " ")
	}
	res.StdinBytes = probeStdinBytes
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	return 0
}

// probeStdinBytes is how much the probe read on stdin (TestMain sets it).
var probeStdinBytes int

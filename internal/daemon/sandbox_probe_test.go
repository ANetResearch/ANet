package daemon

import (
	"encoding/json"
	"fmt"
	"net"
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
}

// sandboxProbeResult is what the probe could do. Every field except WorkDirWritable must come back
// false (or zero) for the sandbox to hold.
type sandboxProbeResult struct {
	ControlTokenRead bool   `json:"control_token_read"`
	A2ATokenRead     bool   `json:"a2a_token_read"`
	SocketConnected  bool   `json:"socket_connected"`
	DBusConnected    bool   `json:"dbus_connected"`
	DataDirVisible   bool   `json:"data_dir_visible"`
	HomeEntries      int    `json:"home_entries"`
	WorkDirWritable  bool   `json:"workdir_writable"`
	Cwd              string `json:"cwd"`
	Error            string `json:"error,omitempty"`
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

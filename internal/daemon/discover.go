package daemon

import (
	"net/http"
	"time"
)

// RunningDaemons returns the locally-recorded identities whose daemon is actually reachable right now
// (a fast loopback /ping). The registry can hold stale entries for daemons that crashed without cleaning
// up, so the CLI probes each before showing it — used to tell an operator "a daemon IS running, but at a
// different data dir/port; set ANET_DATA_DIR" when their default data dir has no live daemon.
func RunningDaemons() []IdentityEntry {
	list, err := listRegistry()
	if err != nil || len(list) == 0 {
		return nil
	}
	client := &http.Client{Timeout: 400 * time.Millisecond}
	out := make([]IdentityEntry, 0, len(list))
	for _, e := range list {
		if e.ControlAddr == "" {
			continue
		}
		resp, err := client.Get("http://" + e.ControlAddr + "/ping")
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			out = append(out, e)
		}
	}
	return out
}

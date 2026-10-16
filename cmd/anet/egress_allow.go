//go:build !no_autoreply

package main

// egress_allow.go is `anet autoreply set --egress-allow host[,host…]` (v0.3.0 LEC CN-1, CN-8, CL-1; ux
// UX-C12): the hosts a sandboxed agent may reach with CONNECT, each by its exact name, on port 443.
//
// Every allowed host is a way for an untrusted peer's task to send data out, so adding one is confirmed
// on the operator's terminal (ttyConfirm, with egress_confirm naming the hosts added). Without a
// terminal nothing is sent and the command says, in tty_required's words, who has to run it where.
// Removing hosts, or clearing the list with --egress-allow "", needs no confirmation. The list is
// checked here first (CN-1), so that nobody is asked to confirm what the daemon would refuse; the daemon
// checks it again, against its own api_base.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module/autoreply"
)

// egressStderr is where the command's sentences go; tests replace it.
var egressStderr io.Writer = os.Stderr

// egressAllowUsage is the flag's usage line.
const egressAllowUsage = `autoreply set --egress-allow host[,host…]   (--egress-allow "" clears the list)`

// egressAllowRequest reads --egress-allow (value v, other the API base the same command sets) and
// returns the sandbox member of the /autoreply request, after the terminal confirmation that adding a
// host needs. An error has been said already (errExit1) or is a usage error.
func egressAllowRequest(c *client, v, apiBase string, configuring bool) (map[string]any, error) {
	if v == "true" { // the flag without a value
		return nil, usageError{egressAllowUsage}
	}
	lang := a2ashape.Lang()
	var list []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.TrimSpace(h); h != "" {
			list = append(list, h)
		}
	}
	hosts, ref := autoreply.CheckEgressAllow(list, apiBase)
	if ref != nil {
		say(egressStderr, a2ashape.Sentence(ref.Key, lang, ref.Args), a2ashape.Next(ref.Key, lang, ref.Args))
		return nil, errExit1
	}
	if len(hosts) > 0 {
		current, configured, err := currentEgressAllow(c)
		if err != nil {
			return nil, err
		}
		if !configured && !configuring {
			// A list of hosts for a sandbox that is not configured yet: the backend comes first.
			needBackendFirst(lang)
			return nil, errExit1
		}
		var added []string
		for _, h := range hosts {
			if !slices.Contains(current, h) {
				added = append(added, h)
			}
		}
		if len(added) > 0 {
			err := ttyConfirm("egress", func() (string, error) {
				return a2ashape.Sentence("egress_confirm", lang, a2ashape.Args{"hosts": strings.Join(added, ", ")}), nil
			})
			if errors.Is(err, errNoTTY) || errors.Is(err, errDeclined) {
				// Said already (tty.go): tty_required, or confirm.declined.
				return nil, errExit1
			}
			if err != nil {
				return nil, err
			}
		}
	}
	return map[string]any{"egress_allow": hosts}, nil
}

// currentEgressAllow is the allowlist the daemon holds now (/status auto_reply.sandbox.egress_allow).
func currentEgressAllow(c *client) (list []string, configured bool, err error) {
	b, code, err := c.fetch("/status", nil)
	if err != nil {
		return nil, false, diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return nil, false, fmt.Errorf("status: %s", strings.TrimSpace(string(b)))
	}
	var st struct {
		AutoReply *struct {
			Sandbox struct {
				EgressAllow []string `json:"egress_allow"`
			} `json:"sandbox"`
		} `json:"auto_reply"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, false, fmt.Errorf("status: %v", err)
	}
	if st.AutoReply == nil {
		return nil, false, nil
	}
	return st.AutoReply.Sandbox.EgressAllow, true, nil
}

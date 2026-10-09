//go:build !no_anetlink

package main

// doctor_anetlink.go is doctor's reading of the ANetLink module (modules.anetlink, LEC §5.5, ux
// §5.2): doctor dials the C1 socket itself, through the same checks the daemon's client makes, and
// says whether the socket is safe to use and speaks C1 v1 (anetlink.socket), and which public
// capabilities ANetLink would refuse to a remote caller (anetlink.publish). The daemon leaves those
// off the card and the hub registration (LEC CC-5); this is where the operator learns why.
//
// A value of the block the daemon refuses to start with is config_invalid, which the config check
// reports; the probe is skipped then.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
	"github.com/ANetResearch/ANet/internal/daemon"
	linkprov "github.com/ANetResearch/ANet/provider/anetlink"
)

// anetlinkProbeTimeout bounds doctor's exchange with anetlinkd, a local process.
const anetlinkProbeTimeout = 5 * time.Second

// anetlinkChecks adds anetlink.socket, anetlink.publish and the socket's backends.peer_check.
func anetlinkChecks(add func(id, status, detail, hint string), say func(id, status, key string, args a2ashape.Args),
	cfg daemon.Config, public []daemon.PublicCapability) {
	raw, ok := cfg.Modules["anetlink"]
	if !ok {
		return
	}
	var m struct {
		Socket string `json:"socket"`
		backendconn.Policy
	}
	if json.Unmarshal(raw, &m) != nil || m.AllowTCP {
		return // config_invalid
	}
	sock := m.Socket
	if sock == "" {
		var err error
		if sock, err = linkprov.DefaultSocket(); err != nil {
			return // config_invalid
		}
	}
	rules, err := m.Policy.Resolve()
	if err != nil {
		return // config_invalid
	}
	p, err := linkprov.New("anetlink", sock, rules)
	if err != nil {
		return // config_invalid
	}
	peerCheckFinding(add, say, "ANetLink", sock, rules)

	const id = "anetlink.socket"
	ctx, cancel := context.WithTimeout(context.Background(), anetlinkProbeTimeout)
	defer cancel()
	err = p.SpeaksC1v1(ctx)
	var re *backendconn.RefusedError
	var c1e *linkprov.C1Error
	switch {
	case err == nil:
	case linkprov.NotC1v1(err):
		say(id, stFail, "c1_version", nil)
		return
	case errors.As(err, &re) && re.Code == backendconn.CodePeerUIDMismatch:
		args := a2ashape.Args{}
		for k, v := range re.Args {
			args[k] = v
		}
		say(id, stFail, re.Code, args)
		return
	case errors.As(err, &re) && re.Code == backendconn.CodePeerCredUnsupported:
		return // backends.peer_check says it
	case errors.As(err, &re):
		add(id, stFail, "ANetLink's socket "+sock+": "+re.Reason+"; the daemon sends it nothing",
			"the socket's directory, and those above it, must be writable only by root, this user or ANetLink's user (or socket_group)")
		return
	case errors.As(err, &c1e):
		// It answered in C1 v1, with a refusal: it is there, and failing.
		add(id, stFail, "ANetLink at "+sock+" speaks C1 v1 but failed its health check ("+err.Error()+")",
			"see anetlinkd's log for why")
		return
	default:
		add(id, stFail, "nothing answers on ANetLink's socket "+sock+" ("+err.Error()+"); the daemon does not start without it",
			"start anetlinkd (its --c1-socket and modules.anetlink.socket must name the same path)")
		return
	}
	allowed, err := p.RemoteAllowed(ctx)
	if err != nil {
		if linkprov.NotC1v1(err) {
			say(id, stFail, "c1_version", nil)
		} else {
			add(id, stFail, "ANetLink at "+sock+" did not list its capabilities: "+err.Error(), "")
		}
		return
	}
	checked := "the account listening is checked on every connection"
	if rules.PathOnly() {
		checked = "the account listening is not checked (peer_check path-only)"
	}
	add(id, stOK, fmt.Sprintf("ANetLink at %s speaks C1 v1 and serves %d capabilities; %s", sock, len(allowed), checked), "")

	var refused []string
	for _, pc := range public {
		cid := strings.TrimSpace(pc.ID)
		if remote, served := allowed[cid]; served && !remote {
			refused = append(refused, cid)
		}
	}
	sort.Strings(refused)
	for _, cid := range refused {
		say("anetlink.publish", stWarn, "anetlink.not_remote", a2ashape.Args{"name": cid})
	}
}

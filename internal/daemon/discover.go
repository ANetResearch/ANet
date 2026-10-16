package daemon

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/module"
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

// keyStateOf is a directory entry's key_state as listings show it (KEL
// D-28): fresh when the entry carried a notice that passed C-51 here at
// arrival = now, stale otherwise. It only describes the entry; whether a
// message may be sealed to it is resolvePeer's to decide (D-26).
func keyStateOf(noticeOK bool) string {
	if noticeOK {
		return module.KeyStateFresh
	}
	return module.KeyStateStale
}

// dirNoticeOK checks a notice a directory or registry entry carried about
// kel at now (KEL C-51), with the hub KEL set refreshed once when it names
// a witness this node does not hold far enough (witness-unknown, D-24).
func (d *Daemon) dirNoticeOK(ctx context.Context, notice []byte, kel []identity.SignedEvent, now uint64) bool {
	if len(notice) == 0 || len(notice) > seal.MaxNoticeBytes || len(kel) == 0 {
		return false
	}
	err := identity.VerifyNotice(notice, kel, d.hubKELs, now)
	if errors.Is(err, identity.ErrNoticeUnknownWitness) && d.refreshHubKELs(ctx) {
		err = identity.VerifyNotice(notice, kel, d.hubKELs, now)
	}
	return err == nil
}

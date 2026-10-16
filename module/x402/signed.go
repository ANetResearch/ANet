//go:build !no_x402

package x402

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// signedGet reads one of this node's own account records off its hub:
// balance, ledger, redemptions or a liability proof (A2A-DESIGN §3.7). The
// hub serves them to the account holder only, so the request is signed as
// this node with relayauth v2 — by the node (PaymentSeam.SignHubRequest),
// which keeps the time of every request to a hub in order, the daemon's
// and this module's alike. An unsigned read is answered 401 and no data:
// the account history showed anyone which accounts paid which, how much
// and when.
func (m *Module) signedGet(ctx context.Context, path, action string, into any) error {
	status, reason, err := m.signedGetStatus(ctx, path, action, into)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &hubStatusError{path: path, status: status, reason: reason}
	}
	return nil
}

// hubStatusError is a hub answer that is not 200.
type hubStatusError struct {
	path   string
	status int
	reason string
}

func (e *hubStatusError) Error() string {
	if e.reason != "" {
		return fmt.Sprintf("x402: hub answered %d %s for %s", e.status, e.reason, e.path)
	}
	return fmt.Sprintf("x402: hub answered %d %s for %s", e.status, http.StatusText(e.status), e.path)
}

// errClockRefused is a hub whose floor for this node's signing time lies
// beyond what other hubs accept: the hub is unreachable for this request.
var errClockRefused = errors.New("x402: the hub asked for a signing time the other hubs would refuse, " +
	"so the clock of this machine or of the hub is wrong")

// signedGetStatus is signedGet that hands a non-200 answer back as its
// status and reason code, for a caller that reads the hub's codes itself
// (a liability proof the hub does not have). A ts_stale answer is signed
// again once above the floor the hub named (PAY-R11).
func (m *Module) signedGetStatus(ctx context.Context, path, action string, into any) (int, string, error) {
	hub := m.hubURL()
	if hub == "" {
		return 0, "", fmt.Errorf("x402: no hub configured, so there is no ledger to read")
	}
	if m.hubAID() == "" {
		return 0, "", fmt.Errorf("x402: cannot learn this hub's identity, so cannot sign a request to it")
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, hub+path, nil)
		if err != nil {
			return 0, "", err
		}
		if err := m.seam.SignHubRequest(req, action, nil); err != nil {
			return 0, "", err
		}
		resp, err := m.hubDo(req)
		if err != nil {
			return 0, "", err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if rerr != nil {
			return 0, "", rerr
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(body, into); err != nil {
				return 0, "", fmt.Errorf("x402: the hub's answer for %s is unreadable: %w", path, err)
			}
			return http.StatusOK, "", nil
		}
		var eb struct {
			Reason string `json:"reason"`
			Floor  int64  `json:"floor"`
		}
		_ = json.Unmarshal(body, &eb)
		if resp.StatusCode == http.StatusUnauthorized && eb.Reason == "ts_stale" && attempt == 0 {
			floor := eb.Floor
			if h, err := strconv.ParseInt(resp.Header.Get("X-ANet-TS-Floor"), 10, 64); err == nil && floor == 0 {
				floor = h
			}
			if m.seam.NoteHubStale(floor) {
				continue
			}
			return 0, "", errClockRefused
		}
		return resp.StatusCode, eb.Reason, nil
	}
}

// hubDo sends one request to this node's hub through the node
// (PaymentSeam.HubDo): on the connection side its route belongs to, with
// the hub wire version, like every request the node makes (sealed-sender
// §4.3.11). hubCallTimeout bounds it from the moment it is sent until its
// body is closed.
func (m *Module) hubDo(req *http.Request) (*http.Response, error) {
	if m.seam == nil {
		return nil, errors.New("x402: no node to reach the hub through")
	}
	ctx, cancel := context.WithTimeout(req.Context(), hubCallTimeout)
	resp, err := m.seam.HubDo(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

// cancelOnClose ends a request's context when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

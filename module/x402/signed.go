//go:build !no_x402

package x402

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ANetResearch/ANetCore/relayauth"
)

// signedGet reads one of this node's own account records off its hub:
// balance, ledger or redemptions (A2A-DESIGN §3.7). The hub serves them to
// the account holder only, so the request is signed as this node with
// relayauth v2 — the action, the hub's AID, the time and a hash of the
// method and the request target — in the X-ANet-* headers. An unsigned
// read is answered 401 and no data: the account history showed anyone
// which accounts paid which, how much and when.
func (m *Module) signedGet(ctx context.Context, path, action string, into any) error {
	hub := m.hubURL()
	if hub == "" {
		return fmt.Errorf("x402: no hub configured, so there is no ledger to read")
	}
	hubAID := m.hubAID()
	if hubAID == "" {
		return fmt.Errorf("x402: cannot learn this hub's identity, so cannot sign a request to it")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hub+path, nil)
	if err != nil {
		return err
	}
	// Strictly increasing: Ed25519 signatures are deterministic, and the
	// hub refuses a signature it has seen, so two identical reads in one
	// millisecond must not carry the same time.
	ts := uint64(time.Now().UnixMilli())
	for {
		last := m.lastSignTS.Load()
		if ts <= last {
			ts = last + 1
		}
		if m.lastSignTS.CompareAndSwap(last, ts) {
			break
		}
	}
	sig, seq := m.seam.Sign(relayauth.PreimageV2(action, m.AID(), hubAID, ts, http.MethodGet, req.URL.RequestURI(), nil))
	req.Header.Set(relayauth.HeaderAID, m.AID())
	req.Header.Set(relayauth.HeaderTS, strconv.FormatUint(ts, 10))
	req.Header.Set(relayauth.HeaderSeq, strconv.FormatUint(seq, 10))
	req.Header.Set(relayauth.HeaderSig, relayauth.EncodeSig(sig))
	resp, err := (&http.Client{Timeout: hubCallTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("x402: hub answered %s for %s", resp.Status, path)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(into)
}

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// verify checks a receipt the way a stranger would.
//
// The README has always promised results "anyone can verify and nobody can
// forge — not even the network operator". Until this command, anyone meant
// this repository: Receipt lived in internal/, which Go forbids any other
// module from importing, and no command exposed it. The claim was true of
// the design and unavailable to anybody.
//
// Two modes, and the second is the one that matters:
//
//	anet verify <interaction-id>
//	anet verify --receipt B64 --kel B64 [--result FILE]
//
// The second needs no daemon, no hub, no network, and no trust in either.
// It takes a receipt, the signer's key history, and optionally the bytes
// the receipt claims to cover, and answers from arithmetic alone.
func verify(layout daemon.Layout, rest []string) error {
	pos, flags := splitFlags(rest)

	if r := flags["receipt"]; r != "" {
		kel := flags["kel"]
		if kel == "" {
			// A receipt names the AID that signed it, so the key history
			// can be fetched rather than supplied — from any hub that
			// knows the signer. The receipt is still the thing being
			// checked; the hub only hands over a public log, and holding
			// it lets you check signatures, never make them.
			hub := flags["hub"]
			if hub == "" {
				return fmt.Errorf("verify: give --kel, or --hub URL to fetch the signer's key history\n" +
					"       (a receipt alone proves nothing without the key that signed it)")
			}
			var err error
			kel, err = fetchKEL(hub, r)
			if err != nil {
				return err
			}
		}
		return verifyOffline(r, kel, flags["result"])
	}
	if flags["kel"] != "" {
		return fmt.Errorf("verify: --kel needs a --receipt to check")
	}
	if len(pos) < 1 {
		return fmt.Errorf("verify <interaction-id>\n" +
			"       verify --receipt <base64> --kel <base64> [--result FILE]\n" +
			"       verify --receipt <base64> --hub <url>    [--result FILE]\n\n" +
			"The second form needs nothing at all: a receipt and the signer's key\n" +
			"history are enough, offline. The third fetches that key history from a\n" +
			"hub, for a stranger who was handed a receipt and nothing else.")
	}
	return verifyStored(layout, pos[0])
}

// verifyStored looks an interaction up in this node's own results and checks
// it — the same arithmetic as the offline path, over what we hold.
func verifyStored(layout daemon.Layout, interactionID string) error {
	base, token, err := daemon.ResolveControl(layout)
	if err != nil {
		return diagnoseNoDaemon("http://"+daemon.LocalControlAddr(layout), layout.Root, err)
	}
	c := &client{base: base, token: token, timeout: 30 * time.Second, dataDir: layout.Root}
	var out struct {
		Results []struct {
			InteractionID string `json:"interaction_id"`
			Provider      string `json:"provider"`
			ResultCID     string `json:"result_cid"`
			Receipt       string `json:"receipt"`
			ProviderKEL   string `json:"provider_kel"`
		} `json:"results"`
	}
	raw, code, err := c.fetch("/results", map[string]any{})
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("daemon returned %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	for _, r := range out.Results {
		if r.InteractionID != interactionID {
			continue
		}
		if r.ProviderKEL == "" {
			return fmt.Errorf("verify: no key history for %s — this node never verified one, "+
				"so it cannot vouch for the receipt and neither should you", r.Provider)
		}
		return report(r.Receipt, r.ProviderKEL, r.ResultCID, "")
	}
	return fmt.Errorf("verify: no completed interaction %s", interactionID)
}

func verifyOffline(receiptB64, kelB64, resultPath string) error {
	expectCID := ""
	if resultPath != "" {
		body, err := os.ReadFile(resultPath)
		if err != nil {
			return err
		}
		cid, err := anetcid.Sum(body)
		if err != nil {
			return err
		}
		expectCID = cid
	}
	return report(receiptB64, kelB64, expectCID, resultPath)
}

// report performs the check and prints what was and was not established.
func report(receiptB64, kelB64, expectResultCID, resultPath string) error {
	rb, err := base64.StdEncoding.DecodeString(strings.TrimSpace(receiptB64))
	if err != nil {
		return fmt.Errorf("verify: receipt is not base64: %w", err)
	}
	kb, err := base64.StdEncoding.DecodeString(strings.TrimSpace(kelB64))
	if err != nil {
		return fmt.Errorf("verify: kel is not base64: %w", err)
	}
	rc, err := evidence.UnmarshalReceipt(rb)
	if err != nil {
		return fmt.Errorf("verify: undecodable receipt: %w", err)
	}
	kel, err := identity.UnmarshalKEL(kb)
	if err != nil {
		return fmt.Errorf("verify: undecodable key history: %w", err)
	}

	if err := rc.Verify(kel, uint64(time.Now().UnixMilli())); err != nil {
		fmt.Printf("✗ receipt does NOT verify: %v\n", err)
		return errQuiet
	}
	cid, err := rc.CID()
	if err != nil {
		return err
	}

	fmt.Printf("✓ signature verifies under %s\n", rc.ProviderAID)
	fmt.Printf("  interaction  %s\n", rc.InteractionID)
	fmt.Printf("  requester    %s\n", rc.RequesterAID)
	fmt.Printf("  request cid  %s\n", rc.RequestCID)
	fmt.Printf("  result cid   %s\n", rc.ResultCID)
	fmt.Printf("  completed    %s\n", time.UnixMilli(int64(rc.CompletedAt)).UTC().Format(time.RFC3339))
	fmt.Printf("  receipt cid  %s\n", cid)

	switch {
	case expectResultCID == "":
		// Say so rather than implying the content was checked. A signature
		// over a CID proves nothing about bytes you have not hashed.
		fmt.Println("\n  note: the content was not checked — pass --result FILE to bind")
		fmt.Println("        this receipt to the bytes it claims to cover.")
	case expectResultCID != rc.ResultCID:
		what := "the stored result"
		if resultPath != "" {
			what = resultPath
		}
		fmt.Printf("\n✗ but it does NOT cover %s\n", what)
		fmt.Printf("  receipt covers %s\n  those bytes hash to %s\n", rc.ResultCID, expectResultCID)
		return errQuiet
	default:
		fmt.Println("\n✓ and it covers exactly the result bytes checked.")
	}
	return nil
}

// errQuiet exits non-zero without printing again: the verdict is already on
// stdout and a verification failure is an answer, not a malfunction.
var errQuiet = quietError{}

type quietError struct{}

func (quietError) Error() string { return "" }

// anetcidSum and base64Std are named locally so a test can build a receipt
// that covers exactly the bytes it writes to disk.
func anetcidSum(b []byte) (string, error) { return anetcid.Sum(b) }
func base64Std(b []byte) string           { return base64.StdEncoding.EncodeToString(b) }

// fetchKEL asks a hub for the key history of whoever signed this receipt.
//
// The AID comes out of the receipt, so the caller does not have to know
// who signed it — which is the ordinary case for a third party handed a
// receipt and asked whether it is real.
func fetchKEL(hubURL, receiptB64 string) (string, error) {
	rb, err := base64.StdEncoding.DecodeString(strings.TrimSpace(receiptB64))
	if err != nil {
		return "", fmt.Errorf("verify: receipt is not base64: %w", err)
	}
	rc, err := evidence.UnmarshalReceipt(rb)
	if err != nil {
		return "", fmt.Errorf("verify: undecodable receipt: %w", err)
	}
	if rc.ProviderAID == "" {
		return "", fmt.Errorf("verify: the receipt names no signer")
	}
	u := strings.TrimSuffix(hubURL, "/") + "/agents/" + url.PathEscape(rc.ProviderAID) + "/kel"
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(u)
	if err != nil {
		return "", fmt.Errorf("verify: fetch key history: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("verify: %s does not know %s (hub returned %d)",
			hubURL, rc.ProviderAID, resp.StatusCode)
	}
	var out struct {
		KEL string `json:"kel"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("verify: hub reply: %w", err)
	}
	if out.KEL == "" {
		return "", fmt.Errorf("verify: %s published no key history for %s", hubURL, rc.ProviderAID)
	}
	fmt.Printf("· key history for %s fetched from %s\n", rc.ProviderAID, hubURL)
	return out.KEL, nil
}

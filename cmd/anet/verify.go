package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	evidence "github.com/ANetResearch/ANet/internal/evidencev1" // v030-todo(PAY-14): evidence v2
	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/anetcid"
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
//	anet verify --receipt B64 --kel B64 [--result FILE] [--request FILE]
//
// The second needs no daemon, no hub, no network, and no trust in either.
// It takes a receipt, the signer's key history, and optionally the bytes
// the receipt claims to cover and the request it claims to answer, and
// answers from arithmetic alone.
func verify(layout daemon.Layout, rest []string) error {
	pos, flags := splitFlags(rest)

	if dir := flags["chain"]; dir != "" && dir != "true" {
		// An exported evidence chain (`anet audit --export DIR`).
		return verifyChain(dir, flags["kel"], flags["hub"], flags["head"])
	}
	if flags["head"] != "" {
		return fmt.Errorf("verify: --head names a record of an exported chain; it needs --chain DIR")
	}
	if r := flags["receipt"]; r != "" {
		kel := flags["kel"]
		if kel == "" {
			// A receipt names the AID that signed it, so the key history
			// can be fetched rather than supplied: through this node's
			// daemon, or from the directory of a hub that knows the
			// signer. The receipt is still the thing being checked; the
			// hub only hands over a public log, and holding it lets you
			// check signatures, never make them.
			if flags["hub"] == "" && !daemonConfigured(layout) {
				return fmt.Errorf("verify: give --kel, or --hub URL to fetch the signer's key history\n" +
					"       (a receipt alone proves nothing without the key that signed it)")
			}
			var err error
			kel, err = fetchKEL(layout, flags["hub"], r)
			if err != nil {
				return err
			}
		}
		return verifyOffline(r, kel, flags["result"], flags["request"])
	}
	if a := flags["attestation"]; a != "" {
		return verifyAttestation(layout, a, flags["kel"], flags["hub"])
	}
	if flags["kel"] != "" {
		return fmt.Errorf("verify: --kel needs a --receipt, --attestation or --chain to check")
	}
	if len(pos) < 1 {
		return fmt.Errorf("verify <interaction-id>\n" +
			"       verify --receipt <base64> --kel <base64> [--result FILE] [--request FILE]\n" +
			"       verify --receipt <base64> --hub <url>    [--result FILE] [--request FILE]\n" +
			"       verify --attestation <base64> --hub <url>\n" +
			"       verify --chain DIR [--kel <base64>|--hub <url>] [--head ID]\n\n" +
			"The second form needs nothing at all: a receipt and the signer's key\n" +
			"history are enough, offline. The third fetches that key history from a\n" +
			"hub's directory, for a stranger who was handed a receipt and nothing\n" +
			"else; with a daemon running, the daemon fetches it and --hub may go.\n" +
			"The fourth checks a witness attestation from a hub's /x402/witnesses.\n" +
			"The fifth checks an evidence chain exported by `anet audit --export DIR`.")
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
			RequestCID    string `json:"request_cid"`
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
		// The request too, as the daemon binds it when the result arrives
		// (redteam F15): a row stored before it did is checked here.
		return report(r.Receipt, r.ProviderKEL, r.ResultCID, "", r.RequestCID, "")
	}
	return fmt.Errorf("verify: no completed interaction %s", interactionID)
}

func verifyOffline(receiptB64, kelB64, resultPath, requestPath string) error {
	expectCID, err := fileCID(resultPath)
	if err != nil {
		return err
	}
	expectRequest, err := fileCID(requestPath)
	if err != nil {
		return err
	}
	return report(receiptB64, kelB64, expectCID, resultPath, expectRequest, requestPath)
}

// fileCID is the CID of the file at path, or "" for no path.
func fileCID(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return anetcid.Sum(body)
}

// report performs the check and prints what was and was not established:
// the signature, then — when given — that the receipt covers the result
// bytes and answers the request (redteam F15: a provider can sign a receipt
// naming any request, so a receipt checked without its request says nothing
// about what was asked).
func report(receiptB64, kelB64, expectResultCID, resultPath, expectRequestCID, requestPath string) error {
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
	switch {
	case expectRequestCID == "":
		fmt.Println("  note: the request was not checked — pass --request FILE (the request's bytes) to bind")
		fmt.Println("        this receipt to what was asked.")
	case expectRequestCID != rc.RequestCID:
		what := "the request this node sent"
		if requestPath != "" {
			what = requestPath
		}
		fmt.Printf("\n✗ but it is NOT a receipt for %s\n", what)
		fmt.Printf("  receipt answers %s\n  the request is  %s\n", rc.RequestCID, expectRequestCID)
		return errQuiet
	default:
		fmt.Println("✓ and it answers exactly the request checked.")
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

// fetchKEL gets the key history of whoever signed this receipt.
//
// The AID comes out of the receipt, so the caller does not have to know
// who signed it — which is the ordinary case for a third party handed a
// receipt and asked whether it is real.
func fetchKEL(layout daemon.Layout, hubURL, receiptB64 string) (string, error) {
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
	return fetchKELThrough(layout, hubURL, rc.ProviderAID,
		"(a receipt alone proves nothing without the key that signed it)")
}

// daemonConfigured reports whether layout names a data directory whose
// daemon the CLI can address (it may still not be running).
func daemonConfigured(layout daemon.Layout) bool {
	if layout.Root == "" {
		return false
	}
	_, _, err := daemon.ResolveControl(layout)
	return err == nil
}

// fetchKELThrough gets one AID's key history without naming that AID to
// any hub (sealed-sender SSR-D14, SSR-D16): from the daemon of layout when
// one is running (POST /agents/kel: its own record of the signer, or its
// hub's directory), else from the directory of the hub given with --hub
// (fetchKELFor). why says, when neither is at hand, why a key is needed.
func fetchKELThrough(layout daemon.Layout, hubURL, aid, why string) (string, error) {
	if daemonConfigured(layout) {
		if base, token, err := daemon.ResolveControl(layout); err == nil {
			c := &client{base: base, token: token, timeout: 60 * time.Second, dataDir: layout.Root}
			raw, code, err := c.fetch("/agents/kel", map[string]any{"aid": aid})
			if err == nil {
				var out struct {
					KEL    string `json:"kel"`
					Source string `json:"source"`
					Error  string `json:"error"`
				}
				_ = json.Unmarshal(raw, &out)
				switch {
				case code == http.StatusOK && out.KEL != "":
					fmt.Printf("· key history for %s fetched through this node's daemon (%s)\n", aid, out.Source)
					return out.KEL, nil
				case code == http.StatusNotFound && out.Error != "":
					// The daemon's own answer: the directory holds no
					// entry for aid, in a whole answer.
					return "", fmt.Errorf("verify: %s", out.Error)
				case hubURL == "":
					why := out.Error
					if why == "" {
						why = strings.TrimSpace(string(raw))
					}
					return "", fmt.Errorf("verify: the daemon could not fetch %s's key history (%d): %s; try again later, or give --hub URL",
						aid, code, printable(why, 200))
				}
				// The daemon cannot read its directory now; the hub given
				// with --hub may.
			}
		}
	}
	if hubURL == "" {
		return "", fmt.Errorf("verify: give --kel, or --hub URL to fetch the key history of %s\n"+
			"       (or run this where its daemon runs) %s", aid, why)
	}
	return fetchKELFor(hubURL, aid)
}

// fetchKELFor gets one AID's key history from the directory of the hub at
// hubURL, with no daemon (daemon.DirectoryKEL): a bucket of agents asked
// by a few bits of the AID's directory hash and the one entry opened here,
// so the request names nobody; the hub's own key history when the AID is
// the hub's.
//
// Split out from fetchKEL, which could only start from a receipt. A
// witness attestation and an exported chain name their signer the same way
// and need the same lookup.
func fetchKELFor(hubURL, aid string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	kel, err := daemon.DirectoryKEL(ctx, hubURL, aid)
	if err != nil {
		return "", fmt.Errorf("verify: fetch the key history of %s: %w", aid, err)
	}
	raw, err := identity.MarshalKEL(kel)
	if err != nil {
		return "", err
	}
	// Said out loud, because it is the difference between a check that
	// needed nothing and one that asked a hub. A verifier that cannot
	// tell those apart will record the stronger.
	//
	// Printed here rather than at the call sites: it was at one of them,
	// and splitting the fetch out of the receipt path dropped it for that
	// path while the attestation path had its own copy. One place, every
	// caller.
	fmt.Printf("· key history for %s fetched from %s (its directory, which was not told whose)\n", aid, hubURL)
	return base64Std(raw), nil
}

// verifyAttestation checks a witness's statement about a chain head.
//
// A hub's /x402/witnesses publishes attestations and tells the reader to
// resolve each witness and check the signature. Nothing could do that:
// this command knew only receipts, so the instruction named a step with
// no tool behind it. Evidence published with no way to check it is not
// evidence, and witnessing is the entire basis for believing a hub has
// not rewritten its own issuance chain.
//
// What it proves and what it does not, stated because the difference is
// the whole point: a verifying attestation means this witness signed
// "chain X had head H at sequence N when I looked". It does NOT mean the
// chain is honest — it means that if the hub ever serves a different
// record at sequence N, this signed statement contradicts it and nobody
// has to take anyone's word for which came first.
func verifyAttestation(layout daemon.Layout, attB64, kelB64, hubURL string) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(attB64))
	if err != nil {
		return fmt.Errorf("verify: attestation is not base64: %w", err)
	}
	att, err := ael.UnmarshalHeadAttestation(raw)
	if err != nil {
		return fmt.Errorf("verify: undecodable attestation: %w", err)
	}
	if att.Envelope == nil || att.Envelope.SignerAID == "" {
		return fmt.Errorf("verify: the attestation names no witness")
	}
	witness := att.Envelope.SignerAID
	if kelB64 == "" {
		kelB64, err = fetchKELThrough(layout, hubURL, witness, "(the witness is "+witness+")")
		if err != nil {
			return err
		}
	}
	kb, err := base64.StdEncoding.DecodeString(strings.TrimSpace(kelB64))
	if err != nil {
		return fmt.Errorf("verify: key history is not base64: %w", err)
	}
	kel, err := identity.UnmarshalKEL(kb)
	if err != nil {
		return fmt.Errorf("verify: undecodable key history: %w", err)
	}
	if err := att.Verify(kel, witness, time.Now().UnixMilli()); err != nil {
		fmt.Printf("✗ the attestation does not verify: %v\n", err)
		return fmt.Errorf("verify: attestation refused")
	}
	fmt.Printf("✓ signature verifies under %s…\n", short(witness))
	fmt.Printf("  chain    %s…\n", short(att.ChainDID))
	fmt.Printf("  head     seq %d, id %s…\n", att.Seq, short(att.HeadID))
	fmt.Printf("  observed %s\n", time.UnixMilli(att.ObservedAt).UTC().Format(time.RFC3339))
	fmt.Println("  this says the witness saw that head at that time. It does not say the")
	fmt.Println("  chain is honest — it says a different record at that sequence would")
	fmt.Println("  contradict a statement the witness signed.")
	return nil
}

// short trims an AID for display without pretending it is the whole thing.
func short(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

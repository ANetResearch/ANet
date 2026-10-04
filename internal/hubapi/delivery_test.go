package hubapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/wire"
	"github.com/ANetResearch/ANetCore/wire/scan"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// The delivery-token bodies have exactly the members the registry gives
// /relay/ack and /relay/status (core §2.12.2, CORE-S5), one level down
// into the items too, so the daemon and the hub cannot drift apart on
// them without one side's test failing.
func TestTheDeliveryTokenBodiesAreTheRegistrys(t *testing.T) {
	r, err := wire.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path     string
		response bool
		typ      reflect.Type
	}{
		{"/relay/ack", false, reflect.TypeFor[hubapi.RelayAckRequest]()},
		{"/relay/ack", true, reflect.TypeFor[hubapi.RelayAckResponse]()},
		{"/relay/status", false, reflect.TypeFor[hubapi.RelayStatusRequest]()},
		{"/relay/status", true, reflect.TypeFor[hubapi.RelayStatusResponse]()},
		// The send's answer and the first-contact session (core §2.12.2;
		// the send's request is the envelope, not JSON).
		{hubapi.RelaySendPath, true, reflect.TypeFor[hubapi.RelaySendResponse]()},
		{hubapi.RelaySessionPath, false, reflect.TypeFor[hubapi.SessionRequest]()},
		{hubapi.RelaySessionPath, true, reflect.TypeFor[hubapi.SessionResponse]()},
	} {
		ep := r.Endpoint("POST", c.path)
		if ep == nil {
			t.Fatalf("POST %s is not a registered endpoint", c.path)
		}
		body := ep.Request
		if c.response {
			body = ep.Response
		}
		for _, d := range scan.JSONFields(c.typ, body) {
			t.Errorf("POST %s %s: %s", c.path, c.typ, d)
		}
	}
	if hubapi.HandleLen != seal.HandleLen || hubapi.DAckLen != seal.DAckLen {
		t.Errorf("sizes handle %d dack %d; want seal's %d and %d", hubapi.HandleLen, hubapi.DAckLen, seal.HandleLen, seal.DAckLen)
	}
	if got := [3]int{hubapi.RIDLen, hubapi.HandleLen, hubapi.DAckLen}; got != [3]int{16, 16, 80} {
		t.Errorf("sizes rid, handle, dack %v; want 16, 16, 80 (core §2.7, §2.16)", got)
	}
	if hubapi.RelayAckMaxItems != 1000 || hubapi.RelayStatusMaxItems != 64 {
		t.Errorf("item bounds %d and %d; want 1000 and 64 (DLV-W-14, W-16)", hubapi.RelayAckMaxItems, hubapi.RelayStatusMaxItems)
	}
	for want, got := range map[string]string{"queued": hubapi.RelayStatusQueued, "done": hubapi.RelayStatusDone, "unknown": hubapi.RelayStatusUnknown} {
		if got != want {
			t.Errorf("state %q, want %q", got, want)
		}
	}
}

// The revocation bodies have exactly the members the registry gives POST
// /mailbox/revoke, and the batch bound is the hub's (sealed-sender §2.12,
// ANetHub MaxRevokeBatch).
func TestTheRevocationBodiesAreTheRegistrys(t *testing.T) {
	r, err := wire.Load()
	if err != nil {
		t.Fatal(err)
	}
	ep := r.Endpoint("POST", hubapi.MailboxRevokePath)
	if ep == nil {
		t.Fatalf("POST %s is not a registered endpoint", hubapi.MailboxRevokePath)
	}
	for _, d := range scan.JSONFields(reflect.TypeFor[hubapi.MailboxRevokeRequest](), ep.Request) {
		t.Errorf("request: %s", d)
	}
	for _, d := range scan.JSONFields(reflect.TypeFor[hubapi.MailboxRevokeResponse](), ep.Response) {
		t.Errorf("response: %s", d)
	}
	if hubapi.MailboxRevokeMaxIDs != 1000 {
		t.Errorf("batch bound %d, want 1000", hubapi.MailboxRevokeMaxIDs)
	}
}

// deliverySample is one body of testdata/delivery.json.
type deliverySample struct {
	Name string          `json:"name"`
	Path string          `json:"path"`
	Dir  string          `json:"dir"`
	Type string          `json:"type"`
	Wire json.RawMessage `json:"wire"`
}

func loadDeliverySamples(t *testing.T) []deliverySample {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "delivery.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Format  string           `json:"format"`
		Samples []deliverySample `json:"samples"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Format != "hubapi-samples/1" || len(doc.Samples) < 6 {
		t.Fatalf("testdata/delivery.json: format %q, %d samples", doc.Format, len(doc.Samples))
	}
	return doc.Samples
}

// T42, the daemon's part (DLV §7.3): each body of the delivery-token
// endpoints decodes into its type with no member left over, encodes back to
// the same bytes, and its binary values decode strictly to their sizes.
func TestTheDeliveryTokenBodiesRoundTrip(t *testing.T) {
	types := map[string]func() any{
		"RelayAckRequest":     func() any { return new(hubapi.RelayAckRequest) },
		"RelayAckResponse":    func() any { return new(hubapi.RelayAckResponse) },
		"RelayStatusRequest":  func() any { return new(hubapi.RelayStatusRequest) },
		"RelayStatusResponse": func() any { return new(hubapi.RelayStatusResponse) },
	}
	for _, s := range loadDeliverySamples(t) {
		t.Run(s.Name, func(t *testing.T) {
			mk, ok := types[s.Type]
			if !ok {
				t.Fatalf("unknown type %q", s.Type)
			}
			var want bytes.Buffer
			if err := json.Compact(&want, s.Wire); err != nil {
				t.Fatal(err)
			}
			v := mk()
			dec := json.NewDecoder(bytes.NewReader(s.Wire))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("round trip\n got  %s\n want %s", got, want.Bytes())
			}
			checkBinaryMembers(t, v)
		})
	}
}

// checkBinaryMembers decodes every rid, handle and dack of a body strictly.
func checkBinaryMembers(t *testing.T, v any) {
	t.Helper()
	must := func(what, s string, n int) {
		t.Helper()
		if _, err := hubapi.DecodeB64(s, n); err != nil {
			t.Errorf("%s %q: %v", what, s, err)
		}
	}
	switch b := v.(type) {
	case *hubapi.RelayAckRequest:
		for _, it := range b.Items {
			if (it.Handle == "") != (it.DAck == "") {
				t.Errorf("item %d: handle and dack do not go together", it.ID)
			}
			if it.Handle != "" {
				must("handle", it.Handle, hubapi.HandleLen)
				must("dack", it.DAck, hubapi.DAckLen)
			}
		}
	case *hubapi.RelayStatusRequest:
		for _, it := range b.Items {
			must("rid", it.RID, hubapi.RIDLen)
			must("handle", it.Handle, hubapi.HandleLen)
		}
	case *hubapi.RelayStatusResponse:
		for _, it := range b.Items {
			if it.State == hubapi.RelayStatusDone {
				if _, err := it.DAckBytes(); err != nil {
					t.Errorf("done: %v", err)
				}
			} else if it.DAck != "" {
				t.Errorf("%s carries a dack", it.State)
			}
		}
	}
}

// A binary value has one spelling (core CORE-E5): the base64url alphabet,
// no padding, no white space, no stray bits; and it has its member's size.
func TestDecodeB64IsStrict(t *testing.T) {
	sixteen := bytes.Repeat([]byte{0xfb}, 16) // encodes with '-' and '_'
	canon := hubapi.B64(sixteen)
	if !strings.ContainsAny(canon, "-_") || strings.ContainsAny(canon, "+/=") || len(canon) != 22 {
		t.Fatalf("B64 = %q", canon)
	}
	if b, err := hubapi.DecodeB64(canon, 16); err != nil || !bytes.Equal(b, sixteen) {
		t.Fatalf("canonical value: %x %v", b, err)
	}
	if b, err := hubapi.DecodeB64("", -1); err != nil || len(b) != 0 {
		t.Fatalf("empty, any length: %x %v", b, err)
	}
	std := base64.StdEncoding.EncodeToString(sixteen)
	for name, s := range map[string]string{
		"padded":             base64.URLEncoding.EncodeToString(sixteen),
		"standard alphabet":  strings.TrimRight(std, "="),
		"a line break":       canon[:10] + "\n" + canon[10:],
		"a carriage return":  canon[:10] + "\r" + canon[10:],
		"a space":            canon[:10] + " " + canon[10:],
		"a trailing newline": canon + "\n",
		"stray bits":         canon[:21] + "0", // the same two data bits as the canonical 'w', with low bits set
		"one byte short":     hubapi.B64(sixteen[:15]),
		"one byte long":      hubapi.B64(append(append([]byte{}, sixteen...), 1)),
		"a lone character":   "A",
		"empty":              "",
	} {
		if b, err := hubapi.DecodeB64(s, 16); !errors.Is(err, hubapi.ErrBadB64) {
			t.Errorf("%s (%q): %x %v, want ErrBadB64", name, s, b, err)
		}
	}
}

// The decoders of the hub's answers: a rid or dack that is missing, mis-
// spelled or of the wrong size is an error the daemon acts on (DLV-W-12b,
// W-18a), never a short or zero-padded value.
func TestTheAnswerDecoders(t *testing.T) {
	rid := bytes.Repeat([]byte{7}, 16)
	if got, err := (hubapi.RelaySendResponse{RID: hubapi.B64(rid)}).RIDBytes(); err != nil || !bytes.Equal(got[:], rid) {
		t.Fatalf("RIDBytes: %x %v", got, err)
	}
	for _, bad := range []string{"", "AAAA", hubapi.B64(rid) + "=", hubapi.B64(bytes.Repeat([]byte{7}, 32))} {
		if _, err := (hubapi.RelaySendResponse{RID: bad}).RIDBytes(); !errors.Is(err, hubapi.ErrBadB64) {
			t.Errorf("RIDBytes(%q): %v", bad, err)
		}
	}
	d := bytes.Repeat([]byte{9}, 80)
	if got, err := (hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: hubapi.B64(d)}).DAckBytes(); err != nil || !bytes.Equal(got[:], d) {
		t.Fatalf("DAckBytes: %x %v", got, err)
	}
	for _, bad := range []string{"", hubapi.B64(d[:79]), hubapi.B64(append(d, 0)), base64.StdEncoding.EncodeToString(d)} {
		if _, err := (hubapi.RelayStatusResult{State: hubapi.RelayStatusDone, DAck: bad}).DAckBytes(); !errors.Is(err, hubapi.ErrBadB64) {
			t.Errorf("DAckBytes(%q): %v", bad, err)
		}
	}
	var h [hubapi.HandleLen]byte
	var k [hubapi.DAckLen]byte
	h[0], k[79] = 1, 2
	it := hubapi.NewRelayAckItem(42, h, k)
	if it.ID != 42 || it.Handle != hubapi.B64(h[:]) || it.DAck != hubapi.B64(k[:]) {
		t.Fatalf("NewRelayAckItem: %+v", it)
	}
}

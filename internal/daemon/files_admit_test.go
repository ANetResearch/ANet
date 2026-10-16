package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
)

// png and jpeg are the shortest bodies http.DetectContentType knows by
// their magic bytes.
var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 40)...)
	jpegBytes = append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{9}, 40)...)
)

func attOf(t testing.TB, name string, data []byte) delegation.Attachment {
	t.Helper()
	cid, err := anetcid.SumRaw(data)
	if err != nil {
		t.Fatal(err)
	}
	return delegation.Attachment{Name: name, Mime: "image/png", Size: int64(len(data)), CID: cid, Data: data}
}

// argsFor are the args of a call that names atts as the requester does.
func argsFor(atts ...delegation.Attachment) map[string]any {
	_, list := filesOfCall(atts)
	args := map[string]any{"k": "v"}
	if len(atts) > 0 {
		args[argsFilesKey] = list
	}
	return args
}

// Each clause of CUX-F10, in its order, has a case that fails there and
// nowhere earlier.
func TestFileAdmissionOrder(t *testing.T) {
	cfg := CapFilesConfig{Count: 2, MaxBytes: 100, Types: []string{"image/png"}}
	a, b := attOf(t, "a.png", pngBytes), attOf(t, "b.png", pngBytes)
	big := attOf(t, "c.png", append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{1}, 100)...))
	jpg := attOf(t, "d.jpg", jpegBytes)
	cases := []struct {
		name string
		cfg  CapFilesConfig
		args map[string]any
		atts []delegation.Attachment
		want string
	}{
		{"fine", cfg, argsFor(a, b), []delegation.Attachment{a, b}, ""},
		{"1 no declaration, files sent", CapFilesConfig{}, argsFor(a), []delegation.Attachment{a}, "files_count"},
		{"1 no declaration, $files alone", CapFilesConfig{}, map[string]any{argsFilesKey: []any{}}, nil, "files_count"},
		{"1 no declaration, no files", CapFilesConfig{}, map[string]any{"k": 1}, nil, ""},
		{"2 fewer attachments than $files", cfg, argsFor(a, b), []delegation.Attachment{a}, "bad_request"},
		{"2 $files missing", cfg, map[string]any{"k": 1}, []delegation.Attachment{a, b}, "bad_request"},
		{"2 a name differs", cfg, argsFor(a, b), []delegation.Attachment{a, func() delegation.Attachment { x := b; x.Name = "z.png"; return x }()}, "bad_request"},
		{"2 a cid differs", cfg, argsFor(a, b), []delegation.Attachment{a, func() delegation.Attachment { x := b; x.Data = append([]byte{}, x.Data...); x.Data[20] ^= 1; return x }()}, "bad_request"},
		{"2 a size differs", cfg, argsFor(a, b), []delegation.Attachment{a, func() delegation.Attachment { x := b; x.Size++; return x }()}, "bad_request"},
		{"2 an entry has another member", cfg, map[string]any{argsFilesKey: []any{map[string]any{"name": "a.png", "mime": "image/png", "size": a.Size, "cid": a.CID, "x": 1}}}, []delegation.Attachment{a}, "bad_request"},
		{"2 the mime has a parameter", cfg, map[string]any{argsFilesKey: []any{map[string]any{"name": "a.png", "mime": "image/png; q=1", "size": a.Size, "cid": a.CID}}}, []delegation.Attachment{a}, "bad_request"},
		{"3 one file for two", cfg, argsFor(a), []delegation.Attachment{a}, "files_count"},
		{"4 over max_bytes", cfg, argsFor(a, big), []delegation.Attachment{a, big}, "files_too_large"},
		{"5 sniffed type not listed", cfg, argsFor(a, jpg), []delegation.Attachment{a, jpg}, "file_type_not_accepted"},
		// A wrong count is told before a size, a size before a type.
		{"3 before 4", cfg, argsFor(big), []delegation.Attachment{big}, "files_count"},
		{"4 before 5", CapFilesConfig{Count: 1, MaxBytes: 10, Types: []string{"image/png"}}, argsFor(jpg), []delegation.Attachment{jpg}, "files_too_large"},
	}
	for _, c := range cases {
		if got := admitFiles(c.cfg, c.args, c.atts); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// The declared type is not believed: a file claiming image/png that is
	// a jpeg is a jpeg.
	liar := a
	liar.Data, liar.Mime = jpegBytes, "image/png"
	liar.CID, liar.Size = jpg.CID, jpg.Size
	if got := admitFiles(CapFilesConfig{Count: 1, MaxBytes: 100, Types: []string{"image/png"}}, argsFor(liar), []delegation.Attachment{liar}); got != "file_type_not_accepted" {
		t.Errorf("a jpeg claiming png: %q", got)
	}
}

func TestFilesConfigIsChecked(t *testing.T) {
	good := CapFilesConfig{Count: 1, MaxBytes: 1, Types: []string{"image/jpeg"}}
	if zh, _ := checkFilesConfig(good); zh != "" {
		t.Fatal(zh)
	}
	for _, c := range []CapFilesConfig{
		{}, {Count: 9, MaxBytes: 1, Types: good.Types}, {Count: 1, MaxBytes: 0, Types: good.Types},
		{Count: 1, MaxBytes: 64<<20 + 1, Types: good.Types}, {Count: 1, MaxBytes: 1},
		{Count: 1, MaxBytes: 1, Types: []string{"image/heic"}},
	} {
		if zh, _ := checkFilesConfig(c); zh == "" {
			t.Errorf("%+v was accepted", c)
		}
	}
}

// fileProvider takes files for its one capability, and keeps what it was
// handed so the test can look.
type fileProvider struct {
	id      string
	capID   string
	accepts bool

	mu   sync.Mutex
	got  []provider.File
	args map[string]any
}

func (p *fileProvider) ID() string { return p.id }
func (p *fileProvider) Capabilities(context.Context) ([]string, error) {
	return []string{p.capID}, nil
}
func (p *fileProvider) Describe(context.Context) (string, error) { return "", nil }
func (p *fileProvider) Health(context.Context) error             { return nil }
func (p *fileProvider) AcceptsFiles(id string) bool              { return p.accepts && id == p.capID }
func (p *fileProvider) Invoke(_ context.Context, c provider.Call) (effect.Effect, error) {
	p.mu.Lock()
	p.got, p.args = c.Files, c.Args
	p.mu.Unlock()
	return effect.Effect{Status: effect.OK, Message: "compared"}, nil
}

// plainProvider serves a capability and cannot take files.
type plainProvider struct{ fileProvider }

func (*plainProvider) AcceptsFiles(string) bool { panic("not a FileReceiver") }

type noFiles struct{ p *fileProvider }

func (n noFiles) ID() string                                       { return n.p.ID() }
func (n noFiles) Capabilities(c context.Context) ([]string, error) { return n.p.Capabilities(c) }
func (n noFiles) Describe(c context.Context) (string, error)       { return "", nil }
func (n noFiles) Health(context.Context) error                     { return nil }
func (n noFiles) Invoke(c context.Context, call provider.Call) (effect.Effect, error) {
	return n.p.Invoke(c, call)
}

const filesCap = "face.compare"

var twoPNG = CapFilesConfig{Count: 2, MaxBytes: 1 << 20, Types: []string{"image/png"}}

// A capability that declares files needs a provider that implements
// FileReceiver and says yes for it; none serving it is the same (CUX-F15).
func TestFilesRequireAFileReceiver(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	d := registered(t, srv.URL, "prov")
	declare := []PublicCapability{{ID: filesCap, Files: &twoPNG}}

	if err := d.SetPublicCapabilities(declare); err == nil {
		t.Fatal("files declared with no provider")
	}
	fp := &fileProvider{id: "plain", capID: filesCap}
	if err := d.Providers().Register(ctx, noFiles{fp}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPublicCapabilities(declare); err == nil || !strings.Contains(err.Error(), filesCap) {
		t.Fatalf("a provider that is not a FileReceiver was accepted: %v", err)
	}
	d.Providers().Deregister("plain")
	fp2 := &fileProvider{id: "rcv", capID: filesCap, accepts: false}
	if err := d.Providers().Register(ctx, fp2); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPublicCapabilities(declare); err == nil {
		t.Fatal("a FileReceiver that answers no was accepted")
	}
	fp2.accepts = true
	if err := d.SetPublicCapabilities(declare); err != nil {
		t.Fatal(err)
	}
	// Without files declared nothing is asked of the provider.
	d.Providers().Deregister("rcv")
	if err := d.SetPublicCapabilities([]PublicCapability{{ID: filesCap}}); err != nil {
		t.Fatal(err)
	}
}

func fileCall(t *testing.T, req *Daemon, to string, atts ...delegation.Attachment) string {
	t.Helper()
	id, err := newInteractionID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.delegateCapabilityCtx(context.Background(), id, to, filesCap, map[string]any{"k": "v"}, atts, "", nil); err != nil {
		t.Fatal(err)
	}
	return id
}

// dataFilesContain reports whether any file under dir holds needle.
func dataFilesContain(t *testing.T, dir string, needle []byte) []string {
	t.Helper()
	var hits []string
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Size() > 64<<20 {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, needle) {
			hits = append(hits, p)
		}
		return nil
	})
	return hits
}

func filesFixture(t *testing.T, discard bool) (req, prov *Daemon, fp *fileProvider) {
	t.Helper()
	srv := newFakeHub(t)
	req = registered(t, srv.URL, "req")
	prov = registered(t, srv.URL, "prov")
	fp = &fileProvider{id: "rcv", capID: filesCap, accepts: true}
	if err := prov.Providers().Register(context.Background(), fp); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: filesCap, Files: &twoPNG, Discard: discard}}); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyClosed)
	return req, prov, fp
}

// A call with files is admitted, the provider gets the files with the
// sniffed type, and the request carries every file's CID in the signed
// args (CUX-F3, CUX-F20).
func TestACallWithFilesReachesTheProvider(t *testing.T) {
	req, prov, fp := filesFixture(t, false)
	a, b := attOf(t, "a.png", pngBytes), attOf(t, "b.png", append(append([]byte{}, pngBytes...), 1))
	b.Mime = "IMAGE/PNG; x=1"
	id := fileCall(t, req, prov.AID(), a, b)
	if err := prov.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.got) != 2 || fp.got[0].Name != "a.png" || fp.got[0].Mime != "image/png" || !bytes.Equal(fp.got[1].Data, b.Data) {
		t.Fatalf("the provider got %+v", fp.got)
	}
	if _, ok := fp.args[argsFilesKey]; !ok {
		t.Fatal("the provider's args lost $files")
	}
	ix, err := prov.ix.Get(id)
	if err != nil || ix.State != interactions.StateCompleted {
		t.Fatalf("%+v %v", ix, err)
	}
	if prov.callFilesOf(id) != nil {
		t.Fatal("the files outlived the answer")
	}
	// The requester keeps its own copy like any it sends (CUX-F5).
	atts, err := req.ix.Attachments(id)
	if err != nil || len(atts) != 2 {
		t.Fatalf("requester attachments %d %v", len(atts), err)
	}
}

// A call that fails the files' admission is refused with its code, and
// takes nothing from the quotas.
func TestFilesRefusalsCarryTheirCode(t *testing.T) {
	req, prov, fp := filesFixture(t, false)
	one := attOf(t, "a.png", pngBytes)
	id := fileCall(t, req, prov.AID(), one)
	if err := prov.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	ran := fp.got != nil
	fp.mu.Unlock()
	if ran {
		t.Fatal("a call with the wrong number of files ran")
	}
	if _, err := prov.ix.Get(id); err == nil {
		t.Fatal("a refused call left a task on the provider")
	}
	dec := prov.decideDelegate(req.AID(), delegateForm{capID: filesCap, args: argsFor(one), atts: []delegation.Attachment{one}})
	if dec.action != actRefuse || dec.reason != "files_count" {
		t.Fatalf("%+v", dec)
	}
	if n := len(prov.admission.callers); n != 0 {
		t.Fatalf("a refused call took quota: %d callers", n)
	}
}

// The args of a discard call and the files are nowhere on the disk after
// it: not the file bytes, not the args, not the result (CUX-F13, T-CUX1 in
// the small).
func TestDiscardLeavesNothing(t *testing.T) {
	req, prov, _ := filesFixture(t, true)
	marker := []byte("DISCARD-MARKER-0123456789abcdef0123456789abcdef0123456789abcdef01234567")
	a := attOf(t, "a.png", append(append([]byte{}, pngBytes...), marker...))
	b := attOf(t, "b.png", append(append([]byte{}, pngBytes...), 1))
	id := fileCall(t, req, prov.AID(), a, b)
	if err := prov.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ix, err := prov.ix.Get(id)
	if err != nil || ix.State != interactions.StateCompleted {
		t.Fatalf("%+v %v", ix, err)
	}
	if len(ix.RequestDoc) != 0 || len(ix.Result) != 0 {
		t.Fatalf("the terminal write left the request (%d) or the result (%d)", len(ix.RequestDoc), len(ix.Result))
	}
	if ix.RequestCID == "" || ix.ResultCID == "" {
		t.Fatal("a discard call lost its CIDs")
	}
	if atts, _ := prov.ix.Attachments(id); len(atts) != 0 {
		t.Fatalf("%d attachment rows", len(atts))
	}
	if n := countMsgs(t, prov, id); n != 0 {
		t.Fatalf("%d message rows", n)
	}
	if hits := dataFilesContain(t, prov.layout.Root, marker); len(hits) != 0 {
		t.Fatalf("the file bytes are on the disk: %v", hits)
	}
	ev := lastLedgerPayload(t, prov, EvCapabilityEffect)
	for _, k := range []string{"metrics", "evidence", "verifiable"} {
		if _, ok := ev[k]; ok {
			t.Errorf("a discard call's event holds %s", k)
		}
	}
	if ev["request_cid"] != ix.RequestCID || ev["result_cid"] != ix.ResultCID {
		t.Fatalf("event %v", ev)
	}
}

// Under approve a discard capability is never held: to a caller it is not
// open to it is refused outright (CUX-F13 item 4), where any other
// capability would be queued.
func TestDiscardUnderApproveRefused(t *testing.T) {
	req, prov, _ := filesFixture(t, true)
	setPolicy(t, prov, PolicyApprove)
	if err := prov.SetPublicCapabilities([]PublicCapability{
		{ID: filesCap, Files: &twoPNG, Discard: true, AllowOnly: true},
		{ID: "plain.cap", AllowOnly: true}}); err != nil {
		t.Fatal(err)
	}
	dec := prov.decideDelegate(req.AID(), delegateForm{capID: filesCap, args: map[string]any{}})
	if dec.action != actRefuse || dec.reason != reasonNotAccepting {
		t.Fatalf("discard under approve: %+v", dec)
	}
	if dec := prov.decideDelegate(req.AID(), delegateForm{capID: "plain.cap", args: map[string]any{}}); dec.action != actHold {
		t.Fatalf("a non-discard allow_only capability under approve: %+v", dec)
	}
	if dec := prov.decideDelegate(req.AID(), delegateForm{capID: "absent.cap", args: map[string]any{}}); dec.action != actHold {
		t.Fatalf("an absent capability under approve: %+v", dec)
	}
	if held, _ := prov.ix.ListPending(); len(held) != 0 {
		t.Fatalf("%d held", len(held))
	}
}

// A call that named files and that a restart cut off is not run again (its
// bytes were in memory): it ends failed, interrupted, effect UNVERIFIED
// (CUX-F13 item 5), and a discard one leaves nothing behind.
func TestDiscardRecoveryInterrupted(t *testing.T) {
	for _, discard := range []bool{true, false} {
		req, prov, fp := filesFixture(t, discard)
		a := attOf(t, "a.png", pngBytes)
		args := argsFor(a)
		argsJSON, _ := json.Marshal(args)
		td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{
			Intent:   tsir.Intent{Summary: "s", Body: "b"},
			Requires: []tsir.Require{{ID: filesCap, Type: RequireTypeCapability, Necessity: "must"}},
			Contexts: append([]tsir.Context{{Key: "args", Value: string(argsJSON), Format: "json"}}, bindingContexts(t, req.AID())...),
		}}}
		if err := td.Sign(req.self); err != nil {
			t.Fatal(err)
		}
		doc, err := coredet.Marshal(td)
		if err != nil {
			t.Fatal(err)
		}
		cid, _ := anetcid.Sum(doc)
		ixID, _ := newInteractionID()
		er, err := newOwnIXKey(ixID, req.AID())
		if err != nil {
			t.Fatal(err)
		}
		err = prov.ix.Update(func(tx *interactions.Tx) error {
			if err := tx.Create(interactions.New{ID: ixID, Role: interactions.RoleInbound, PeerAID: req.AID(),
				Goal: filesCap, RequestCID: cid, RequestDoc: doc, Trust: interactions.TrustPublicCap, IsCapability: true}); err != nil {
				return err
			}
			if err := putOwnIXKeyTx(tx, er); err != nil {
				return err
			}
			_, err := tx.SetState(ixID, interactions.StateWorking, interactions.CauseNotTerminal)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		prov.recoverInterrupted()
		ix, err := prov.ix.Get(ixID)
		if err != nil || ix.State != interactions.StateFailed {
			t.Fatalf("discard=%v: %+v %v", discard, ix, err)
		}
		var meta map[string]any
		_ = json.Unmarshal([]byte(ix.ResultMeta), &meta)
		if meta["anet.reason"] != "interrupted" || meta["anet.effect_status"] != "UNVERIFIED" {
			t.Fatalf("discard=%v: meta %v", discard, meta)
		}
		if discard && (len(ix.RequestDoc) != 0 || len(ix.Result) != 0) {
			t.Fatalf("the interrupted discard call kept its content")
		}
		fp.mu.Lock()
		ran := fp.got != nil
		fp.mu.Unlock()
		if ran {
			t.Fatalf("discard=%v: the call ran again", discard)
		}
	}
}

// A capability call may carry raw parts (CUX-F1), not text; a "$" key in
// its args is refused before anything is written (CUX-F2).
func TestCapabilityInputTakesFilesAndRefusesReservedKeys(t *testing.T) {
	msg := func(args map[string]any, extra ...a2ashape.Part) a2ashape.Message {
		parts := []a2ashape.Part{{Kind: a2ashape.PartData, Data: map[string]any{"skill": filesCap, "args": args}}}
		return a2ashape.Message{Parts: append(parts, extra...)}
	}
	raw := a2ashape.Part{Kind: a2ashape.PartRaw, Filename: "a.png", Raw: pngBytes}
	in, err := parseTaskInput(msg(map[string]any{"k": 1}, raw, raw), a2ashape.RoleUser)
	if err != nil || in.capID != filesCap || len(in.atts) != 2 {
		t.Fatalf("%+v %v", in, err)
	}
	if _, err := parseTaskInput(msg(map[string]any{"k": 1}, a2ashape.Part{Kind: a2ashape.PartText, Text: "hi"}), a2ashape.RoleUser); err == nil {
		t.Fatal("text with a capability call was accepted")
	}
	_, err = parseTaskInput(msg(map[string]any{"$files": []any{}}), a2ashape.RoleUser)
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Code() != "args_reserved_key" {
		t.Fatalf("%v", err)
	}
}

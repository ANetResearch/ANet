package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// spyPreviewer is a Previewer that records what the kernel hands it and
// answers as set.
type spyPreviewer struct {
	previewsUnavailable
	requested []string
	arrived   int
	arriveAs  bool
}

func (s *spyPreviewer) Requested(ix, answers, mode string) {
	s.requested = append(s.requested, ix+"|"+answers+"|"+mode)
}
func (s *spyPreviewer) Arrived(previewIn) bool { s.arrived++; return s.arriveAs }

// turnPreviewOff makes d a node without the preview module.
func turnPreviewOff(t *testing.T, d *Daemon) {
	t.Helper()
	av, err := module.NewAvailability([]string{"preview"})
	if err != nil {
		t.Fatal(err)
	}
	d.avail = av
	d.setPreviewer(nil)
}

// MOD-5 (1), mutations mpv-k1 and mpv-k2: SendFrame hands a frame to the
// one-shot sender only for the task's current round, asked for previews,
// not answered, on an inbound text task that has not ended, while this
// node's stream_preview is on; the outbox is not touched.
func TestPreviewSendFrameEnforces(t *testing.T) {
	needPreviewModule(t)
	req, prov, co, clk, id, log := previewPair(t, previewModeOn)
	_ = req
	seam := previewSeam{prov}
	answers := lastPeerText(t, prov, id)
	rows0, vers0 := outboxCounts(t, prov)
	frame := func(a string) module.PreviewOut {
		return module.PreviewOut{Answers: a, Seq: clk.now(), PreviewContent: module.PreviewContent{Text: "so far", TextAt: clk.now()}}
	}
	ctx := context.Background()
	none := func(why string, o module.PreviewOut) {
		t.Helper()
		before := log.count()
		if r := seam.SendFrame(ctx, id, o); r.Sent || log.count() != before {
			t.Errorf("%s: sent %+v, %d envelopes", why, r, log.count()-before)
		}
	}
	none("a round that is not the current one", frame("another-message"))
	none("a task this node does not hold", module.PreviewOut{Answers: answers})
	if r := seam.SendFrame(ctx, "no-such-task", frame(answers)); r.Sent {
		t.Error("a task this node does not hold: sent")
	}
	// The node's setting is off: nothing goes.
	if _, err := prov.SetStreamPreview(streamPreviewOff); err != nil {
		t.Fatal(err)
	}
	none("stream_preview off", frame(answers))
	if _, err := prov.SetStreamPreview(streamPreviewOn); err != nil {
		t.Fatal(err)
	}
	// The one that is right goes once.
	if r := seam.SendFrame(ctx, id, frame(answers)); !r.Sent || log.count() != 1 {
		t.Fatalf("the current round: %+v, %d envelopes", r, log.count())
	}
	if rows, vers := outboxCounts(t, prov); rows != rows0 || vers != vers0 {
		t.Errorf("outbox %d/%d → %d/%d rows/versions", rows0, vers0, rows, vers)
	}
	// Answered: the round is canceled in the kernel's table whatever the
	// implementation does.
	prov.cancelPreviewRound(id)
	none("a round that was answered", frame(answers))
	_ = co
	// Ended: the task no longer takes frames.
	req2, prov2, _, clk2, id2, log2 := previewPair(t, previewModeOn)
	_ = req2
	if _, err := prov2.CancelTask(context.Background(), id2); err != nil {
		t.Fatal(err)
	}
	a2 := lastPeerText(t, prov2, id2)
	sent2 := log2.count() // the cancel's own envelope
	if r := (previewSeam{prov2}).SendFrame(ctx, id2, module.PreviewOut{Answers: a2, Seq: clk2.now()}); r.Sent || log2.count() != sent2 {
		t.Errorf("an ended task: %+v", r)
	}
}

// MOD-5 (1), mutation mpv-k2: the facts come before the Previewer is
// asked; a frame that fails them never reaches it.
func TestPreviewArrivedChecksFactsFirst(t *testing.T) {
	needPreviewModule(t)
	req, prov, co, clk, id, _ := previewPair(t, previewModeOn)
	spy := &spyPreviewer{arriveAs: true}
	req.setPreviewer(spy)
	answers := lastOwnText(t, req, id)
	offerAs(co, id, "first", "", previewModeOn)
	// A frame for a round it does not answer.
	env := craftPreview(t, prov, req, id, previewFrameMsg("not-the-round", clk.now(), "x"), nil)
	if r := receive(t, req, env); taken(r) || spy.arrived != 0 {
		t.Fatalf("a wrong round: taken %v, the Previewer asked %d times", taken(r), spy.arrived)
	}
	// One for the round: the Previewer is asked, and its answer is the answer.
	env = craftPreview(t, prov, req, id, previewFrameMsg(answers, clk.now()+1, "y"), nil)
	if r := receive(t, req, env); !taken(r) || spy.arrived != 1 {
		t.Fatalf("the round: taken %v, asked %d times", taken(r), spy.arrived)
	}
	spy.arriveAs = false
	env = craftPreview(t, prov, req, id, previewFrameMsg(answers, clk.now()+2, "z"), nil)
	if r := receive(t, req, env); taken(r) || spy.arrived != 2 {
		t.Fatalf("refused by the Previewer: taken %v, asked %d times", taken(r), spy.arrived)
	}
}

// MOD-5 (1), mutation M36 through the seam: the mode is normalized in the
// kernel; a value that is no mode is no request, and the Previewer is not
// told of a round.
func TestPreviewModeNormalizedInKernel(t *testing.T) {
	needPreviewModule(t)
	req, prov, _, _ := quietPairWithPreviewSpy(t)
	spy := prov.previews().(*spyPreviewer)
	id := previewDelegation(t, req, prov, "future")
	if len(spy.requested) != 0 {
		t.Fatalf(`"future" told the Previewer: %v`, spy.requested)
	}
	if _, ok := prov.previewRounds.wanted(id); ok {
		t.Error(`"future" is a requested round`)
	}
	id = previewDelegation(t, req, prov, previewModeWithReasoning)
	if len(spy.requested) != 1 || !strings.HasSuffix(spy.requested[0], "|"+previewModeWithReasoning) {
		t.Fatalf("with_reasoning: %v", spy.requested)
	}
	if m, ok := prov.previewRounds.wanted(id); !ok || m != previewModeWithReasoning {
		t.Errorf("the kernel's table: %q %v", m, ok)
	}
}

func quietPairWithPreviewSpy(t *testing.T) (req, prov *Daemon, spy *spyPreviewer, clk *previewClock) {
	t.Helper()
	_, req, prov = quietPair(t)
	spy = &spyPreviewer{}
	prov.setPreviewer(spy)
	return req, prov, spy, newPreviewClock()
}

// MOD-5 (2), TestNoPreviewDropsFrames: a node without the module drops a
// frame at step 9 and counts it preview-unsupported; nothing is written or
// answered.
func TestNoPreviewDropsFrames(t *testing.T) {
	_, req, prov := quietPair(t)
	clk := frozenClock(req)
	id := previewDelegation(t, req, prov, previewModeOn)
	answers := lastOwnText(t, req, id)
	turnPreviewOff(t, req)
	env := craftPreview(t, prov, req, id, previewFrameMsg(answers, clk.now(), "draft"), nil)
	time.Sleep(50 * time.Millisecond) // let whatever the setup started settle
	before := storeFiles(t, req)
	n := counter(req, cntPreviewUnsupported)
	if r := receive(t, req, env); taken(r) {
		t.Fatalf("taken: %+v", r)
	}
	if counter(req, cntPreviewUnsupported) != n+1 {
		t.Errorf("preview-unsupported %d → %d", n, counter(req, cntPreviewUnsupported))
	}
	if after := storeFiles(t, req); string(after[0]) != string(before[0]) || string(after[1]) != string(before[1]) {
		t.Error("the store changed")
	}
	if _, ok := req.previews().Current(id); ok {
		t.Error("a preview is shown")
	}
}

// MOD-5 (2), TestNoPreviewNeverRequests: a send on a node without the
// module carries no request for previews, and does not echo the extension.
func TestNoPreviewNeverRequests(t *testing.T) {
	r := withoutPreview(module.TaskSend{Preview: "on", Extensions: []string{module.ExtPreviewURI, "https://example.test/x"}})
	if r.Preview != "" || len(r.Extensions) != 1 || r.Extensions[0] != "https://example.test/x" {
		t.Fatalf("%+v", r)
	}
	if got := clientMeta(module.TaskSend{Preview: "on"})[metaPreviewMode]; got != "on" {
		t.Fatalf("a send with previews: %v", got)
	}
	if got := clientMeta(withoutPreview(module.TaskSend{Preview: "on"}))[metaPreviewMode]; got != nil {
		t.Fatalf("a send without the module carries %v", got)
	}
	_, req, prov := quietPair(t)
	turnPreviewOff(t, req)
	if req.TaskSeam().PreviewAvailable() {
		t.Error("PreviewAvailable on a node without the module")
	}
	_ = prov
}

// MOD-5 (2), TestNoPreviewCards: the card contributions, with stream_preview
// on in the file, never declare the extension while the module is off.
func TestNoPreviewCards(t *testing.T) {
	needPreviewModule(t)
	d := newTestDaemon(t, "", true)
	d.streamPreviewFn = func() string { return "on" }
	if exts, _ := d.cardContributions(module.CardContext{}); len(exts) != 1 {
		t.Fatalf("with previews: %v", exts)
	}
	turnPreviewOff(t, d)
	if exts, _ := d.cardContributions(module.CardContext{}); len(exts) != 0 {
		t.Fatalf("without the module: %v", exts)
	}
}

// MOD-5 (2), TestStreamPreviewNeedsModule: stream_preview on with the module
// turned off is a configuration the daemon refuses to start on (MOD-F1), and
// the control plane's route answers module_absent (TestTasksPreviewWithoutModule).
func TestStreamPreviewNeedsModule(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": []string{"preview"}, "stream_preview": "on"})
	d, err := New(NewLayout(root))
	if err == nil {
		d.Close()
		t.Fatal("started with stream_preview on and no preview module")
	}
	if !strings.Contains(err.Error(), "preview") {
		t.Errorf("the refusal does not name the module: %v", err)
	}
	// With the setting off the node starts, has no previews and no status member.
	root = t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": []string{"preview"}})
	d, err = New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.previewAvailable() || d.TaskSeam().PreviewAvailable() {
		t.Error("previews are available")
	}
	if ref := d.avail.Absent("preview", ""); ref == nil {
		t.Error("no module_absent refusal for preview")
	} else if s := a2ashape.Sentence(ref.Key, a2ashape.English, ref.Args); !strings.Contains(s, "preview") {
		t.Errorf("the sentence does not name the module: %q", s)
	}
}

// With the module available the Previewer is the node's, and the seam is
// granted to the preview Host alone.
func TestPreviewHostIsGrantedTheSeam(t *testing.T) {
	needPreviewModule(t)
	d := newTestDaemon(t, "", true)
	h, _ := newModuleHost(d, "preview")
	ph, ok := h.(module.PreviewHost)
	if !ok {
		t.Fatal("the preview Host has no PreviewHost")
	}
	if s, ok := ph.PreviewSeam(); !ok || s == nil {
		t.Fatal("no seam")
	}
	for _, info := range module.Known() {
		if info.Name == "preview" {
			continue
		}
		h, _ := newModuleHost(d, info.Name)
		if _, ok := h.(module.PreviewHost); ok {
			t.Errorf("%s is granted PreviewHost", info.Name)
		}
	}
	turnPreviewOff(t, d)
	if s, ok := ph.PreviewSeam(); ok || s != nil {
		t.Error("a seam on a node without the module")
	}
}

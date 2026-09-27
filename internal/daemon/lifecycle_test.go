package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// Two modules that exist only in this test binary. Both stay off unless a
// test configures them (newTestDaemonCfg), so every other test daemon
// builds without them.
//
//   - lifecycle_rec records what the kernel did to it: when it was
//     stopped, whether its context had ended by then, and whether the
//     evidence ledger still took a record at that moment.
//   - lifecycle_fail refuses to start. It registers after lifecycle_rec,
//     so a daemon configured with both has one module up when the second
//     fails.
func init() {
	module.Register("lifecycle_rec", func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		var c struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &c); err != nil || c.ID == "" {
			return nil, errors.New("lifecycle_rec: config needs an id")
		}
		m := &recModule{id: c.ID}
		recModules.Store(c.ID, m)
		return m, nil
	})
	module.Register("lifecycle_fail", func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		return failModule{}, nil
	})
}

// recModules maps a configured id to the module built for it.
var recModules sync.Map // id -> *recModule

type recModule struct {
	id string

	mu       sync.Mutex
	host     module.Host
	startCtx context.Context
	stops    int
	// ctxDoneAtStop is whether the Start context had ended when Stop ran;
	// evidenceAtStop is RecordEvidence's answer from inside Stop.
	ctxDoneAtStop  bool
	evidenceAtStop error
}

func (m *recModule) Name() string { return "lifecycle_rec" }

func (m *recModule) Start(ctx context.Context, h module.Host) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.host, m.startCtx = h, ctx
	return nil
}

func (m *recModule) Stop(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops++
	m.ctxDoneAtStop = m.startCtx != nil && m.startCtx.Err() != nil
	m.evidenceAtStop = m.host.RecordEvidence("test.module.stop", map[string]any{"id": m.id})
	return nil
}

// ForbiddenTokens makes lifecycle_rec a Confidential module: its secret
// must never reach a publication, before Close or after.
func (m *recModule) ForbiddenTokens() []string { return []string{m.secret()} }

func (m *recModule) secret() string { return "lifecycle-secret-" + m.id }

func (m *recModule) snapshot() (stops int, ctxDone bool, evidence error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stops, m.ctxDoneAtStop, m.evidenceAtStop
}

type failModule struct{}

func (failModule) Name() string { return "lifecycle_fail" }
func (failModule) Start(context.Context, module.Host) error {
	return errors.New("lifecycle_fail: refusing to start")
}
func (failModule) Stop(context.Context) error { return nil }

// recConfig is the modules block that starts a lifecycle_rec for this
// test, and the id to find it by.
func recConfig(t *testing.T) (map[string]any, string) {
	id := t.Name()
	return map[string]any{"lifecycle_rec": map[string]any{"id": id}}, id
}

func recFor(t *testing.T, id string) *recModule {
	t.Helper()
	v, ok := recModules.Load(id)
	if !ok {
		t.Fatalf("no lifecycle_rec module was built for %s", id)
	}
	t.Cleanup(func() { recModules.Delete(id) })
	return v.(*recModule)
}

// openFilesUnder lists this process's open file descriptors that point
// under root. Linux only; elsewhere the test skips.
func openFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot list open files here: %v", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue // closed between the listing and the read
		}
		if strings.HasPrefix(target, root+string(os.PathSeparator)) {
			out = append(out, target)
		}
	}
	return out
}

// Close stops every module, after the daemon's context is cancelled and
// before the evidence ledger is closed — a module may record on its way
// out — and only once, however often Close is called.
func TestCloseStopsTheModules(t *testing.T) {
	mods, id := recConfig(t)
	d := newTestDaemonCfg(t, "", mods)
	m := recFor(t, id)
	if stops, _, _ := m.snapshot(); stops != 0 {
		t.Fatalf("the module was stopped %d times before Close", stops)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	stops, ctxDone, evidence := m.snapshot()
	if stops != 1 {
		t.Fatalf("Close stopped the module %d times, want 1", stops)
	}
	if !ctxDone {
		t.Error("the module was stopped while its Start context was still live")
	}
	if evidence != nil {
		t.Errorf("the ledger was closed before the module stopped: %v", evidence)
	}
	_ = d.Close()
	if stops, _, _ := m.snapshot(); stops != 1 {
		t.Fatalf("a second Close stopped the module again (%d stops)", stops)
	}
}

// Stopping the modules does not take them out of the publication screen.
// A control request can outlive the server's shutdown and publish while
// Close runs, or after it; that publication still meets every module's
// confidential tokens (INV-2), and reading the module list meanwhile is
// not a data race.
func TestClosingLeavesThePublicationScreenInPlace(t *testing.T) {
	mods, id := recConfig(t)
	d := newTestDaemonCfg(t, "", mods)
	m := recFor(t, id)
	body := map[string]any{"name": "node " + m.secret()}
	if err := d.screenPublication("a test body", body); err == nil {
		t.Fatal("the module's secret was not screened while the daemon ran")
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if d.screenPublication("a test body", body) == nil {
				t.Error("the module's secret passed the screen while the daemon closed")
				return
			}
		}
	}()
	_ = d.Close()
	close(stop)
	<-done
	if stops, _, _ := m.snapshot(); stops != 1 {
		t.Fatalf("Close stopped the module %d times, want 1", stops)
	}
	if err := d.screenPublication("a test body", body); err == nil {
		t.Fatal("after Close the module's secret passes the screen")
	}
}

// A start that fails after a module came up stops that module and
// releases the store and the ledger; the module that failed is not
// stopped, it cleans up after itself.
func TestAFailedModuleStartReleasesWhatWasOpened(t *testing.T) {
	root := t.TempDir()
	mods, id := recConfig(t)
	mods["lifecycle_fail"] = map[string]any{}
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "modules": mods})
	d, err := New(NewLayout(root))
	if err == nil {
		d.Close()
		t.Fatal("a daemon with a module that cannot start came up")
	}
	m := recFor(t, id)
	stops, ctxDone, evidence := m.snapshot()
	if stops != 1 {
		t.Fatalf("the module that did start was stopped %d times, want 1", stops)
	}
	if !ctxDone {
		t.Error("the daemon context was still live when the started module was stopped")
	}
	if evidence != nil {
		t.Errorf("the ledger was closed before the started module stopped: %v", evidence)
	}
	if open := openFilesUnder(t, root); len(open) > 0 {
		t.Fatalf("a failed start left files open: %v", open)
	}
}

// A ledger that cannot be opened fails the start, and the interaction
// store opened before it is closed again.
func TestAFailedLedgerOpenClosesTheStore(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0"})
	// A directory where the ledger file goes: reading it fails.
	if err := os.Mkdir(NewLayout(root).EvidenceLedgerPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := New(NewLayout(root))
	if err == nil {
		d.Close()
		t.Fatal("a daemon whose ledger cannot be opened came up")
	}
	if !strings.Contains(err.Error(), filepath.Base(NewLayout(root).EvidenceLedgerPath())) {
		t.Fatalf("the start failed somewhere other than the ledger: %v", err)
	}
	if open := openFilesUnder(t, root); len(open) > 0 {
		t.Fatalf("a failed start left files open: %v", open)
	}
}

func writeTestConfig(t *testing.T, root string, cfg map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

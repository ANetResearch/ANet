package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// directHookWrite is a test writing the clock or the receive fault
// without the setters: an assignment, or a Store, Swap or CompareAndSwap
// on the atomic field.
var directHookWrite = regexp.MustCompile(`\.(clock|rxFault)\s*(=([^=]|$)|\.(Store|Swap|CompareAndSwap)\()`)

// No test sets a hook on a running daemon except through setClock and
// setRxFault (testhooks.go). The daemon's loops read both while the test
// runs, so anything else is the data race `go test -race` found in
// TestTheKeyRingRotatesAndRetires; the compiler refuses a plain
// assignment to the atomic field, and this catches the other shapes.
func TestNoTestAssignsAHookDirectly(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no test files found: the scan would pass vacuously")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if directHookWrite.MatchString(line) {
				t.Errorf("%s:%d sets a test hook directly; use setClock/setRxFault: %s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The setters are what the hooks read back, and nil restores the default.
func TestTheHookSettersRoundTrip(t *testing.T) {
	d := &Daemon{}
	if d.testClock() != nil || d.testRxFault() != nil {
		t.Fatal("a fresh daemon has a hook set")
	}
	d.setClock(func() uint64 { return 42 })
	if got := d.nowMS(); got != 42 {
		t.Fatalf("nowMS = %d with the clock set to 42", got)
	}
	d.setClock(nil)
	if got := d.nowMS(); got == 42 {
		t.Fatal("setClock(nil) left the test clock in place")
	}
	d.setRxFault(func(string) error { return os.ErrClosed })
	if f := d.testRxFault(); f == nil || f("x") != os.ErrClosed {
		t.Fatal("setRxFault did not install the fault")
	}
	d.setRxFault(nil)
	if d.testRxFault() != nil {
		t.Fatal("setRxFault(nil) left the fault in place")
	}
}

// The scan's pattern, on the shapes it must catch and the ones it must
// leave alone — so the scan cannot pass by matching nothing. The shapes
// it must catch are split at the dot, or the scan would find them here.
func TestTheHookScanCatchesEveryShape(t *testing.T) {
	for _, line := range []string{
		"\td." + "clock = func() uint64 { return 1 }",
		"\tprov." + "rxFault = nil",
		"\td." + "clock =",
		"\td." + "clock.Store(&f)",
		"\treq." + "rxFault.Swap(nil)",
	} {
		if !directHookWrite.MatchString(line) {
			t.Errorf("not caught: %q", line)
		}
	}
	for _, line := range []string{
		"\td.setClock(clock.Load)",
		"\tprov.setRxFault(nil)",
		"\tclock.Store(start + 6*dayMS)",
		"\tif d.clock == nil {",
		"\tif fault := d.testRxFault(); fault != nil {",
	} {
		if directHookWrite.MatchString(line) {
			t.Errorf("caught by mistake: %q", line)
		}
	}
}

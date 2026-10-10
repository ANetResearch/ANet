package daemon

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
)

// crashOnceAt makes d's orchestration stop the first time it reaches point,
// as a crash there would.
func crashOnceAt(d *Daemon, point string) *atomic.Bool {
	var hit atomic.Bool
	d.idOps.mu.Lock()
	d.idOps.crash = func(p string) bool { return p == point && hit.CompareAndSwap(false, true) }
	d.idOps.mu.Unlock()
	return &hit
}

// restart stops d — whatever it holds in memory is lost, as in a crash, and
// only the data directory stays — and starts a daemon on its data
// directory.
func restart(t *testing.T, d *Daemon) *Daemon {
	t.Helper()
	_ = d.Close()
	n, err := New(d.layout)
	if err != nil {
		t.Fatalf("the restart after the crash: %v", err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

// TestIdentityOpsCrashMatrix (KEL D-35, engine CR11, S-6): at every point
// where the orchestration writes something, a crash and a restart converge
// on what the run without a crash ends with — for the witness designation,
// a planned and an emergency rotation (E0–E5), two emergency rotations in a
// row with a crash between them (D-16), the deactivation and the stop.
func TestIdentityOpsCrashMatrix(t *testing.T) {
	noCheckLimit(t)
	type op struct {
		name   string
		points []string
		run    func(t *testing.T, d *Daemon, hubURL string)
		check  func(t *testing.T, d *Daemon)
	}
	rotate := func(mode string, revealed, next int) func(*testing.T, *Daemon, string) {
		return func(t *testing.T, d *Daemon, _ string) {
			t.Helper()
			if code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate", rotateBody(mode, revealed, next, "")); code != http.StatusOK {
				t.Fatalf("rotate: %d %v", code, out)
			}
		}
	}
	tipIs := func(seq uint64, emergency bool) func(*testing.T, *Daemon) {
		return func(t *testing.T, d *Daemon) {
			t.Helper()
			kel := d.self.Accepted()
			if tip := kel[len(kel)-1].Event; tip.Seq != seq || tip.Emergency != emergency {
				t.Fatalf("the tip after the restart: seq %d emergency %v", tip.Seq, tip.Emergency)
			}
		}
	}
	modes := func(want ...string) func(*testing.T, *Daemon) {
		return func(t *testing.T, d *Daemon) {
			t.Helper()
			got := rotations(t, d)
			if len(got) != len(want) {
				t.Fatalf("anet.identity.rotated after the restart: %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("anet.identity.rotated after the restart: %v, want %v", got, want)
				}
			}
		}
	}
	ops := []op{
		{name: "planned", points: []string{"rot-saved", "rot-accepted"},
			run: rotate(rotateScheduled, 2, 3),
			check: func(t *testing.T, d *Daemon) {
				waitIdentity(t, d, IdentityOK)
				tipIs(2, false)(t, d)
				modes(rotationWitness, rotationScheduled)(t, d)
			}},
		{name: "emergency", points: []string{"rot-saved", "rot-accepted", "E1", "E2-regenerated", "E2-published", "E3"},
			run: rotate(rotateEmergency, 2, 3),
			check: func(t *testing.T, d *Daemon) {
				waitIdentity(t, d, IdentityOK)
				tipIs(2, true)(t, d)
				modes(rotationWitness, rotationEmergency)(t, d)
				if !d.enc.regeneratedFor(2) || d.keysHeld() {
					t.Fatal("E2 did not converge")
				}
			}},
		{name: "two emergencies", points: []string{"rot-saved", "rot-accepted", "E1", "E3"},
			run: func(t *testing.T, d *Daemon, hub string) {
				// The first runs through (no crash point is hit before the
				// second one's: the hook lets the first pass).
				d.idOps.mu.Lock()
				crash := d.idOps.crash
				d.idOps.crash = nil
				d.idOps.mu.Unlock()
				rotate(rotateEmergency, 2, 3)(t, d, hub)
				waitIdentity(t, d, IdentityOK)
				d.idOps.mu.Lock()
				d.idOps.crash = crash
				d.idOps.mu.Unlock()
				rotate(rotateEmergency, 3, 4)(t, d, hub)
			},
			check: func(t *testing.T, d *Daemon) {
				waitIdentity(t, d, IdentityOK)
				tipIs(3, true)(t, d)
				modes(rotationWitness, rotationEmergency, rotationEmergency)(t, d)
			}},
		{name: "deactivation", points: []string{"dip-saved", "dip-closed", "dip-keys-deleted"},
			run: func(t *testing.T, d *Daemon, _ string) {
				controlCall(t, d.hIdentityDeactivate, "/identity/deactivate", `{"revealed_seed":"`+b64std(kelSeed(2))+`"}`)
			},
			check: func(t *testing.T, d *Daemon) {
				waitIdentity(t, d, IdentityDeactivated)
				kel := d.self.Accepted()
				if kel[len(kel)-1].Event.Type != identity.Deactivation {
					t.Fatal("the dip is not the tip after the restart")
				}
				if len(d.enc.keys) != 0 {
					t.Fatal("epoch keys survived the deactivation")
				}
				waitUntil(t, "the witness confirmed the dip", func() bool { return len(d.self.WitnessesToPush()) == 0 })
			}},
		{name: "stop", points: []string{"stopped-saved"},
			run: func(t *testing.T, d *Daemon, _ string) { d.enterStopped("test") },
			check: func(t *testing.T, d *Daemon) {
				waitIdentity(t, d, IdentityStopped)
				if n := interactionsCountOpen(t, d); n != 0 {
					t.Fatalf("%d tasks open after the restart of a stopped identity", n)
				}
			}},
	}
	for _, o := range ops {
		for _, point := range o.points {
			t.Run(o.name+"/"+point, func(t *testing.T) {
				_, d, prov := opsPair(t)
				if _, err := d.Delegate(context.Background(), prov.AID(), "in flight", nil); err != nil {
					t.Fatal(err)
				}
				hit := crashOnceAt(d, point)
				o.run(t, d, d.config().HubURL)
				if !hit.Load() {
					t.Fatalf("the crash point %s was not reached", point)
				}
				n := restart(t, d)
				o.check(t, n)
			})
		}
	}

	// The witness designation.
	for _, point := range []string{"designation-saved", "designation-accepted"} {
		t.Run("designation/"+point, func(t *testing.T) {
			srv := newFakeHub(t)
			d := opsDaemon(t)
			hit := crashOnceAt(d, point)
			opsJoin(t, d, srv.URL)
			if !hit.Load() {
				t.Fatalf("the crash point %s was not reached", point)
			}
			n := restart(t, d)
			waitJoinState(t, n, JoinRegistered)
			waitIdentity(t, n, IdentityOK)
			if kel := n.self.Accepted(); len(kel) != 2 {
				t.Fatalf("%d events after the restart", len(kel))
			}
			modes(rotationWitness)(t, n)
		})
	}
}

// FS-4 §3 item 6, KEL-9: key sets are held from the emergency rot's
// building until E2 regenerated the ring for it — read from the identity
// file and the ring, so it holds across a crash — and not after E5,
// whatever a later regeneration tags the ring with.
func TestKeysHeldUntilE2(t *testing.T) {
	d, _, _ := opsNode(t)
	waitIdentity(t, d, IdentityOK)
	if d.keysHeld() {
		t.Fatal("key sets held with no emergency rotation")
	}
	hit := crashOnceAt(d, "E1")
	if code, out := controlCall(t, d.hIdentityRotate, "/identity/rotate", rotateBody(rotateEmergency, 2, 3, "")); code != http.StatusOK {
		t.Fatalf("emergency rotation: %d %v", code, out)
	}
	if !hit.Load() || !d.keysHeld() {
		t.Fatal("key sets are not held between E1 and E2")
	}
	if g := d.identityGate(); g.Open || g.State != IdentityPublishing {
		t.Fatalf("the gate between E1 and E5: %+v, want closed, publishing (core §4.7)", g)
	}
	n := restart(t, d)
	waitIdentity(t, n, IdentityOK)
	if n.keysHeld() {
		t.Fatal("key sets still held after E5")
	}
	// A later regeneration with another tag (a local state rollback) does
	// not hold them again.
	if _, err := n.enc.regenerate(99, 0, n.ringInputsAt(n.nowMS())); err != nil {
		t.Fatal(err)
	}
	if n.keysHeld() {
		t.Fatal("a regeneration after E5 held the key sets again")
	}
}

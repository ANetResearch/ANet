//go:build linux

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Regression tests for F20 (red team, si7): /pull reused an anet-<ix> subdirectory that already existed
// without checking its owner or mode, and then wrote each file by full path, so another local user could
// prepare the directory in a shared out_dir, or swap it for a symbolic link between the check and the
// writes and have peer-named files land in any directory the daemon can write (skeptic PoCs
// TestSkeptic1F20PreexistingWorldWritableSubdirAccepted and TestSkeptic1F20SubdirSwapRedirectsWrites).
// A2A-DESIGN §7.7 [redteam:F20].

// A subdirectory someone else can write into is refused, and nothing is written in it.
func TestPullRefusesAPreparedSubdirOthersCanWrite(t *testing.T) {
	out := t.TempDir()
	sub := filepath.Join(out, "anet-ix_aaaabbbbc")
	if err := os.Mkdir(sub, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(sub, 0o777)
	if _, err := pullInto(out, "ix_aaaabbbbcccc", []*interactions.Attachment{pullAtt("report.md", []byte("peer content"))}); err == nil ||
		!strings.Contains(err.Error(), "writable by others") {
		t.Fatalf("pull into a world-writable prepared subdir: %v", err)
	}
	if ents, _ := os.ReadDir(sub); len(ents) != 0 {
		t.Fatalf("wrote %d file(s) into it", len(ents))
	}
}

// One of another uid is refused too. A test cannot create a directory as another user; the rule is
// checked on this user's directory against another uid.
func TestPullSubdirMustBelongToThisUser(t *testing.T) {
	d := t.TempDir()
	_ = os.Chmod(d, 0o700)
	fi, err := os.Lstat(d)
	if err != nil {
		t.Fatal(err)
	}
	if why := pullDirTrouble(fi, os.Getuid()); why != "" {
		t.Fatalf("own private directory refused: %s", why)
	}
	if why := pullDirTrouble(fi, os.Getuid()+1); !strings.Contains(why, "belongs to uid") {
		t.Fatalf("another user's directory: %q", why)
	}
	link := filepath.Join(t.TempDir(), "l")
	_ = os.Symlink(d, link)
	if li, err := os.Lstat(link); err == nil && pullDirTrouble(li, os.Getuid()) == "" {
		t.Fatal("a symbolic link qualified")
	}
}

const sysRenameat2 = 316 // amd64

func renameExchange(a, b string) error {
	pa, _ := syscall.BytePtrFromString(a)
	pb, _ := syscall.BytePtrFromString(b)
	atFdcwd, exchange := -100, 2
	_, _, e := syscall.Syscall6(sysRenameat2, uintptr(atFdcwd), uintptr(unsafe.Pointer(pa)), uintptr(atFdcwd), uintptr(unsafe.Pointer(pb)), uintptr(exchange), 0)
	if e != 0 {
		return e
	}
	return nil
}

// The swap race: another user keeps exchanging the subdirectory entry with a symbolic link to a
// directory of the victim's while pulls run. Writes relative to the opened directory never follow it.
// (Before the fix the first pull already put files in the victim's directory.)
func TestPullWritesAreNotRedirectedBySwappingTheSubdir(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("renameat2 syscall number is amd64's")
	}
	out := t.TempDir()
	victimDir := filepath.Join(out, "victim") // inside out_dir, so even a root-confined open could reach it
	if err := os.Mkdir(victimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ix := "ix_ddddeeeeffff0000"
	sub := filepath.Join(out, "anet-"+prefix(ix, 12))
	alt := filepath.Join(out, "attacker-alt")
	if err := os.Mkdir(sub, 0o700); err != nil { // as the victim's own earlier pull left it
		t.Fatal(err)
	}
	if err := os.Symlink(victimDir, alt); err != nil {
		t.Fatal(err)
	}
	if err := renameExchange(sub, alt); err != nil {
		t.Skipf("renameat2(RENAME_EXCHANGE): %v", err)
	}
	_ = renameExchange(sub, alt)
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = renameExchange(sub, alt)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	pulls, wrote := 0, 0
	for time.Now().Before(deadline) {
		pulls++
		atts := make([]*interactions.Attachment, 0, 8)
		for i := 0; i < 8; i++ {
			atts = append(atts, pullAtt(fmt.Sprintf("evil-%d-%d.desktop", pulls, i), []byte(fmt.Sprintf("peer %d %d", pulls, i))))
		}
		if res, err := pullInto(out, ix, atts); err == nil {
			wrote += len(res)
		}
		if ents, _ := os.ReadDir(victimDir); len(ents) > 0 {
			stop.Store(true)
			<-done
			t.Fatalf("after %d pulls, %d peer-named file(s) landed in the victim's directory, e.g. %s", pulls, len(ents), ents[0].Name())
		}
	}
	stop.Store(true)
	<-done
	t.Logf("%d pulls under a swap race, %d files written, none redirected", pulls, wrote)
}

// The same race one level up [redteam:F20, second pass]. out_dir itself lies in a directory another user
// can write (a group project directory, say): Pull resolves and checks it — not the data dir, not the
// exec work dir — and then opened it by path, so an entry swapped for a symbolic link after the check
// was followed, and the anet-<ix> subdirectory and the peer's files were created wherever it pointed,
// the data dir included. The out_dir is now opened one component at a time, none followed as a link.
func TestPullOutDirIsNotRedirectedBySwappingAComponent(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("renameat2 syscall number is amd64's")
	}
	shared := t.TempDir()
	_ = os.Chmod(shared, 0o777) // writable by the other user
	proj := filepath.Join(shared, "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	forbidden := filepath.Join(t.TempDir(), "data") // the data dir, which /pull never writes into
	if err := os.Mkdir(forbidden, 0o700); err != nil {
		t.Fatal(err)
	}
	alt := filepath.Join(shared, "alt")
	if err := os.Symlink(forbidden, alt); err != nil {
		t.Fatal(err)
	}
	if err := renameExchange(proj, alt); err != nil {
		t.Skipf("renameat2(RENAME_EXCHANGE): %v", err)
	}
	_ = renameExchange(proj, alt)
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = renameExchange(proj, alt)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	pulls, wrote := 0, 0
	for time.Now().Before(deadline) {
		pulls++
		ix := fmt.Sprintf("ix_%04d_race", pulls)
		// proj is what checkPullOutDir resolved and checked; the swap happens after.
		if res, err := pullInto(proj, ix, []*interactions.Attachment{pullAtt("config.json", []byte(fmt.Sprintf("peer %d", pulls)))}); err == nil {
			wrote += len(res)
		}
		if ents, _ := os.ReadDir(forbidden); len(ents) > 0 {
			stop.Store(true)
			<-done
			t.Fatalf("after %d pulls, %s was created inside the directory the out_dir check excluded", pulls, ents[0].Name())
		}
	}
	stop.Store(true)
	<-done
	if wrote == 0 {
		t.Fatalf("no pull succeeded in %d tries: the defence must not refuse an out_dir nobody touched", pulls)
	}
	t.Logf("%d pulls under a swap race, %d files written, none redirected", pulls, wrote)
}

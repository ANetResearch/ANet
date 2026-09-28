//go:build !no_service

package service

// token_redteam_test.go — adversarial review of the service module's
// token_file permission gate (A2A-DESIGN §6, §15; lens=official).
//
// The invariant the module states (service.go, Config.TokenFile doc and the
// package comment): the bearer token the daemon presents to a loopback
// backend "must not be readable by other users", because a loopback port is
// reachable by every process on the host — without the token a backend
// cannot tell the daemon from anything else that can open a socket, and
// X-ANet-Caller becomes a header anyone can write.
//
// The check is `fi.Mode().Perm()&0o007 != 0`, which inspects the "other"
// bits only. A group-readable token file (0640/0660) passes it, so a second
// account in the file's group reads the shared daemon↔backend secret and can
// then call the loopback backend directly, presenting whatever X-ANet-Caller
// it likes and bypassing the daemon's admission, quotas and payment.
//
// The test asserts the attack succeeds (a 0640 token file is accepted),
// so the test passing = the gap exists.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGroupReadableTokenFileIsAccepted_redteam(t *testing.T) {
	dir := t.TempDir()
	// A directory 0700 so the fixture is realistic; the file itself is what
	// the check looks at.
	path := filepath.Join(dir, "token")
	const token = "0123456789abcdef0123456789abcdef" // >= minTokenBytes, no whitespace
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 0640: owner rw, GROUP r, other none. A user in the file's group can
	// read the token; the stated invariant says it "must not be readable by
	// other users".
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o040 == 0 {
		t.Fatalf("fixture is not group-readable (mode %04o); the OS or umask changed it", fi.Mode().Perm())
	}

	got, err := readToken(path)
	// ATTACK SUCCEEDS: readToken accepts a group-readable token file, so a
	// second account in the group can obtain the daemon↔backend secret.
	if err != nil {
		t.Fatalf("readToken rejected the group-readable file (%v) — the gap this test documents is closed", err)
	}
	if got != token {
		t.Fatalf("readToken returned %q, want the token", got)
	}
	t.Logf("gap confirmed: a group-readable (0640) token file is accepted; "+
		"the check inspects only the 'other' bits (&0o007), not group (&0o070). mode=%04o", fi.Mode().Perm())
}

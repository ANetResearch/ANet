package fsperm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

// testSID builds a binary SID from "S-1-<authority>-<sub>…" (decimal authority).
func testSID(t *testing.T, s string) []byte {
	t.Helper()
	parts := strings.Split(s, "-")
	if len(parts) < 3 || parts[0] != "S" || parts[1] != "1" {
		t.Fatalf("bad SID %q", s)
	}
	auth, err := strconv.ParseUint(parts[2], 10, 48)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte{1, byte(len(parts) - 3), 0, 0, 0, 0, 0, 0}
	for i := 0; i < 6; i++ {
		b[7-i] = byte(auth >> (8 * i))
	}
	for _, p := range parts[3:] {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		b = binary.LittleEndian.AppendUint32(b, uint32(v))
	}
	return b
}

// testACE encodes one ACE; body is everything after the 4-byte header.
func testACE(typ, flags byte, body ...[]byte) []byte {
	b := []byte{typ, flags, 0, 0}
	for _, p := range body {
		b = append(b, p...)
	}
	binary.LittleEndian.PutUint16(b[2:4], uint16(len(b)))
	return b
}

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

// testACL encodes an ACL holding aces.
func testACL(aces ...[]byte) []byte {
	b := []byte{2, 0, 0, 0, byte(len(aces)), 0, 0, 0}
	for _, a := range aces {
		b = append(b, a...)
	}
	binary.LittleEndian.PutUint16(b[2:4], uint16(len(b)))
	return b
}

const (
	maskFA = 0x1F01FF // FILE_ALL_ACCESS
	maskFR = 0x120089 // FILE_GENERIC_READ
	maskFW = 0x120116 // FILE_GENERIC_WRITE
)

func TestSIDString(t *testing.T) {
	for want, sid := range map[string][]byte{
		"S-1-1-0": sidEveryone, "S-1-3-0": sidCreatorOwner, "S-1-3-4": sidOwnerRights,
		"S-1-5-18": sidLocalSystem, "S-1-5-32-544": sidAdmins,
	} {
		if got := sidString(sid); got != want {
			t.Errorf("sidString = %q, want %q", got, want)
		}
		if !bytes.Equal(testSID(t, want), sid) {
			t.Errorf("%s: constant does not match its string form", want)
		}
	}
	user := testSID(t, "S-1-5-21-1004336348-1177238915-682003330-1001")
	if got := sidString(user); got != "S-1-5-21-1004336348-1177238915-682003330-1001" {
		t.Errorf("user SID: %q", got)
	}
	big := []byte{1, 1, 0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC, 7, 0, 0, 0}
	if got := sidString(big); got != "S-1-0x123456789ABC-7" {
		t.Errorf("large authority: %q", got)
	}
	if got := sidString([]byte{1, 2, 0, 0, 0, 0, 0, 5, 1, 0, 0, 0}); got != "(malformed SID)" {
		t.Errorf("short SID: %q", got)
	}
}

func TestScanACL(t *testing.T) {
	user := testSID(t, "S-1-5-21-1-2-3-1001")
	other := testSID(t, "S-1-5-21-1-2-3-1002")
	users := testSID(t, "S-1-5-32-545")
	trusted := func(sid []byte) bool {
		return sidIn(sid, user, sidLocalSystem, sidAdmins, sidOwnerRights, sidCreatorOwner)
	}
	guid := bytes.Repeat([]byte{0xAB}, 16)
	private := testACL(
		testACE(aceAllowed, 0x3, le32(maskFA), user),
		testACE(aceAllowed, 0x3, le32(maskFA), sidLocalSystem),
		testACE(aceAllowed, 0x3, le32(maskFA), sidAdmins),
		testACE(aceAllowed, 0xB, le32(maskFA), sidCreatorOwner),
		testACE(aceAllowed, 0x0, le32(maskFA), sidOwnerRights),
	)
	base := func(extra ...[]byte) []byte {
		aces := [][]byte{testACE(aceAllowed, 0x3, le32(maskFA), user), testACE(aceAllowed, 0x3, le32(maskFA), sidLocalSystem)}
		return testACL(append(aces, extra...)...)
	}
	for _, c := range []struct {
		name        string
		acl         []byte
		read, write bool
		why         string
	}{
		{"trusted principals only", private, false, false, ""},
		{"everyone may read", base(testACE(aceAllowed, 0, le32(maskFR), sidEveryone)), true, false, "access granted to S-1-1-0"},
		{"users may write", base(testACE(aceAllowed, 0, le32(maskFW), users)), true, true, "access granted to S-1-5-32-545"},
		{"generic all", base(testACE(aceAllowed, 0, le32(0x10000000), other)), true, true, "access granted to S-1-5-21-1-2-3-1002"},
		{"maximum allowed", base(testACE(aceAllowed, 0, le32(0x02000000), other)), true, true, "access granted to S-1-5-21-1-2-3-1002"},
		{"delete only", base(testACE(aceAllowed, 0, le32(0x00010000), other)), true, true, "access granted to S-1-5-21-1-2-3-1002"},
		{"read control only", base(testACE(aceAllowed, 0, le32(0x00020000), other)), true, false, "access granted to S-1-5-21-1-2-3-1002"},
		{"empty mask grants nothing", base(testACE(aceAllowed, 0, le32(0), other)), false, false, ""},
		{"deny entries are ignored", base(testACE(aceDenied, 0, le32(maskFA), sidEveryone)), false, false, ""},
		{"inherit-only counts", base(testACE(aceAllowed, 0xB, le32(maskFR), users)), true, false, "access granted to S-1-5-32-545"},
		{"callback allow counts, application data after the SID", base(testACE(aceAllowedCallback, 0, le32(maskFR), other, []byte("app data"))), true, false, "access granted to S-1-5-21-1-2-3-1002"},
		{"callback deny is ignored", base(testACE(aceDeniedCallback, 0, le32(maskFA), other)), false, false, ""},
		{"object allow with both GUIDs", base(testACE(aceAllowedObject, 0, le32(maskFW), le32(3), guid, guid, other)), true, true, "access granted to S-1-5-21-1-2-3-1002"},
		{"object allow without GUIDs", base(testACE(aceAllowedObject, 0, le32(maskFR), le32(0), other)), true, false, "access granted to S-1-5-21-1-2-3-1002"},
		{"callback object allow, inherited type only", base(testACE(aceAllowedCallbackObject, 0, le32(maskFR), le32(2), guid, users)), true, false, "access granted to S-1-5-32-545"},
		{"trusted object allow", base(testACE(aceAllowedObject, 0, le32(maskFA), le32(1), guid, user)), false, false, ""},
		{"unknown type fails closed", base(testACE(0x11, 0, le32(0), sidEveryone)), true, true, "access control entry of unsupported type 17"},
		{"audit entry fails closed", base(testACE(0x2, 0, le32(maskFA), sidEveryone)), true, true, "access control entry of unsupported type 2"},
		{"first offender is named", base(testACE(aceAllowed, 0, le32(maskFR), users), testACE(aceAllowed, 0, le32(maskFW), other)), true, true, "access granted to S-1-5-32-545"},
	} {
		got, err := scanACL(c.acl, trusted)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.othersRead != c.read || got.othersWrite != c.write || got.why != c.why {
			t.Errorf("%s: got read=%v write=%v why=%q, want read=%v write=%v why=%q",
				c.name, got.othersRead, got.othersWrite, got.why, c.read, c.write, c.why)
		}
	}
}

func TestScanACLRejectsMalformed(t *testing.T) {
	user := testSID(t, "S-1-5-21-1-2-3-1001")
	good := testACL(testACE(aceAllowed, 0, le32(maskFA), user))
	trusted := func([]byte) bool { return true }
	if _, err := scanACL(good, trusted); err != nil {
		t.Fatalf("well-formed ACL: %v", err)
	}
	mut := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(good)) }
	for name, b := range map[string][]byte{
		"empty":            nil,
		"short header":     good[:6],
		"size below 8":     mut(func(b []byte) []byte { binary.LittleEndian.PutUint16(b[2:4], 4); return b }),
		"size beyond data": mut(func(b []byte) []byte { binary.LittleEndian.PutUint16(b[2:4], uint16(len(b)+4)); return b }),
		"count beyond data": mut(func(b []byte) []byte {
			b[4] = 2
			return b
		}),
		"ace size beyond acl": mut(func(b []byte) []byte { binary.LittleEndian.PutUint16(b[10:12], 200); return b }),
		"ace size below header": mut(func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[10:12], 2)
			return b
		}),
		"bad SID revision":          mut(func(b []byte) []byte { b[16] = 2; return b }),
		"SID longer than its ACE":   mut(func(b []byte) []byte { b[17] = 9; return b }),
		"object ACE without fields": testACL(testACE(aceAllowedObject, 0, le32(maskFA))),
		"object ACE GUIDs cut off":  testACL(testACE(aceAllowedObject, 0, le32(maskFA), le32(3), make([]byte, 20))),
		"basic ACE without mask":    testACL(testACE(aceAllowed, 0, []byte{1, 2})),
	} {
		if _, err := scanACL(b, trusted); !errors.Is(err, errMalformedACL) {
			t.Errorf("%s: err = %v, want errMalformedACL", name, err)
		}
	}
}

func TestSyntheticMode(t *testing.T) {
	for _, c := range []struct {
		dir, read, write bool
		want             os.FileMode
	}{
		{true, false, false, 0o700}, {false, false, false, 0o600},
		{true, true, false, 0o755}, {false, true, false, 0o644},
		{true, true, true, 0o777}, {false, true, true, 0o666},
	} {
		if got := syntheticMode(c.dir, c.read, c.write); got != c.want {
			t.Errorf("syntheticMode(%v, %v, %v) = %04o, want %04o", c.dir, c.read, c.write, got, c.want)
		}
	}
}

func TestCheckPrivateReasons(t *testing.T) {
	for _, c := range []struct {
		i    Info
		want string
	}{
		{Info{OwnedBySelf: true, Mode: 0o700}, ""},
		{Info{Link: true, OwnedBySelf: true}, "symbolic link or reparse point"},
		{Info{Owner: "uid 0"}, "owned by uid 0"},
		{Info{OwnedBySelf: true, OthersRead: true, Mode: 0o755}, "mode 0755"},
		{Info{OwnedBySelf: true, OthersRead: true, readWhy: "access granted to S-1-1-0"}, "access granted to S-1-1-0"},
	} {
		if got := c.i.why(); got != c.want {
			t.Errorf("why(%+v) = %q, want %q", c.i, got, c.want)
		}
		if c.i.Private() != (c.want == "") {
			t.Errorf("Private(%+v) = %v", c.i, c.i.Private())
		}
	}
	err := notPrivate("/x", "mode 0755")
	if !errors.Is(err, ErrNotPrivate) || err.Error() != "/x: not private to this user (mode 0755)" {
		t.Errorf("notPrivate: %v", err)
	}
}

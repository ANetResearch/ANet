package backendconn

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// FuzzPolicyResolve: whatever config.json holds in expected_user,
// socket_group and peer_check, Resolve either refuses it with a PolicyError
// naming one of the three keys, or accepts it as LEC §3.2 says — all decimal
// digits taken as that id (never past maxID), a name only as an account
// this machine has, peer_check only "" or "path-only".
func FuzzPolicyResolve(f *testing.F) {
	f.Add("1000", "", "")
	f.Add("root", "0", "path-only")
	f.Add("4294967294", "4294967295", " path-only ")
	f.Add("0001", "no-such-group", "strict")
	f.Add("-1", "+5", "PATH-ONLY")
	f.Add(" 12 ", "1e3", "")
	f.Fuzz(func(t *testing.T, user, group, check string) {
		r, err := Policy{ExpectedUser: user, SocketGroup: group, PeerCheck: check}.Resolve()
		if err != nil {
			var pe *PolicyError
			if !errors.As(err, &pe) || (pe.Key != "expected_user" && pe.Key != "socket_group" && pe.Key != "peer_check") {
				t.Fatalf("%q %q %q: %v is not a PolicyError naming a key", user, group, check, err)
			}
			return
		}
		switch c := strings.TrimSpace(check); c {
		case "", PeerCheckPathOnly:
			if r.pathOnly != (c == PeerCheckPathOnly) {
				t.Fatalf("peer_check %q: pathOnly %v", check, r.pathOnly)
			}
		default:
			t.Fatalf("peer_check %q accepted", check)
		}
		for _, c := range []struct {
			v   string
			got int
		}{{user, r.expected}, {group, r.group}} {
			v := strings.TrimSpace(c.v)
			switch {
			case v == "":
				if c.got != -1 {
					t.Fatalf("%q unset gives %d", c.v, c.got)
				}
			case strings.Trim(v, "0123456789") == "":
				n, perr := strconv.ParseUint(v, 10, 64)
				if perr != nil || n > maxID || c.got != int(n) {
					t.Fatalf("%q gives %d", c.v, c.got)
				}
			default:
				if c.got < 0 || c.got > maxID {
					t.Fatalf("name %q gives %d", c.v, c.got)
				}
			}
		}
	})
}

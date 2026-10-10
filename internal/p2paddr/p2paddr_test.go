package p2paddr

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

// SSR-P4's table: which IPs a direct delivery may be dialled at, with LAN
// mode off and on. An IPv4-mapped IPv6 address is judged as the IPv4 one.
func TestRefused(t *testing.T) {
	for _, c := range []struct {
		ip       string
		off, lan bool // dialled with LAN mode off, on
	}{
		{"203.0.113.9", true, true},
		{"198.51.100.9", true, true},
		{"2001:db8::1", true, true},
		{"2001:db8:ffff::1", true, true},
		{"10.0.0.1", false, true},
		{"172.16.5.4", false, true},
		{"192.168.1.1", false, true},
		{"100.64.0.1", false, true},
		{"100.127.255.254", false, true},
		{"100.128.0.1", true, true},
		{"fd12:3456::1", false, true},
		{"fc00::1", false, true},
		{"127.0.0.1", false, false},
		{"127.255.255.254", false, false},
		{"::1", false, false},
		{"0.0.0.0", false, false},
		{"0.255.255.255", false, false},
		{"::", false, false},
		{"169.254.169.254", false, false},
		{"fe80::1", false, false},
		{"fe80::1%eth0", false, false},
		{"224.0.0.251", false, false},
		{"239.1.2.3", false, false},
		{"ff02::fb", false, false},
		{"ff0e::1", false, false},
		{"::ffff:127.0.0.1", false, false},
		{"::ffff:10.0.0.1", false, true},
		{"::ffff:169.254.1.1", false, false},
		{"::ffff:203.0.113.9", true, true},
	} {
		ip := netip.MustParseAddr(c.ip)
		for _, lan := range []bool{false, true} {
			want := c.off
			if lan {
				want = c.lan
			}
			if why := Refused(ip, lan); (why == "") != want {
				t.Errorf("Refused(%s, lan=%v) = %q, want dialled=%v", c.ip, lan, why, want)
			}
		}
	}
	if Refused(netip.Addr{}, true) == "" {
		t.Error("the zero address is dialled")
	}
}

func TestSplit(t *testing.T) {
	for in, want := range map[string]string{
		"peer.example.org:39100": "peer.example.org 39100",
		"203.0.113.9:1":          "203.0.113.9 1",
		"[2001:db8::1]:65535":    "2001:db8::1 65535",
		"[fe80::1%eth0]:4001":    "fe80::1%eth0 4001",
	} {
		h, p, err := Split(in)
		if err != nil || h+" "+strconv.Itoa(int(p)) != want {
			t.Errorf("Split(%q) = %q %d %v, want %s", in, h, p, err, want)
		}
	}
	for _, bad := range []string{"", "peer.example.org", ":39100", "peer.example.org:", "peer.example.org:0",
		"peer.example.org:65536", "peer.example.org:+80", "peer.example.org:-1", "peer.example.org: 80",
		"peer.example.org:http", "2001:db8::1:80", "[2001:db8::1]", strings.Repeat("a", MaxLen) + ":1"} {
		if h, p, err := Split(bad); err == nil {
			t.Errorf("Split(%q) = %q %d", bad, h, p)
		}
	}
}

func TestLiteralRefused(t *testing.T) {
	for addr, want := range map[string]bool{ // refused?
		"203.0.113.9:4001":       false,
		"peer.example.org:4001":  false,
		"localhost.example:4001": false,
		"127.0.0.1:4001":         true,
		"[::1]:4001":             true,
		"[::ffff:127.0.0.1]:1":   true,
		"10.0.0.1:4001":          true,
		"localhost:4001":         true,
		"LOCALHOST.:4001":        true,
		"x.localhost:4001":       true,
		"peer.example.org":       true,
	} {
		err := LiteralRefused(addr, false)
		if (err != nil) != want {
			t.Errorf("LiteralRefused(%q) = %v, want refused=%v", addr, err, want)
		}
		if err != nil && !errors.Is(err, ErrRefused) {
			t.Errorf("LiteralRefused(%q): %v is not ErrRefused", addr, err)
		}
	}
	if err := LiteralRefused("10.0.0.1:4001", true); err != nil {
		t.Errorf("LAN mode: %v", err)
	}
	if err := LiteralRefused("127.0.0.1:4001", true); err == nil {
		t.Error("LAN mode dials loopback")
	}
}

func TestCheckAdvertise(t *testing.T) {
	for _, ok := range []string{"", "tcp://peer.example:4001", "peer.example:4001", "[2001:db8::1]:4001",
		"127.0.0.1:4001", " peer.example:4001 "} {
		if err := CheckAdvertise(ok); err != nil {
			t.Errorf("CheckAdvertise(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"/run/a/wire.sock", "unix:///run/a.sock", "peer.example", ":4001", "tcp://",
		"tcp://peer.example:4001/path", "peer.example:http", "peer.example:0", "peer.example:70000"} {
		if err := CheckAdvertise(bad); err == nil {
			t.Errorf("CheckAdvertise(%q) took it", bad)
		}
	}
}

// Split takes host:port and nothing else: what it accepts joins back to
// the same text, the port is 1 to 65535, the host is not empty, and the
// whole is at most MaxLen bytes.
func FuzzSplit(f *testing.F) {
	for _, s := range []string{"peer.example.org:39100", "[2001:db8::1]:1", "a:65535", "a:65536", "a:0", ":1",
		"[::1]:80", "[fe80::1%eth0]:4001", "a:+1", "a:01", "a:1:2", "[a]:1", "a]:1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, p, err := Split(s)
		if err != nil {
			return
		}
		if h == "" || p == 0 || len(s) > MaxLen {
			t.Fatalf("%q: host %q port %d", s, h, p)
		}
		back := net.JoinHostPort(h, strconv.Itoa(int(p)))
		if hh, pp, err := net.SplitHostPort(back); err != nil || hh != h || pp != strconv.Itoa(int(p)) {
			t.Fatalf("%q: %q does not split back", s, back)
		}
	})
}

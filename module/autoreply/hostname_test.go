package autoreply

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// CN-1: what an allowlist entry may be, and the {why} of every other shape.
func TestEgressHostNames(t *testing.T) {
	for _, tc := range []struct{ in, host, why string }{
		{"pypi.org", "pypi.org", ""},
		{"files.pythonhosted.org", "files.pythonhosted.org", ""},
		{"PyPI.Org", "pypi.org", ""},
		{"xn--fiqs8s.example", "xn--fiqs8s.example", ""},
		{"a-b.c0", "a-b.c0", ""},
		{"1password.com", "1password.com", ""},
		{strings.Repeat("a", 63) + ".com", strings.Repeat("a", 63) + ".com", ""},
		{"*.pypi.org", "", "wildcard"},
		{"pypi.*", "", "wildcard"},
		{"1.2.3.4", "", "ip"},
		{"1.2.3.4:443", "", "ip"},
		{"::1", "", "ip"},
		{"[::1]", "", "ip"},
		{"[2001:db8::1]:443", "", "ip"},
		{"fe80::1%eth0", "", "ip"},
		{"999.999.999.999", "", "ip"}, // not an address, but an all-numeric top label
		{"example.123", "", "ip"},
		{"pypi.org:443", "", "port"},
		{"pypi.org:", "", "port"},
		{"localhost", "", "local"},
		{"LOCALHOST", "", "local"},
		{"api.localhost", "", "local"},
		{"printer.local", "", "local"},
		{"metadata.google.internal", "", "local"},
		{"nas.home.arpa", "", "local"},
		{"home.arpa", "", "local"},
		{"router.lan", "", "local"},
		{"bücher.example", "", "non_ascii"},
		{"pypi.orgK", "", "non_ascii"},
		{"https://pypi.org/simple", "", "not_host"},
		{"user@pypi.org", "", "not_host"},
		{"pypi", "", "not_host"},
		{"", "", "not_host"},
		{"pypi.org.", "", "not_host"},
		{".pypi.org", "", "not_host"},
		{"pypi..org", "", "not_host"},
		{"-pypi.org", "", "not_host"},
		{"pypi-.org", "", "not_host"},
		{"py_pi.org", "", "not_host"},
		{"py pi.org", "", "not_host"},
		{strings.Repeat("a", 64) + ".com", "", "not_host"},
		{strings.Repeat("a.", 127) + "com", "", "not_host"}, // 257 characters
	} {
		h, why := normalizeEgressHost(tc.in)
		if h != tc.host || why != tc.why {
			t.Errorf("%q: %q %q, want %q %q", tc.in, h, why, tc.host, tc.why)
		}
		if why != "" {
			_, ref := checkEgressAllow([]string{tc.in}, "")
			s := a2ashape.Sentence(ref.Key, a2ashape.English, ref.Args)
			if ref.Key != "egress_allow_invalid" || strings.Contains(s, "—") || strings.Contains(s, "{") {
				t.Errorf("%q: %s: %s", tc.in, ref.Key, s)
			}
		}
	}
}

// CN-1 (M-CN-8): the list as a whole: normalized, each host once; a model service host is refused —
// the built-in ones, and the host of api_base — and so are more than 64 hosts.
func TestEgressAllowList(t *testing.T) {
	got, ref := checkEgressAllow([]string{"PyPI.org", "files.pythonhosted.org", "pypi.org"}, "")
	if ref != nil || strings.Join(got, ",") != "pypi.org,files.pythonhosted.org" {
		t.Errorf("%v %v", got, ref)
	}
	if got, ref := checkEgressAllow(nil, ""); ref != nil || got == nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, ref)
	}
	for _, tc := range []struct {
		list    []string
		apiBase string
		code    string
		value   string
	}{
		{[]string{"pypi.org", "api.anthropic.com"}, "", "egress_allow_model_host", "api.anthropic.com"},
		{[]string{"API.OpenAI.com"}, "", "egress_allow_model_host", "api.openai.com"},
		{[]string{"openrouter.ai"}, "", "egress_allow_model_host", "openrouter.ai"},
		{[]string{"llm.example.net"}, "https://LLM.example.net./v1", "egress_allow_model_host", "llm.example.net"},
		{[]string{"pypi.org", "10.0.0.1"}, "", "egress_allow_invalid", "10.0.0.1"},
		{[]string{"*.github.com"}, "", "egress_allow_invalid", "*.github.com"},
	} {
		_, ref := checkEgressAllow(tc.list, tc.apiBase)
		if ref == nil || ref.Key != tc.code || ref.Args["value"] != tc.value {
			t.Errorf("%v (api_base %q): %+v, want %s %s", tc.list, tc.apiBase, ref, tc.code, tc.value)
		}
	}
	var many []string
	for i := 0; i < egressAllowMax; i++ {
		many = append(many, "h"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+".example"+string(rune('a'+i/26)))
	}
	if got, ref := checkEgressAllow(many, ""); ref != nil || len(got) != 64 {
		t.Fatalf("64 hosts: %d %v", len(got), ref)
	}
	if _, ref := checkEgressAllow(append(many, "one-more.example"), ""); ref == nil || ref.Key != "egress_allow_too_many" {
		t.Errorf("65 hosts: %v", ref)
	}
	if got, ref := checkEgressAllow(append(many, many[0], strings.ToUpper(many[1])), ""); ref != nil || len(got) != 64 {
		t.Errorf("64 hosts, two of them twice: %d %v", len(got), ref)
	}
}

// The run's CONNECT allowlist is the configured one, normalized; whatever does not pass CN-1 now is left
// out, model service hosts included.
func TestEgressAllowSet(t *testing.T) {
	cfg := AutoReplyConfig{Agent: agentClaude, APIKey: "k", Sandbox: &AutoReplySandboxConfig{
		EgressAllow: []string{"PyPI.org", "files.pythonhosted.org", "127.0.0.1", "api.anthropic.com", "*.x.org"}}}
	set := egressAllowSet(cfg)
	if len(set) != 2 || !set["pypi.org"] || !set["files.pythonhosted.org"] {
		t.Errorf("%v", set)
	}
	up, err := resolveEgressUpstream(cfg)
	if err != nil || len(up.ConnectAllow) != 2 || !up.ConnectAllow["pypi.org"] {
		t.Errorf("upstream allowlist %v %v", up, err)
	}
	// At most 64 hosts, whatever the configuration holds.
	cfg.Sandbox = &AutoReplySandboxConfig{}
	for i := 0; i < egressAllowMax+6; i++ {
		cfg.Sandbox.EgressAllow = append(cfg.Sandbox.EgressAllow, fmt.Sprintf("h%d.example", i))
	}
	if set := egressAllowSet(cfg); len(set) != egressAllowMax {
		t.Errorf("%d hosts allowed of %d configured", len(set), len(cfg.Sandbox.EgressAllow))
	}
	cfg.Sandbox = nil
	if up, _ := resolveEgressUpstream(cfg); len(up.ConnectAllow) != 0 {
		t.Errorf("no sandbox block: %v", up.ConnectAllow)
	}
}

// P-HOST: whatever the input, the checker never accepts an IP literal, a wildcard, a port, a non-ASCII
// character or a local name, and what it accepts is a fixed point.
func TestEgressHostNameProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	alphabet := []byte("abcdefgxyzKLM0129-._*:/@[]% \x00\xc3\xbc\xe2\x84\xaa")
	pieces := []string{"localhost", ".local", ".internal", ".home.arpa", ".lan", ".localhost", "127.0.0.1", "::1",
		"[::1]", ":443", "pypi", ".org", "xn--", "*.", "1.2", ".3", "fe80::1%lo", "0x7f.1"}
	for i := 0; i < 200000; i++ {
		var b strings.Builder
		for n := r.IntN(6); n >= 0; n-- {
			if r.IntN(3) == 0 {
				b.WriteString(pieces[r.IntN(len(pieces))])
			} else {
				for m := r.IntN(8); m >= 0; m-- {
					b.WriteByte(alphabet[r.IntN(len(alphabet))])
				}
			}
		}
		in := b.String()
		h, why := normalizeEgressHost(in)
		if why != "" {
			if h != "" {
				t.Fatalf("%q: refused (%s) with a host %q", in, why, h)
			}
			continue
		}
		if egressIPLiteral(h) || strings.ContainsAny(h, "*:/@[]% \x00") || !asciiOnly(h) || egressLocalName(h) {
			t.Fatalf("%q accepted as %q", in, h)
		}
		if _, err := netip.ParseAddr(h); err == nil {
			t.Fatalf("%q accepted as an address %q", in, h)
		}
		if h2, why2 := normalizeEgressHost(h); h2 != h || why2 != "" {
			t.Fatalf("%q: %q is not a fixed point (%q %q)", in, h, h2, why2)
		}
	}
}

// --- CN-4 ---

// egressSpecialRef is CN-4's table, plus what this implementation refuses beyond it (IPv6 outside
// 2000::/3; 3fff::/20), as the reference P-IP compares against: one row per range, read by a plain
// bit-by-bit prefix comparison, independent of netip.Prefix.
var egressSpecialRef = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
	"2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	// beyond the table
	"3fff::/20", "::/3", "4000::/2", "8000::/1",
}

// refRefused is the reference: an IPv4-mapped address is its IPv4 address; then refused when the first
// bits of the address equal a row's.
func refRefused(b [16]byte, is4 bool) bool {
	mapped := !is4 && binary.BigEndian.Uint64(b[:8]) == 0 && binary.BigEndian.Uint32(b[8:12]) == 0xffff
	if mapped {
		is4 = true
		copy(b[:4], b[12:])
	}
	for _, row := range egressSpecialRef {
		ip, bitsStr, _ := strings.Cut(row, "/")
		v4 := !strings.Contains(ip, ":")
		if v4 != is4 {
			continue
		}
		p := netip.MustParseAddr(ip)
		var pb [16]byte
		if v4 {
			a := p.As4()
			copy(pb[:4], a[:])
		} else {
			pb = p.As16()
		}
		bits := 0
		for _, c := range bitsStr {
			bits = bits*10 + int(c-'0')
		}
		match := true
		for i := 0; i < bits; i++ {
			if (b[i/8]>>(7-i%8))&1 != (pb[i/8]>>(7-i%8))&1 {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// P-IP: for random IPv4 and IPv6 addresses — around every range's edges, in the IPv4-mapped, NAT64, 6to4
// and Teredo forms, and anywhere — an address is refused exactly when the reference says so.
func TestEgressAddressProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	check := func(a netip.Addr) {
		t.Helper()
		var b [16]byte
		if a.Is4() {
			v := a.As4()
			copy(b[:4], v[:])
		} else {
			b = a.As16()
		}
		if got, want := egressAddrRefused(a), refRefused(b, a.Is4()); got != want {
			t.Fatalf("%s: refused %v, the reference says %v", a, got, want)
		}
	}
	random := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.IntN(256))
		}
		return b
	}
	for _, row := range egressSpecialRef {
		p := netip.MustParsePrefix(row)
		base := p.Masked().Addr()
		for i := 0; i < 2000; i++ {
			// inside the range, then its neighbours: one past the last address, one before the first
			b := random(base.BitLen() / 8)
			raw := base.AsSlice()
			for bit := 0; bit < p.Bits(); bit++ {
				m := byte(1) << (7 - bit%8)
				b[bit/8] = b[bit/8]&^m | raw[bit/8]&m
			}
			a, _ := netip.AddrFromSlice(b)
			check(a)
			check(a.Next())
			check(a.Prev())
			if a.Is4() {
				check(netip.AddrFrom16(a.As16())) // ::ffff:a.b.c.d
				v := a.As4()
				nat64 := netip.MustParseAddr("64:ff9b::").As16()
				copy(nat64[12:], v[:])
				check(netip.AddrFrom16(nat64))
				sixto4 := netip.MustParseAddr("2002::").As16()
				copy(sixto4[2:6], v[:])
				check(netip.AddrFrom16(sixto4))
				teredo := netip.MustParseAddr("2001::").As16()
				copy(teredo[12:], v[:])
				check(netip.AddrFrom16(teredo))
				compat := [16]byte{}
				copy(compat[12:], v[:])
				check(netip.AddrFrom16(compat)) // ::a.b.c.d, deprecated
			}
		}
	}
	for i := 0; i < 200000; i++ {
		v4, _ := netip.AddrFromSlice(random(4))
		v6, _ := netip.AddrFromSlice(random(16))
		check(v4)
		check(v6)
		g := v6.As16()
		g[0] = 0x20 | g[0]&0x1f // global unicast
		check(netip.AddrFrom16(g))
	}
}

// M-CN-1: the addresses an attacker would point an allowed name at are refused, and public servers are
// not.
func TestEgressSpecialAddresses(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "127.255.0.9", "169.254.169.254", "10.1.2.3", "172.16.0.1", "192.168.1.1", "100.64.0.1",
		"0.0.0.0", "224.0.0.1", "255.255.255.255", "198.18.0.1", "192.0.0.170",
		"::", "::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:10.0.0.1", "64:ff9b::a9fe:a9fe",
		"64:ff9b:1::1", "fe80::1", "fc00::1", "fd12:3456::1", "ff02::1", "2002:7f00:1::", "2001:0:4136:e378::1",
		"2001:db8::1", "3fff::1", "::7f00:1", "100::1", "5f00::1", "fe80::1%eth0",
	} {
		if !egressAddrRefused(netip.MustParseAddr(s)) {
			t.Errorf("%s is not refused", s)
		}
	}
	for _, s := range []string{"151.101.0.223", "8.8.8.8", "1.1.1.1", "2a04:4e42::223", "2606:4700::1111", "::ffff:151.101.0.223"} {
		if egressAddrRefused(netip.MustParseAddr(s)) {
			t.Errorf("%s is refused", s)
		}
	}
	if !egressAddrRefused(netip.Addr{}) {
		t.Error("the zero address is not refused")
	}
}

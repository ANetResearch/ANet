package autoreply

// hostname.go is what the CONNECT allowlist holds and where a CONNECT may go (LEC §4.3.5, CN-1, CN-2,
// CN-4; SI-55).
//
// auto_reply.sandbox.egress_allow names the hosts a sandboxed agent may reach with CONNECT, each by its
// exact name, on port 443 only; it is empty by default, and then a sandboxed agent has no network beyond
// the model route. An entry is a whole DNS host name in lower-case ASCII: no address, wildcard, port,
// local name, or model service host (the agent would reach its model with a key of its own, around the
// injection route). A CONNECT names a host that equals an entry exactly (CN-2): no suffix ever matches.
//
// Whatever the host resolves to is checked before anything is dialed (CN-4): an address of a special
// range (loopback, private networks, link-local and the cloud metadata service, carrier-grade NAT,
// documentation, multicast, NAT64 and 6to4 translation, and IPv6 outside global unicast) or of this
// machine refuses the CONNECT as a whole. Only the checked addresses are dialed.

import (
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// egressAllowMax is the longest allowlist (CN-1).
const egressAllowMax = 64

// egressModelHosts are the model service hosts no allowlist may hold (CN-1); the host of
// auto_reply.api_base is one too.
var egressModelHosts = []string{"api.anthropic.com", "api.openai.com", "openrouter.ai"}

// egressLocalSuffixes end the names that resolve only on a local network (CN-1).
var egressLocalSuffixes = []string{".localhost", ".local", ".internal", ".home.arpa", ".lan"}

// The {why} of egress_allow_invalid (ux §1.4 G): what keeps an entry from being a whole host name.
const (
	egressWhyIP       = "ip"
	egressWhyWildcard = "wildcard"
	egressWhyPort     = "port"
	egressWhyLocal    = "local"
	egressWhyNonASCII = "non_ascii"
	// egressWhyNotHost is any other shape: a URL, an address with a user, one label, an empty label, a
	// character a host name cannot have, a trailing dot.
	egressWhyNotHost = "not_host"
)

// normalizeEgressHost reads s as an allowlist entry (CN-1): the host name it is, in lower case, or the
// {why} of egress_allow_invalid. A CONNECT's host goes through the same reading before it is compared
// (CN-2), so what is not a valid entry is never allowed.
func normalizeEgressHost(s string) (host, why string) {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return "", egressWhyNonASCII
		}
	}
	h := asciiLower(s)
	switch {
	case strings.Contains(h, "*"):
		return "", egressWhyWildcard
	case egressIPLiteral(h):
		return "", egressWhyIP
	case strings.ContainsAny(h, "/@"):
		return "", egressWhyNotHost
	case strings.Contains(h, ":"):
		return "", egressWhyPort
	case egressLocalName(h):
		return "", egressWhyLocal
	}
	if len(h) < 1 || len(h) > 253 {
		return "", egressWhyNotHost
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", egressWhyNotHost
	}
	for _, l := range labels {
		if !egressLabel(l) {
			return "", egressWhyNotHost
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", egressWhyIP // an all-numeric top label is an address written short, not a name
	}
	return h, ""
}

// egressLabel is one label of a host name: 1 to 63 letters, digits or hyphens, with no hyphen first or last.
func egressLabel(l string) bool {
	if len(l) < 1 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// egressIPLiteral reports an IP address however it is written: v4, v6, v6 in brackets, either with a
// port.
func egressIPLiteral(h string) bool {
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if i := strings.IndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	_, err := netip.ParseAddr(h)
	return err == nil
}

// asciiLower lower-cases the ASCII letters of s and nothing else. strings.ToLower is not used: it folds
// some non-ASCII letters into ASCII ones (the Kelvin sign into k), and a name that only became an
// allowed host on the way here must not be taken for it.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// egressLocalName is localhost, one of the local-only domains, or a name under one.
func egressLocalName(h string) bool {
	if h == "localhost" {
		return true
	}
	for _, x := range egressLocalSuffixes {
		if strings.HasSuffix(h, x) || h == x[1:] {
			return true
		}
	}
	return false
}

// egressModelHostSet is the model service hosts an allowlist may not hold: the built-in ones and the
// host of apiBase (lower case, without a trailing dot).
func egressModelHostSet(apiBase string) map[string]bool {
	set := map[string]bool{}
	for _, h := range egressModelHosts {
		set[h] = true
	}
	if u, err := url.Parse(strings.TrimSpace(apiBase)); err == nil && u.Hostname() != "" {
		set[strings.TrimSuffix(asciiLower(u.Hostname()), ".")] = true
	}
	return set
}

// checkEgressAllow checks an allowlist as config.json or POST /autoreply gives it (CN-1, LEC §3.2): the
// entries in lower case, each once, in the order given, or the refusal for the first entry that is not a
// whole host name (egress_allow_invalid), that is a model service host (egress_allow_model_host), or for
// more than 64 hosts (egress_allow_too_many).
func checkEgressAllow(list []string, apiBase string) ([]string, *a2ashape.Refusal) {
	model := egressModelHostSet(apiBase)
	out := make([]string, 0, len(list))
	for _, raw := range list {
		h, why := normalizeEgressHost(raw)
		if why != "" {
			shown := raw
			if strings.TrimSpace(shown) == "" {
				shown = `""` // an empty entry, shown as one
			}
			return nil, a2ashape.Refuse("egress_allow_invalid", a2ashape.Args{"value": shown, "why": why})
		}
		if model[h] {
			return nil, a2ashape.Refuse("egress_allow_model_host", a2ashape.Args{"value": h})
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	if len(out) > egressAllowMax {
		return nil, a2ashape.Refuse("egress_allow_too_many", nil)
	}
	return out, nil
}

// CheckEgressAllow is checkEgressAllow for the CLI, which checks a list before it asks for a
// confirmation (the daemon checks it again).
func CheckEgressAllow(list []string, apiBase string) ([]string, *a2ashape.Refusal) {
	return checkEgressAllow(list, apiBase)
}

// egressAllowSet is the allowlist a sandboxed run's CONNECTs are held to (CN-2): the configured entries
// that pass CN-1. The configuration was checked when it was written and when the daemon started; an
// entry that does not pass now is left out rather than trusted.
func egressAllowSet(cfg AutoReplyConfig) map[string]bool {
	if cfg.Sandbox == nil {
		return nil
	}
	model := egressModelHostSet(cfg.APIBase)
	set := map[string]bool{}
	for _, raw := range cfg.Sandbox.EgressAllow {
		if h, why := normalizeEgressHost(raw); why == "" && !model[h] && len(set) < egressAllowMax {
			set[h] = true
		}
	}
	return set
}

// --- where a CONNECT may go (CN-4) ---

// egressSpecialPrefixes are the special-purpose ranges of CN-4: a host that resolves to an address in
// one of them is refused. The IPv4 ranges of ::ffff:0:0/96 are judged by the address inside.
var egressSpecialPrefixes = mustPrefixes(
	// IPv4: this network, private networks, carrier-grade NAT, loopback, link-local (and the cloud
	// metadata service on it), IETF protocol assignments, documentation, 6to4 relay anycast,
	// benchmarking, multicast, reserved and broadcast.
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	// IPv6: unspecified, loopback, NAT64 (well-known and local use), discard, IETF protocol assignments
	// (Teredo among them), documentation, 6to4, unique local, link-local and multicast.
	"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
	"2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	// Beyond the table of CN-4: the newer IPv6 documentation range (RFC 9637).
	"3fff::/20",
)

// egressGlobalUnicast6 is IPv6 global unicast. An IPv6 address outside it (other than an IPv4-mapped
// one) is reserved or special and never a public server: refused too, beyond the table of CN-4, which
// covers, for one, the deprecated IPv4-compatible ::a.b.c.d that some kernels still tunnel to the
// IPv4 address inside.
var egressGlobalUnicast6 = netip.MustParsePrefix("2000::/3")

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// egressAddrRefused reports an address a CONNECT may not reach (CN-4): a special-purpose address, an
// IPv6 address outside global unicast, or anything that is not an address.
func egressAddrRefused(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	// A zone would make every prefix test below false.
	a = a.WithZone("")
	if a.Is4In6() {
		a = a.Unmap()
	}
	if a.Is6() && !egressGlobalUnicast6.Contains(a) {
		return true
	}
	for _, p := range egressSpecialPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// egressLocalAddrs are the addresses of this machine's interfaces: a host that resolves to one reaches
// whatever listens on this machine, as the loopback would.
func egressLocalAddrs() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if na, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, na.Unmap().WithZone(""))
		}
	}
	return out
}

// egressAllowOf is the allowlist of an auto-reply configuration, [] for none.
func egressAllowOf(c *AutoReplyConfig) []string {
	if c == nil || c.Sandbox == nil {
		return []string{}
	}
	return append([]string{}, c.Sandbox.EgressAllow...)
}

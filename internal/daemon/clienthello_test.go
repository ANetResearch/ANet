package daemon

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	mrand "math/rand/v2"
	"net"
	"strings"
	"testing"
	"time"
)

// realClientHello is the record stream crypto/tls sends to open a connection with cfg: what an agent's
// TLS client writes into its tunnel first.
func realClientHello(t testing.TB, cfg *tls.Config) []byte {
	t.Helper()
	agent, endpoint := net.Pipe()
	defer endpoint.Close()
	go func() {
		defer agent.Close()
		_ = tls.Client(agent, cfg).Handshake()
	}()
	_ = endpoint.SetReadDeadline(time.Now().Add(10 * time.Second))
	raw, _, code := readClientHello(endpoint)
	if code != "" {
		t.Fatalf("crypto/tls's ClientHello read as %s", code)
	}
	return raw
}

// echConfigList is an ECHConfigList (draft-ietf-tls-esni §4) with a fresh X25519 key and public_name.
func echConfigList(t testing.TB, publicName string) []byte {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vec16 := func(b []byte) []byte { return append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...) }
	var c []byte
	c = append(c, 7)                             // config_id
	c = binary.BigEndian.AppendUint16(c, 0x0020) // DHKEM(X25519, HKDF-SHA256)
	c = append(c, vec16(k.PublicKey().Bytes())...)
	c = append(c, vec16([]byte{0, 1, 0, 1})...) // HKDF-SHA256, AES-128-GCM
	c = append(c, 0)                            // maximum_name_length
	c = append(c, byte(len(publicName)))
	c = append(c, publicName...)
	c = append(c, vec16(nil)...) // extensions
	cfg := append(binary.BigEndian.AppendUint16(nil, tlsExtECH), vec16(c)...)
	return vec16(cfg)
}

// resplit puts the handshake bytes of a record stream into records cut where cuts say (each cut byte is
// the next record's length less one).
func resplit(raw []byte, cuts []byte) []byte {
	var hs []byte
	for len(raw) >= 5 {
		n := int(binary.BigEndian.Uint16(raw[3:5]))
		hs = append(hs, raw[5:5+n]...)
		raw = raw[5+n:]
	}
	var out []byte
	for i := 0; len(hs) > 0; i++ {
		n := len(hs)
		if i < len(cuts) {
			n = min(n, int(cuts[i])+1)
		}
		out = append(out, tlsRecordHandshake, 3, 1)
		out = binary.BigEndian.AppendUint16(out, uint16(n))
		out = append(out, hs[:n]...)
		hs = hs[n:]
	}
	return out
}

// helloRecord is a ClientHello with extensions exts (each type and data) in one record; no extensions
// block at all when exts is nil.
func helloRecord(exts [][2][]byte) []byte {
	body := []byte{3, 3}
	body = append(body, make([]byte, 32)...)
	body = append(body, 0)                // session_id
	body = append(body, 0, 2, 0x13, 0x01) // cipher_suites
	body = append(body, 1, 0)             // compression_methods
	if exts != nil {
		var e []byte
		for _, x := range exts {
			e = append(e, x[0]...)
			e = binary.BigEndian.AppendUint16(e, uint16(len(x[1])))
			e = append(e, x[1]...)
		}
		body = binary.BigEndian.AppendUint16(body, uint16(len(e)))
		body = append(body, e...)
	}
	msg := append([]byte{tlsHandshakeClientHello, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	rec := append([]byte{tlsRecordHandshake, 3, 1}, binary.BigEndian.AppendUint16(nil, uint16(len(msg)))...)
	return append(rec, msg...)
}

// sniExt is a server_name extension listing entries of (type, name).
func sniExt(entries ...[2]string) [2][]byte {
	var list []byte
	for _, e := range entries {
		list = append(list, e[0][0])
		list = binary.BigEndian.AppendUint16(list, uint16(len(e[1])))
		list = append(list, e[1]...)
	}
	return [2][]byte{{0, 0}, append(binary.BigEndian.AppendUint16(nil, uint16(len(list))), list...)}
}

func host(name string) [2]string { return [2]string{"\x00", name} }

// helloVerdict reads a record stream as the endpoint does and checks it against host.
func helloVerdict(stream []byte, host string) (code string, raw []byte) {
	raw, body, code := readClientHello(bytes.NewReader(stream))
	if code == "" {
		code = checkClientHello(body, host)
	}
	return code, raw
}

// CN-5 on crypto/tls's own ClientHellos: the one name is found in one record or cut into many, compared
// without case; another name, or Encrypted Client Hello, is dropped.
func TestClientHelloFromCryptoTLS(t *testing.T) {
	r := mrand.New(mrand.NewPCG(5, 6))
	for _, name := range []string{"pypi.org", "files.pythonhosted.org", "a.b", "xn--fiqs8s.example"} {
		raw := realClientHello(t, &tls.Config{ServerName: name, InsecureSkipVerify: true})
		for i := 0; i < 50; i++ {
			cuts := make([]byte, 400)
			for j := range cuts {
				cuts[j] = byte(r.IntN(1 + r.IntN(256)))
			}
			stream := resplit(raw, cuts)
			if code, got := helloVerdict(stream, name); code != "" || !bytes.Equal(got, stream) {
				t.Fatalf("%s cut into records: %q (read %d of %d bytes)", name, code, len(got), len(stream))
			}
			if code, _ := helloVerdict(stream, "x"+name); code != egressSNIMismatch {
				t.Fatalf("%s against another host: %q", name, code)
			}
		}
	}
	upper := realClientHello(t, &tls.Config{ServerName: "PyPI.Org", InsecureSkipVerify: true})
	if code, _ := helloVerdict(upper, "pypi.org"); code != "" {
		t.Errorf("an upper-case name: %q", code)
	}
	ech := realClientHello(t, &tls.Config{ServerName: "pypi.org", MinVersion: tls.VersionTLS13,
		EncryptedClientHelloConfigList: echConfigList(t, "pypi.org")})
	if code, _ := helloVerdict(ech, "pypi.org"); code != egressECH {
		t.Errorf("Encrypted Client Hello: %q", code)
	}
	if info, ok := parseClientHello(mustBody(t, ech)); !ok || !info.ech || info.sni != "pypi.org" {
		t.Errorf("ECH hello read as %+v %v", info, ok)
	}
}

func mustBody(t testing.TB, stream []byte) []byte {
	t.Helper()
	_, body, code := readClientHello(bytes.NewReader(stream))
	if code != "" {
		t.Fatal(code)
	}
	return body
}

// CN-5: what is not a ClientHello naming exactly the host, and how it is told apart.
func TestClientHelloVerdicts(t *testing.T) {
	good := helloRecord([][2][]byte{sniExt(host("pypi.org"))})
	big := helloRecord([][2][]byte{sniExt(host("pypi.org")), {{0, 21}, make([]byte, 16<<10)}}) // padding over 16 KiB
	withAfter := append([]byte(nil), good...)
	withAfter[4]++ // the record holds one byte more than the hello
	withAfter = append(withAfter, 0)
	// One byte after the extensions, inside the message and its record.
	trailing := helloRecord([][2][]byte{sniExt(host("pypi.org"))})
	trailing = append(trailing[:len(trailing):len(trailing)], 0)
	binary.BigEndian.PutUint16(trailing[3:5], binary.BigEndian.Uint16(trailing[3:5])+1)
	trailing[8]++
	for _, tc := range []struct {
		name   string
		stream []byte
		want   string
	}{
		{"good", good, ""},
		{"good, then early data", append(append([]byte(nil), good...), 0x17, 3, 3, 0, 1, 0xaa), ""},
		{"upper case", helloRecord([][2][]byte{sniExt(host("PYPI.ORG"))}), ""},
		{"plain http", []byte("GET / HTTP/1.1\r\nHost: pypi.org\r\n\r\n"), egressNotTLS},
		{"ssh", []byte("SSH-2.0-OpenSSH_9.6\r\n"), egressNotTLS},
		{"empty", nil, egressNotTLS},
		{"truncated header", good[:3], egressNotTLS},
		{"truncated record", good[:len(good)-1], egressNotTLS},
		{"alert first", append([]byte{0x15, 3, 1, 0, 2, 2, 40}, good...), egressNotTLS},
		{"zero-length record", append([]byte{0x16, 3, 1, 0, 0}, good...), egressNotTLS},
		{"not TLS 1.x", append([]byte{0x16, 2}, good[2:]...), egressNotTLS},
		{"server hello", append(good[:5:5], append([]byte{2}, good[6:]...)...), egressNotTLS},
		{"over 16 KiB", big, egressNotTLS},
		{"more after the hello in its record", withAfter, egressNotTLS},
		{"another host", helloRecord([][2][]byte{sniExt(host("evil.org"))}), egressSNIMismatch},
		{"a subdomain", helloRecord([][2][]byte{sniExt(host("x.pypi.org"))}), egressSNIMismatch},
		{"trailing dot", helloRecord([][2][]byte{sniExt(host("pypi.org."))}), egressSNIMismatch},
		{"kelvin sign", helloRecord([][2][]byte{sniExt(host("pypi.orK"))}), egressSNIMismatch},
		{"no extensions", helloRecord(nil), egressSNIMismatch},
		{"no server_name", helloRecord([][2][]byte{{{0, 10}, {0, 2, 0, 29}}}), egressSNIMismatch},
		{"two names", helloRecord([][2][]byte{sniExt(host("pypi.org"), host("evil.org"))}), egressSNIMismatch},
		{"the name twice", helloRecord([][2][]byte{sniExt(host("pypi.org"), host("pypi.org"))}), egressSNIMismatch},
		{"not a host_name", helloRecord([][2][]byte{sniExt([2]string{"\x01", "pypi.org"})}), egressSNIMismatch},
		{"two server_name extensions", helloRecord([][2][]byte{sniExt(host("pypi.org")), sniExt(host("evil.org"))}), egressNotTLS},
		{"empty name", helloRecord([][2][]byte{sniExt(host(""))}), egressNotTLS},
		{"empty list", helloRecord([][2][]byte{{{0, 0}, {0, 0}}}), egressNotTLS},
		{"list longer than the extension", helloRecord([][2][]byte{{{0, 0}, {0, 9, 0, 0, 1, 'a'}}}), egressNotTLS},
		{"ech", helloRecord([][2][]byte{sniExt(host("pypi.org")), {{0xfe, 0x0d}, {0}}}), egressECH},
		{"ech, no name", helloRecord([][2][]byte{{{0xfe, 0x0d}, {0}}}), egressECH},
		{"extensions past the end", trailing, egressNotTLS},
	} {
		code, raw := helloVerdict(tc.stream, "pypi.org")
		if code != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, code, tc.want)
		}
		if !bytes.HasPrefix(tc.stream, raw) || len(raw) > clientHelloMax {
			t.Errorf("%s: read %d bytes that are not the stream's first within 16 KiB", tc.name, len(raw))
		}
		if code == "" && !bytes.Equal(raw, good) && tc.name != "upper case" {
			t.Errorf("%s: read %d bytes, the hello is %d: nothing past it may be taken", tc.name, len(raw), len(good))
		}
	}
}

// F-CH (CN-5): the reader never panics, reads at most 16 KiB and only bytes it was given, and whatever
// crypto/tls sends for a random host name, cut into random records, is found to name that host, and
// is dropped when it carries Encrypted Client Hello. Under plain go test it runs its seeds only.
func FuzzClientHello(f *testing.F) {
	f.Add(helloRecord([][2][]byte{sniExt(host("pypi.org"))}), "pypi.org", []byte{0, 3, 200}, false)
	f.Add(helloRecord([][2][]byte{sniExt(host("pypi.org"), host("evil.org"))}), "files.pythonhosted.org", []byte{}, true)
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"), "x.example", []byte{1, 1, 1, 1}, false)
	f.Add(realClientHello(f, &tls.Config{ServerName: "pypi.org", InsecureSkipVerify: true}), "a-b.c0", []byte{255, 0, 17}, false)
	f.Fuzz(func(t *testing.T, stream []byte, name string, cuts []byte, ech bool) {
		raw, body, code := readClientHello(bytes.NewReader(stream))
		if len(raw) > clientHelloMax || !bytes.HasPrefix(stream, raw) {
			t.Fatalf("read %d bytes, not a prefix of the stream within 16 KiB", len(raw))
		}
		if code == "" {
			info, ok := parseClientHello(body)
			verdict := checkClientHello(body, "pypi.org")
			switch {
			case !ok && verdict != egressNotTLS,
				ok && info.ech && verdict != egressECH,
				verdict == "" && (info.sni == "" || asciiLower(info.sni) != "pypi.org"):
				t.Fatalf("%+v %v read as %q", info, ok, verdict)
			}
		}
		h, why := normalizeEgressHost(name)
		if why != "" || len(cuts) > 64 {
			return
		}
		cfg := &tls.Config{ServerName: h, InsecureSkipVerify: true}
		if ech {
			cfg.MinVersion = tls.VersionTLS13
			cfg.EncryptedClientHelloConfigList = echConfigList(t, h)
		}
		real := resplit(realClientHello(t, cfg), cuts)
		got, read := helloVerdict(real, h)
		want := ""
		if ech {
			want = egressECH
		}
		if len(real) > clientHelloMax {
			want = egressNotTLS
		}
		if got != want || (want == "" && !bytes.Equal(read, real)) {
			t.Fatalf("crypto/tls's hello for %q (ECH %v) in %d bytes: %q, want %q", h, ech, len(real), got, want)
		}
		if !ech && want == "" {
			if other, _ := helloVerdict(real, strings.TrimSuffix("z"+h, ".")); other != egressSNIMismatch {
				t.Fatalf("against another host: %q", other)
			}
		}
	})
}

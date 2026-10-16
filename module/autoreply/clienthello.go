package autoreply

// clienthello.go reads the first TLS message a sandboxed agent sends into a CONNECT tunnel (LEC CN-5).
//
// After the 200, and before anything is dialed, the endpoint reads the agent's TLS records until the
// first handshake message is whole: it must be a ClientHello with exactly one server_name, a host_name
// equal to the CONNECT's host, and no Encrypted Client Hello. A hello that names another host would let
// the agent reach any site served from the allowed host's address; one with ECH hides the name it
// really asks for. Anything else is not TLS as the tunnel promised. In each case the connection is
// dropped: after the 200 there is no answer to give, so the reason goes to the run's evidence and the
// log only (ux §1.4 I).
//
// The bytes read are kept and sent on unchanged once the tunnel is up: the endpoint reads the hello, it
// does not rewrite it.

import (
	"io"
)

// The TLS values CN-5 reads (RFC 8446, RFC 6066; ECH: draft-ietf-tls-esni).
const (
	tlsRecordHandshake      = 0x16
	tlsHandshakeClientHello = 1
	tlsMaxFragment          = 1 << 14
	tlsExtServerName        = 0x0000
	tlsExtECH               = 0xfe0d
	tlsServerNameHost       = 0
)

// clientHelloMax bounds what is read before the hello is whole: records and their headers together.
const clientHelloMax = 16 << 10

// The reasons a tunnel is dropped after its 200 (ux §1.4 I): no sentence, evidence and log only.
const (
	egressNotTLS      = "egress_not_tls"
	egressSNIMismatch = "egress_sni_mismatch"
	egressECH         = "egress_ech"
)

// readClientHello reads TLS handshake records from r until the first handshake message is whole. It
// returns every byte it read, to be passed on, and the message's body, or egress_not_tls for a stream
// that is not TLS handshake records carrying a ClientHello within clientHelloMax bytes. It reads
// nothing past the record that completes the message.
func readClientHello(r io.Reader) (raw, body []byte, code string) {
	var hs []byte
	for {
		var hdr [5]byte
		if len(raw)+len(hdr) > clientHelloMax {
			return raw, nil, egressNotTLS
		}
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return raw, nil, egressNotTLS
		}
		raw = append(raw, hdr[:]...)
		n := int(hdr[3])<<8 | int(hdr[4])
		// The first byte of every record before the hello is whole is a handshake record's; a record is
		// never empty (RFC 8446 §5.1) nor longer than a fragment.
		if hdr[0] != tlsRecordHandshake || hdr[1] != 3 || n == 0 || n > tlsMaxFragment || len(raw)+n > clientHelloMax {
			return raw, nil, egressNotTLS
		}
		start := len(raw)
		raw = append(raw, make([]byte, n)...)
		if _, err := io.ReadFull(r, raw[start:]); err != nil {
			return raw[:start], nil, egressNotTLS
		}
		hs = append(hs, raw[start:]...)
		if len(hs) < 4 {
			continue
		}
		if hs[0] != tlsHandshakeClientHello {
			return raw, nil, egressNotTLS
		}
		total := 4 + (int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3]))
		switch {
		case total > clientHelloMax:
			return raw, nil, egressNotTLS
		case len(hs) == total:
			return raw, hs[4:], ""
		case len(hs) > total:
			// A client sends nothing after its ClientHello before the server answers.
			return raw, nil, egressNotTLS
		}
	}
}

// clientHelloInfo is what CN-5 needs of a ClientHello.
type clientHelloInfo struct {
	// sni is the one host_name, "" unless the hello has exactly one server_name extension holding exactly
	// one name, of type host_name.
	sni string
	// ech: the hello carries encrypted_client_hello.
	ech bool
}

// parseClientHello reads a ClientHello body (RFC 8446 §4.1.2; TLS 1.2 is the same up to its extensions,
// which it may leave out). ok is false for a body that is not one, whole and nothing after it, or that
// repeats an extension.
func parseClientHello(body []byte) (info clientHelloInfo, ok bool) {
	p := tlsParser{b: body}
	p.skip(2 + 32) // legacy_version, random
	if sid := p.vec8(); len(sid) > 32 {
		return info, false
	}
	if cs := p.vec16(); len(cs) < 2 || len(cs)%2 != 0 {
		return info, false
	}
	if cm := p.vec8(); len(cm) < 1 {
		return info, false
	}
	if p.bad {
		return info, false
	}
	if p.empty() {
		return info, true // no extensions: no server_name
	}
	exts := tlsParser{b: p.vec16()}
	if p.bad || !p.empty() {
		return info, false
	}
	seen := map[int]bool{}
	for !exts.empty() {
		typ := exts.u16()
		data := exts.vec16()
		if exts.bad || seen[typ] {
			return info, false
		}
		seen[typ] = true
		switch typ {
		case tlsExtServerName:
			host, n, wellFormed := serverNameList(data)
			if !wellFormed {
				return info, false
			}
			if n == 1 {
				info.sni = host
			}
		default:
			if tlsExtIsECH(uint16(typ)) {
				info.ech = true
			}
		}
	}
	return info, true
}

// tlsExtIsECH is whether typ is an encrypted-hello extension: the final ECH number, the numbers the ECH
// drafts used (0xfe08 to 0xfe0c, 0xff02, 0xff03) and the ESNI draft's (0xffce). A server that still reads
// a draft number would hide the host name the same way (T-1 EXE F-4).
func tlsExtIsECH(typ uint16) bool {
	switch typ {
	case tlsExtECH, 0xfe08, 0xfe09, 0xfe0a, 0xfe0b, 0xfe0c, 0xff02, 0xff03, 0xffce:
		return true
	}
	return false
}

// serverNameList reads a server_name extension (RFC 6066 §3): the host_name when the list holds exactly
// one entry and it is of that type, and the number of entries. Every entry of whatever type begins with
// a 16-bit length.
func serverNameList(data []byte) (host string, n int, ok bool) {
	p := tlsParser{b: data}
	list := tlsParser{b: p.vec16()}
	if p.bad || !p.empty() || list.empty() {
		return "", 0, false
	}
	for !list.empty() {
		typ := list.u8()
		name := list.vec16()
		if list.bad || len(name) == 0 {
			return "", 0, false
		}
		n++
		if n == 1 && typ == tlsServerNameHost {
			host = string(name)
		} else {
			host = ""
		}
	}
	return host, n, true
}

// checkClientHello is CN-5 for a whole ClientHello body and the CONNECT's host (an allowed entry, lower
// case): "" when the tunnel may go on, else the code it is dropped with.
func checkClientHello(body []byte, host string) string {
	info, ok := parseClientHello(body)
	switch {
	case !ok:
		return egressNotTLS
	case info.ech:
		return egressECH
	case info.sni == "" || !asciiOnly(info.sni) || asciiLower(info.sni) != host:
		return egressSNIMismatch
	}
	return ""
}

// asciiOnly reports s has no byte above 0x7f.
func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// tlsParser reads the big-endian integers and length-prefixed vectors of TLS. Reading past the end sets
// bad and returns zero values.
type tlsParser struct {
	b   []byte
	bad bool
}

func (p *tlsParser) empty() bool { return len(p.b) == 0 }

func (p *tlsParser) take(n int) []byte {
	if p.bad || n < 0 || n > len(p.b) {
		p.bad = true
		return nil
	}
	out := p.b[:n:n]
	p.b = p.b[n:]
	return out
}

func (p *tlsParser) skip(n int) { p.take(n) }

func (p *tlsParser) u8() int {
	b := p.take(1)
	if b == nil {
		return 0
	}
	return int(b[0])
}

func (p *tlsParser) u16() int {
	b := p.take(2)
	if b == nil {
		return 0
	}
	return int(b[0])<<8 | int(b[1])
}

func (p *tlsParser) vec8() []byte  { return p.take(p.u8()) }
func (p *tlsParser) vec16() []byte { return p.take(p.u16()) }

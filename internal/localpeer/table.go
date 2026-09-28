package localpeer

// table.go reads the kernel's TCP socket table in the format of Linux /proc/net/tcp and /proc/net/tcp6.
// The parsing is portable so it is tested everywhere; only Linux has the files (owner_linux.go).

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// TCP states as the table prints them.
const (
	stateEstablished = 0x01
	stateSynRecv     = 0x03
	stateListen      = 0x0A
)

// sockEntry is one row of the table.
//
// inode is 0 for a socket no process holds: a connection still in its listener's accept queue, or a
// request socket. For such a connection the uid column says nothing about the server — kernels before
// 6.10 print 0 (root) there, from a socket that has no file yet — so the owner is the listener's.
type sockEntry struct {
	local, remote netip.AddrPort
	state         uint8
	uid           int
	inode         uint64
}

// scanTable returns the rows of one table file that match keep.
//
// A row is "sl local rem st tx:rx tr:when retrnsmt uid timeout inode …"; the addresses are hex, the IP
// in the kernel's in-memory (network) byte order printed 32 bits at a time as host-order words, the
// port in host order.
func scanTable(r io.Reader, keep func(sockEntry) bool) ([]sockEntry, error) {
	var out []sockEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	first := true
	for sc.Scan() {
		if first { // the header line
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		local, err := parseHexAddr(f[1])
		if err != nil {
			return nil, err
		}
		remote, err := parseHexAddr(f[2])
		if err != nil {
			return nil, err
		}
		st, err := strconv.ParseUint(f[3], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("localpeer: socket table state %q: %w", f[3], err)
		}
		uid, err := strconv.Atoi(f[7])
		if err != nil {
			return nil, fmt.Errorf("localpeer: socket table uid %q: %w", f[7], err)
		}
		inode, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("localpeer: socket table inode %q: %w", f[9], err)
		}
		e := sockEntry{local: local, remote: remote, state: uint8(st), uid: uid, inode: inode}
		if keep(e) {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// parseHexAddr parses "0100007F:9B63" (IPv4) or a 32-digit IPv6 form, returning the address unmapped.
func parseHexAddr(s string) (netip.AddrPort, error) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("localpeer: socket table address %q", s)
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("localpeer: socket table port %q: %w", s, err)
	}
	var b [16]byte
	switch len(h) {
	case 8, 32:
	default:
		return netip.AddrPort{}, errors.New("localpeer: socket table address " + strconv.Quote(s))
	}
	for i := 0; i < len(h)/8; i++ {
		w, err := strconv.ParseUint(h[8*i:8*i+8], 16, 32)
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("localpeer: socket table address %q: %w", s, err)
		}
		binary.NativeEndian.PutUint32(b[4*i:], uint32(w))
	}
	var a netip.Addr
	if len(h) == 8 {
		a = netip.AddrFrom4([4]byte(b[:4]))
	} else {
		a = netip.AddrFrom16(b).Unmap()
	}
	return netip.AddrPortFrom(a, uint16(port)), nil
}

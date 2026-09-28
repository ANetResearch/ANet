//go:build !no_a2a

package a2a

// addr.go keeps the interface where its clients were told it is. A client's
// configuration holds the address and the token — Hermes' a2a_agents
// entries, a script, an agent framework's settings — so both survive a
// restart: the port is written to a2a_addr.txt the first time it is chosen
// and bound again every time after, and the token is made once
// (A2A-DESIGN §11.1, X5).
//
// Those clients send the token to the recorded port without asking who is
// listening there; they are not ours to change. So the interface never
// leaves the recorded port for another one while clients still point at it
// [redteam:F18]. Moving used to be silent: the clients went on sending
// Authorization: Bearer <token> to whoever took the old port, and the token
// kept working on the new one. Now a taken recorded port means the
// interface does not start (portTaken, in module.go), and whenever the
// port does change — a2a_addr.txt was removed — the token changes with it,
// so a token that clients may have sent to the old port is worth nothing.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/localpeer"
	"github.com/ANetResearch/ANet/internal/loopguard"
)

// The interface's files in its state directory. `anet doctor` and
// `anet agents wire` read them, and other identities' allocators read the
// address, all through internal/anethome, which names them once for every
// build (no_a2a included).
const (
	AddrFile     = anethome.A2AAddrFile
	TokenFile    = anethome.A2ATokenFile
	ConflictFile = anethome.A2AConflictFile
)

// Where a first start looks for a free port: 43811-45810 (the range
// docs/notes/0014 B3-07 names), clear of the control plane's own scan range
// (39811-41810, internal/daemon controlPortBase) with room between them, so
// the two allocators never compete.
const (
	portBase = 43811
	portSpan = 2000
)

// listen binds the interface's address.
//
// A recorded address is bound again, or not at all: if another process holds
// its port, listen returns a *portTakenError and the caller leaves the
// interface down rather than move it (see the top of this file). Only when
// no address is recorded is one chosen, recorded, and reported as fresh, so
// that the caller replaces a token that clients may hold for another port.
// An address that is not loopback is refused.
func listen(dir string) (ln net.Listener, fresh bool, err error) {
	path := filepath.Join(dir, AddrFile)
	recorded, err := readSmallFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, false, err
	default:
		addr := strings.TrimSpace(recorded)
		if err := checkLoopbackAddr(addr); err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, false, nil
		}
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, false, &portTakenError{addr: addr, err: err}
		}
		return nil, false, err
	}
	ln, port, err := allocate(otherIdentityPorts(dir))
	if err != nil {
		return nil, false, err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if err := writePrivateFile(path, []byte(addr+"\n")); err != nil {
		_ = ln.Close()
		return nil, false, err
	}
	return ln, true, nil
}

// portTakenError is listen's report that another process holds the recorded
// address.
type portTakenError struct {
	addr string
	err  error
}

func (e *portTakenError) Error() string { return e.addr + " is taken: " + e.err.Error() }
func (e *portTakenError) Unwrap() error { return e.err }

// portHolder says who holds a taken address, and whether that could be
// another local user. It could, unless the socket table shows only this
// uid's listeners on it; where there is no socket table (not Linux) the
// holder cannot be named, and is treated as another user.
func portHolder(addr string) (who string, otherUser bool) {
	uids, err := localpeer.ListenerUIDs(addr)
	if err != nil {
		return "a process whose owner this system cannot name", true
	}
	for _, u := range uids {
		if u != os.Getuid() {
			return fmt.Sprintf("a process of uid %d (another local user)", u), true
		}
	}
	if len(uids) == 0 {
		return "a process that has let go of it since", true
	}
	return "another process of this user", false
}

// allocate binds the first free loopback port in the scan range that no
// other identity has recorded. It holds what it finds: the bind is what
// arbitrates between two daemons starting at the same moment.
func allocate(used map[int]bool) (net.Listener, int, error) {
	for p := portBase; p < portBase+portSpan; p++ {
		if used[p] {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			continue
		}
		return ln, p, nil
	}
	return nil, 0, fmt.Errorf("no free port for the local A2A interface in %d-%d", portBase, portBase+portSpan-1)
}

// checkLoopbackAddr accepts host:port on 127.0.0.1, localhost or [::1]
// (internal/loopguard, the control plane's rule) with a real port: unlike
// the control address, a recorded address is what clients were given.
func checkLoopbackAddr(addr string) error {
	if err := loopguard.CheckLoopbackAddr(addr); err != nil {
		if errors.Is(err, loopguard.ErrNotLoopback) {
			return fmt.Errorf("%q is not a loopback address; the local A2A interface listens only on 127.0.0.1, localhost or [::1]", addr)
		}
		return fmt.Errorf("%q is not host:port", addr)
	}
	_, ps, _ := net.SplitHostPort(addr)
	if p, err := strconv.Atoi(ps); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("%q has no valid port", addr)
	}
	return nil
}

// otherIdentityPorts are the ports other identities of this user have
// recorded for their own local A2A interface, so a first start does not
// take the port of an identity that is merely stopped.
//
// It sees the identities under ANET_HOME (or ~/.anet) through
// internal/anethome, the same walk the control plane's allocator makes
// (internal/daemon ListIdentities); a daemon run from an unrelated data
// directory is found only by the bind itself.
func otherIdentityPorts(selfDir string) map[int]bool {
	used := map[int]bool{}
	self := canonical(selfDir)
	for _, id := range anethome.Identities() {
		dir := anethome.ModuleDir(id.Dir, name)
		if canonical(dir) == self {
			continue
		}
		b, err := readSmallFile(filepath.Join(dir, AddrFile))
		if err != nil {
			continue
		}
		if _, ps, err := net.SplitHostPort(strings.TrimSpace(b)); err == nil {
			if p, err := strconv.Atoi(ps); err == nil {
				used[p] = true
			}
		}
	}
	return used
}

func canonical(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return filepath.Clean(p)
}

// tokenBytes is the size of a new token: 32 random bytes, hex.
const tokenBytes = 32

// loadOrCreateToken returns the interface's bearer token, making it on the
// first start. The file is 0600 in a 0700 directory; a token that other
// users could read would authorize them to send work and agent-tier
// payments as this node.
//
// Rotation is deleting the file and restarting, or rotateToken; clients
// then need the new token (anet agents wire --refresh).
func loadOrCreateToken(dir string) (string, error) {
	path := filepath.Join(dir, TokenFile)
	for attempt := 0; attempt < 2; attempt++ {
		b, err := readSmallFile(path)
		if err == nil {
			tok := strings.TrimSpace(b)
			if len(tok) < 32 || strings.ContainsAny(tok, " \t\r\n\"'\\") {
				return "", fmt.Errorf("%s does not hold a token; delete it to have a new one made", path)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return "", err
			}
			return tok, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		raw := make([]byte, tokenBytes)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		tok := hex.EncodeToString(raw)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue // made by a concurrent start; read that one
		}
		if err != nil {
			return "", err
		}
		_, werr := f.WriteString(tok + "\n")
		cerr := f.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(path)
			return "", errors.Join(werr, cerr)
		}
		return tok, nil
	}
	return "", fmt.Errorf("%s: could not be read or made", path)
}

// rotateToken replaces the token with a new one. The old one stops working
// the moment the interface next starts with the file, which is the point:
// it is called when clients may have handed the old one to somebody else.
func rotateToken(dir string) (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := writePrivateFile(filepath.Join(dir, TokenFile), []byte(tok+"\n")); err != nil {
		return "", err
	}
	return tok, nil
}

// maxStateFile bounds what is read from a state file: an address or a
// token, never more than a line.
const maxStateFile = 4096

// readSmallFile reads a regular file, refusing a symbolic link: the state
// directory is private, and a link in it pointing elsewhere is not
// something to follow with a credential.
func readSmallFile(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > maxStateFile {
		return "", fmt.Errorf("%s is too large to be an address or a token", path)
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

// writePrivateFile replaces path with b, 0600, through a rename so that a
// reader never sees half an address.
func writePrivateFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

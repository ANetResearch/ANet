//go:build !no_a2a

package a2a

// addr.go keeps the interface where its clients were told it is. A client's
// configuration holds the address and the token — Hermes' a2a_agents
// entries, a script, an agent framework's settings — so both survive a
// restart: the port is written to a2a_addr.txt the first time it is chosen
// and bound again every time after, and the token is made once
// (A2A-DESIGN §11.1, X5).

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// The interface's files in its state directory. `anet doctor` and
// `anet agents wire` read them.
const (
	AddrFile  = "a2a_addr.txt"
	TokenFile = "a2a_token.txt"
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
// A recorded address is bound again. If its port has been taken since, and
// it is one this allocator chose (loopback, inside the scan range), a new
// one is chosen, recorded and logged — the clients configured with the old
// one stop working, which `anet doctor` reports; refusing to start would
// take the whole node down over a port. An address somebody wrote into the
// file by hand is a decision: a collision there is an error, the rule the
// control plane follows for its own address. An address that is not
// loopback is refused either way.
func listen(dir string) (net.Listener, error) {
	path := filepath.Join(dir, AddrFile)
	recorded, err := readSmallFile(path)
	old := ""
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		addr := strings.TrimSpace(recorded)
		if err := checkLoopbackAddr(addr); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) || !autoAssigned(addr) {
			return nil, err
		}
		old = addr
	}
	ln, port, err := allocate(otherIdentityPorts(dir))
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if err := writePrivateFile(path, []byte(addr+"\n")); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if old != "" {
		log.Printf("anet: a2a: %s was taken; the local A2A interface moved to %s and %s was updated. "+
			"Clients configured with the old address must be updated (anet agents wire --refresh)", old, addr, path)
	}
	return ln, nil
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

// autoAssigned reports whether an address looks like one allocate chose.
func autoAssigned(addr string) bool {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		return false
	}
	p, err := strconv.Atoi(ps)
	return err == nil && p >= portBase && p < portBase+portSpan
}

// checkLoopbackAddr accepts host:port on 127.0.0.1, localhost or [::1].
func checkLoopbackAddr(addr string) error {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if p, err := strconv.Atoi(ps); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("%q has no valid port", addr)
	}
	if !loopbackName(host) {
		return fmt.Errorf("%q is not a loopback address; the local A2A interface listens only on 127.0.0.1, localhost or [::1]", addr)
	}
	return nil
}

// otherIdentityPorts are the ports other identities of this user have
// recorded for their own local A2A interface, so a first start does not
// take the port of an identity that is merely stopped.
//
// It sees the identities under ANET_HOME (or ~/.anet), the same set the
// control plane's allocator consults (internal/daemon AnetHome and
// ListIdentities); a daemon run from an unrelated data directory is found
// only by the bind itself.
func otherIdentityPorts(selfDir string) map[int]bool {
	used := map[int]bool{}
	home := anetHome()
	roots := []string{home}
	if ents, err := os.ReadDir(filepath.Join(home, "ids")); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				roots = append(roots, filepath.Join(home, "ids", e.Name()))
			}
		}
	}
	self := canonical(selfDir)
	for _, r := range roots {
		// The layout of a module's state directory: <data dir>/modules/<name>
		// (internal/daemon moduleStateDir).
		dir := filepath.Join(r, "modules", name)
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

// anetHome is the identity container: ANET_HOME, else ~/.anet (the
// daemon's AnetHome, which this package may not import).
func anetHome() string {
	if d := os.Getenv("ANET_HOME"); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return ".anet"
	}
	return filepath.Join(h, ".anet")
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
// Rotation is deleting the file and restarting; clients then need the new
// token (anet agents wire --refresh).
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

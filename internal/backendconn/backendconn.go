// Package backendconn connects the daemon to the local services behind it — the service module's
// backends and module/a2a's provider-side backends (A2A-DESIGN §11.6, §15) — and makes sure the process
// on the far side is the one the operator meant before a token, a call's arguments or a task's text is
// written to it (docs/notes/0030 N1).
//
// A loopback TCP port is not private. While a backend is down — a restart, an upgrade, a crash — any
// local account can bind its port, and the next call hands that account the daemon's bearer token, the
// caller's arguments or task, and the chance to answer in the backend's place, an answer the daemon then
// signs a receipt for. So the recommended and default form of a backend URL is a Unix domain socket:
//
//	unix:///run/anet-official/anet-tools/backend.sock                 requests go to / on that socket
//	unix:///run/anet-official/anet-tools/backend.sock:/v1/tools/x     requests go to /v1/tools/x
//
// The path after "unix://" is the socket; an HTTP request path may follow after a colon, as in nginx's
// "unix:/path:/uri". Requests carry "Host: localhost".
//
// Before every new connection to a socket the path is walked from the root, following symbolic links,
// and each entry on it must be one only trusted accounts can replace (checkPath): root, this daemon's
// user, the account named by expected_uid/expected_user, and the socket's own owner where that owner's
// directory sits in one nobody else can write. A directory writable by every user is allowed above the
// socket only when it is sticky (/tmp), and never as the socket's own directory; a group-writable one
// only when socket_group names its group. After connecting, on Linux, SO_PEERCRED gives the uid of the
// process that listens on the socket, which must be expected_uid when it is set and the socket file's
// owner otherwise. A failed check sends nothing.
//
// TCP (http:// and https://) stays possible, but only with allow_tcp: true. A TCP connection that lands
// on a loopback address is then checked the way anet's own clients check the daemon (A2A-DESIGN §7 item
// 10 [redteam:F18]): the kernel's socket table must show the listener owned by this daemon's uid, or
// nothing is sent. Where the table cannot be read (every system but Linux) a loopback TCP backend cannot
// be verified and is refused. A connection to another host is authenticated by TLS, or carries no token
// (the modules refuse a token over plain http to another host).
//
// Connections never go through an HTTP proxy: the checks are about the socket the request travels on.
//
// Standard library and internal/localpeer only; never internal/daemon, a module or an A2A SDK (the deps
// test in internal/loopguard).
package backendconn

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/localpeer"
)

// Policy is what the operator says about a backend's far side, as it appears in a module's
// configuration (the service module's block, each module/a2a backend).
type Policy struct {
	// AllowTCP accepts an http:// or https:// backend URL. Without it only unix:// URLs are accepted:
	// a TCP port can be taken by another local user while the backend is down.
	AllowTCP bool `json:"allow_tcp,omitempty"`
	// ExpectedUID is the uid the backend runs as. When set, the process listening on the socket must
	// run as it (SO_PEERCRED, Linux) and the socket must be its, this daemon's or root's. When neither
	// it nor ExpectedUser is set, the listener must run as the socket file's owner.
	ExpectedUID *int `json:"expected_uid,omitempty"`
	// ExpectedUser is ExpectedUID by name, looked up once at start. A systemd DynamicUser has no stable
	// uid and is not in /etc/passwd: leave both unset for one and rely on the socket's directory.
	ExpectedUser string `json:"expected_user,omitempty"`
	// SocketGroup is a group (name or number) whose members may write the socket's directory, or a
	// directory above it: a group-writable directory on the path is refused unless it is this group's.
	// Members of the group are trusted as much as the backend itself.
	SocketGroup string `json:"socket_group,omitempty"`
}

// Rules is a Policy with its names looked up, ready to check connections.
type Rules struct {
	allowTCP bool
	expected int // -1: none
	group    int // -1: none
	self     int
}

// Resolve looks up the names in p. A user or group that does not exist is an error: the operator named
// an account the checks could never match.
func (p Policy) Resolve() (Rules, error) {
	r := Rules{allowTCP: p.AllowTCP, expected: -1, group: -1, self: os.Getuid()}
	if p.ExpectedUID != nil && p.ExpectedUser != "" {
		return r, errors.New("expected_uid and expected_user name the same thing; set one")
	}
	if p.ExpectedUID != nil {
		if *p.ExpectedUID < 0 {
			return r, fmt.Errorf("expected_uid %d is not a uid", *p.ExpectedUID)
		}
		r.expected = *p.ExpectedUID
	}
	if p.ExpectedUser != "" {
		u, err := user.Lookup(p.ExpectedUser)
		if err != nil {
			return r, fmt.Errorf("expected_user %q: %w", p.ExpectedUser, err)
		}
		id, err := strconv.Atoi(u.Uid)
		if err != nil {
			return r, fmt.Errorf("expected_user %q: uid %q is not a number on this system", p.ExpectedUser, u.Uid)
		}
		r.expected = id
	}
	if g := strings.TrimSpace(p.SocketGroup); g != "" {
		id, err := strconv.Atoi(g)
		if err != nil {
			grp, lerr := user.LookupGroup(g)
			if lerr != nil {
				return r, fmt.Errorf("socket_group %q: %w", g, lerr)
			}
			if id, err = strconv.Atoi(grp.Gid); err != nil {
				return r, fmt.Errorf("socket_group %q: gid %q is not a number on this system", g, grp.Gid)
			}
		}
		if id < 0 {
			return r, fmt.Errorf("socket_group %q is not a group", g)
		}
		r.group = id
	}
	return r, nil
}

// Target is a parsed backend URL.
type Target struct {
	// Raw is the URL as configured.
	Raw string
	// Socket is the socket's path, for a unix:// URL; "" for http(s).
	Socket string
	// URL is where HTTP requests go: the configured URL for http(s), http://localhost<path> for a
	// socket (the transport dials the socket whatever the host).
	URL *url.URL
}

// Unix reports a unix:// target.
func (t Target) Unix() bool { return t.Socket != "" }

// SchemeUnix is the scheme of a socket backend URL.
const SchemeUnix = "unix"

// unixHost is the Host of requests to a socket. "localhost", so a service that answers only a loopback
// Host (cmd/anet-official) answers them.
const unixHost = "localhost"

// maxSocketPath is the longest socket path the kernel accepts: sun_path less its terminating NUL.
func maxSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// Parse reads a backend URL: unix:///abs/path[:/request/path], http://… or https://….
func Parse(raw string) (Target, error) {
	if rest, ok := strings.CutPrefix(raw, SchemeUnix+"://"); ok {
		return parseUnix(raw, rest)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Target{}, fmt.Errorf("url %q is not a unix:// or http(s) URL", raw)
	}
	if u.User != nil {
		return Target{}, fmt.Errorf("url %q carries credentials; use token_file", raw)
	}
	return Target{Raw: raw, URL: u}, nil
}

func parseUnix(raw, rest string) (Target, error) {
	if !strings.HasPrefix(rest, "/") {
		return Target{}, fmt.Errorf("url %q: a socket URL is unix:///absolute/path (three slashes), optionally followed by :/request/path", raw)
	}
	sock, reqPath, _ := strings.Cut(rest, ":")
	if filepath.Clean(sock) != sock || strings.ContainsAny(sock, "?#%\\") || strings.IndexFunc(sock, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return Target{}, fmt.Errorf("url %q: the socket path must be clean and absolute, without spaces, control characters or ?#%%\\", raw)
	}
	if sock == "/" {
		return Target{}, fmt.Errorf("url %q names no socket", raw)
	}
	if len(sock) > maxSocketPath() {
		return Target{}, fmt.Errorf("url %q: the socket path is %d bytes, more than the %d a Unix socket address holds", raw, len(sock), maxSocketPath())
	}
	if reqPath == "" {
		reqPath = "/"
	}
	if !strings.HasPrefix(reqPath, "/") || strings.Contains(reqPath, "#") {
		return Target{}, fmt.Errorf("url %q: what follows the socket path is \":/request/path\"", raw)
	}
	u, err := url.Parse("http://" + unixHost + reqPath)
	if err != nil || u.Host != unixHost {
		return Target{}, fmt.Errorf("url %q: bad request path %q", raw, reqPath)
	}
	return Target{Raw: raw, Socket: sock, URL: u}, nil
}

// Admit refuses, at configuration time, a target these rules do not accept: TCP without allow_tcp.
func (r Rules) Admit(t Target) error {
	if !t.Unix() && !r.allowTCP {
		return fmt.Errorf("url %q is reached over TCP, where another local user can take the port while the backend is down; "+
			"serve it on a Unix socket and write unix:///path/to/socket (the recommended form), or set allow_tcp: true "+
			"(a listener on loopback must then run as this daemon's user)", t.Raw)
	}
	return nil
}

// AllowsTCP reports allow_tcp.
func (r Rules) AllowsTCP() bool { return r.allowTCP }

// ErrRefused is wrapped by every error that refuses a backend's far side; nothing was sent to it.
var ErrRefused = errors.New("backend refused before anything was sent")

// RefusedError says which backend was refused and why.
type RefusedError struct {
	Where  string // the socket path or the TCP address
	Reason string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("backend %s refused, nothing was sent: %s", e.Where, e.Reason)
}

func (e *RefusedError) Unwrap() error { return ErrRefused }

func refuse(where, format string, a ...any) error {
	return &RefusedError{Where: where, Reason: fmt.Sprintf(format, a...)}
}

// Transport returns an HTTP transport for t whose every new connection is checked before it is used:
// the socket's path and listener for a unix:// target, the listener's owner for TCP on loopback. It
// never uses a proxy. One transport per target keeps its connections pooled.
func (r Rules) Transport(t Target) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if t.Unix() {
		sock := t.Socket
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return r.DialUnix(ctx, d, sock)
		}
		return tr
	}
	tr.ForceAttemptHTTP2 = true
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if err := r.checkTCP(c); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	}
	return tr
}

// Client is an HTTP client over Transport(t) that follows no redirect: a redirect would take the
// request, token and all, somewhere these checks never looked at. A zero timeout means none.
func (r Rules) Client(t Target, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport:     r.Transport(t),
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// DialUnix checks sock's path, connects, and checks the listener (SO_PEERCRED on Linux).
func (r Rules) DialUnix(ctx context.Context, d *net.Dialer, sock string) (net.Conn, error) {
	real, owner, err := r.CheckPath(sock)
	if err != nil {
		return nil, err
	}
	if d == nil {
		d = &net.Dialer{Timeout: 10 * time.Second}
	}
	c, err := d.DialContext(ctx, "unix", real)
	if err != nil {
		return nil, err
	}
	if err := r.checkPeer(c, sock, owner); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// checkPeer compares the listener's uid with the expected one, or with the socket file's owner.
func (r Rules) checkPeer(c net.Conn, sock string, owner int) error {
	uid, gid, err := peerCred(c)
	if errors.Is(err, errNoPeerCred) {
		return nil // not Linux: the path checks stand alone
	}
	if err != nil {
		return refuse(sock, "cannot read the listener's credentials (SO_PEERCRED): %v", err)
	}
	want, what := owner, "the socket file's owner"
	if r.expected >= 0 {
		want, what = r.expected, "expected_uid/expected_user"
	}
	if uid != want {
		return refuse(sock, "the process listening on it runs as uid %d (gid %d), not uid %d (%s)", uid, gid, want, what)
	}
	return nil
}

// checkTCP checks a TCP connection before anything is written to it: a listener on loopback must be
// this daemon's user's; another host is left to TLS, or gets no token (the modules' URL rules).
func (r Rules) checkTCP(c net.Conn) error {
	ta, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return refuse(c.RemoteAddr().String(), "not a TCP connection")
	}
	if !ta.IP.IsLoopback() {
		return nil
	}
	where := ta.String()
	uid, err := localpeer.OwnerUID(c)
	switch {
	case errors.Is(err, localpeer.ErrNoSocketTable):
		return refuse(where, "this system has no socket table to tell who holds a loopback port (only Linux does); serve the backend on a Unix socket (unix:///path)")
	case err != nil:
		return refuse(where, "%v", err)
	case uid != r.self:
		return refuse(where, "the port is held by uid %d, not by this daemon's uid %d (another local user may have taken it while the backend was down); "+
			"a backend that runs as another user belongs on a Unix socket shared with it (unix:///path)", uid, r.self)
	}
	return nil
}

// meta is what the checks read of a path entry.
type meta struct {
	uid, gid int
	mode     fs.FileMode // type bits, permission bits and ModeSticky, as os.Lstat reports them
}

// entry is one step of a walked path: a directory, a symbolic link or the socket, and the index of
// the directory it was found in (-1 for the root).
type entry struct {
	path   string
	m      meta
	parent int
}

// fsys is the part of the file system a walk reads; tests substitute one.
type fsys interface {
	lstat(path string) (meta, error)
	readlink(path string) (string, error)
}

// maxLinks bounds the symbolic links one walk follows, as the kernel does (ELOOP).
const maxLinks = 40

// CheckPath walks sock from the root and applies the ownership and permission rules, without
// connecting. It returns the socket's real path (symbolic links resolved: the path that is dialled is
// the one that was checked) and the socket file's owner.
func (r Rules) CheckPath(sock string) (real string, owner int, err error) {
	return r.checkPath(osFS{}, sock)
}

func (r Rules) checkPath(f fsys, sock string) (string, int, error) {
	entries, err := walk(f, sock)
	if err != nil {
		return "", 0, err
	}
	if err := r.judge(sock, entries); err != nil {
		return "", 0, err
	}
	last := entries[len(entries)-1]
	return last.path, last.m.uid, nil
}

// walk resolves sock component by component from the root, recording every directory, link and the
// socket it passes. A link's target is walked in its turn: whoever could change the link is judged
// through the directory it sits in, which is on the list.
func walk(f fsys, sock string) ([]entry, error) {
	if !filepath.IsAbs(sock) {
		return nil, refuse(sock, "not an absolute path")
	}
	root, err := f.lstat("/")
	if err != nil {
		return nil, refuse(sock, "%v", err)
	}
	entries := []entry{{path: "/", m: root, parent: -1}}
	cur := 0 // index of the directory the walk is in
	comps := split(sock)
	links := 0
	for len(comps) > 0 {
		c := comps[0]
		comps = comps[1:]
		if c == "." {
			continue
		}
		if c == ".." {
			// Up from cur: to the entry cur was found in. Its parent is on the list already, so
			// nothing new needs judging.
			if p := entries[cur].parent; p >= 0 {
				cur = p
			}
			continue
		}
		p := filepath.Join(entries[cur].path, c)
		m, err := f.lstat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("backend socket %s: %s does not exist (is the backend running?)", sock, p)
			}
			return nil, refuse(sock, "%v", err)
		}
		e := entry{path: p, m: m, parent: cur}
		switch {
		case m.mode&fs.ModeSymlink != 0:
			links++
			if links > maxLinks {
				return nil, refuse(sock, "more than %d symbolic links", maxLinks)
			}
			t, err := f.readlink(p)
			if err != nil {
				return nil, refuse(sock, "%v", err)
			}
			entries = append(entries, e)
			if filepath.IsAbs(t) {
				cur = 0
			}
			comps = append(split(t), comps...)
		case m.mode.IsDir():
			entries = append(entries, e)
			cur = len(entries) - 1
			if len(comps) == 0 {
				return nil, refuse(sock, "%s is a directory, not a socket", p)
			}
		case len(comps) == 0 && m.mode&fs.ModeSocket != 0:
			return append(entries, e), nil
		case len(comps) == 0:
			return nil, refuse(sock, "%s is not a socket (mode %s)", p, m.mode)
		default:
			return nil, refuse(sock, "%s is not a directory", p)
		}
	}
	return nil, refuse(sock, "names no socket")
}

func split(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// judge applies the rules to a walked path whose last entry is the socket.
//
// Trusted outright: root, this daemon's user, and the expected backend user. The socket's owner is
// trusted for the entries it owns only where they sit in a directory nobody else can write: otherwise
// anyone could create a directory of their own in /tmp and a socket in it, and be its "owner".
func (r Rules) judge(sock string, entries []entry) error {
	last := len(entries) - 1
	owner := entries[last].m.uid
	final := entries[last].parent // the socket's directory
	trusted := func(uid int) bool { return uid == 0 || uid == r.self || (r.expected >= 0 && uid == r.expected) }
	groupOK := func(m meta) bool { return r.group >= 0 && m.gid == r.group }
	// strict: only the directory's owner (and root) can add, rename or remove entries in it, or also
	// the configured group's members.
	strict := func(m meta) bool {
		perm := m.mode.Perm()
		return perm&0o002 == 0 && (perm&0o020 == 0 || groupOK(m))
	}
	if r.expected >= 0 && !trusted(owner) {
		return refuse(sock, "the socket is owned by uid %d, not by the expected uid %d, this daemon's or root", owner, r.expected)
	}
	// With no expected user, the backend's account is the one whose directory the socket is in (a
	// RuntimeDirectory), or a member of socket_group where that group may write it. A socket of anyone
	// else in a trusted directory was left there when the directory was not (and would pass SO_PEERCRED,
	// its owner listening).
	if fd := entries[final].m; !trusted(owner) && fd.uid != owner && (!groupOK(fd) || fd.mode.Perm()&0o020 == 0) {
		return refuse(sock, "the socket is owned by uid %d, but its directory %s by uid %d; a backend's socket is its own directory's, "+
			"or set expected_uid/expected_user or socket_group", owner, entries[final].path, fd.uid)
	}
	for i, e := range entries {
		if e.m.mode.IsDir() {
			perm := e.m.mode.Perm()
			sticky := e.m.mode&fs.ModeSticky != 0
			// A sticky directory lets others add entries but not replace or remove ones they do not own,
			// so it is allowed above the socket, never as the socket's own directory.
			if perm&0o002 != 0 && (!sticky || i == final) {
				return refuse(sock, "%s is writable by every user (mode %04o)", e.path, perm|stickyBits(e.m.mode))
			}
			if perm&0o020 != 0 && !groupOK(e.m) && (!sticky || i == final) {
				return refuse(sock, "%s is writable by group %d (mode %04o); set socket_group if that group's members are trusted with the backend", e.path, e.m.gid, perm|stickyBits(e.m.mode))
			}
		}
		if trusted(e.m.uid) {
			continue
		}
		if e.m.uid == owner && e.parent >= 0 && strict(entries[e.parent].m) {
			continue
		}
		what := "is owned by"
		if e.m.uid == owner {
			what = "belongs to the socket's owner, but sits in a directory others can write:"
		}
		return refuse(sock, "%s %s uid %d, which is neither root, this daemon's uid %d nor the backend's", e.path, what, e.m.uid, r.self)
	}
	return nil
}

func stickyBits(m fs.FileMode) fs.FileMode {
	if m&fs.ModeSticky != 0 {
		return 0o1000
	}
	return 0
}

// osFS reads the real file system.
type osFS struct{}

func (osFS) lstat(p string) (meta, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return meta{}, err
	}
	uid, gid, ok := owners(fi)
	if !ok {
		return meta{}, fmt.Errorf("%s: this system does not report file owners", p)
	}
	return meta{uid: uid, gid: gid, mode: fi.Mode()}, nil
}

func (osFS) readlink(p string) (string, error) { return os.Readlink(p) }

// errNoPeerCred means this system gives no listener credentials for a Unix socket connection.
var errNoPeerCred = errors.New("backendconn: no peer credentials on this system")

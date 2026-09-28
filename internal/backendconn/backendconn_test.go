package backendconn

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/localpeer"
)

func TestParse(t *testing.T) {
	for raw, want := range map[string][2]string{ // socket, request URL
		"unix:///run/x/backend.sock":                      {"/run/x/backend.sock", "http://localhost/"},
		"unix:///run/x/backend.sock:/v1/tools/text.x":     {"/run/x/backend.sock", "http://localhost/v1/tools/text.x"},
		"unix:///run/x/backend.sock:/a?b=c":               {"/run/x/backend.sock", "http://localhost/a?b=c"},
		"http://127.0.0.1:8080/digest":                    {"", "http://127.0.0.1:8080/digest"},
		"https://agent.example/a2a":                       {"", "https://agent.example/a2a"},
		"unix:///tmp/x.sock:/.well-known/agent-card.json": {"/tmp/x.sock", "http://localhost/.well-known/agent-card.json"},
	} {
		tg, err := Parse(raw)
		if err != nil {
			t.Errorf("Parse(%q): %v", raw, err)
			continue
		}
		if tg.Socket != want[0] || tg.URL.String() != want[1] || tg.Unix() != (want[0] != "") {
			t.Errorf("Parse(%q) = socket %q url %q; want %q %q", raw, tg.Socket, tg.URL, want[0], want[1])
		}
	}
	for _, bad := range []string{
		"unix://run/x.sock", "unix:/run/x.sock", "unix:///", "unix:///run/../x.sock", "unix:///run/x//y.sock",
		"unix:///run/x.sock/", "unix:///run/x y.sock", "unix:///run/x%2e.sock", "unix:///run/x.sock:v1",
		"unix:///run/x.sock:/a#b", "unix:///" + strings.Repeat("a", 120), "ftp://127.0.0.1/", "http://",
		"http://user:pw@127.0.0.1/", "127.0.0.1:8080", "",
	} {
		if tg, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted: %+v", bad, tg)
		}
	}
}

func TestResolve(t *testing.T) {
	uid := 1234
	if r, err := (Policy{ExpectedUID: &uid, SocketGroup: "0"}).Resolve(); err != nil || r.expected != 1234 || r.group != 0 {
		t.Fatalf("Resolve: %+v %v", r, err)
	}
	neg := -1
	for _, p := range []Policy{
		{ExpectedUID: &uid, ExpectedUser: "root"},
		{ExpectedUID: &neg},
		{ExpectedUser: "no-such-user-anet-test"},
		{SocketGroup: "no-such-group-anet-test"},
	} {
		if _, err := p.Resolve(); err == nil {
			t.Errorf("Resolve(%+v) accepted", p)
		}
	}
	r, err := Policy{}.Resolve()
	if err != nil || r.expected != -1 || r.group != -1 || r.self != os.Getuid() || r.allowTCP {
		t.Fatalf("the empty policy: %+v %v", r, err)
	}
}

// TCP is refused unless the operator says otherwise.
func TestTCPIsRefusedByDefault(t *testing.T) {
	r, _ := Policy{}.Resolve()
	for _, raw := range []string{"http://127.0.0.1:9900", "https://agent.example/a2a"} {
		tg, _ := Parse(raw)
		if err := r.Admit(tg); err == nil || !strings.Contains(err.Error(), "allow_tcp") {
			t.Errorf("%s without allow_tcp: %v", raw, err)
		}
	}
	tg, _ := Parse("unix:///run/x.sock")
	if err := r.Admit(tg); err != nil {
		t.Errorf("a socket: %v", err)
	}
	r2, _ := Policy{AllowTCP: true}.Resolve()
	tg, _ = Parse("http://127.0.0.1:9900")
	if err := r2.Admit(tg); err != nil {
		t.Errorf("allow_tcp: %v", err)
	}
}

// server is an HTTP server on a Unix socket that counts the requests it is sent.
type server struct {
	path string
	hits atomic.Int32
	srv  *http.Server
}

func serveUnix(t *testing.T, path string) *server {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{path: path}
	s.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		io.WriteString(w, `{"host":"`+r.Host+`","path":"`+r.URL.Path+`"}`)
	})}
	go s.srv.Serve(ln)
	t.Cleanup(func() { s.srv.Close() })
	return s
}

// privateDir is a fresh directory only this user can write, whatever the umask.
func privateDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func get(t *testing.T, r Rules, raw string) (string, error) {
	t.Helper()
	tg, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Client(tg, 5*time.Second).Get(tg.URL.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func needUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no file owners here")
	}
}

// The recommended form: a socket in a directory only this user writes, answered like any HTTP server,
// with Host: localhost and the request path after the colon.
func TestASocketBackendIsReached(t *testing.T) {
	needUnix(t)
	d := privateDir(t)
	s := serveUnix(t, filepath.Join(d, "b.sock"))
	r, _ := Policy{}.Resolve()
	body, err := get(t, r, "unix://"+s.path+":/v1/echo/net.echo")
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"host":"localhost","path":"/v1/echo/net.echo"}` {
		t.Fatalf("the backend saw %s", body)
	}
	// Through a symbolic link, too: what is dialled is the real path, and the link is judged by the
	// directory it sits in.
	link := filepath.Join(d, "link.sock")
	if err := os.Symlink(s.path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, r, "unix://"+link); err != nil {
		t.Fatalf("through a link: %v", err)
	}
	if real, owner, err := r.CheckPath(link); err != nil || real != s.path || owner != os.Getuid() {
		t.Fatalf("CheckPath through the link: %q %d %v", real, owner, err)
	}
}

// A socket whose directory others can write could have been put there by any of them: refused, and
// the listener is never sent a byte.
func TestAWritableDirectoryIsRefused(t *testing.T) {
	needUnix(t)
	d := privateDir(t)
	s := serveUnix(t, filepath.Join(d, "b.sock"))
	r, _ := Policy{}.Resolve()
	// Writable by others only (0703), by the group only (0730), by both, sticky or not.
	for _, mode := range []fs.FileMode{0o703, fs.ModeSticky | 0o703, 0o777, fs.ModeSticky | 0o777, 0o770, 0o730} {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		_, err := get(t, r, "unix://"+s.path)
		if !errors.Is(err, ErrRefused) {
			t.Errorf("directory mode %v: %v, want refused", mode, err)
		}
	}
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("the listener received %d requests", n)
	}
	// A group-writable directory is accepted when socket_group names its group.
	if err := os.Chmod(d, 0o770); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(d)
	_, gid, _ := owners(fi)
	rg, err := Policy{SocketGroup: strconv.Itoa(gid)}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, rg, "unix://"+s.path); err != nil {
		t.Fatalf("group-writable with socket_group %d: %v", gid, err)
	}
}

// SO_PEERCRED: the process listening on the socket must be the expected one. The test cannot listen as
// a second user, so it expects a uid it is not: the socket is this user's (allowed), the listener is
// not the expected uid, and nothing is sent.
func TestAListenerThatIsNotTheExpectedUserIsRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_PEERCRED is read on Linux only")
	}
	d := privateDir(t)
	s := serveUnix(t, filepath.Join(d, "b.sock"))
	other := os.Getuid() + 1
	r, _ := Policy{ExpectedUID: &other}.Resolve()
	_, err := get(t, r, "unix://"+s.path)
	var re *RefusedError
	if !errors.As(err, &re) || !strings.Contains(re.Reason, "runs as uid") {
		t.Fatalf("a listener of uid %d when %d is expected: %v", os.Getuid(), other, err)
	}
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("the listener received %d requests", n)
	}
	// The same listener, expected as what it is.
	me := os.Getuid()
	r, _ = Policy{ExpectedUID: &me}.Resolve()
	if _, err := get(t, r, "unix://"+s.path); err != nil {
		t.Fatalf("expected_uid = the listener's uid: %v", err)
	}
	// And the default: the listener must be the socket file's owner. A socket this user owns, listened
	// on (as far as the check can tell) by someone else, is refused: checkPeer given another owner.
	c, err := net.Dial("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r0, _ := Policy{}.Resolve()
	if err := r0.checkPeer(c, s.path, other); !errors.Is(err, ErrRefused) {
		t.Fatalf("a listener that is not the socket's owner: %v", err)
	}
	if err := r0.checkPeer(c, s.path, os.Getuid()); err != nil {
		t.Fatalf("the socket's owner listening: %v", err)
	}
}

// A socket that is not there is the backend being down, not an attack: not refused, and says so.
func TestAMissingSocketIsDownNotRefused(t *testing.T) {
	needUnix(t)
	r, _ := Policy{}.Resolve()
	_, err := get(t, r, "unix://"+filepath.Join(privateDir(t), "gone.sock"))
	if err == nil || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "is the backend running") {
		t.Fatalf("a missing socket: %v", err)
	}
	// A regular file where the socket should be is refused.
	f := filepath.Join(privateDir(t), "file.sock")
	os.WriteFile(f, nil, 0o600)
	if _, err := get(t, r, "unix://"+f); !errors.Is(err, ErrRefused) {
		t.Fatalf("a regular file: %v", err)
	}
}

// fakeFS is a file system for judging paths with owners a test cannot create.
type fakeFS map[string]fakeEntry

type fakeEntry struct {
	uid, gid int
	mode     fs.FileMode
	link     string
}

func (f fakeFS) lstat(p string) (meta, error) {
	e, ok := f[p]
	if !ok {
		return meta{}, fs.ErrNotExist
	}
	return meta{uid: e.uid, gid: e.gid, mode: e.mode}, nil
}

func (f fakeFS) readlink(p string) (string, error) { return f[p].link, nil }

const (
	self     = 1000
	stranger = 1500
	dyn      = 61234 // a systemd DynamicUser
	ipc      = 990   // the group the daemon and the backend share
)

func dir(uid int, perm fs.FileMode) fakeEntry {
	return fakeEntry{uid: uid, gid: uid, mode: fs.ModeDir | perm}
}
func sock(uid, gid int) fakeEntry { return fakeEntry{uid: uid, gid: gid, mode: fs.ModeSocket | 0o660} }

func judgeFake(f fakeFS, r Rules, path string) error {
	_, _, err := r.checkPath(f, path)
	return err
}

func TestJudge(t *testing.T) {
	base := func() fakeFS {
		return fakeFS{
			"/":     dir(0, 0o755),
			"/run":  dir(0, 0o755),
			"/tmp":  {uid: 0, gid: 0, mode: fs.ModeDir | fs.ModeSticky | 0o777},
			"/home": dir(0, 0o755),
		}
	}
	rules := Rules{expected: -1, group: -1, self: self}
	cases := []struct {
		name  string
		add   fakeFS
		rules Rules
		path  string
		ok    bool
	}{
		{"deploy/official: RuntimeDirectory of a DynamicUser backend under a root directory",
			fakeFS{"/run/anet-official": dir(0, 0o755), "/run/anet-official/e": dir(dyn, 0o755), "/run/anet-official/e/b.sock": sock(dyn, ipc)},
			rules, "/run/anet-official/e/b.sock", true},
		{"the same with an expected uid the backend is not",
			fakeFS{"/run/anet-official": dir(0, 0o755), "/run/anet-official/e": dir(dyn, 0o755), "/run/anet-official/e/b.sock": sock(dyn, ipc)},
			Rules{expected: dyn + 1, group: -1, self: self}, "/run/anet-official/e/b.sock", false},
		{"the same with the expected uid it is",
			fakeFS{"/run/anet-official": dir(0, 0o755), "/run/anet-official/e": dir(dyn, 0o755), "/run/anet-official/e/b.sock": sock(dyn, ipc)},
			Rules{expected: dyn, group: -1, self: self}, "/run/anet-official/e/b.sock", true},
		{"a directory of this user in /tmp",
			fakeFS{"/tmp/jo": dir(self, 0o700), "/tmp/jo/b.sock": sock(self, self)},
			rules, "/tmp/jo/b.sock", true},
		{"a stranger's directory in /tmp, with the stranger's socket in it (made while the backend was down)",
			fakeFS{"/tmp/jo": dir(stranger, 0o755), "/tmp/jo/b.sock": sock(stranger, stranger)},
			rules, "/tmp/jo/b.sock", false},
		{"a stranger's socket directly in /tmp",
			fakeFS{"/tmp/b.sock": sock(stranger, stranger)},
			rules, "/tmp/b.sock", false},
		{"this user's socket directly in /tmp (the directory itself is everyone's)",
			fakeFS{"/tmp/b.sock": sock(self, self)},
			rules, "/tmp/b.sock", false},
		{"a stranger's socket in this user's directory",
			fakeFS{"/run/me": dir(self, 0o700), "/run/me/b.sock": sock(stranger, stranger)},
			rules, "/run/me/b.sock", false},
		{"a socket owned by a stranger when a backend user is expected",
			fakeFS{"/run/me": dir(self, 0o755), "/run/me/b.sock": sock(stranger, stranger)},
			Rules{expected: dyn, group: -1, self: self}, "/run/me/b.sock", false},
		{"a directory the stranger owns above this user's",
			fakeFS{"/home/s": dir(stranger, 0o755), "/home/s/me": dir(self, 0o700), "/home/s/me/b.sock": sock(self, self)},
			rules, "/home/s/me/b.sock", false},
		{"a group-writable directory without socket_group",
			fakeFS{"/run/g": {uid: 0, gid: ipc, mode: fs.ModeDir | 0o770}, "/run/g/b.sock": sock(dyn, ipc)},
			rules, "/run/g/b.sock", false},
		{"a group-writable directory of socket_group, a member's socket",
			fakeFS{"/run/g": {uid: 0, gid: ipc, mode: fs.ModeDir | 0o770}, "/run/g/b.sock": sock(dyn, ipc)},
			Rules{expected: -1, group: ipc, self: self}, "/run/g/b.sock", true},
		{"a group-writable directory of another group than socket_group",
			fakeFS{"/run/g": {uid: 0, gid: ipc + 1, mode: fs.ModeDir | 0o770}, "/run/g/b.sock": sock(dyn, ipc)},
			Rules{expected: -1, group: ipc, self: self}, "/run/g/b.sock", false},
		{"socket activation: root's socket in root's directory",
			fakeFS{"/run/s": dir(0, 0o755), "/run/s/b.sock": sock(0, ipc)},
			rules, "/run/s/b.sock", true},
		{"a link in /tmp that a stranger made, to the stranger's own socket",
			fakeFS{"/tmp/l": {uid: stranger, mode: fs.ModeSymlink | 0o777, link: "/home/s/b.sock"},
				"/home/s": dir(stranger, 0o700), "/home/s/b.sock": sock(stranger, stranger)},
			rules, "/tmp/l", false},
		{"a link root made (/var/run -> /run)",
			fakeFS{"/var": dir(0, 0o755), "/var/run": {uid: 0, mode: fs.ModeSymlink | 0o777, link: "../run"},
				"/run/me": dir(self, 0o700), "/run/me/b.sock": sock(self, self)},
			rules, "/var/run/me/b.sock", true},
		{"a link loop",
			fakeFS{"/run/a": {uid: 0, mode: fs.ModeSymlink | 0o777, link: "/run/b"}, "/run/b": {uid: 0, mode: fs.ModeSymlink | 0o777, link: "/run/a"}},
			rules, "/run/a", false},
		{"a directory where the socket should be",
			fakeFS{"/run/me": dir(self, 0o700)},
			rules, "/run/me", false},
	}
	for _, c := range cases {
		f := base()
		for k, v := range c.add {
			f[k] = v
		}
		err := judgeFake(f, c.rules, c.path)
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v, want refused", c.name, err)
		}
	}
	// The resolved path is what gets dialled.
	f := base()
	f["/var"] = dir(0, 0o755)
	f["/var/run"] = fakeEntry{uid: 0, mode: fs.ModeSymlink | 0o777, link: "../run"}
	f["/run/me"] = dir(self, 0o700)
	f["/run/me/b.sock"] = sock(self, self)
	if real, owner, err := rules.checkPath(f, "/var/run/me/b.sock"); err != nil || real != "/run/me/b.sock" || owner != self {
		t.Fatalf("resolved: %q %d %v", real, owner, err)
	}
}

// allow_tcp: a loopback listener must be this user's; one another uid holds gets nothing.
func TestATCPListenerOfAnotherUserIsRefused(t *testing.T) {
	if _, err := localpeer.ListenerUIDs("127.0.0.1:1"); err != nil {
		t.Skipf("no socket table here: %v", err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	r, _ := Policy{AllowTCP: true}.Resolve()
	if body, err := get(t, r, srv.URL); err != nil || body != "ok" {
		t.Fatalf("this user's listener: %q %v", body, err)
	}
	srv.CloseClientConnections()
	restore := localpeer.TreatAsForeignForTest(strings.TrimPrefix(srv.URL, "http://"))
	defer restore()
	before := hits.Load()
	// A fresh client: a pooled connection would be the old, verified one.
	_, err := get(t, r, srv.URL)
	var re *RefusedError
	if !errors.As(err, &re) || !strings.Contains(re.Reason, "held by uid") {
		t.Fatalf("another user's listener: %v", err)
	}
	if hits.Load() != before {
		t.Fatal("the listener was sent the request")
	}
}

// A URL naming one of this host's own addresses (its LAN address, not 127.0.0.1) still reaches a local
// listener whose port any local user can take: it is checked as loopback is, not waved through as
// another host.
func TestATCPListenerOnAnAddressOfThisHostIsChecked(t *testing.T) {
	if _, err := localpeer.ListenerUIDs("127.0.0.1:1"); err != nil {
		t.Skipf("no socket table here: %v", err)
	}
	var ip net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			ip = n.IP
			break
		}
	}
	if ip == nil {
		t.Skip("this host has no non-loopback IPv4 address")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", ip, err)
	}
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "ok")
	}))
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	r, _ := Policy{AllowTCP: true}.Resolve()
	if body, err := get(t, r, srv.URL); err != nil || body != "ok" {
		t.Fatalf("this user's listener on %s: %q %v", ln.Addr(), body, err)
	}
	srv.CloseClientConnections()
	defer localpeer.TreatAsForeignForTest(ln.Addr().String())()
	before := hits.Load()
	_, err = get(t, r, srv.URL)
	var re *RefusedError
	if !errors.As(err, &re) || !strings.Contains(re.Reason, "held by uid") {
		t.Fatalf("another user's listener on this host's address %s: %v", ln.Addr(), err)
	}
	if hits.Load() != before {
		t.Fatal("the listener was sent the request")
	}
	// Which connections count as this host's.
	for _, c := range []struct {
		far, local string
		want       bool
	}{
		{"127.0.0.1:80", "127.0.0.1:5000", true},
		{"[::1]:80", "[::1]:5000", true},
		{"0.0.0.0:80", "127.0.0.1:5000", true},
		{"192.0.2.7:80", "192.0.2.7:5000", true},
		{"[::ffff:192.0.2.7]:80", "192.0.2.7:5000", true},
		{"192.0.2.7:443", "198.51.100.3:5000", false},
	} {
		far, _ := net.ResolveTCPAddr("tcp", c.far)
		local, _ := net.ResolveTCPAddr("tcp", c.local)
		if got := onThisHost(far, local); got != c.want {
			t.Errorf("onThisHost(%s, %s) = %v", c.far, c.local, got)
		}
	}
}

// DialUnix is what module code outside HTTP would use; it shares the checks.
func TestDialUnixSharesTheChecks(t *testing.T) {
	needUnix(t)
	d := privateDir(t)
	s := serveUnix(t, filepath.Join(d, "b.sock"))
	r, _ := Policy{}.Resolve()
	c, err := r.DialUnix(context.Background(), nil, s.path)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	os.Chmod(d, 0o777)
	if _, err := r.DialUnix(context.Background(), nil, s.path); !errors.Is(err, ErrRefused) {
		t.Fatalf("world-writable directory: %v", err)
	}
}

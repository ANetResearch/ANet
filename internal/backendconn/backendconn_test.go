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
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/localpeer"
	"github.com/ANetResearch/ANet/internal/peercred"
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

// expected_user is a user name, looked up, or a decimal uid, taken as it is: one /etc/passwd does not
// hold is still a uid (LEC §3.2). peer_check is "" or "path-only". Anything else is a PolicyError naming
// the key, which the daemon reports as config_invalid.
func TestResolve(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p        Policy
		expected int
		group    int
		pathOnly bool
	}{
		{Policy{}, -1, -1, false},
		{Policy{ExpectedUser: "1234", SocketGroup: "0"}, 1234, 0, false},
		{Policy{ExpectedUser: "3999999999"}, 3999999999, -1, false}, // no such account here
		{Policy{ExpectedUser: "0001000"}, 1000, -1, false},
		{Policy{ExpectedUser: "0"}, 0, -1, false},
		{Policy{ExpectedUser: me.Username}, os.Getuid(), -1, false},
		{Policy{ExpectedUser: " " + me.Username + " "}, os.Getuid(), -1, false},
		{Policy{PeerCheck: "path-only"}, -1, -1, true},
		{Policy{PeerCheck: ""}, -1, -1, false},
	} {
		r, err := c.p.Resolve()
		if err != nil || r.expected != c.expected || r.group != c.group || r.pathOnly != c.pathOnly || r.self != os.Getuid() {
			t.Errorf("Resolve(%+v) = %+v %v", c.p, r, err)
		}
	}
	for _, c := range []struct {
		p   Policy
		key string
	}{
		{Policy{ExpectedUser: "no-such-user-anet-test"}, "expected_user"},
		{Policy{ExpectedUser: "-1"}, "expected_user"},
		{Policy{ExpectedUser: "4294967295"}, "expected_user"}, // (uid_t)-1
		{Policy{ExpectedUser: "99999999999999999999999"}, "expected_user"},
		{Policy{SocketGroup: "no-such-group-anet-test"}, "socket_group"},
		{Policy{SocketGroup: "4294967296"}, "socket_group"},
		{Policy{PeerCheck: "strict"}, "peer_check"},
		{Policy{PeerCheck: "Path-Only"}, "peer_check"},
		{Policy{PeerCheck: "none"}, "peer_check"},
	} {
		_, err := c.p.Resolve()
		var pe *PolicyError
		if !errors.As(err, &pe) || pe.Key != c.key {
			t.Errorf("Resolve(%+v): %v, want a PolicyError for %s", c.p, err, c.key)
		}
	}
}

// The three modules' backend keys are one struct, and it has no expected_uid: expected_user takes a
// uid as well (LEC §4.4, M-PC-3). A key added here is a key of all three modules.
func TestPolicyKeysAreTheSameForEveryModule(t *testing.T) {
	var got []string
	ty := reflect.TypeOf(Policy{})
	for i := 0; i < ty.NumField(); i++ {
		tag, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		got = append(got, tag)
	}
	if want := []string{"allow_tcp", "expected_user", "socket_group", "peer_check"}; !slices.Equal(got, want) {
		t.Fatalf("Policy's keys %v, want %v", got, want)
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
	// The directory's group is this user's primary group, a user-private one on Debian and Ubuntu
	// (accepted: TestAUserPrivateGroupIsPrivate); judged here as a group others are in.
	r.users = privateGroups{}
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
	rg.users = privateGroups{}
	if _, err := get(t, rg, "unix://"+s.path); err != nil {
		t.Fatalf("group-writable with socket_group %d: %v", gid, err)
	}
}

// The account listening on the socket must be the expected one (peercred). The test cannot listen as
// a second user without sudo (TestAListenerOfAnotherAccountIsRefusedUnlessPathOnly does), so it
// expects a uid it is not: the socket is this user's (allowed), the listener is not the expected uid,
// and nothing is sent.
func TestAListenerThatIsNotTheExpectedUserIsRefused(t *testing.T) {
	if !peercred.Supported() {
		t.Skip("the listening account is not readable on " + runtime.GOOS)
	}
	d := privateDir(t)
	s := serveUnix(t, filepath.Join(d, "b.sock"))
	other := strconv.Itoa(os.Getuid() + 1)
	r, _ := Policy{ExpectedUser: other}.Resolve()
	_, err := get(t, r, "unix://"+s.path)
	var re *RefusedError
	if !errors.As(err, &re) || re.Code != CodePeerUIDMismatch || re.Args["n"] != strconv.Itoa(os.Getuid()) || re.Args["name"] != other {
		t.Fatalf("a listener of uid %d when %s is expected: %v", os.Getuid(), other, err)
	}
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("the listener received %d requests", n)
	}
	// The same listener, expected as what it is, by number and by name.
	me, _ := user.Current()
	for _, as := range []string{strconv.Itoa(os.Getuid()), me.Username} {
		r, _ = Policy{ExpectedUser: as}.Resolve()
		if _, err := get(t, r, "unix://"+s.path); err != nil {
			t.Fatalf("expected_user %q = the listener's account: %v", as, err)
		}
	}
	// And the default: the listener must be the socket file's owner. A socket this user owns, listened
	// on (as far as the check can tell) by someone else, is refused: checkPeer given another owner.
	c, err := net.Dial("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r0, _ := Policy{}.Resolve()
	if err := r0.checkPeer(c, s.path, os.Getuid()+1); !errors.As(err, &re) || re.Code != CodePeerUIDMismatch ||
		re.Args["name"] != "uid "+strconv.Itoa(os.Getuid()+1) {
		t.Fatalf("a listener that is not the socket's owner: %v", err)
	}
	if err := r0.checkPeer(c, s.path, os.Getuid()); err != nil {
		t.Fatalf("the socket's owner listening: %v", err)
	}
}

// M-PC-1: where the platform cannot say who listens (ErrUnsupported, every platform but Linux, macOS
// and FreeBSD), a socket is refused with peer_cred_unsupported and sent nothing, however good its path,
// unless that backend's peer_check is "path-only". path-only does not ask the listener at all: an
// account that is not the expected one is let through, on the operator's word.
func TestAPlatformThatCannotTellIsRefusedUnlessPathOnly(t *testing.T) {
	needUnix(t)
	d := privateDir(t)
	s := serveUnix(t, filepath.Join(d, "b.sock"))
	old := peerCredOf
	peerCredOf = func(net.Conn) (peercred.Cred, error) {
		return peercred.Cred{UID: -1, GID: -1, PID: -1}, peercred.ErrUnsupported
	}
	t.Cleanup(func() { peerCredOf = old })
	r, _ := Policy{}.Resolve()
	_, err := get(t, r, "unix://"+s.path)
	var re *RefusedError
	if !errors.As(err, &re) || re.Code != CodePeerCredUnsupported || re.Args["path"] != s.path || !errors.Is(err, ErrRefused) {
		t.Fatalf("no listener credentials: %v", err)
	}
	if _, err := r.DialUnix(context.Background(), nil, s.path); !errors.As(err, &re) || re.Code != CodePeerCredUnsupported {
		t.Fatalf("DialUnix without listener credentials: %v", err)
	}
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("the listener received %d requests", n)
	}
	other := strconv.Itoa(os.Getuid() + 1)
	for _, p := range []Policy{{PeerCheck: PeerCheckPathOnly}, {PeerCheck: PeerCheckPathOnly, ExpectedUser: strconv.Itoa(os.Getuid())}} {
		r, _ := p.Resolve()
		if body, err := get(t, r, "unix://"+s.path); err != nil || !strings.Contains(body, `"path":"/"`) {
			t.Fatalf("%+v: %q %v", p, body, err)
		}
	}
	peerCredOf = old
	if !peercred.Supported() {
		return
	}
	r, _ = Policy{PeerCheck: PeerCheckPathOnly, ExpectedUser: other}.Resolve()
	if _, err := get(t, r, "unix://"+s.path); err != nil {
		t.Fatalf("path-only with a listener that is not the expected account: %v", err)
	}
	// The path checks still stand under path-only.
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, r, "unix://"+s.path); !errors.Is(err, ErrRefused) {
		t.Fatalf("path-only, a world-writable directory: %v", err)
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

// privateGroups is a userDB: uid → its user-private group; an account not in it has none.
type privateGroups map[int]int

func (p privateGroups) privateGroup(uid int) (int, bool) {
	g, ok := p[uid]
	return g, ok
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
	rules := Rules{expected: -1, group: -1, self: self, users: privateGroups{}}
	// Debian and Ubuntu: every user has a group of its own (here gid = uid), and umask 002.
	upg := Rules{expected: -1, group: -1, self: self, users: privateGroups{self: self, dyn: dyn, stranger: stranger}}
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
		{"an internal note §5.1: this user's 0775 directory, writable by this user's private group",
			fakeFS{"/home/me": dir(self, 0o750), "/home/me/u": dir(self, 0o775), "/home/me/u/b.sock": sock(self, self)},
			upg, "/home/me/u/b.sock", true},
		{"the same directory where the group has other members",
			fakeFS{"/home/me": dir(self, 0o750), "/home/me/u": dir(self, 0o775), "/home/me/u/b.sock": sock(self, self)},
			rules, "/home/me/u/b.sock", false},
		{"a directory above the socket, writable by this user's private group",
			fakeFS{"/home/me": {uid: self, gid: self, mode: fs.ModeDir | 0o775}, "/home/me/u": dir(self, 0o700), "/home/me/u/b.sock": sock(self, self)},
			upg, "/home/me/u/b.sock", true},
		{"a backend user's 0775 directory, writable by the backend's (the socket owner's) private group",
			fakeFS{"/run/svc": dir(dyn, 0o775), "/run/svc/b.sock": sock(dyn, dyn)},
			upg, "/run/svc/b.sock", true},
		{"this user's directory writable by a stranger's private group",
			fakeFS{"/home/me": {uid: self, gid: stranger, mode: fs.ModeDir | 0o775}, "/home/me/b.sock": sock(self, self)},
			upg, "/home/me/b.sock", false},
		{"a private group does not make a world-writable directory private",
			fakeFS{"/home/me": dir(self, 0o777), "/home/me/b.sock": sock(self, self)},
			upg, "/home/me/b.sock", false},
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

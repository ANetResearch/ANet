package backendconn

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
)

// A user-private group is the user's own: named as the user, the user's primary group, with no member
// but the user and no other account's primary group (the judgement scripts/lib.sh own_dir makes).
// Anything this reading cannot account for is not one.
func TestPrivateGroupOf(t *testing.T) {
	users := []passwdEntry{{"root", 0, 0}, {"ink", 1001, 1001}, {"bob", 1002, 1002}, {"svc", 1003, 1003}, {"web", 33, 33}}
	groups := []groupEntry{{"root", 0, nil}, {"ink", 1001, nil}, {"bob", 1002, []string{"bob", "ink"}},
		{"svcgroup", 1003, nil}, {"www-data", 33, nil}, {"adm", 4, []string{"ink"}}}
	for _, c := range []struct {
		name string
		uid  int
		gid  int
		ok   bool
	}{
		{"ink: its own group, listed by nobody", 1001, 1001, true},
		{"bob: ink is in bob's group", 1002, 0, false},
		{"svc: the group is not named as the user", 1003, 0, false},
		{"an account that is not in the file", 4242, 0, false},
	} {
		gid, ok := privateGroupOf(c.uid, users, groups)
		if ok != c.ok || (ok && gid != c.gid) {
			t.Errorf("%s: %d %v", c.name, gid, ok)
		}
	}
	// Another account has the group as its primary group.
	if _, ok := privateGroupOf(1001, append(users, passwdEntry{"ink2", 1004, 1001}), groups); ok {
		t.Error("a group that is another account's primary group was taken as private")
	}
	// The member list naming the user itself is fine.
	g2 := append([]groupEntry(nil), groups...)
	g2[1].members = []string{"ink"}
	if _, ok := privateGroupOf(1001, users, g2); !ok {
		t.Error("a group whose only member is its user was not taken as private")
	}
	// Two accounts on one uid, or two groups on one gid: whose is it?
	if _, ok := privateGroupOf(1001, append(users, passwdEntry{"alias", 1001, 1001}), groups); ok {
		t.Error("a uid two accounts share")
	}
	if _, ok := privateGroupOf(1001, users, append(groups, groupEntry{"other", 1001, nil})); ok {
		t.Error("a gid two groups share")
	}
}

// The files are read as the system writes them; a file that cannot be read, a NIS compat entry or a
// malformed line means no private group at all.
func TestPrivateGroupFromFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	passwd := write("passwd", "# accounts\nroot:x:0:0:root:/root:/bin/bash\nink:x:1001:1001:Ink,,,:/home/ink:/bin/bash\n\n")
	group := write("group", "root:x:0:\nadm:x:4:syslog,ink\nink:x:1001:\n")
	if gid, ok := (etcFiles{passwd, group}).privateGroup(1001); !ok || gid != 1001 {
		t.Fatalf("private group %d %v", gid, ok)
	}
	for name, db := range map[string]etcFiles{
		"no passwd":       {filepath.Join(dir, "none"), group},
		"no group":        {passwd, filepath.Join(dir, "none")},
		"NIS compat":      {write("passwd-nis", "ink:x:1001:1001::/home/ink:/bin/sh\n+::::::\n"), group},
		"a malformed gid": {passwd, write("group-bad", "ink:x:ten:\n")},
		"a member":        {passwd, write("group-member", "ink:x:1001:bob\n")},
	} {
		if _, ok := db.privateGroup(1001); ok {
			t.Errorf("%s: taken as private", name)
		}
	}
}

// On this system: when this user's primary group is a user-private one by /etc/passwd and /etc/group,
// a socket in a 0775 directory of it is reached (docs/notes/0035 §5.1: Ubuntu's umask 002); a socket
// in one writable by the group without being private stays refused (TestAWritableDirectoryIsRefused).
func TestAUserPrivateGroupIsPrivate(t *testing.T) {
	needUnix(t)
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	gid, ok := systemUsers.privateGroup(os.Getuid())
	if !ok || strconv.Itoa(gid) != u.Gid {
		t.Skipf("%s's primary group %s is not a user-private group here", u.Username, u.Gid)
	}
	d := privateDir(t)
	s := serveUnix(t, filepath.Join(d, "b.sock"))
	if err := os.Chmod(d, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(d, -1, gid); err != nil {
		t.Skip(err)
	}
	r, _ := Policy{}.Resolve()
	if _, err := get(t, r, "unix://"+s.path); err != nil {
		t.Fatalf("a 0775 directory of %s's private group: %v", u.Username, err)
	}
}

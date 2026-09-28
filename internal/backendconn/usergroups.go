package backendconn

// usergroups.go answers the one question about accounts the path checks ask: which group, if any, is a
// user's private group (docs/notes/0035 §5.1).
//
// Debian and Ubuntu give every user a group of their own, named as the user, and a umask of 002, so a
// directory a user makes is 0775 and group-writable by that group. When nobody else is in the group, it
// can write nothing its user cannot, and the directory is as private as a 0755 one. The judgement is the
// one scripts/lib.sh own_dir makes: the group is named as the user, is the user's primary group, lists
// no one but the user, and is no other account's primary group.
//
// It is read from /etc/passwd and /etc/group. An account or a group those files do not hold (one an NSS
// service such as LDAP provides), a file that cannot be read, or a file with NIS compat entries ("+"/"-",
// which pull in members this reading cannot see) all mean "not a private group": the directory is then
// judged as before, and refused unless socket_group names the group.

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// userDB is what the checks read about accounts; tests substitute one.
type userDB interface {
	// privateGroup is uid's user-private group, when it has one.
	privateGroup(uid int) (gid int, ok bool)
}

// etcFiles reads /etc/passwd and /etc/group.
type etcFiles struct{ passwd, group string }

var systemUsers userDB = etcFiles{passwd: "/etc/passwd", group: "/etc/group"}

type passwdEntry struct {
	name     string
	uid, gid int
}

type groupEntry struct {
	name    string
	gid     int
	members []string
}

func (e etcFiles) privateGroup(uid int) (int, bool) {
	users, ok := readPasswd(e.passwd)
	if !ok {
		return 0, false
	}
	groups, ok := readGroup(e.group)
	if !ok {
		return 0, false
	}
	return privateGroupOf(uid, users, groups)
}

// privateGroupOf applies the rule (see the file comment) to the accounts and groups read.
func privateGroupOf(uid int, users []passwdEntry, groups []groupEntry) (int, bool) {
	var me *passwdEntry
	for i := range users {
		if users[i].uid == uid {
			if me != nil {
				return 0, false // two accounts share the uid: whose group is it?
			}
			me = &users[i]
		}
	}
	if me == nil || me.name == "" {
		return 0, false
	}
	var g *groupEntry
	for i := range groups {
		if groups[i].gid == me.gid {
			if g != nil {
				return 0, false // two groups share the gid
			}
			g = &groups[i]
		}
	}
	if g == nil || g.name != me.name {
		return 0, false
	}
	for _, m := range g.members {
		if m != me.name {
			return 0, false
		}
	}
	for _, u := range users {
		if u.gid == me.gid && u.name != me.name {
			return 0, false
		}
	}
	return me.gid, true
}

// readPasswd reads name:x:uid:gid:… lines; ok is false when the file cannot be read or holds a line
// this reading cannot account for.
func readPasswd(path string) ([]passwdEntry, bool) {
	var out []passwdEntry
	ok := eachLine(path, func(f []string) bool {
		if len(f) < 4 {
			return false
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			return false
		}
		out = append(out, passwdEntry{name: f[0], uid: uid, gid: gid})
		return true
	})
	return out, ok
}

// readGroup reads name:x:gid:member,… lines, as readPasswd.
func readGroup(path string) ([]groupEntry, bool) {
	var out []groupEntry
	ok := eachLine(path, func(f []string) bool {
		if len(f) < 4 {
			return false
		}
		gid, err := strconv.Atoi(f[2])
		if err != nil {
			return false
		}
		var members []string
		for _, m := range strings.Split(f[3], ",") {
			if m = strings.TrimSpace(m); m != "" {
				members = append(members, m)
			}
		}
		out = append(out, groupEntry{name: f[0], gid: gid, members: members})
		return true
	})
	return out, ok
}

// eachLine hands each entry of a colon-separated account file to fn, skipping blank lines and comments.
// It reports false when the file cannot be read, a line is a NIS compat entry, or fn refuses a line.
func eachLine(path string, fn func(fields []string) bool) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			return false
		}
		if !fn(strings.Split(line, ":")) {
			return false
		}
	}
	return sc.Err() == nil
}

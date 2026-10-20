package fsperm

// This file reads a Windows access control list from its binary form. It is plain Go over a byte
// slice, with no system call, so that the parser is tested on every platform; only the Windows
// build calls it.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ACE types (winnt.h ACE_HEADER.AceType).
const (
	aceAllowed               = 0x0
	aceDenied                = 0x1
	aceAllowedObject         = 0x5
	aceDeniedObject          = 0x6
	aceAllowedCallback       = 0x9
	aceDeniedCallback        = 0xA
	aceAllowedCallbackObject = 0xB
	aceDeniedCallbackObject  = 0xC
)

// Flags of an object ACE (ACCESS_ALLOWED_OBJECT_ACE.Flags): which optional GUIDs precede the SID.
const (
	aceObjectTypePresent          = 0x1
	aceInheritedObjectTypePresent = 0x2
)

// aceWriteRights are the access rights that make a grant a write grant (Info.OthersWrite):
// FILE_WRITE_DATA, FILE_APPEND_DATA, FILE_WRITE_EA, FILE_DELETE_CHILD, FILE_WRITE_ATTRIBUTES,
// DELETE, WRITE_DAC, WRITE_OWNER, GENERIC_WRITE, GENERIC_ALL and MAXIMUM_ALLOWED.
const aceWriteRights uint32 = 0x00000002 | 0x00000004 | 0x00000010 | 0x00000040 | 0x00000100 |
	0x00010000 | 0x00040000 | 0x00080000 | 0x40000000 | 0x10000000 | 0x02000000

// Binary SIDs this package compares against.
var (
	sidEveryone     = []byte{1, 1, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0}                    // S-1-1-0
	sidCreatorOwner = []byte{1, 1, 0, 0, 0, 0, 0, 3, 0, 0, 0, 0}                    // S-1-3-0
	sidOwnerRights  = []byte{1, 1, 0, 0, 0, 0, 0, 3, 4, 0, 0, 0}                    // S-1-3-4
	sidLocalSystem  = []byte{1, 1, 0, 0, 0, 0, 0, 5, 18, 0, 0, 0}                   // S-1-5-18
	sidAdmins       = []byte{1, 2, 0, 0, 0, 0, 0, 5, 32, 0, 0, 0, 0x20, 0x02, 0, 0} // S-1-5-32-544
)

var errMalformedACL = errors.New("malformed access control list")

// aclEntry is one ACE: its type, flags, access mask and SID. sid is nil for a type whose layout
// this package does not read.
type aclEntry struct {
	typ, flags uint8
	mask       uint32
	sid        []byte
}

// parseACL reads every ACE of the ACL b (header included). Every length is checked against b and
// against the ACL's own AclSize; an inconsistency is errMalformedACL.
func parseACL(b []byte) ([]aclEntry, error) {
	if len(b) < 8 {
		return nil, errMalformedACL
	}
	size := int(binary.LittleEndian.Uint16(b[2:4]))
	if size < 8 || size > len(b) {
		return nil, errMalformedACL
	}
	b = b[:size]
	count := int(binary.LittleEndian.Uint16(b[4:6]))
	out := make([]aclEntry, 0, count)
	off := 8
	for n := 0; n < count; n++ {
		if off+4 > len(b) {
			return nil, errMalformedACL
		}
		aceSize := int(binary.LittleEndian.Uint16(b[off+2 : off+4]))
		if aceSize < 4 || off+aceSize > len(b) {
			return nil, errMalformedACL
		}
		e := aclEntry{typ: b[off], flags: b[off+1]}
		body := b[off+4 : off+aceSize]
		off += aceSize
		var sidAt int
		switch e.typ {
		case aceAllowed, aceDenied, aceAllowedCallback, aceDeniedCallback:
			sidAt = 4
		case aceAllowedObject, aceDeniedObject, aceAllowedCallbackObject, aceDeniedCallbackObject:
			if len(body) < 8 {
				return nil, errMalformedACL
			}
			objFlags := binary.LittleEndian.Uint32(body[4:8])
			sidAt = 8
			if objFlags&aceObjectTypePresent != 0 {
				sidAt += 16
			}
			if objFlags&aceInheritedObjectTypePresent != 0 {
				sidAt += 16
			}
		default:
			out = append(out, e)
			continue
		}
		if len(body) < sidAt {
			return nil, errMalformedACL
		}
		e.mask = binary.LittleEndian.Uint32(body[0:4])
		sid, ok := sidPrefix(body[sidAt:])
		if !ok {
			return nil, errMalformedACL
		}
		e.sid = sid
		out = append(out, e)
	}
	return out, nil
}

// sidPrefix returns the SID at the start of b, trimmed to its own length (a callback ACE carries
// application data after it), and whether b starts with a well-formed SID.
func sidPrefix(b []byte) ([]byte, bool) {
	if len(b) < 8 || b[0] != 1 || b[1] > 15 {
		return nil, false
	}
	n := 8 + 4*int(b[1])
	if len(b) < n {
		return nil, false
	}
	return b[:n], true
}

// sidString formats a binary SID as ConvertSidToStringSid does ("S-1-5-21-…"; an identifier
// authority of 2^32 or more in hexadecimal).
func sidString(sid []byte) string {
	s, ok := sidPrefix(sid)
	if !ok {
		return "(malformed SID)"
	}
	var auth uint64
	for _, c := range s[2:8] {
		auth = auth<<8 | uint64(c)
	}
	var b strings.Builder
	b.WriteString("S-1-")
	if auth >= 1<<32 {
		fmt.Fprintf(&b, "0x%012X", auth)
	} else {
		b.WriteString(strconv.FormatUint(auth, 10))
	}
	for i := 8; i < len(s); i += 4 {
		b.WriteByte('-')
		b.WriteString(strconv.FormatUint(uint64(binary.LittleEndian.Uint32(s[i:i+4])), 10))
	}
	return b.String()
}

// sidIn reports whether sid is one of set.
func sidIn(sid []byte, set ...[]byte) bool {
	for _, s := range set {
		if bytes.Equal(sid, s) {
			return true
		}
	}
	return false
}

// aclAccess is what a DACL grants to principals outside the trusted set.
type aclAccess struct {
	othersRead, othersWrite bool
	// why explains othersRead: the first offending entry.
	why string
}

// scanACL classifies the DACL b. trusted reports the SIDs whose grants keep the path private.
// ACCESS_ALLOWED entries and their object and callback forms, inherit-only ones included, that
// grant any right to another SID make it not private; deny entries are ignored; an entry type
// whose meaning is not known here makes it not private and writable by others (fail closed).
func scanACL(b []byte, trusted func(sid []byte) bool) (aclAccess, error) {
	ents, err := parseACL(b)
	if err != nil {
		return aclAccess{}, err
	}
	var a aclAccess
	for _, e := range ents {
		switch e.typ {
		case aceDenied, aceDeniedObject, aceDeniedCallback, aceDeniedCallbackObject:
			continue
		case aceAllowed, aceAllowedObject, aceAllowedCallback, aceAllowedCallbackObject:
			if e.mask == 0 || trusted(e.sid) {
				continue
			}
			if a.why == "" {
				a.why = "access granted to " + sidString(e.sid)
			}
			a.othersRead = true
			if e.mask&aceWriteRights != 0 {
				a.othersWrite = true
			}
		default:
			if a.why == "" {
				a.why = fmt.Sprintf("access control entry of unsupported type %d", e.typ)
			}
			a.othersRead, a.othersWrite = true, true
		}
	}
	return a, nil
}

// syntheticMode is the display mode of a Windows path: 0o700/0o600 (directory/file) when nobody
// else has access, 0o755/0o644 when others may read but not write, 0o777/0o666 when others may write.
func syntheticMode(dir, othersRead, othersWrite bool) os.FileMode {
	switch {
	case othersWrite && dir:
		return 0o777
	case othersWrite:
		return 0o666
	case othersRead && dir:
		return 0o755
	case othersRead:
		return 0o644
	case dir:
		return 0o700
	}
	return 0o600
}

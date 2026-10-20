//go:build windows

package fsperm

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// reparseTagNameSurrogate is the bit of a reparse tag that marks a name surrogate: a reparse point
// that names another file or directory (symbolic link, junction, mount point), as opposed to one
// that stores data (deduplication, cloud files, compression). IsReparseTagNameSurrogate in winnt.h.
const reparseTagNameSurrogate = 0x20000000

// fileAttributeTagInfo is FILE_ATTRIBUTE_TAG_INFO (GetFileInformationByHandleEx, FileAttributeTagInfo).
type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// fileIDInfo is FILE_ID_INFO (GetFileInformationByHandleEx, FileIdInfo).
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

// self is the user of this process's token, read once.
var self struct {
	once sync.Once
	sid  []byte // binary SID
	str  string // "S-1-5-21-…"
	err  error
}

// selfSID returns the binary SID of the process token's user.
func selfSID() ([]byte, error) {
	self.once.Do(func() {
		tu, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			self.err = fmt.Errorf("read the user of the process token: %w", err)
			return
		}
		b, err := sidBytes(tu.User.Sid)
		if err != nil {
			self.err = fmt.Errorf("read the user of the process token: %w", err)
			return
		}
		self.sid, self.str = b, sidString(b)
	})
	return self.sid, self.err
}

// sidBytes copies the SID s into a byte slice.
func sidBytes(s *windows.SID) ([]byte, error) {
	if s == nil || !s.IsValid() {
		return nil, errors.New("invalid SID")
	}
	n := windows.GetLengthSid(s)
	out, ok := sidPrefix(bytes.Clone(unsafe.Slice((*byte)(unsafe.Pointer(s)), n)))
	if !ok {
		return nil, errors.New("invalid SID")
	}
	return out, nil
}

// CurrentUser names this process's account for messages: unix "uid N"; windows the token user SID.
func CurrentUser() string {
	if _, err := selfSID(); err != nil {
		return "unknown user"
	}
	return self.str
}

// openMeta opens path itself, never a reparse point's target, for reading or changing its
// attributes and security; a directory opens too.
func openMeta(path string, access uint32) (windows.Handle, error) {
	if path == "" {
		return windows.InvalidHandle, windows.ERROR_FILE_NOT_FOUND
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// tagInfo reads the attributes and reparse tag of the file h.
func tagInfo(h windows.Handle) (fileAttributeTagInfo, error) {
	var ti fileAttributeTagInfo
	err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&ti)), uint32(unsafe.Sizeof(ti)))
	return ti, err
}

// nameSurrogate reports a reparse point that names another file: a symbolic link, a junction or a
// mount point.
func (ti fileAttributeTagInfo) nameSurrogate() bool {
	return ti.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && ti.ReparseTag&reparseTagNameSurrogate != 0
}

// fileID reads the volume and file identifier of h.
func fileID(h windows.Handle) (fileIDInfo, error) {
	var id fileIDInfo
	err := windows.GetFileInformationByHandleEx(h, windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id)))
	return id, err
}

// trustedGrantee reports the principals whose grants keep a path private.
func trustedGrantee(user []byte) func([]byte) bool {
	return func(sid []byte) bool {
		return sidIn(sid, user, sidLocalSystem, sidAdmins, sidOwnerRights, sidCreatorOwner)
	}
}

// inspectHandle reads Info from the open file h (opened with READ_CONTROL|FILE_READ_ATTRIBUTES
// and FILE_FLAG_OPEN_REPARSE_POINT).
func inspectHandle(h windows.Handle) (Info, error) {
	user, err := selfSID()
	if err != nil {
		return Info{}, err
	}
	ti, err := tagInfo(h)
	if err != nil {
		return Info{}, fmt.Errorf("read the file attributes: %w", err)
	}
	i := Info{
		Link: ti.nameSurrogate(),
		dir:  ti.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return Info{}, fmt.Errorf("read the security descriptor: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return Info{}, fmt.Errorf("read the owner: %w", err)
	}
	if owner == nil {
		i.Owner = "no owner"
	} else {
		ob, err := sidBytes(owner)
		if err != nil {
			return Info{}, fmt.Errorf("read the owner: %w", err)
		}
		i.Owner = sidString(ob)
		i.OwnedBySelf = sidIn(ob, user, sidLocalSystem, sidAdmins)
	}
	dacl, _, err := sd.DACL()
	switch {
	case errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND), err == nil && dacl == nil:
		// No DACL, or a NULL DACL: everyone has every right.
		i.OthersRead, i.OthersWrite = true, true
		i.readWhy = "access granted to " + sidString(sidEveryone) + " (no access control list)"
	case err != nil:
		return Info{}, fmt.Errorf("read the access control list: %w", err)
	default:
		b, err := aclBytes(sd, dacl)
		if err != nil {
			return Info{}, err
		}
		a, err := scanACL(b, trustedGrantee(user))
		if err != nil {
			return Info{}, err
		}
		i.OthersRead, i.OthersWrite = a.othersRead, a.othersWrite
		if a.othersRead {
			i.readWhy = a.why
		}
	}
	i.Mode = syntheticMode(i.dir, i.OthersRead, i.OthersWrite)
	return i, nil
}

// aclBytes is the ACL dacl, which lies inside the self-relative security descriptor sd, as a byte
// slice bounded by both the ACL's AclSize and the descriptor's length.
func aclBytes(sd *windows.SECURITY_DESCRIPTOR, dacl *windows.ACL) ([]byte, error) {
	sdStart, sdLen := uintptr(unsafe.Pointer(sd)), uintptr(sd.Length())
	at := uintptr(unsafe.Pointer(dacl))
	if at < sdStart || at-sdStart+8 > sdLen {
		return nil, errMalformedACL
	}
	hdr := unsafe.Slice((*byte)(unsafe.Pointer(dacl)), 8)
	size := uintptr(hdr[2]) | uintptr(hdr[3])<<8
	if size < 8 || at-sdStart+size > sdLen {
		return nil, errMalformedACL
	}
	return bytes.Clone(unsafe.Slice((*byte)(unsafe.Pointer(dacl)), size)), nil
}

// Inspect reads Info for path. It never follows a final link.
func Inspect(path string) (Info, error) {
	h, err := openMeta(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return Info{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	i, err := inspectHandle(h)
	if err != nil {
		return Info{}, &os.PathError{Op: "inspect", Path: path, Err: err}
	}
	return i, nil
}

// aceInherited is INHERITED_ACE (ACE_HEADER.AceFlags): the entry was inherited from a parent.
const aceInherited = 0x10

// ExplicitSIDs returns the SIDs ("S-1-…") that the explicit entries (allow and deny; not inherited) of
// the DACL of path name, each once, in the order of the list. It never follows a final link. A NULL
// or missing DACL has none. An explicit entry of a type whose SID this package does not read is an
// error: the SID could not be named.
func ExplicitSIDs(path string) ([]string, error) {
	h, err := openMeta(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	fail := func(err error) ([]string, error) {
		return nil, &os.PathError{Op: "inspect", Path: path, Err: err}
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fail(err)
	}
	dacl, _, err := sd.DACL()
	switch {
	case errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND), err == nil && dacl == nil:
		return nil, nil
	case err != nil:
		return fail(err)
	}
	b, err := aclBytes(sd, dacl)
	if err != nil {
		return fail(err)
	}
	entries, err := parseACL(b)
	if err != nil {
		return fail(err)
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.flags&aceInherited != 0 {
			continue
		}
		if e.sid == nil {
			return fail(fmt.Errorf("an entry of type %#x names no SID this package reads", e.typ))
		}
		if s := sidString(e.sid); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// privateSDDL is the private descriptor for a directory (inherited by its children, owner set to
// the user) or a file.
func privateSDDL(dir bool) (string, error) {
	if _, err := selfSID(); err != nil {
		return "", err
	}
	u := self.str
	if dir {
		return "O:" + u + "D:P(A;OICI;FA;;;" + u + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", nil
	}
	return "D:P(A;;FA;;;" + u + ")(A;;FA;;;SY)(A;;FA;;;BA)", nil
}

// privateDescriptor builds the private security descriptor for a directory or a file.
func privateDescriptor(dir bool) (*windows.SECURITY_DESCRIPTOR, error) {
	s, err := privateSDDL(dir)
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(s)
	if err != nil {
		return nil, fmt.Errorf("build the private security descriptor: %w", err)
	}
	return sd, nil
}

// MkdirAllPrivate creates dir and missing parents.
// unix: exactly os.MkdirAll(dir, 0o700) (no check; callers that check on unix keep their own code).
// windows: every missing directory, the parents and the leaf, is created with CreateDirectory and a
// SECURITY_ATTRIBUTES whose descriptor is "O:<user>D:P(A;OICI;FA;;;<user>)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)",
// so that a parent is private as os.MkdirAll(dir, 0o700) makes it on unix, and does not inherit the
// access another account has to the directory above it; a parent that appears meanwhile as a Link is an
// error (ErrNotPrivate), and one that appears as another directory is used as it is. An existing leaf
// that is a Link or not OwnedBySelf is an error (ErrNotPrivate); one that is OwnedBySelf but OthersRead is
// Protect-ed; the result must pass CheckPrivate. Existing parents are not changed.
func MkdirAllPrivate(dir string) error {
	if dir == "" {
		return &os.PathError{Op: "mkdir", Path: dir, Err: windows.ERROR_PATH_NOT_FOUND}
	}
	dir = filepath.Clean(dir)
	i, err := Inspect(dir)
	if err == nil {
		return privateExistingDir(dir, i)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// The missing parents, nearest first, up to the first one that exists.
	var missing []string
	for p := dir; ; {
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		if _, err := os.Lstat(parent); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, parent)
		p = parent
	}
	for k := len(missing) - 1; k >= 0; k-- {
		created, err := createPrivateDir(missing[k])
		if err != nil {
			return err
		}
		if !created {
			// Another creator made it first: use it unless it is a link, or not a directory.
			i, err := Inspect(missing[k])
			if err != nil {
				return err
			}
			if i.Link {
				return notPrivate(missing[k], whyLink)
			}
			if !i.dir {
				return &os.PathError{Op: "mkdir", Path: missing[k], Err: syscall.ENOTDIR}
			}
		}
	}
	created, err := createPrivateDir(dir)
	if err != nil {
		return err
	}
	if !created {
		// Another creator won the race: judge what is there now.
		i, err := Inspect(dir)
		if err != nil {
			return err
		}
		return privateExistingDir(dir, i)
	}
	return CheckPrivate(dir)
}

// createPrivateDir creates the directory dir with the private descriptor. It reports false, and no
// error, when something by that name exists already.
func createPrivateDir(dir string) (bool, error) {
	sd, err := privateDescriptor(true)
	if err != nil {
		return false, &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return false, &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := windows.CreateDirectory(p, &sa); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return false, nil
		}
		return false, &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return true, nil
}

// CreateTempPrivate is os.CreateTemp for a file that holds a secret: a new file in dir (os.TempDir()
// when empty) whose name is pattern with its last "*" replaced by a random string (appended when there
// is none), opened for reading and writing.
// unix: exactly os.CreateTemp(dir, pattern) (the file is created 0600).
// windows: OpenNoFollow(name, O_RDWR|O_CREATE|O_EXCL, 0o600), so that the file has the private
// descriptor from the moment it exists, rather than the access control list it would inherit from dir;
// a name that exists is retried with another random string.
func CreateTempPrivate(dir, pattern string) (*os.File, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	if strings.ContainsAny(pattern, `/\`) {
		return nil, &os.PathError{Op: "createtemp", Path: pattern, Err: errors.New("pattern contains path separator")}
	}
	prefix, suffix := pattern, ""
	if k := strings.LastIndexByte(pattern, '*'); k >= 0 {
		prefix, suffix = pattern[:k], pattern[k+1:]
	}
	for try := 0; try < 10000; try++ {
		name := filepath.Join(dir, prefix+strconv.FormatUint(uint64(rand.Uint32()), 10)+suffix)
		f, err := OpenNoFollow(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, prefix+"*"+suffix), Err: fs.ErrExist}
}

// privateExistingDir is MkdirAllPrivate for a leaf that already exists.
func privateExistingDir(dir string, i Info) error {
	if err := refuseUnowned(dir, i); err != nil {
		return err
	}
	if !i.dir {
		return &os.PathError{Op: "mkdir", Path: dir, Err: syscall.ENOTDIR}
	}
	if i.OthersRead {
		if err := Protect(dir); err != nil {
			return err
		}
	}
	return CheckPrivate(dir)
}

// Protect makes path private. unix: os.Chmod(path, 0o700) for a directory, 0o600 otherwise.
// windows: SetSecurityInfo(DACL|PROTECTED_DACL), on the handle the check was made on (opened without
// following a reparse point), with user:F, SYSTEM:F, Administrators:F (OI|CI on directories).
// A Link, or a path not OwnedBySelf, is refused with ErrNotPrivate.
func Protect(path string) error {
	h, err := openMeta(path, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return &os.PathError{Op: "protect", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	i, err := inspectHandle(h)
	if err != nil {
		return &os.PathError{Op: "protect", Path: path, Err: err}
	}
	if err := refuseUnowned(path, i); err != nil {
		return err
	}
	sd, err := privateDescriptor(i.dir)
	if err != nil {
		return &os.PathError{Op: "protect", Path: path, Err: err}
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return &os.PathError{Op: "protect", Path: path, Err: errMalformedACL}
	}
	err = windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		return &os.PathError{Op: "protect", Path: path, Err: err}
	}
	return nil
}

// openFlags are the os.OpenFile flags OpenNoFollow accepts on Windows.
const openFlags = os.O_RDONLY | os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_EXCL | os.O_TRUNC

// OpenNoFollow is os.OpenFile that refuses a final symbolic link or other name-surrogate reparse point
// (error: *os.PathError wrapping syscall.ELOOP).
// unix: os.OpenFile(name, flag|syscall.O_NOFOLLOW, perm).
// windows: CreateFile with FILE_FLAG_OPEN_REPARSE_POINT; access from O_RDONLY/O_WRONLY/O_RDWR (+O_APPEND
// as FILE_APPEND_DATA); disposition CREATE_NEW for O_CREATE|O_EXCL, OPEN_ALWAYS for O_CREATE,
// OPEN_EXISTING otherwise; share FILE_SHARE_READ|FILE_SHARE_WRITE; FILE_ATTRIBUTE_READONLY when
// O_CREATE and perm&0o200 == 0; with O_CREATE and perm&0o077 == 0 a new file gets the private
// descriptor "D:P(A;;FA;;;<user>)(A;;FA;;;SY)(A;;FA;;;BA)"; after open, FileAttributeTagInfo: a
// name-surrogate reparse point is closed and refused, a directory opened for writing is refused;
// O_TRUNC is applied after the check (File.Truncate(0)). Any other flag bit is an error.
func OpenNoFollow(name string, flag int, perm os.FileMode) (*os.File, error) {
	fail := func(err error) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	accMode := flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR)
	if flag&^openFlags != 0 || accMode == os.O_WRONLY|os.O_RDWR {
		return fail(syscall.EINVAL)
	}
	if name == "" {
		return fail(windows.ERROR_FILE_NOT_FOUND)
	}
	namep, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return fail(err)
	}

	// Access rights as the standard library's syscall.Open computes them.
	var access uint32
	switch accMode {
	case os.O_RDONLY:
		access = windows.GENERIC_READ
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	}
	if flag&os.O_CREATE != 0 {
		access |= windows.GENERIC_WRITE
	}
	if flag&os.O_APPEND != 0 {
		// Without FILE_WRITE_DATA every write goes to the end of the file; O_TRUNC needs it.
		if flag&os.O_TRUNC == 0 {
			access &^= windows.GENERIC_WRITE
		}
		access |= windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA |
			windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE
	}
	writes := access&(windows.GENERIC_WRITE|windows.FILE_APPEND_DATA) != 0
	// The checks after the open read the file's attributes, which GENERIC_WRITE alone does not allow.
	access |= windows.FILE_READ_ATTRIBUTES

	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if flag&os.O_CREATE != 0 && perm&0o200 == 0 {
		attrs = windows.FILE_ATTRIBUTE_READONLY
	}
	attrs |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	if !writes {
		// A directory opens only with FILE_FLAG_BACKUP_SEMANTICS; one opened for writing fails.
		attrs |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}

	var disposition uint32
	switch {
	case flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		disposition = windows.CREATE_NEW
	case flag&os.O_CREATE != 0:
		disposition = windows.OPEN_ALWAYS
	default:
		disposition = windows.OPEN_EXISTING
	}

	var sa *windows.SecurityAttributes
	if flag&os.O_CREATE != 0 && perm&0o077 == 0 {
		sd, err := privateDescriptor(false)
		if err != nil {
			return fail(err)
		}
		sa = &windows.SecurityAttributes{SecurityDescriptor: sd}
		sa.Length = uint32(unsafe.Sizeof(*sa))
	}

	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	h, err := windows.CreateFile(namep, access, share, sa, disposition, attrs, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) && attrs&windows.FILE_FLAG_BACKUP_SEMANTICS == 0 {
			// A directory, or a link to one, cannot be opened for writing. Say which it is.
			err = deniedDirectory(name, err)
		}
		return fail(err)
	}
	ti, err := tagInfo(h)
	if err != nil {
		windows.CloseHandle(h)
		return fail(err)
	}
	if ti.nameSurrogate() {
		windows.CloseHandle(h)
		return fail(syscall.ELOOP)
	}
	if ti.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		// A data reparse point (deduplication, cloud files, compression) is read through its
		// filter, which FILE_FLAG_OPEN_REPARSE_POINT bypasses. Open the file again normally and
		// keep that handle only when it is the same file.
		h2, err := reopenSame(h, namep, access, share, attrs&^windows.FILE_FLAG_OPEN_REPARSE_POINT)
		windows.CloseHandle(h)
		if err != nil {
			return fail(err)
		}
		h = h2
	}
	if writes && ti.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		windows.CloseHandle(h)
		return fail(syscall.EISDIR)
	}
	f := os.NewFile(uintptr(h), name)
	if flag&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return fail(err)
		}
	}
	return f, nil
}

// deniedDirectory explains an ERROR_ACCESS_DENIED from opening name for writing: ELOOP when name is
// a name-surrogate reparse point (a directory symbolic link or a junction), EISDIR when it is a
// directory, err otherwise.
func deniedDirectory(name string, err error) error {
	h, e := openMeta(name, windows.FILE_READ_ATTRIBUTES)
	if e != nil {
		return err
	}
	defer windows.CloseHandle(h)
	ti, e := tagInfo(h)
	switch {
	case e != nil:
		return err
	case ti.nameSurrogate():
		return syscall.ELOOP
	case ti.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
		return syscall.EISDIR
	}
	return err
}

// reopenSame opens namep again with OPEN_EXISTING and attrs while h is still open, and returns the
// new handle only when it refers to the same file as h; otherwise the name was replaced between the
// two opens, and the open is refused.
func reopenSame(h windows.Handle, namep *uint16, access, share, attrs uint32) (windows.Handle, error) {
	want, err := fileID(h)
	if err != nil {
		return windows.InvalidHandle, err
	}
	h2, err := windows.CreateFile(namep, access, share, nil, windows.OPEN_EXISTING, attrs, 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	got, err := fileID(h2)
	if err != nil || got != want {
		windows.CloseHandle(h2)
		if err != nil {
			return windows.InvalidHandle, err
		}
		return windows.InvalidHandle, syscall.ELOOP
	}
	return h2, nil
}

// Bounds of ReplaceFile's retries.
const (
	replaceWait     = 2 * time.Second
	replaceFirstGap = 10 * time.Millisecond
	replaceMaxGap   = 200 * time.Millisecond
)

// OpenReplaceable opens the existing file name for reading, writing or both (flag: os.O_RDONLY,
// os.O_WRONLY or os.O_RDWR, nothing else) so that, while the handle stays open, the path can still be
// replaced by ReplaceFile or removed, and the handle keeps referring to the file it opened.
// unix: os.OpenFile(name, flag, 0); a rename over an open file or its removal never depends on it.
// windows: CreateFile with FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE, OPEN_EXISTING, following
// links as os.OpenFile does. os.OpenFile omits FILE_SHARE_DELETE, and a handle without it makes every
// rename over the path and every removal of it fail until the handle is closed.
func OpenReplaceable(name string, flag int) (*os.File, error) {
	fail := func(err error) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	var access uint32
	switch flag {
	case os.O_RDONLY:
		access = windows.GENERIC_READ
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	default:
		return fail(syscall.EINVAL)
	}
	if name == "" {
		return fail(windows.ERROR_FILE_NOT_FOUND)
	}
	namep, err := windows.UTF16PtrFromString(fixLongPath(name))
	if err != nil {
		return fail(err)
	}
	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	h, err := windows.CreateFile(namep, access, share, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fail(err)
	}
	return os.NewFile(uintptr(h), name), nil
}

// fixLongPath gives an absolute path of MAX_PATH characters or more the \\?\ prefix, so that the
// functions this file calls with a path accept it as os.OpenFile and os.Rename do. Any other path is
// returned as it is.
func fixLongPath(path string) string {
	if len(path) < windows.MAX_PATH || !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) {
		return path
	}
	return `\\?\` + filepath.Clean(path)
}

// fileRenameInfo is FILE_RENAME_INFO with its union read as Flags (SetFileInformationByHandle,
// FileRenameInfoEx); FileName continues past the end of the struct.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// renamePOSIX renames oldpath over newpath with POSIX semantics: newpath is replaced even while it is
// open through handles that share delete access, and those handles keep the replaced file. A link at
// oldpath is renamed itself, as MoveFileEx renames it. File systems without POSIX semantics (FAT,
// exFAT, versions before Windows 10 1607) fail it, and ReplaceFile then uses os.Rename.
func renamePOSIX(oldpath, newpath string) error {
	dst, err := filepath.Abs(newpath)
	if err != nil {
		return err
	}
	src, err := windows.UTF16PtrFromString(fixLongPath(oldpath))
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(fixLongPath(dst))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(src, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	nameBytes := uintptr(len(name)-1) * 2 // without the terminating NUL
	size := max(unsafe.Offsetof(fileRenameInfo{}.FileName)+nameBytes+2, unsafe.Sizeof(fileRenameInfo{}))
	buf := make([]uint64, (size+7)/8) // 8-byte aligned, as the struct requires
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(nameBytes)
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	return windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, (*byte)(unsafe.Pointer(info)), uint32(size))
}

// replaceOnce is one attempt of ReplaceFile: renamePOSIX, then os.Rename when that fails.
func replaceOnce(oldpath, newpath string) error {
	if renamePOSIX(oldpath, newpath) == nil {
		return nil
	}
	return os.Rename(oldpath, newpath)
}

// ReplaceFile renames oldpath over newpath. unix: os.Rename. windows: a rename with POSIX semantics
// (SetFileInformationByHandle, FileRenameInfoEx, FILE_RENAME_FLAG_REPLACE_IF_EXISTS|
// FILE_RENAME_FLAG_POSIX_SEMANTICS), which replaces newpath even while handles opened with
// FILE_SHARE_DELETE (OpenReplaceable) hold it, those handles keeping the replaced file; when that
// rename fails, os.Rename (MoveFileEx), whose error is the one returned. Either is retried for at most
// 2 s (backoff 10 ms doubling, cap 200 ms) while it fails with ERROR_ACCESS_DENIED,
// ERROR_SHARING_VIOLATION or ERROR_LOCK_VIOLATION (a reader without FILE_SHARE_DELETE holds newpath;
// F10).
//
// MoveFileEx alone is not enough: without POSIX semantics a file that any handle holds open cannot be
// replaced, even when every such handle shares delete access, because the replaced name stays in the
// directory until its last handle closes.
func ReplaceFile(oldpath, newpath string) error {
	deadline := time.Now().Add(replaceWait)
	gap := replaceFirstGap
	for {
		err := replaceOnce(oldpath, newpath)
		if err == nil || !replaceRetryable(err) {
			return err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return err
		}
		time.Sleep(min(gap, left))
		gap = min(2*gap, replaceMaxGap)
	}
}

// replaceRetryable reports the errors of a rename that a reader holding the target causes.
func replaceRetryable(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION:
		return true
	}
	return false
}

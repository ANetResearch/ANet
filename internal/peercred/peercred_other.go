//go:build !linux && !darwin && !freebsd

package peercred

// supported: no other platform is read (openbsd and netbsd are not release
// targets, LEC §1.2), so every caller refuses there.
const supported = false

func fromFD(int) (Cred, error) { return unknown, ErrUnsupported }

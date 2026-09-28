//go:build linux

package daemon

import (
	"fmt"
	"os"
)

// openExistingDir opens the existing directory p (absolute, clean, resolved through symbolic links when
// it was checked) and makes sure the directory opened is the one p names now: the kernel's name for the
// open directory (/proc/self/fd) must be p. A component swapped for a link after the check leads the
// open elsewhere, and the name says so; opening by path, unlike walkDir, needs no read permission on
// the directories above p [redteam:F20]. Without /proc it walks.
func openExistingDir(p string) (*os.Root, error) {
	r, err := os.OpenRoot(p)
	if err != nil {
		return nil, err
	}
	f, err := r.Open(".")
	if err != nil {
		r.Close()
		return nil, err
	}
	got, lerr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	f.Close()
	if lerr != nil {
		r.Close()
		return walkDir(p)
	}
	if got != p {
		r.Close()
		return nil, fmt.Errorf("%s changed while it was being opened (it led to %s)", p, got)
	}
	return r, nil
}

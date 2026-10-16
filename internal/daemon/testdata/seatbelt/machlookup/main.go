//go:build darwin && cgo

// Command machlookup is a test-only helper of the macOS sandbox tests (internal/daemon,
// TestSeatbeltKeychainIsOutOfReach; LEC §7.5 J8): it asks the bootstrap server for one Mach service by
// name and exits 0 when the lookup succeeds and 3 when it is refused. It lives under testdata, so no
// build of anet ever contains it; the test builds it with cgo when it runs.
package main

/*
#include <mach/mach.h>
#include <servers/bootstrap.h>

static int lookup(const char *name) {
	mach_port_t port = MACH_PORT_NULL;
	kern_return_t kr = bootstrap_look_up(bootstrap_port, name, &port);
	if (kr == KERN_SUCCESS && port != MACH_PORT_NULL) {
		mach_port_deallocate(mach_task_self(), port);
	}
	return (int)kr;
}
*/
import "C"

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: machlookup <service>")
		os.Exit(2)
	}
	if kr := C.lookup(C.CString(os.Args[1])); kr != 0 {
		fmt.Printf("refused %d\n", int(kr))
		os.Exit(3)
	}
	fmt.Println("found")
}

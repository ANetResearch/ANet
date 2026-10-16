package daemon

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// updateSandboxGolden rewrites testdata/seccomp and testdata/bwrap from the current code. Review the diff:
// these files are the G-SECCOMP and G-BWRAP vectors (LEC §7.1), and a byte that changes is a change of the
// sandbox.
var updateSandboxGolden = flag.Bool("update-sandbox", false, "rewrite the sandbox golden files in testdata")

// specSyscalls is LEC SB-6 transcribed independently of seccomp_linux.go: the system calls the filter
// refuses with EPERM, and the numbers each architecture gives them. The numbers were checked against
// golang.org/x/sys/unix v0.47.0 zsysnum_linux_{amd64,arm64}.go (EXE-1 notes).
var specDenied = []string{
	"keyctl", "add_key", "request_key", "bpf", "perf_event_open", "userfaultfd",
	"io_uring_setup", "io_uring_enter", "io_uring_register", "ptrace", "process_vm_readv",
	"process_vm_writev", "kexec_load", "kexec_file_load", "init_module", "finit_module",
	"delete_module", "mount", "umount2", "pivot_root", "setns", "open_by_handle_at",
	"name_to_handle_at", "acct", "swapon", "swapoff", "reboot", "syslog",
	// The new mount API, refused for the same reason as mount (SB-6, 2026-10-04).
	"fsopen", "fsconfig", "fsmount", "fspick", "move_mount", "open_tree", "mount_setattr",
}

// The new mount API has the same numbers on every architecture (include/uapi/asm-generic/unistd.h;
// arch/x86/entry/syscalls/syscall_64.tbl), checked against golang.org/x/sys/unix v0.47.0.
var specNewMountNr = map[string]uint32{
	"open_tree": 428, "move_mount": 429, "fsopen": 430, "fsconfig": 431, "fsmount": 432, "fspick": 433,
	"mount_setattr": 442,
}

func init() {
	for _, nr := range specNr {
		for name, n := range specNewMountNr {
			nr[name] = n
		}
	}
}

var specNr = map[string]map[string]uint32{
	"amd64": {
		"read": 0, "write": 1, "openat": 257, "getpid": 39, "clone": 56, "clone3": 435, "unshare": 272,
		"keyctl": 250, "add_key": 248, "request_key": 249, "bpf": 321, "perf_event_open": 298,
		"userfaultfd": 323, "io_uring_setup": 425, "io_uring_enter": 426, "io_uring_register": 427,
		"ptrace": 101, "process_vm_readv": 310, "process_vm_writev": 311, "kexec_load": 246,
		"kexec_file_load": 320, "init_module": 175, "finit_module": 313, "delete_module": 176,
		"mount": 165, "umount2": 166, "pivot_root": 155, "setns": 308, "open_by_handle_at": 304,
		"name_to_handle_at": 303, "acct": 163, "swapon": 167, "swapoff": 168, "reboot": 169,
		"syslog": 103, "personality": 135, "ioctl": 16, "prctl": 157,
	},
	"arm64": {
		"read": 63, "write": 64, "openat": 56, "getpid": 172, "clone": 220, "clone3": 435, "unshare": 97,
		"keyctl": 219, "add_key": 217, "request_key": 218, "bpf": 280, "perf_event_open": 241,
		"userfaultfd": 282, "io_uring_setup": 425, "io_uring_enter": 426, "io_uring_register": 427,
		"ptrace": 117, "process_vm_readv": 270, "process_vm_writev": 271, "kexec_load": 104,
		"kexec_file_load": 294, "init_module": 105, "finit_module": 273, "delete_module": 106,
		"mount": 40, "umount2": 39, "pivot_root": 41, "setns": 268, "open_by_handle_at": 265,
		"name_to_handle_at": 264, "acct": 89, "swapon": 224, "swapoff": 225, "reboot": 142,
		"syslog": 116, "personality": 92, "ioctl": 29, "prctl": 167,
	},
}

var specAudit = map[string]uint32{"amd64": 0xc000003e, "arm64": 0xc00000b7}

const (
	retKill   = 0x80000000
	retEPERM  = 0x00050000 | 1
	retENOSYS = 0x00050000 | 38
	retAllow  = 0x7fff0000
)

// seccompData is struct seccomp_data, the input a filter runs on.
type seccompData struct {
	nr   uint32
	arch uint32
	ip   uint64
	args [6]uint64
}

func (d seccompData) bytes() []byte {
	b := make([]byte, 64)
	binary.LittleEndian.PutUint32(b[0:], d.nr)
	binary.LittleEndian.PutUint32(b[4:], d.arch)
	binary.LittleEndian.PutUint64(b[8:], d.ip)
	for i, a := range d.args {
		binary.LittleEndian.PutUint64(b[16+8*i:], a)
	}
	return b
}

// runBPF interprets a classic BPF program the way the kernel's seccomp does for the instructions a
// filter may use. It is written from the cBPF encoding (linux/filter.h, linux/bpf_common.h), not from
// seccomp_linux.go, so it is an independent check of what the assembled bytes do.
func runBPF(t *testing.T, prog []byte, in []byte) uint32 {
	t.Helper()
	if len(prog)%8 != 0 || len(prog) == 0 || len(prog)/8 > 4096 {
		t.Fatalf("program of %d bytes", len(prog))
	}
	n := len(prog) / 8
	var a uint32
	pc := 0
	for steps := 0; steps <= n; steps++ {
		if pc >= n {
			t.Fatalf("pc %d ran off the end", pc)
		}
		ins := prog[pc*8:]
		code := binary.LittleEndian.Uint16(ins[0:])
		jt, jf := int(ins[2]), int(ins[3])
		k := binary.LittleEndian.Uint32(ins[4:])
		switch code {
		case 0x20: // BPF_LD | BPF_W | BPF_ABS
			if k%4 != 0 || int(k)+4 > len(in) {
				t.Fatalf("pc %d: load at %d", pc, k)
			}
			a = binary.LittleEndian.Uint32(in[k:])
			pc++
		case 0x15: // BPF_JMP | BPF_JEQ | BPF_K
			if a == k {
				pc += 1 + jt
			} else {
				pc += 1 + jf
			}
		case 0x45: // BPF_JMP | BPF_JSET | BPF_K
			if a&k != 0 {
				pc += 1 + jt
			} else {
				pc += 1 + jf
			}
		case 0x05: // BPF_JMP | BPF_JA
			pc += 1 + int(k)
		case 0x06: // BPF_RET | BPF_K
			return k
		default:
			t.Fatalf("pc %d: opcode %#x is not one a seccomp filter here may use", pc, code)
		}
	}
	t.Fatal("the program did not return")
	return 0
}

// disassembleBPF renders a program in the format of `tcpdump -d`, annotated with the system call or the
// action an instruction stands for. The annotations come from the transcription above, not from the
// code under test.
func disassembleBPF(prog []byte, goarch string) string {
	names := map[uint32]string{}
	for name, nr := range specNr[goarch] {
		names[nr] = name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "; seccomp filter for %s (LEC SB-6), %d instructions\n", goarch, len(prog)/8)
	lastLoad := uint32(0)
	for pc := 0; pc*8 < len(prog); pc++ {
		ins := prog[pc*8:]
		code := binary.LittleEndian.Uint16(ins[0:])
		jt, jf := int(ins[2]), int(ins[3])
		k := binary.LittleEndian.Uint32(ins[4:])
		var line, note string
		switch code {
		case 0x20:
			line = fmt.Sprintf("ld       [%d]", k)
			lastLoad = k
			switch {
			case k == 0:
				note = "nr"
			case k == 4:
				note = "arch"
			case k >= 16:
				note = fmt.Sprintf("args[%d] low 32 bits", (k-16)/8)
			}
		case 0x15:
			line = fmt.Sprintf("jeq      #%#-14x jt %d\tjf %d", k, pc+1+jt, pc+1+jf)
			if lastLoad == 0 {
				note = names[k]
			}
		case 0x45:
			line = fmt.Sprintf("jset     #%#-14x jt %d\tjf %d", k, pc+1+jt, pc+1+jf)
		case 0x05:
			line = fmt.Sprintf("ja       %d", pc+1+int(k))
		case 0x06:
			line = fmt.Sprintf("ret      #%#x", k)
			switch k {
			case retKill:
				note = "KILL_PROCESS"
			case retEPERM:
				note = "ERRNO(EPERM)"
			case retENOSYS:
				note = "ERRNO(ENOSYS)"
			case retAllow:
				note = "ALLOW"
			}
		default:
			line = fmt.Sprintf("??? code=%#x jt=%d jf=%d k=%#x", code, jt, jf, k)
		}
		if note != "" {
			line = fmt.Sprintf("%-44s ; %s", line, note)
		}
		fmt.Fprintf(&b, "(%03d) %s\n", pc, strings.TrimRight(line, " "))
	}
	return b.String()
}

func goldenCompare(t *testing.T, path string, got []byte) {
	t.Helper()
	if *updateSandboxGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update-sandbox to create it)", err, t.Name())
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the code's output (%d bytes, want %d); if the change is intended, rerun with -update-sandbox and review the diff",
			path, len(got), len(want))
	}
}

// G-SECCOMP: the two programs are byte-for-byte the vectors in testdata/seccomp, and the listing beside
// each is their disassembly.
func TestSeccompGolden(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		prog, err := seccompProgram(arch)
		if err != nil {
			t.Fatal(err)
		}
		goldenCompare(t, filepath.Join("testdata", "seccomp", "filter-"+arch+".bpf"), prog)
		goldenCompare(t, filepath.Join("testdata", "seccomp", "filter-"+arch+".txt"), []byte(disassembleBPF(prog, arch)))
	}
	if _, err := seccompProgram("riscv64"); err == nil {
		t.Error("an architecture without a filter got one")
	}
}

// The program's bytes do not depend on anything but the architecture: assembling twice gives the same
// bytes, and the host's program is the one for runtime.GOARCH.
func TestSeccompProgramIsDeterministic(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		a, _ := seccompProgram(arch)
		b, _ := seccompProgram(arch)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: two assemblies differ", arch)
		}
	}
	host, err := seccompProgramForHost()
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		if err == nil {
			t.Fatal("a host without a filter got one")
		}
		return
	}
	want, _ := seccompProgram(runtime.GOARCH)
	if err != nil || !bytes.Equal(host, want) {
		t.Fatalf("host program: %v", err)
	}
}

// SB-6 rule by rule, on the interpreter: every listed system call is EPERM, clone3 is ENOSYS, clone and
// unshare are EPERM exactly when CLONE_NEWUSER is set, personality other than PER_LINUX and ioctl
// TIOCSTI/TIOCLINUX are EPERM (the high 32 bits of either argument do not hide them), a foreign
// architecture and the x32 ABI are killed, and everything else is allowed.
func TestSeccompRules(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		prog, err := seccompProgram(arch)
		if err != nil {
			t.Fatal(err)
		}
		nr := specNr[arch]
		audit := specAudit[arch]
		run := func(name string, args ...uint64) uint32 {
			d := seccompData{nr: nr[name], arch: audit, ip: 0x7fff12345678}
			copy(d.args[:], args)
			return runBPF(t, prog, d.bytes())
		}
		check := func(what string, got, want uint32) {
			t.Helper()
			if got != want {
				t.Errorf("%s %s: action %#x, want %#x", arch, what, got, want)
			}
		}
		for _, name := range specDenied {
			check(name, run(name), retEPERM)
			check(name+" with arguments", run(name, ^uint64(0), 1, 2, 3, 4, 5), retEPERM)
		}
		check("clone3", run("clone3"), retENOSYS)
		const newUser = 0x10000000
		for _, name := range []string{"clone", "unshare"} {
			check(name+"(CLONE_NEWUSER)", run(name, newUser), retEPERM)
			check(name+"(CLONE_NEWUSER|others)", run(name, newUser|0x00000100|0x00020000|0x11), retEPERM)
			check(name+"(CLONE_NEWUSER, high bits set)", run(name, 0xffffffff00000000|newUser), retEPERM)
			check(name+"(threads)", run(name, 0x003d0f00), retAllow)
			check(name+"(0)", run(name, 0), retAllow)
			check(name+"(high bits only)", run(name, 0xffffffff00000000), retAllow)
		}
		check("personality(PER_LINUX)", run("personality", 0), retAllow)
		check("personality(PER_LINUX, high bits)", run("personality", 0xffffffff00000000), retAllow)
		// The query (0xffffffff) changes nothing and is allowed (SB-6, 2026-10-04); every other persona is
		// refused, including the query's value with one bit less and with the high bits set around it.
		check("personality(query)", run("personality", 0xffffffff), retAllow)
		check("personality(query, high bits)", run("personality", 0xffffffff_ffffffff), retAllow)
		check("personality(0xfffffffe)", run("personality", 0xfffffffe), retEPERM)
		check("personality(0x7fffffff)", run("personality", 0x7fffffff), retEPERM)
		check("personality(ADDR_NO_RANDOMIZE)", run("personality", 0x0040000), retEPERM)
		check("personality(READ_IMPLIES_EXEC)", run("personality", 0x0400000), retEPERM)
		check("personality(PER_LINUX32)", run("personality", 0x0008), retEPERM)
		// The new mount API is refused whatever its arguments, like mount (SB-6, 2026-10-04).
		for name := range specNewMountNr {
			check(name+"()", run(name), retEPERM)
			check(name+"(-1, all bits)", run(name, 0xffffffff, ^uint64(0), ^uint64(0)), retEPERM)
		}
		check("ioctl(TIOCSTI)", run("ioctl", 0, 0x5412), retEPERM)
		check("ioctl(TIOCLINUX)", run("ioctl", 0, 0x541c), retEPERM)
		check("ioctl(TIOCSTI, high bits)", run("ioctl", 0, 0x1_0000_5412), retEPERM)
		check("ioctl(TIOCLINUX, high bits)", run("ioctl", 0, 0xffffffff_0000541c), retEPERM)
		check("ioctl(TCGETS)", run("ioctl", 0, 0x5401), retAllow)
		check("ioctl(FIONREAD)", run("ioctl", 3, 0x541b), retAllow)
		for _, name := range []string{"read", "write", "openat", "getpid", "prctl"} {
			check(name, run(name, 1, 2, 3), retAllow)
		}
		// System calls above every listed number, and number 0xffffffff.
		check("nr 999", runBPF(t, prog, seccompData{nr: 999, arch: audit}.bytes()), retAllow)
		// A foreign architecture is killed, whatever the number.
		for other, oa := range specAudit {
			if other == arch {
				continue
			}
			check("foreign arch "+other, runBPF(t, prog, seccompData{nr: nr["read"], arch: oa}.bytes()), retKill)
		}
		check("i386", runBPF(t, prog, seccompData{nr: 3, arch: 0x40000003}.bytes()), retKill)
		check("arm", runBPF(t, prog, seccompData{nr: 3, arch: 0x40000028}.bytes()), retKill)
		x32 := runBPF(t, prog, seccompData{nr: 0x40000000 | nr["getpid"], arch: audit}.bytes())
		if arch == "amd64" {
			check("x32 getpid", x32, retKill)
			check("x32 keyctl", runBPF(t, prog, seccompData{nr: 0x40000000 | nr["keyctl"], arch: audit}.bytes()), retKill)
		}
	}
}

// The filter as the kernel runs it. The test binary is started again; the child installs the host's
// filter on one locked thread (no_new_privs, PR_SET_SECCOMP) and makes the calls below on that thread
// with arguments that cannot do anything, once without and once with the filter. With the filter every
// listed call answers EPERM, clone3 ENOSYS, and the allowed calls answer exactly as without it. A set of
// calls the kernel itself would not answer with EPERM shows the filter, and not the kernel, refused them.
// On amd64 a system call with the x32 bit kills the child with SIGSYS. Needs no bwrap and no namespaces.
func TestSeccompFilterInKernel(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("no filter for %s", runtime.GOARCH)
	}
	run := func(mode string) map[string]string {
		t.Helper()
		out, err := exec.Command(os.Args[0], seccompSelfTestArg, mode).Output()
		if err != nil {
			t.Fatalf("%s: %v (%s)", mode, err, out)
		}
		var res map[string]string
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("%s: %v: %s", mode, err, out)
		}
		return res
	}
	plain := run("plain")
	filtered := run("filtered")
	t.Logf("without the filter: %v", plain)
	t.Logf("with the filter:    %v", filtered)
	if filtered["installed"] != "ok" {
		t.Fatalf("the filter was not installed: %v", filtered)
	}
	for _, name := range append([]string{"keyctl-session"}, seccompSelfTestDenied...) {
		if filtered[name] != "EPERM" {
			t.Errorf("%s under the filter: %s, want EPERM", name, filtered[name])
		}
	}
	for _, name := range []string{"unshare-newuser", "personality-addr-no-randomize", "ioctl-tiocsti", "ioctl-tioclinux",
		"ioctl-tiocsti-high", "clone-newuser"} {
		if filtered[name] != "EPERM" {
			t.Errorf("%s under the filter: %s, want EPERM", name, filtered[name])
		}
	}
	if filtered["clone3"] != "ENOSYS" {
		t.Errorf("clone3 under the filter: %s, want ENOSYS", filtered["clone3"])
	}
	// The personality query answers as it does without the filter (it changes nothing), and so do the
	// calls the filter lets through.
	for _, name := range []string{"getpid", "unshare-0", "personality-linux", "personality-query", "ioctl-tcgets", "clone-plain"} {
		if filtered[name] != plain[name] {
			t.Errorf("%s: %s under the filter, %s without it", name, filtered[name], plain[name])
		}
	}
	// These would not be EPERM without the filter on any kernel we ship for, so EPERM above came from it.
	// (fsopen, fsmount, fspick and move_mount check the mount capability before anything else and answer
	// EPERM to an unprivileged caller either way; open_tree, fsconfig and mount_setattr look at their
	// arguments first.)
	for _, name := range []string{"unshare-newuser", "personality-addr-no-randomize", "ioctl-tiocsti", "ioctl-tiocsti-high",
		"clone3", "setns", "name_to_handle_at", "io_uring_enter", "process_vm_readv",
		"open_tree", "fsconfig", "mount_setattr"} {
		if plain[name] == filtered[name] {
			t.Errorf("%s answers %s with and without the filter; the check cannot tell them apart", name, plain[name])
		}
	}
	if runtime.GOARCH == "amd64" {
		cmd := exec.Command(os.Args[0], seccompSelfTestArg, "x32")
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("x32 child: %v", err)
		}
		ws := ee.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != syscall.SIGSYS {
			t.Fatalf("an x32 system call under the filter: %v, want killed by SIGSYS", ws)
		}
	}
}

// seccompSelfTestArg starts the in-kernel check in a child (TestMain).
const seccompSelfTestArg = "__seccomp-selftest"

func init() {
	testMainHooks[seccompSelfTestArg] = func(args []string) int {
		if len(args) != 1 {
			return 2
		}
		if args[0] == "exit0" {
			return 0
		}
		return runSeccompSelfTest(args[0])
	}
}

// seccompSelfTestDenied are the listed calls whose answer is EPERM under the filter; the kernel itself
// may also answer EPERM for the privileged ones, which the discrimination list above leaves out.
var seccompSelfTestDenied = []string{"add_key", "request_key", "bpf", "perf_event_open", "io_uring_setup",
	"io_uring_enter", "io_uring_register", "ptrace", "process_vm_readv", "process_vm_writev", "kexec_load",
	"kexec_file_load", "init_module", "finit_module", "delete_module", "mount", "umount2", "pivot_root",
	"setns", "open_by_handle_at", "name_to_handle_at", "acct", "swapon", "swapoff", "reboot", "syslog",
	"fsopen", "fsconfig", "fsmount", "fspick", "move_mount", "open_tree", "mount_setattr"}

// runSeccompSelfTest is the child of TestSeccompFilterInKernel. It prints a JSON object mapping each
// probe to the errno name it got ("ok" for success).
func runSeccompSelfTest(mode string) int {
	runtime.LockOSThread()
	res := map[string]string{}
	if mode == "filtered" || mode == "x32" {
		prog, err := seccompProgramForHost()
		if err == nil {
			err = installSeccompOnThisThread(prog)
		}
		if err != nil {
			res["installed"] = err.Error()
			b, _ := json.Marshal(res)
			fmt.Println(string(b))
			return 0
		}
		res["installed"] = "ok"
	}
	nr := specNr[runtime.GOARCH]
	if mode == "x32" {
		syscall.RawSyscall(uintptr(0x40000000|nr["getpid"]), 0, 0, 0)
		fmt.Println(`{"x32":"survived"}`)
		return 0
	}
	errname := func(r1 uintptr, e syscall.Errno) string {
		if e == 0 {
			return "ok"
		}
		switch e {
		case syscall.EPERM:
			return "EPERM"
		case syscall.ENOSYS:
			return "ENOSYS"
		case syscall.EBADF:
			return "EBADF"
		case syscall.EINVAL:
			return "EINVAL"
		case syscall.EFAULT:
			return "EFAULT"
		}
		return fmt.Sprintf("errno %d", int(e))
	}
	call := func(name string, args ...uintptr) string {
		var a [6]uintptr
		copy(a[:], args)
		r1, _, e := syscall.RawSyscall6(uintptr(nr[name]), a[0], a[1], a[2], a[3], a[4], a[5])
		return errname(r1, e)
	}
	bad := ^uintptr(0) // fd -1, an impossible pointer, or "all bits"
	res["getpid"] = call("getpid")
	res["keyctl-session"] = call("keyctl", 0, uintptr(int32ToUintptr(-3)), 1) // KEYCTL_GET_KEYRING_ID, session, create
	res["add_key"] = call("add_key", 0, 0, 0, 0, 0)
	res["request_key"] = call("request_key", 0, 0, 0, 0)
	res["bpf"] = call("bpf", 9999, 0, 0)
	res["perf_event_open"] = call("perf_event_open", 0, 0, bad, bad, 0)
	res["io_uring_setup"] = call("io_uring_setup", 0, 0)
	res["io_uring_enter"] = call("io_uring_enter", bad, 0, 0, 0, 0, 0)
	res["io_uring_register"] = call("io_uring_register", bad, 0, 0, 0)
	res["ptrace"] = call("ptrace", 3 /* PTRACE_PEEKUSER */, 1, 0, 0)
	res["process_vm_readv"] = call("process_vm_readv", uintptr(os.Getpid()), 0, 0, 0, 0, 0)
	res["process_vm_writev"] = call("process_vm_writev", uintptr(os.Getpid()), 0, 0, 0, 0, 0)
	res["kexec_load"] = call("kexec_load", 0, 0, 0, 0)
	res["kexec_file_load"] = call("kexec_file_load", bad, bad, 0, 0, 0)
	res["init_module"] = call("init_module", 0, 0, 0)
	res["finit_module"] = call("finit_module", bad, 0, 0)
	res["delete_module"] = call("delete_module", 0, 0)
	res["mount"] = call("mount", 0, 0, 0, 0, 0)
	res["umount2"] = call("umount2", 0, 0)
	res["pivot_root"] = call("pivot_root", 0, 0)
	res["setns"] = call("setns", bad, 0)
	res["open_by_handle_at"] = call("open_by_handle_at", bad, 0, 0)
	res["name_to_handle_at"] = call("name_to_handle_at", bad, 0, 0, 0, 0)
	res["acct"] = call("acct", 0)
	res["swapon"] = call("swapon", 0, 0)
	res["swapoff"] = call("swapoff", 0)
	res["reboot"] = call("reboot", 0, 0, 0, 0)
	res["syslog"] = call("syslog", 10 /* SYSLOG_ACTION_SIZE_BUFFER */, 0, 0)
	res["clone3"] = call("clone3", 0, 0)
	res["unshare-newuser"] = call("unshare", 0x10000000)
	res["unshare-0"] = call("unshare", 0)
	res["personality-query"] = call("personality", 0xffffffff)
	res["personality-linux"] = call("personality", 0)
	// ADDR_NO_RANDOMIZE takes effect only at the next exec, which this child never does; PER_LINUX is set
	// back right after.
	res["personality-addr-no-randomize"] = call("personality", 0x0040000)
	_ = call("personality", 0)
	// The new mount API with arguments that cannot mount anything: no file system name, bad descriptors,
	// flags no kernel defines.
	res["fsopen"] = call("fsopen", 0, 0)
	res["fsconfig"] = call("fsconfig", bad, 0, 0, 0, 0)
	res["fsmount"] = call("fsmount", bad, 0, 0)
	res["fspick"] = call("fspick", bad, 0, 0)
	res["move_mount"] = call("move_mount", bad, 0, bad, 0, 0xffffffff)
	res["open_tree"] = call("open_tree", bad, 0, 0)
	res["mount_setattr"] = call("mount_setattr", bad, 0, 0, 0, 0)
	res["ioctl-tiocsti"] = call("ioctl", bad, 0x5412, 0)
	res["ioctl-tioclinux"] = call("ioctl", bad, 0x541c, 0)
	res["ioctl-tiocsti-high"] = call("ioctl", bad, uintptr(uint64(0x1_0000_5412)), 0)
	res["ioctl-tcgets"] = call("ioctl", bad, 0x5401, 0)
	// clone(2) through the runtime's own fork: a child in a new user namespace, and a plain one.
	self, _ := os.Executable()
	for name, flags := range map[string]uintptr{"clone-newuser": 0x10000000, "clone-plain": 0} {
		cmd := exec.Command(self, seccompSelfTestArg, "exit0")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags}
		err := cmd.Run()
		var errno syscall.Errno
		switch {
		case err == nil:
			res[name] = "ok"
		case errors.As(err, &errno):
			res[name] = errname(0, errno)
		default:
			res[name] = err.Error()
		}
	}
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	return 0
}

func int32ToUintptr(v int32) uintptr { return uintptr(uint32(v)) }

// installSeccompOnThisThread installs prog on the calling (locked) thread.
func installSeccompOnThisThread(prog []byte) error {
	const prSetNoNewPrivs, prSetSeccomp, seccompModeFilter = 38, 22, 2
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %v", e)
	}
	fprog := struct {
		len    uint16
		filter unsafe.Pointer
	}{uint16(len(prog) / 8), unsafe.Pointer(&prog[0])}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&fprog)), 0, 0, 0); e != 0 {
		return fmt.Errorf("PR_SET_SECCOMP: %v", e)
	}
	runtime.KeepAlive(prog)
	return nil
}

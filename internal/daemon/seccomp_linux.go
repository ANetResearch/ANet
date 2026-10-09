package daemon

// seccomp_linux.go assembles the seccomp filter every sandboxed run is started under (LEC SB-6). bwrap
// reads the program from fd 5 (`--seccomp 5`) and installs it before it starts `anet __sandbox-init`, so
// it covers init, the agent and everything they start.
//
// The filter is classic BPF written out instruction by instruction with the standard library only; the
// bytes depend on nothing but the CPU architecture, so they are a golden vector (G-SECCOMP,
// testdata/seccomp). It is a list of refusals in front of "allow", not an allow list: the namespaces,
// --cap-drop ALL and the empty network do the confining, and the filter takes away the kernel interfaces
// an unprivileged process can still reach that matter most to a sandbox escape or to the host's keys:
//
//   - another architecture, or the x32 ABI on x86_64 → the process is killed (the numbers below would
//     mean something else there);
//   - clone3 → ENOSYS, because its flags are in memory the filter cannot read; glibc then uses clone;
//   - clone and unshare with CLONE_NEWUSER → EPERM, whether or not bwrap could add --disable-userns;
//   - keyrings, bpf, perf, userfaultfd, io_uring, ptrace and process_vm_*, kexec and modules, mounts (the
//     old call and the new mount API: fsopen, fsconfig, fsmount, fspick, move_mount, open_tree,
//     mount_setattr) and setns, file handles, acct, swap, reboot and syslog → EPERM;
//   - personality other than PER_LINUX and the query 0xffffffff (which changes nothing, and which some
//     runtimes make at start-up), and the TIOCSTI and TIOCLINUX ioctls → EPERM.
//
// The kernel reads personality's argument and ioctl's command as 32-bit values, so the filter compares
// their low 32 bits only: a caller that sets the high bits must not slip a TIOCSTI past an equality test
// on the whole register.

import (
	"encoding/binary"
	"fmt"
	"runtime"
)

// seccomp return values (linux/seccomp.h) and the errno values the filter returns, which are the same on
// both architectures.
const (
	seccompRetKillProcess = 0x80000000
	seccompRetErrno       = 0x00050000
	seccompRetAllow       = 0x7fff0000

	errnoEPERM  = 1
	errnoENOSYS = 38
)

// Values the filter compares against.
const (
	auditArchX86_64  = 0xc000003e // AUDIT_ARCH_X86_64
	auditArchAARCH64 = 0xc00000b7 // AUDIT_ARCH_AARCH64
	x32SyscallBit    = 0x40000000 // __X32_SYSCALL_BIT
	cloneNewUser     = 0x10000000 // CLONE_NEWUSER
	perLinux         = 0x0000     // PER_LINUX
	perQuery         = 0xffffffff // personality(0xffffffff) reads the persona and sets nothing
	ioctlTIOCSTI     = 0x5412
	ioctlTIOCLINUX   = 0x541c
)

// Offsets into struct seccomp_data: nr, arch, then instruction_pointer and six 64-bit arguments. Both
// architectures are little-endian, so an argument's low 32 bits are at its own offset.
const (
	seccompDataNr       = 0
	seccompDataArch     = 4
	seccompDataArg0Low  = 16
	seccompDataArg1Low  = 24
	seccompProgramLimit = 255 // jumps are 8-bit; the program must stay short enough for every target
)

// seccompDeniedEPERM are the system calls refused with EPERM whatever their arguments, in the order LEC
// SB-6 lists them.
var seccompDeniedEPERM = []string{
	"keyctl", "add_key", "request_key", "bpf", "perf_event_open", "userfaultfd",
	"io_uring_setup", "io_uring_enter", "io_uring_register", "ptrace", "process_vm_readv",
	"process_vm_writev", "kexec_load", "kexec_file_load", "init_module", "finit_module",
	"delete_module", "mount", "umount2", "pivot_root", "setns", "open_by_handle_at",
	"name_to_handle_at", "acct", "swapon", "swapoff", "reboot", "syslog",
	// The new mount API reaches what mount does (SB-6, 2026-10-04).
	"fsopen", "fsconfig", "fsmount", "fspick", "move_mount", "open_tree", "mount_setattr",
}

// seccompNewMountNr are the new mount API's numbers, the same on both architectures (they were added after
// the system call tables were unified: include/uapi/asm-generic/unistd.h, syscall_64.tbl).
var seccompNewMountNr = map[string]uint32{
	"open_tree": 428, "move_mount": 429, "fsopen": 430, "fsconfig": 431, "fsmount": 432, "fspick": 433,
	"mount_setattr": 442,
}

func init() {
	for _, abi := range seccompABIs {
		for name, nr := range seccompNewMountNr {
			abi.nr[name] = nr
		}
	}
}

// seccompABI is one architecture's audit value and system call numbers.
type seccompABI struct {
	audit uint32
	x32   bool // x86_64: numbers with the x32 bit are another ABI
	nr    map[string]uint32
}

// seccompABIs are the architectures the sandbox runs on, by GOARCH. The numbers are the kernel's
// (arch/x86/entry/syscalls/syscall_64.tbl; include/uapi/asm-generic/unistd.h for arm64).
var seccompABIs = map[string]seccompABI{
	"amd64": {audit: auditArchX86_64, x32: true, nr: map[string]uint32{
		"clone": 56, "clone3": 435, "unshare": 272, "personality": 135, "ioctl": 16,
		"keyctl": 250, "add_key": 248, "request_key": 249, "bpf": 321, "perf_event_open": 298,
		"userfaultfd": 323, "io_uring_setup": 425, "io_uring_enter": 426, "io_uring_register": 427,
		"ptrace": 101, "process_vm_readv": 310, "process_vm_writev": 311, "kexec_load": 246,
		"kexec_file_load": 320, "init_module": 175, "finit_module": 313, "delete_module": 176,
		"mount": 165, "umount2": 166, "pivot_root": 155, "setns": 308, "open_by_handle_at": 304,
		"name_to_handle_at": 303, "acct": 163, "swapon": 167, "swapoff": 168, "reboot": 169,
		"syslog": 103,
	}},
	"arm64": {audit: auditArchAARCH64, nr: map[string]uint32{
		"clone": 220, "clone3": 435, "unshare": 97, "personality": 92, "ioctl": 29,
		"keyctl": 219, "add_key": 217, "request_key": 218, "bpf": 280, "perf_event_open": 241,
		"userfaultfd": 282, "io_uring_setup": 425, "io_uring_enter": 426, "io_uring_register": 427,
		"ptrace": 117, "process_vm_readv": 270, "process_vm_writev": 271, "kexec_load": 104,
		"kexec_file_load": 294, "init_module": 105, "finit_module": 273, "delete_module": 106,
		"mount": 40, "umount2": 39, "pivot_root": 41, "setns": 268, "open_by_handle_at": 265,
		"name_to_handle_at": 264, "acct": 89, "swapon": 224, "swapoff": 225, "reboot": 142,
		"syslog": 116,
	}},
}

// seccompProgramForHost returns the filter for the architecture this binary runs on, or an error on an
// architecture the sandbox has no filter for (the sandbox is then unavailable: seccomp_failed).
func seccompProgramForHost() ([]byte, error) {
	return seccompProgram(runtime.GOARCH)
}

// seccompProgram returns the filter for goarch as the raw array of struct sock_filter that bwrap's
// --seccomp reads.
func seccompProgram(goarch string) ([]byte, error) {
	abi, ok := seccompABIs[goarch]
	if !ok {
		return nil, fmt.Errorf("no seccomp filter for the %s architecture", goarch)
	}
	var a bpfAssembler
	// Architecture first: the numbers below are this architecture's.
	a.ld(seccompDataArch)
	a.jeq(abi.audit, "", "kill")
	a.ld(seccompDataNr)
	if abi.x32 {
		a.jset(x32SyscallBit, "kill", "")
	}
	a.jeq(abi.nr["clone3"], "enosys", "")
	a.jeq(abi.nr["clone"], "newuser", "")
	a.jeq(abi.nr["unshare"], "newuser", "")
	for _, name := range seccompDeniedEPERM {
		nr, ok := abi.nr[name]
		if !ok {
			return nil, fmt.Errorf("%s: no number for %s", goarch, name)
		}
		a.jeq(nr, "eperm", "")
	}
	a.jeq(abi.nr["personality"], "personality", "")
	a.jeq(abi.nr["ioctl"], "ioctl", "")
	a.ret(seccompRetAllow)

	a.label("newuser") // clone, unshare: flags in args[0]; CLONE_NEWUSER is in the low word
	a.ld(seccompDataArg0Low)
	a.jset(cloneNewUser, "eperm", "allow")

	a.label("personality") // the kernel takes an unsigned int
	a.ld(seccompDataArg0Low)
	a.jeq(perLinux, "allow", "")
	a.jeq(perQuery, "allow", "eperm")

	a.label("ioctl") // the kernel takes the command as an unsigned int
	a.ld(seccompDataArg1Low)
	a.jeq(ioctlTIOCSTI, "eperm", "")
	a.jeq(ioctlTIOCLINUX, "eperm", "allow")

	a.label("kill")
	a.ret(seccompRetKillProcess)
	a.label("eperm")
	a.ret(seccompRetErrno | errnoEPERM)
	a.label("enosys")
	a.ret(seccompRetErrno | errnoENOSYS)
	a.label("allow")
	a.ret(seccompRetAllow)
	return a.assemble()
}

// Classic BPF opcodes the filter uses (linux/bpf_common.h).
const (
	bpfLdWAbs  = 0x00 | 0x00 | 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeqK    = 0x05 | 0x10 | 0x00 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJsetK   = 0x05 | 0x40 | 0x00 // BPF_JMP | BPF_JSET | BPF_K
	bpfRetK    = 0x06 | 0x00        // BPF_RET | BPF_K
	bpfInsnLen = 8                  // sizeof(struct sock_filter)
)

// bpfAssembler collects instructions whose conditional jumps name labels ("" is the next instruction)
// and resolves them into the 8-bit relative offsets cBPF uses.
type bpfAssembler struct {
	ins    []bpfInsn
	labels map[string]int
}

type bpfInsn struct {
	code   uint16
	k      uint32
	jt, jf string
}

func (a *bpfAssembler) ld(off uint32) { a.ins = append(a.ins, bpfInsn{code: bpfLdWAbs, k: off}) }
func (a *bpfAssembler) ret(v uint32)  { a.ins = append(a.ins, bpfInsn{code: bpfRetK, k: v}) }
func (a *bpfAssembler) jeq(k uint32, jt, jf string) {
	a.ins = append(a.ins, bpfInsn{code: bpfJeqK, k: k, jt: jt, jf: jf})
}
func (a *bpfAssembler) jset(k uint32, jt, jf string) {
	a.ins = append(a.ins, bpfInsn{code: bpfJsetK, k: k, jt: jt, jf: jf})
}
func (a *bpfAssembler) label(name string) {
	if a.labels == nil {
		a.labels = map[string]int{}
	}
	a.labels[name] = len(a.ins)
}

func (a *bpfAssembler) assemble() ([]byte, error) {
	if len(a.ins) == 0 || len(a.ins) > seccompProgramLimit {
		return nil, fmt.Errorf("seccomp program of %d instructions", len(a.ins))
	}
	out := make([]byte, 0, len(a.ins)*bpfInsnLen)
	for pc, in := range a.ins {
		off := func(label string) (uint8, error) {
			if label == "" {
				return 0, nil
			}
			target, ok := a.labels[label]
			if !ok {
				return 0, fmt.Errorf("pc %d: no label %q", pc, label)
			}
			d := target - (pc + 1)
			if d < 0 || d > 255 {
				return 0, fmt.Errorf("pc %d: jump to %q is %d instructions away", pc, label, d)
			}
			return uint8(d), nil
		}
		jt, err := off(in.jt)
		if err != nil {
			return nil, err
		}
		jf, err := off(in.jf)
		if err != nil {
			return nil, err
		}
		var b [bpfInsnLen]byte
		binary.LittleEndian.PutUint16(b[0:], in.code)
		b[2], b[3] = jt, jf
		binary.LittleEndian.PutUint32(b[4:], in.k)
		out = append(out, b[:]...)
	}
	if last := a.ins[len(a.ins)-1]; last.code != bpfRetK {
		return nil, fmt.Errorf("seccomp program does not end in a return")
	}
	return out, nil
}

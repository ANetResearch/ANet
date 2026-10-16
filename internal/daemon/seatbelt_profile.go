package daemon

// seatbelt_profile.go writes the Seatbelt profile of one sandboxed run on macOS (LEC §4.3.8, MS-2). It is
// plain text built from paths and a port, so it is built and checked on every platform (G-SBPL, the
// forbidden-name test); sandbox_darwin.go writes it into the run directory and starts
// `/usr/bin/sandbox-exec -f <run>/profile.sb -- <anet> __sandbox-init --spec-fd 3` with it (MS-1).
//
// The profile starts from (deny default) and imports nothing: every operation the agent may perform is
// listed here.
//
//   - processes: fork; exec only of /bin, /usr/bin, path_helper, the command line tools (where /usr/bin/git
//     and python3 lead), the anet binary, the agent's runtime and the files of the run's work dir (as in
//     the Linux sandbox, where the agent runs what it builds there); process information and signals only
//     within the same sandbox (process information is refused first: Seatbelt allows it by default even
//     under (deny default));
//   - files: metadata anywhere (a program walking a path stats every directory on it); contents of the
//     system's libraries and tools, a short list of /private/etc, a few devices, the agent's runtime, the
//     run directory and the work dir; writes only to the work dir, the run directory's home/ and tmp/,
//     and the descriptors a process already holds (/dev/null, /dev/fd, /dev/stdout, /dev/stderr);
//   - IPC: Unix-domain sockets (socketpair, for the pipes between an agent and its children) and the Mach
//     services in seatbeltMachServices: user and group lookups, notifications and logging, nothing that
//     holds or reaches a secret;
//   - network: out to the run's egress endpoint on the host's loopback, nothing else (no bind, no
//     inbound connection);
//   - sysctl reads, except the argument and environment areas of other processes (kern.procargs*),
//     which on macOS a process of the same user could otherwise read.
//
// It ends with what stays refused whatever is allowed above, since a later rule wins over an earlier one:
// executing /usr/bin/security (the keychain), /usr/bin/osascript and /usr/bin/open (other applications),
// the keychain, pasteboard, Launch Services, TCC and window server Mach services (seatbeltMachForbidden),
// binding, accepting and Apple events. It also refuses what a (deny default) profile of version 1 still
// allows by default (seatbeltRefused, seatbeltFcntlForbidden): hard links, clones and NVRAM, the two
// fcntl commands that change a file through a descriptor opened only for reading, Mach lookups across
// domains (around the allowed list), the hardware's properties (its serial number), posting system-wide
// notifications and ioctls on sockets. What a supported agent is seen to need on a Mac is opened by name,
// never by dropping these.
//
// The profile names paths and one port, never a key (EG-3): the run's key reaches the agent in its
// environment alone.

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/pathsafe"
)

// seatbeltEnabled says whether this build runs sandboxed turns on macOS. It is a compile-time constant:
// v0.3.0 is released with the value 's first CI run on macOS decides (MOD-V3). While it is false, a
// turn that would run in the sandbox on a Mac — an untrusted peer's, or any when trusted is sandbox — is
// turned away before the agent starts, sandbox_unavailable with the local reason
// platform_unsupported.pending, and never falls back to a direct run (GT-2). The direct and openai paths
// are not affected.
const seatbeltEnabled = true

// seatbeltGate is the macOS sandbox's one switch: nil while the sandbox is enabled, else the refusal
// every sandboxed run and every self-check on macOS answers.
func seatbeltGate(enabled bool) error {
	if enabled {
		return nil
	}
	return &sandboxProbeError{Code: sandboxCodePending, Detail: "the macOS sandbox is not part of this version"}
}

// sandboxCodePending is the local reason of a macOS sandbox this version does not have yet.
const sandboxCodePending = "platform_unsupported.pending"

// seatbeltPrepareGated is seatbeltPrepare behind the switch.
func seatbeltPrepareGated(enabled bool, p *sandboxPlan, path string) error {
	if err := seatbeltGate(enabled); err != nil {
		return err
	}
	return seatbeltPrepare(p, path)
}

// seatbeltSpec is the input of buildSeatbeltProfile. Every path is absolute, clean and resolved (Seatbelt
// matches the real path of a file, so /tmp must be given as /private/tmp, a home under /var/folders as
// /private/var/folders): the profile is a function of these fields alone (G-SBPL).
type seatbeltSpec struct {
	Init     string   // the anet binary: read and executed
	RunDir   string   // the run directory: read; its home/ and tmp/ are written
	WorkDir  string   // the interaction's sandbox work dir: read and written
	Runtime  []string // the agent's runtime directories: read and executed
	ReadOnly []string // further directories the runtime reads (a package manager's tree): read only
	// Port is the run's egress endpoint on the host's 127.0.0.1; 0 for none: no network at all.
	Port int
	// NeverAllow are paths the profile may not open in any way: an allowed path that equals, contains or
	// lies inside one of them makes the profile fail (the never-bind list of SB-3).
	NeverAllow []string
}

// seatbeltExecSystem are the system directories the agent may execute from (MS-2), each also in
// seatbeltReadSystem: /bin and /usr/bin, and the command line tools, which /usr/bin/git, python3 and the
// other developer tools in /usr/bin are shims for (read only, as every system directory). /usr/sbin and the
// rest of /usr/libexec are left out until a supported agent is seen to need them.
var seatbeltExecSystem = []string{"/bin", "/usr/bin", seatbeltCommandLineTools}

// seatbeltCommandLineTools is where Apple's command line tools are installed (xcode-select --install):
// the programs behind the /usr/bin shims, and the Python they bring.
const seatbeltCommandLineTools = "/Library/Developer/CommandLineTools"

// seatbeltExecSystemFiles are single system programs the agent may execute: path_helper, which a login
// shell's /etc/zprofile runs.
var seatbeltExecSystemFiles = []string{"/usr/libexec/path_helper"}

// seatbeltExecForbidden are programs the agent may never execute, even in an allowed directory (MS-2):
// the keychain tool and the two that drive other applications.
var seatbeltExecForbidden = []string{"/usr/bin/security", "/usr/bin/osascript", "/usr/bin/open"}

// seatbeltReadSystem are the system trees whose contents the agent may read: libraries, tools, shared
// data, the frameworks and the OS cryptexes holding the shared library cache, the command line tools, the
// time zone data /etc/localtime points into and the link /bin/sh reads to choose its shell. None holds
// anything of a user's.
var seatbeltReadSystem = []string{
	"/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/libexec", "/usr/share",
	"/System/Library", "/System/Cryptexes", "/System/Volumes/Preboot/Cryptexes", seatbeltCommandLineTools,
	"/private/var/db/timezone", "/private/var/select/sh",
}

// seatbeltReadEtc are the /private/etc entries the agent may read (/etc is a link to /private/etc):
// names of hosts, users, groups, protocols and services, the time zone link, the CA bundle the agent is
// pointed at (SSL_CERT_FILE, MS-2: no trustd), the search path files and the shells' system start-up
// files.
var seatbeltReadEtc = []string{
	"hosts", "passwd", "group", "protocols", "services", "shells", "localtime", "ssl",
	"paths", "paths.d", "profile", "bashrc", "zshenv", "zprofile", "zshrc",
}

// seatbeltDevRead and seatbeltDevWrite are the devices the agent may read and write: /dev/null and the
// random sources, and the paths of the descriptors a process already has. dtracehelper and
// autofs_nowait are opened by the system libraries of every program and give nothing away.
var (
	seatbeltDevRead = []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/stdin", "/dev/stdout",
		"/dev/stderr", "/dev/dtracehelper", "/dev/autofs_nowait"}
	seatbeltDevWrite = []string{"/dev/null", "/dev/stdout", "/dev/stderr"}
)

// seatbeltSSLCertFile is the CA bundle the agent's TLS reads (MS-2, MS-3): trustd stays closed.
const seatbeltSSLCertFile = "/etc/ssl/cert.pem"

// seatbeltMachServices are the Mach services the agent may look up (MS-2): user and group lookups (getpwuid,
// group membership), the notification centre and the system log. EXE-9 settles the minimal set with the
// supported agents; whatever it adds must stay clear of seatbeltMachForbidden (SI-64).
var seatbeltMachServices = []string{
	"com.apple.system.opendirectoryd.libinfo",
	"com.apple.system.opendirectoryd.membership",
	"com.apple.system.notification_center",
	"com.apple.system.logger",
	"com.apple.logd",
}

// seatbeltMachForbidden are the Mach service names the agent may never reach (MS-2, SI-64): the keychain
// and security daemons, the pasteboard, Launch Services, TCC and the window server. A trailing "*" is a
// prefix. No entry of seatbeltMachServices may match one, and the profile refuses them all at its end.
var seatbeltMachForbidden = []string{
	"com.apple.SecurityServer",
	"com.apple.securityd*",
	"com.apple.secd*",
	"com.apple.security.*",
	"com.apple.trustd*",
	"com.apple.pasteboard*",
	"com.apple.coreservices.launchservicesd",
	"com.apple.lsd.*",
	"com.apple.tccd*",
	"com.apple.windowserver*",
}

// seatbeltMachForbiddenMatch reports the forbidden pattern a Mach service name matches, if any.
func seatbeltMachForbiddenMatch(name string) (string, bool) {
	for _, pat := range seatbeltMachForbidden {
		if prefix, ok := strings.CutSuffix(pat, "*"); ok {
			if strings.HasPrefix(name, prefix) {
				return pat, true
			}
		} else if name == pat {
			return pat, true
		}
	}
	return "", false
}

// seatbeltSysctlForbidden are the sysctl name prefixes the agent may not read: another process's
// arguments and environment.
var seatbeltSysctlForbidden = []string{"kern.procargs"}

// seatbeltRefused are operations Seatbelt allows by default even in a (deny default) profile of version 1,
// which the profile refuses outright: a hard link or a clone could give a file the agent may not read a
// second name in its work dir, and NVRAM holds the machine's settings, some of them tied to the owner's
// account. A supported agent needs none of them, and tools that clone or link where they can fall back to
// copying when refused. The rest are refused because nothing the agent does needs them and each tells the
// peer something of the machine or reaches past the profile (LEC MS-2, 2026-10-10): a Mach lookup in
// another domain could reach a service seatbeltMachServices does not list, the hardware's properties
// include its serial number, a posted notification reaches every process of the user, and a socket ioctl
// reads the network interfaces. One is opened only when a run on a Mac shows a supported agent needs it,
// and then here, in G-SBPL and in the record of that run.
var seatbeltRefused = []string{"file-link", "file-clone", "nvram*",
	"mach-cross-domain-lookup", "iokit-get-properties", "darwin-notification-post", "socket-ioctl"}

// seatbeltFcntlForbidden are the fcntl commands that change a file through a descriptor opened only for
// reading, which no file-write* rule sees and (deny default) does not cover: F_MAKECOMPRESSED (80)
// truncates and compresses the file, F_TRANSFEREXTENTS (110) moves storage blocks between two files. The
// kernel checks only the file's permission bits for them, and the anet binary and the agent's runtime are
// the user's own files the profile lets the agent read.
var seatbeltFcntlForbidden = []int{80, 110}

// buildSeatbeltProfile returns the profile for spec. It fails on a path that is not absolute and clean,
// that holds a character the profile cannot carry, or that overlaps spec.NeverAllow, and on a Mach service
// of the allowed list that matches a forbidden name: the caller then refuses the run, it never runs it
// with less.
func buildSeatbeltProfile(spec seatbeltSpec) (string, error) {
	for _, name := range seatbeltMachServices {
		if pat, bad := seatbeltMachForbiddenMatch(name); bad {
			return "", fmt.Errorf("the Mach service %s matches the forbidden %s", name, pat)
		}
	}
	if spec.Port < 0 || spec.Port > 65535 {
		return "", fmt.Errorf("egress port %d is out of range", spec.Port)
	}
	check := func(what, p string) error {
		if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
			return fmt.Errorf("%s %q is not an absolute, clean path below /", what, p)
		}
		if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("%s holds a control character", what)
		}
		if n, bad := bindConflict(p, spec.NeverAllow); bad {
			return fmt.Errorf("refusing to open %s to the sandbox: it overlaps %s", p, n)
		}
		return nil
	}
	for _, p := range []struct{ what, path string }{{"anet binary", spec.Init}, {"run directory", spec.RunDir},
		{"sandbox work dir", spec.WorkDir}} {
		if err := check(p.what, p.path); err != nil {
			return "", err
		}
	}
	runtime := dedupPaths(spec.Runtime)
	readOnly := dedupPaths(spec.ReadOnly)
	for _, p := range append(slices.Clone(runtime), readOnly...) {
		if err := check("agent runtime", p); err != nil {
			return "", err
		}
	}
	if pathsafe.PathWithin(spec.WorkDir, spec.RunDir) || pathsafe.PathWithin(spec.RunDir, spec.WorkDir) {
		return "", errors.New("the work dir and the run directory overlap")
	}
	home, tmp := filepath.Join(spec.RunDir, "home"), filepath.Join(spec.RunDir, "tmp")

	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\n") }
	rule := func(head string, filters []string) {
		line("(" + head)
		for _, f := range filters {
			line("  " + f)
		}
		line(")")
	}
	lit := func(p string) string { return "(literal " + sbplString(p) + ")" }
	sub := func(p string) string { return "(subpath " + sbplString(p) + ")" }
	lits := func(ps []string) []string {
		out := make([]string, 0, len(ps))
		for _, p := range ps {
			out = append(out, lit(p))
		}
		return out
	}
	subs := func(ps []string) []string {
		out := make([]string, 0, len(ps))
		for _, p := range ps {
			out = append(out, sub(p))
		}
		return out
	}

	line("(version 1)")
	line("(deny default)")
	line("")
	line("; the Seatbelt profile of one sandboxed anet run: paths and a port only")
	line("")
	line("; processes")
	line("(allow process-fork)")
	execs := subs(seatbeltExecSystem)
	execs = append(execs, lits(seatbeltExecSystemFiles)...)
	execs = append(execs, lit(spec.Init))
	execs = append(execs, subs(runtime)...)
	execs = append(execs, sub(spec.WorkDir))
	rule("allow process-exec", execs)
	line("(deny process-info*)")
	line("(allow process-info* (target same-sandbox))")
	line("(allow signal (target same-sandbox))")
	line("(allow sysctl-read)")
	line("")
	line("; files")
	line("(allow file-read-metadata)")
	reads := subs(seatbeltReadSystem)
	for _, e := range seatbeltReadEtc {
		reads = append(reads, sub(filepath.Join("/private/etc", e)))
	}
	reads = append(reads, lits(seatbeltDevRead)...)
	reads = append(reads, sub("/dev/fd"))
	reads = append(reads, lit(spec.Init))
	reads = append(reads, subs(runtime)...)
	reads = append(reads, subs(readOnly)...)
	reads = append(reads, sub(spec.RunDir), sub(spec.WorkDir))
	rule("allow file-read*", reads)
	writes := []string{sub(spec.WorkDir), sub(home), sub(tmp)}
	writes = append(writes, lits(seatbeltDevWrite)...)
	writes = append(writes, sub("/dev/fd"))
	rule("allow file-write*", writes)
	rule("allow file-ioctl", lits([]string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/dtracehelper"}))
	line("")
	line("; IPC")
	line("(allow system-socket (socket-domain AF_UNIX))")
	machs := make([]string, 0, len(seatbeltMachServices))
	for _, n := range seatbeltMachServices {
		machs = append(machs, "(global-name "+sbplString(n)+")")
	}
	rule("allow mach-lookup", machs)
	line("")
	line("; network")
	if spec.Port > 0 {
		line("(allow network-outbound (remote ip " + sbplString("localhost:"+strconv.Itoa(spec.Port)) + "))")
	} else {
		line("; no egress endpoint: no network")
	}
	line("")
	line("; refused whatever is allowed above (a later rule wins)")
	rule("deny process-exec", lits(seatbeltExecForbidden))
	forbidden := make([]string, 0, len(seatbeltMachForbidden))
	for _, pat := range seatbeltMachForbidden {
		if prefix, ok := strings.CutSuffix(pat, "*"); ok {
			forbidden = append(forbidden, "(global-name-prefix "+sbplString(prefix)+")")
		} else {
			forbidden = append(forbidden, "(global-name "+sbplString(pat)+")")
		}
	}
	rule("deny mach-lookup", forbidden)
	sysctls := make([]string, 0, len(seatbeltSysctlForbidden))
	for _, p := range seatbeltSysctlForbidden {
		sysctls = append(sysctls, "(sysctl-name-prefix "+sbplString(p)+")")
	}
	rule("deny sysctl-read", sysctls)
	for _, op := range seatbeltRefused {
		line("(deny " + op + ")")
	}
	fcntls := make([]string, 0, len(seatbeltFcntlForbidden))
	for _, c := range seatbeltFcntlForbidden {
		fcntls = append(fcntls, strconv.Itoa(c))
	}
	line("(deny system-fcntl (fcntl-command " + strings.Join(fcntls, " ") + "))")
	line("(deny network-bind)")
	line("(deny network-inbound)")
	line("(deny appleevent-send)")
	return b.String(), nil
}

// sbplString is s as a profile string literal. buildSeatbeltProfile has refused control characters; a
// backslash and a double quote are escaped.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// dedupPaths is paths without empty entries and duplicates, in their first order.
func dedupPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// seatbeltPackageTrees are the package-manager trees the agent's runtime lies in, read only: a runtime
// installed by Homebrew (<prefix>/Cellar/<formula>/<version>/…) loads the libraries of other formulae
// from the same Cellar, which its own directories do not hold. Nothing else is added.
func seatbeltPackageTrees(runtime []string) []string {
	var out []string
	for _, dir := range runtime {
		parts := strings.Split(filepath.Clean(dir), string(filepath.Separator))
		for i := len(parts) - 3; i >= 1; i-- {
			if parts[i] == "Cellar" {
				if tree := string(filepath.Separator) + filepath.Join(parts[:i+1]...); !slices.Contains(out, tree) {
					out = append(out, tree)
				}
				break
			}
		}
	}
	return out
}

// Command anet is the anet daemon + CLI. `anet daemon` runs the long-lived process (identity and
// encryption keys, the local task store, the hub relay client, the local control plane, and the modules —
// among them the local A2A interface on 127.0.0.1); the other verbs are thin clients that drive the
// running daemon over its control API, except the few that read the data directory directly (init,
// doctor, audit, agents, verify, update). Daemons exchange sealed, signed envelopes through a hub (or
// directly over p2p); tasks are A2A tasks. See internal/daemon and docs/A2A-DESIGN-zh.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/localpeer"
	"github.com/ANetResearch/ANet/internal/loopguard"
	"github.com/ANetResearch/ANet/module"
)

func main() {
	// Internal commands a sandbox starts this binary as; nothing else may run first.
	if code, ok := sandboxSubcommand(os.Args[1:]); ok {
		os.Exit(code)
	}
	// --id <name> is a GLOBAL selector (may appear anywhere) picking which named identity to act on; it is
	// pulled out before verb dispatch. See internal/daemon identities.go for the full selection precedence.
	idFlag, args := extractGlobalID(os.Args[1:])
	// A command or flag 0.3.0 removed is refused before anything is read or
	// sent (ux §4.3, cli_removed, exit 2).
	if len(args) > 0 {
		if s, ok := refuseRemoved(args[0], args[1:], a2ashape.Lang()); ok {
			fmt.Fprintln(os.Stderr, s)
			os.Exit(2)
		}
		// An API key on the command line: refused the same way, the key not repeated (LEC AV-8).
		if s, ok := refuseAPIKeyOnArgv(args[0], args[1:], a2ashape.Lang()); ok {
			fmt.Fprintln(os.Stderr, s)
			os.Exit(2)
		}
		// An <aid> argument that is not a whole AID (a short form, a prefix, a name): aid_invalid, exit
		// 2, before the daemon is asked anything (ux UX-G8a).
		if s, next, ok := refuseBadAID(args[0], args[1:], a2ashape.Lang()); ok {
			say(os.Stderr, s, next)
			os.Exit(2)
		}
	}
	layout := daemon.ResolveLayout(idFlag)
	explicit := daemon.SelectionIsExplicit(idFlag)
	if len(args) == 0 {
		guide(layout)
		return
	}
	cmd, rest := args[0], args[1:]
	fail := func(err error) {
		if err == nil {
			return
		}
		os.Exit(exitStatus(err, os.Stderr))
	}
	// A misspelled flag is a typo, not a silent no-op. See knownFlags.
	if err := checkFlags(cmd, rest); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	switch cmd {
	case "daemon":
		if hasFlag(rest, "--detach", "-d") {
			fail(runDaemonDetached(layout))
			return
		}
		runDaemon(layout)
	case "up": // start a node detached: `anet up [name] [--all]` (alias for `anet daemon --detach`)
		fail(runUp(idFlag, rest))
	case "stop", "down": // graceful shutdown: `anet stop [name] [--all]`
		fail(runStop(idFlag, rest))
	case "id", "ids", "identity": // manage named identities on this machine
		fail(runID(rest))
	case "version", "--version", "-v":
		fmt.Printf("anet %s (commit %s, built %s)\n",
			daemon.Version, daemon.BuildCommit, daemon.BuildAt)
		// Which optional subsystems are in THIS binary, read off the
		// registry rather than off a build-time stamp.
		//
		// version.Tags records the tag string the build was given, and a
		// `go build -tags shell` with no -X ldflag produces a binary that
		// has the module and reports no tags — a field that can say
		// "default" about a binary which can run commands on its host is
		// worse than no field. The registry cannot be wrong about this:
		// a module is in the list because its init() ran, which happened
		// because the linker kept it.
		fmt.Printf("modules: %s\n", compiledModulesReport())
	case "mcp":
		// Serves over stdio, so it must not share the process with
		// anything that prints: a stray line on stdout is a protocol
		// framing error to the client.
		fail(runMCP(layout, explicit))
	case "verify":
		// Above the daemon resolution on purpose: checking a receipt takes
		// a receipt and a key history and nothing else. Requiring a running
		// daemon would make "anyone can verify" mean "anyone running our
		// software", which is a different and much smaller claim.
		fail(verify(layout, rest))
	case "logs":
		printLogs(layout, rest)
	case "agents": // register the MCP server with local coding agents; files only, no daemon
		fail(runAgents(layout, rest))
	case "update":
		fail(runUpdate(rest))
	case "init": // write the SI-5 safe defaults explicitly (A2A-DESIGN §13.1); needs no daemon
		fail(runInit(layout, rest))
	case "doctor": // read-only report from the data directory; needs no daemon
		fail(runDoctor(layout, rest))
	case "retention": // how long task content is kept; works with or without the daemon (FS §5.2)
		fail(runRetention(layout, rest))
	case "audit":
		// The local evidence chain, read and verified from disk with or without a daemon;
		// `anet audit hub` is audit-hub (A2A-DESIGN §14).
		if len(rest) > 0 && rest[0] == "hub" {
			if err := checkFlags("audit-hub", rest[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(2)
			}
			fail(runClient(layout, "audit-hub", rest[1:], explicit))
			return
		}
		fail(runAudit(layout, rest))
	case "help", "-h", "--help":
		// ux §4.2: the common commands, or with --all every one, in the
		// language of this process (UX-G6). Nothing is asked of the daemon.
		if len(rest) > 0 && (rest[0] == "--all" || rest[0] == "all") {
			usageAll()
		} else {
			usageShort()
		}
	default:
		fail(runClient(layout, cmd, rest, explicit))
	}
}

// compiledModulesReport renders the module list `anet version` prints.
//
// Separate from the print statement because the empty case is a real
// build: with every subtraction tag set, the registry holds nothing, and
// CI builds and tests exactly that combination. Printing an empty string
// there would make a kernel-only binary look like a binary whose registry
// failed to populate, so the absence is named rather than shown as
// silence.
func compiledModulesReport() string {
	mods := module.Compiled()
	if len(mods) == 0 {
		return "(none)"
	}
	return strings.Join(mods, ",")
}

// extractGlobalID pulls a global "--id <name>" (or "--id=name") selector out of args, returning the name
// ("" if absent) and the remaining args with it removed, so it can precede or follow the verb.
func extractGlobalID(args []string) (id string, rest []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--id" && i+1 < len(args):
			id = args[i+1]
			i++
		case strings.HasPrefix(a, "--id="):
			id = strings.TrimPrefix(a, "--id=")
		default:
			rest = append(rest, a)
		}
	}
	return id, rest
}

const nextPrefix = "  → "

// shortAID is an AID cut for a listing that is not copied from (anet id
// ls); a command is always given the whole AID (ux UX-G8a).
func shortAID(aid string) string {
	if aid == "" {
		return "(no identity yet)"
	}
	if len(aid) > 20 {
		return aid[:20] + "…"
	}
	return aid
}

// exitStatus is the exit status of a command that ended in err (ux UX-C4),
// after saying what is left to say on w: 2 for a command used wrongly (its
// usage line), 1 for everything else. An error without words (errExit1,
// errQuiet, errNoTTY, errDeclined) was said already: "this receipt does not
// verify" is an answer, not a malfunction of the tool.
func exitStatus(err error, w io.Writer) int {
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(w, ue.Error())
		return 2
	}
	if errors.Is(err, errInputRequired) {
		return 3 // task watch: the task waits for the user (UX-C4)
	}
	if err.Error() != "" {
		fmt.Fprintln(w, "error:", err)
	}
	return 1
}

func runDaemon(layout daemon.Layout) {
	// A data directory an earlier anet wrote (FS-S1), or a config.json with a removed or invalid key
	// (ux UX-D3), does not start: every refusal, one sentence a line, before anything is written.
	if err := refuseConfig(layout); err != nil {
		os.Exit(1)
	}
	// Ensure this identity has a config with a non-colliding control port before New (which would otherwise
	// default a fresh dir to the shared 39811 and clash with another identity).
	if _, _, err := daemon.EnsureLayoutInit(layout); err != nil {
		fmt.Fprintln(os.Stderr, "daemon: init:", err)
		os.Exit(1)
	}
	// Route the standard logger to a file instead of the console (dependency chatter, relay-loop notices).
	_ = layout.EnsureRoot()
	if lf, err := os.OpenFile(layout.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		log.SetOutput(lf)
		defer lf.Close()
	}
	d, err := daemon.New(layout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon: start:", err)
		os.Exit(1)
	}
	defer d.Close()
	cfg, _ := daemon.LoadConfig(layout)
	hub := cfg.HubURL
	if hub == "" {
		hub = "(not registered — run `anet hub-register <url>`)"
	}
	fmt.Printf("anet %s — aid=%s\n", daemon.Version, d.AID())
	fmt.Printf("  control %s · hub %s\n", cfg.ControlAddr, hub)
	fmt.Println("  running in the foreground — press Ctrl+C to stop.")
	fmt.Println("  tip: for a resident node that survives this shell, use `anet daemon --detach` (alias `anet up`)")
	fmt.Println("       instead of `&`; then use `anet hub-register` / `anet console` / `anet status` from anywhere.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := d.ServeControl(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "daemon: control:", err)
		os.Exit(1)
	}
}

// refuseConfig prints why the daemon will not start with this data directory — one an earlier anet wrote
// (FS-S1: identity_v02 alone, since nothing else in it helps), or its config.json (ux UX-D3: a removed
// key, an invalid value) — one sentence a line, from the reason table, and returns errQuiet; nil when it
// starts. An unreadable or unparsable file is reported as it was before.
func refuseConfig(layout daemon.Layout) error {
	err := daemon.CheckDataDirForStart(layout)
	if err == nil {
		err = daemon.CheckConfigForStart(layout)
	}
	var cr *daemon.ConfigRefusal
	switch {
	case err == nil:
		return nil
	case errors.As(err, &cr):
		for _, s := range cr.Sentences(a2ashape.Lang()) {
			fmt.Fprintln(os.Stderr, s)
		}
	default:
		fmt.Fprintln(os.Stderr, "daemon: start:", err)
	}
	return errQuiet
}

// fileExists reports whether path exists (best-effort; used only for a guide hint).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// parseBool accepts the human-friendly on/off (and yes/no, 1/0, true/false) forms for toggle flags.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "yes", "y", "1":
		return true, nil
	case "off", "false", "no", "n", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected true/false (or on/off)")
}

// hasFlag reports whether any of names appears verbatim in args (bare boolean flags).
func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// localDaemonUp strictly probes whether THIS layout's OWN daemon is already serving — using only the data
// dir's own config address + token file, never the uid-scoped pointer fallback that ResolveControl uses.
// That distinction matters for `--detach`: with the fallback, a fresh data dir would "find" some OTHER
// running daemon and wrongly conclude it's already up (never starting the new identity). A missing token
// file means "not started here"; readiness is confirmed once our daemon writes its token and serves.
func localDaemonUp(layout daemon.Layout) bool {
	up, _ := probeLocalDaemon(layout)
	return up
}

// probeLocalDaemon is localDaemonUp with the reason it is not up, when there is one worth telling.
//
// It runs as the first step of `anet up`, which is exactly when the daemon is not holding its port and
// another local user may be: the probe goes through client.http, which verifies the listener before the
// token is written (A2A-DESIGN §7 item 10 [redteam:F18]). A listener that is not this user's daemon is
// reported with localpeer.ErrNotOurs in the chain.
func probeLocalDaemon(layout daemon.Layout) (bool, error) {
	tb, err := os.ReadFile(layout.ControlTokenPath())
	if err != nil {
		return false, nil
	}
	// The token is sent only to a loopback address (A2A-DESIGN §7.1); a daemon cannot be serving on any
	// other, since it refuses to start there.
	addr := daemon.LocalControlAddr(layout)
	if loopguard.CheckLoopbackAddr(addr) != nil {
		return false, nil
	}
	c := &client{
		base:    "http://" + addr,
		token:   strings.TrimSpace(string(tb)),
		timeout: 1500 * time.Millisecond,
	}
	_, code, e := c.fetch("/status", nil)
	return e == nil && code == 200, e
}

// runDaemonDetached starts the daemon as a fully detached background process (its own session, output to
// the data dir's daemon.log) and waits until its control plane answers, so `anet daemon --detach` / `anet
// up` leaves a resident daemon even after the launching shell exits — unlike `anet daemon &`, which dies
// with an agent's short-lived tool-call shell. Idempotent: if a daemon is already up it just prints status.
func runDaemonDetached(layout daemon.Layout) error {
	up, perr := probeLocalDaemon(layout)
	if up {
		fmt.Printf("anet daemon already running (data dir %s)\n", layout.Root)
		return runClient(layout, "status", nil, true)
	}
	if errors.Is(perr, localpeer.ErrNotOurs) {
		// Somebody else holds this identity's control port while its daemon is down. The token was not
		// sent; the daemon moves to a free port and records it in config.json, which every later
		// command reads.
		fmt.Fprintf(os.Stderr, "note: %v\n      starting the daemon on another port\n", perr)
	}
	// The child would refuse the same config.json; say why here, not after the readiness poll.
	if err := refuseConfig(layout); err != nil {
		return err
	}
	// Initialize the identity (auto-allocate a free control port for a fresh dir) BEFORE spawning, so the
	// child daemon binds a non-colliding port and the readiness poll targets the right address.
	if _, _, err := daemon.EnsureLayoutInit(layout); err != nil {
		return fmt.Errorf("init identity: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate anet binary: %w", err)
	}
	if err := layout.EnsureRoot(); err != nil {
		return err
	}
	logf, err := os.OpenFile(layout.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer logf.Close()
	c := exec.Command(exe, "daemon")
	// Pin the child to THIS data dir explicitly, regardless of how the parent resolved it, so a detached
	// daemon started from the default dir and one started with ANET_DATA_DIR both land where expected.
	c.Env = append(os.Environ(), "ANET_DATA_DIR="+layout.Root)
	c.Stdin = nil
	c.Stdout = logf
	c.Stderr = logf
	c.SysProcAttr = detachSysProcAttr() // new session so it outlives the launching shell (see spawn_*.go)
	if err := c.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	pid := c.Process.Pid
	// Keep the handle and reap in the background instead of releasing it.
	//
	// The child is already in its own session, so it outlives this process
	// either way; what the handle buys is the exit status. Without it,
	// every failure that happens AFTER the fork — a module configured but
	// not compiled in, a module name the registry does not know, a service
	// capability with no url, a control port already taken — was reported
	// as the same "did not become ready within 8s", after eight seconds of
	// polling a process that had already exited in about twenty
	// milliseconds with the exact reason on its way to the log.
	//
	// The asymmetry is what made it worth fixing: a config.json that does
	// not parse is caught before the fork and named immediately, so the
	// product plainly can say what is wrong. Seven of the eighteen cells in
	// the release matrix reported this, on both distributions and across
	// six variants.
	died := make(chan error, 1)
	go func() { died <- c.Wait() }()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if localDaemonUp(layout) {
			name := daemon.IdentityNameForDir(layout.Root)
			label := ""
			if name != "" {
				label = " [" + name + "]"
			}
			fmt.Printf("anet daemon started%s (pid %d, data dir %s) — stop it anytime with `anet stop%s`\n\n",
				label, pid, layout.Root, stopHint(name))
			reportA2APortConflict(layout)
			return runClient(layout, "status", nil, true)
		}
		select {
		case werr := <-died:
			// It wrote its reason to the log on the way out; say it here
			// rather than sending the operator to go and look.
			return daemonStartFailed(layout, werr)
		case <-time.After(150 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon did not become ready within 8s — check the log: anet logs")
}

// reportA2APortConflict says, on the terminal that started the node, that the local A2A interface did
// not start because another process held its port (module/a2a leaves a2a_port_conflict.txt). The node
// runs without it; the log line alone is too easy to miss for something that may mean another local
// user is collecting the A2A token from configured clients (A2A-DESIGN §11.1 [redteam:F18]).
func reportA2APortConflict(layout daemon.Layout) {
	b, err := os.ReadFile(filepath.Join(anethome.A2ADir(layout.Root), anethome.A2AConflictFile))
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "WARNING: the local A2A interface did not start.\n  %s\n\n", strings.TrimSpace(string(b)))
}

// daemonStartFailed reports why a detached daemon exited, quoting the log.
//
// The child logs the specific cause and then exits; the parent is the only
// one with a terminal. Reading the tail back is what turns "it did not
// start" into "it did not start because …".
func daemonStartFailed(layout daemon.Layout, werr error) error {
	reason := lastLogLines(layout.LogPath(), 4)
	if reason == "" {
		if werr != nil {
			return fmt.Errorf("the daemon exited immediately (%v) and wrote nothing to the log: %s",
				werr, layout.LogPath())
		}
		return fmt.Errorf("the daemon exited immediately and wrote nothing to the log: %s", layout.LogPath())
	}
	return fmt.Errorf("the daemon exited immediately:\n%s\n(full log: anet logs)", reason)
}

// lastLogLines returns the final n non-empty lines of a log file.
//
// Bounded read from the end: a daemon.log can be large, and this runs on
// the failure path of a command a person is watching.
func lastLogLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const window = 8 << 10
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return ""
	}
	if size > window {
		if _, err := f.Seek(-window, io.SeekEnd); err != nil {
			return ""
		}
	} else if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	var keep []string
	for _, ln := range strings.Split(string(b), "\n") {
		if ln = strings.TrimRight(ln, "\r"); strings.TrimSpace(ln) != "" {
			keep = append(keep, "  "+ln)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}

// stopHint returns the identity suffix for a "stop it with `anet stop<hint>`" message (" <name>" for a
// named identity, "" for the default one so the plain `anet stop` is shown).
func stopHint(name string) string {
	if name == "" || name == "default" {
		return ""
	}
	return " " + name
}

// runUp starts a node detached: `anet up [name] [--all]`. With --all it brings up every known identity;
// with a positional name it targets that identity; otherwise it uses the resolved selection.
func runUp(idFlag string, rest []string) error {
	// ANET_INVITE is ignored, said once (LEC AV-5).
	defaultHubCLI().warnInviteEnv()
	pos, flags := splitFlags(rest)
	if flags["all"] == "true" {
		ids, _ := daemon.ListIdentities()
		if len(ids) == 0 {
			return fmt.Errorf("no identities yet — create one with `anet id new <name>`")
		}
		for _, in := range ids {
			fmt.Printf("── %s ──\n", in.Name)
			if err := runDaemonDetached(daemon.Layout{Root: in.DataDir}); err != nil {
				fmt.Fprintf(os.Stderr, "  %s: %v\n", in.Name, err)
			}
		}
		return nil
	}
	if len(pos) > 0 && pos[0] != "" {
		if !daemon.ValidIdentityName(pos[0]) {
			return usageError{fmt.Sprintf("anet up [name] [--all]  (%q is not a name: use letters, digits and ._-)", printable(pos[0], 64))}
		}
		return runDaemonDetached(daemon.IdentityLayout(pos[0]))
	}
	return runDaemonDetached(daemon.ResolveLayout(idFlag))
}

// runStop gracefully stops a node: `anet stop [name] [--all]`. With --all it stops every RUNNING identity.
func runStop(idFlag string, rest []string) error {
	pos, flags := splitFlags(rest)
	if flags["all"] == "true" {
		// Scope to THIS home's identities (symmetric with `up --all`), so we never stop unrelated daemons
		// (e.g. raw-ANET_DATA_DIR test daemons or a different ANET_HOME).
		ids, _ := daemon.ListIdentities()
		stopped := 0
		for _, in := range ids {
			if !in.Running {
				continue
			}
			stopped++
			if err := stopDaemon(daemon.Layout{Root: in.DataDir}, true); err != nil {
				fmt.Fprintf(os.Stderr, "  %s: %v\n", in.Name, err)
			}
		}
		if stopped == 0 {
			fmt.Println("no identities are running")
		}
		return nil
	}
	if len(pos) > 0 && pos[0] != "" {
		if !daemon.ValidIdentityName(pos[0]) {
			return usageError{fmt.Sprintf("anet stop [name] [--all]  (%q is not a name: use letters, digits and ._-)", printable(pos[0], 64))}
		}
		return stopDaemon(daemon.IdentityLayout(pos[0]), true)
	}
	return stopDaemon(daemon.ResolveLayout(idFlag), daemon.SelectionIsExplicit(idFlag))
}

// stopDaemon asks a daemon to shut down gracefully via the control plane — no PID hunting / kill needed.
// The daemon replies 200 then drains; explicit selection resolves strictly so we never stop the wrong one.
func stopDaemon(layout daemon.Layout, explicit bool) error {
	resolve := daemon.ResolveControl
	if explicit {
		resolve = daemon.ResolveControlStrict
	}
	base, token, err := resolve(layout)
	if err != nil {
		return diagnoseNoDaemon("http://"+daemon.LocalControlAddr(layout), layout.Root, err)
	}
	c := &client{base: base, token: token, timeout: 5 * time.Second, dataDir: layout.Root}
	if _, code, err := c.fetch("/shutdown", nil); err != nil {
		return diagnoseNoDaemon(base, layout.Root, err)
	} else if code != http.StatusOK {
		return fmt.Errorf("stop: daemon at %s returned %d", base, code)
	}
	name := daemon.IdentityNameForDir(layout.Root)
	label := ""
	if name != "" && name != "default" {
		label = " [" + name + "]"
	}
	fmt.Printf("anet daemon stopping%s (data dir %s preserved). Restart with `anet up%s`.\n", label, layout.Root, stopHint(name))
	return nil
}

// runID manages named identities on this machine: `anet id ls|new|use|rm`.
func runID(rest []string) error {
	sub := "ls"
	if len(rest) > 0 {
		sub, rest = rest[0], rest[1:]
	}
	switch sub {
	case "ls", "list":
		return printIdentities()
	case "new", "create", "add":
		return idNew(rest)
	case "use", "switch", "select":
		return idUse(rest)
	case "rm", "remove", "delete":
		return idRm(rest)
	default:
		return usageError{fmt.Sprintf("anet id ls|new|use|rm  (%q is none of them)", printable(sub, 64))}
	}
}

// printIdentities lists every known identity with its status (the ★ marks the current default).
func printIdentities() error {
	ids, err := daemon.ListIdentities()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("No identities yet. Create one with:  anet id new <name>")
		fmt.Println("(or just `anet up` to start the default identity)")
		return nil
	}
	fmt.Printf("%-2s %-14s %-8s %-18s %-22s %s\n", "", "NAME", "STATE", "CONTROL", "AID", "DATA DIR")
	for _, in := range ids {
		star := " "
		if in.Current {
			star = "★"
		}
		state := "stopped"
		if in.Running {
			state = "running"
		}
		aid := in.AID
		if aid == "" {
			aid = "(no key yet)"
		} else {
			aid = shortAID(aid)
		}
		fmt.Printf("%-2s %-14s %-8s %-18s %-22s %s\n", star, in.Name, state, in.ControlAddr, aid, in.DataDir)
	}
	fmt.Println()
	fmt.Println("Act on one:  anet --id <name> <cmd>   ·   make it the default:  anet id use <name>")
	fmt.Println("Start/stop:  anet up <name> | anet stop <name> | anet up --all | anet stop --all")
	return nil
}

// idNew creates a fresh named identity (its own data dir, key, and auto-allocated control port) and starts
// it detached so it's immediately usable. Registration on the Hub is left to the operator's agent.
func idNew(rest []string) error {
	pos, _ := splitFlags(rest)
	if len(pos) == 0 || pos[0] == "" {
		return usageError{"anet id new <name>  (for example: anet id new coder)"}
	}
	name := pos[0]
	if name == "default" {
		return fmt.Errorf("`default` is the built-in identity (~/.anet); pick another name")
	}
	if !daemon.ValidIdentityName(name) {
		return usageError{fmt.Sprintf("anet id new <name>  (%q is not a name: use letters, digits and ._-, at most 64)", printable(name, 64))}
	}
	layout := daemon.IdentityLayout(name)
	if layout.Initialized() {
		return fmt.Errorf("identity %q already exists at %s — bring it up with `anet up %s`", name, layout.Root, name)
	}
	fmt.Printf("Creating identity %q …\n", name)
	if err := runDaemonDetached(layout); err != nil {
		return err
	}
	fmt.Printf("\nNext for %q:\n", name)
	fmt.Printf("  anet --id %s hub-register <hub-url> --name \"%s\" --caps \"...\"   # put it on the Hub\n", name, name)
	fmt.Printf("  anet id use %s                                                   # make it your default\n", name)
	return nil
}

// idUse sets the current default identity so subsequent bare `anet <cmd>` target it.
func idUse(rest []string) error {
	pos, _ := splitFlags(rest)
	if len(pos) == 0 || pos[0] == "" {
		return usageError{"anet id use <name>  (or default)"}
	}
	name := pos[0]
	if name != "default" {
		if !daemon.ValidIdentityName(name) {
			return usageError{fmt.Sprintf("anet id use <name>  (%q is not a name: use letters, digits and ._-)", printable(name, 64))}
		}
		if !daemon.IdentityLayout(name).Initialized() {
			return fmt.Errorf("no identity %q yet — create it with `anet id new %s`", name, name)
		}
	}
	if err := daemon.SetCurrentIdentity(name); err != nil {
		return err
	}
	fmt.Printf("default identity is now %q — bare `anet <cmd>` will target it\n", name)
	return nil
}

// idRm deletes a named identity's data dir (destroying its key/history). Guarded: refuses `default`,
// requires --purge to actually delete, and stops the daemon first if it is running.
func idRm(rest []string) error {
	pos, flags := splitFlags(rest)
	if len(pos) == 0 || pos[0] == "" {
		return usageError{"anet id rm <name> --purge  (destroys that identity's key and history)"}
	}
	name := pos[0]
	if name == "default" {
		return fmt.Errorf("refusing to remove the built-in `default` identity")
	}
	if !daemon.ValidIdentityName(name) {
		return usageError{fmt.Sprintf("anet id rm <name> --purge  (%q is not a name: use letters, digits and ._-)", printable(name, 64))}
	}
	layout := daemon.IdentityLayout(name)
	if !layout.Initialized() {
		return fmt.Errorf("no identity %q to remove", name)
	}
	if flags["purge"] != "true" {
		fmt.Printf("This will PERMANENTLY delete identity %q and its key/history:\n  %s\n", name, layout.Root)
		fmt.Printf("Re-run with --purge to confirm:  anet id rm %s --purge\n", name)
		return nil
	}
	if localDaemonUp(layout) {
		fmt.Printf("stopping running daemon for %q …\n", name)
		_ = stopDaemon(layout, true)
		time.Sleep(300 * time.Millisecond)
	}
	if err := os.RemoveAll(layout.Root); err != nil {
		return fmt.Errorf("remove %s: %w", layout.Root, err)
	}
	daemon.ClearCurrentIfName(name)
	fmt.Printf("removed identity %q (%s)\n", name, layout.Root)
	return nil
}

// runningElsewhere names the other identities on this machine whose daemon
// is running (not the one at base), for daemon_not_running to give the same
// command for each.
func runningElsewhere(base string) []string {
	var out []string
	for _, e := range daemon.RunningDaemons() {
		if "http://"+e.ControlAddr == base {
			continue
		}
		if id := daemon.IdentityNameForDir(e.DataDir); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func printLogs(layout daemon.Layout, rest []string) {
	p := layout.LogPath()
	b, err := os.ReadFile(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no daemon log at %s (has the daemon run yet?)\n", p)
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	n := 80
	if len(rest) > 0 {
		if v, e := strconv.Atoi(rest[0]); e == nil && v > 0 {
			n = v
		} else if rest[0] == "--all" || rest[0] == "all" {
			n = len(lines)
		}
	}
	if len(lines) == 1 && lines[0] == "" {
		fmt.Printf("# %s is empty\n", p)
		return
	}
	if n < len(lines) {
		lines = lines[len(lines)-n:]
	}
	fmt.Printf("# %s (last %d lines; `anet logs --all` for the whole file)\n", p, len(lines))
	fmt.Println(strings.Join(lines, "\n"))
}

// maybeFile returns v, or — if v begins with '@' — the contents of the file it names (so a long readme
// can be passed as `--readme @README.md`).
func maybeFile(v string) (string, error) {
	if strings.HasPrefix(v, "@") {
		b, err := os.ReadFile(strings.TrimPrefix(v, "@"))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return v, nil
}

// openBrowser best-effort opens a local page in the operator's default browser (used by `anet console`).
// It is given a file path, never a URL with a credential in it: its argument is on the opener's and the
// browser's command line, which other local users can read [redteam:F19].
func openBrowser(path string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	return exec.Command(name, append(args, path)...).Start()
}

// splitFlags separates positional args from "--key value" (and bare "--bool") flags in a verb's args.
// extractAttach pulls every `--attach PATH` / `--attach=PATH` / `-a PATH` out of args (repeatable),
// resolving each to an absolute path (the daemon does the disk read, possibly from a different cwd), and
// returns them plus the remaining args. Kept separate from splitFlags so a flag can appear multiple times.
func extractAttach(args []string) (paths []string, rest []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "--attach" || a == "-a") && i+1 < len(args):
			paths = append(paths, absPath(args[i+1]))
			i++
		case strings.HasPrefix(a, "--attach="):
			paths = append(paths, absPath(strings.TrimPrefix(a, "--attach=")))
		default:
			rest = append(rest, a)
		}
	}
	return paths, rest
}

// absPath resolves p to an absolute path (best-effort; returns p unchanged on failure).
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// runAutoReply drives `anet autoreply show|set|off` — the CLI/agent-facing switch for the daemon's
// built-in auto-reply loop, so nobody hand-edits config.json. `set` takes effect live (no restart), and
// changes only what it names: the daemon merges it into the current configuration (LEC §2.3.4).
func runAutoReply(c *client, rest []string) error {
	usage := "autoreply <show|set|test|off>\n" +
		"  set --backend exec  --agent <" + agentChoices() + "> [--model M] [--work-dir DIR] [--system-prompt S]\n" +
		"  set --backend openai --api-base URL --model M [--system-prompt S] [--require-image[=false]] [--usage-hint H]\n" +
		"  set --api-key-stdin < FILE   # the model API key: from a file, or typed on the terminal (never on the command line)\n" +
		"  common: [--poll-interval SEC] [--max-history N] [--api-timeout SEC] [--max-auto-replies N]\n" +
		"  set --egress-allow host[,host…]   # hosts the sandbox may reach on 443 (\"\" clears; adding asks on the terminal)\n" +
		"  test [\"question\"]    # run the configured backend once on this machine (no hub, no identity)\n" +
		"  off"
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "", "show":
		b, code, err := c.fetch("/status", nil)
		if err != nil {
			return err
		}
		if code != 200 {
			return fmt.Errorf("status: %s", strings.TrimSpace(string(b)))
		}
		var st struct {
			AutoReply json.RawMessage `json:"auto_reply"`
		}
		_ = json.Unmarshal(b, &st)
		if len(st.AutoReply) == 0 || string(st.AutoReply) == "null" {
			fmt.Println("auto-reply: off")
			return nil
		}
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, st.AutoReply, "", "  ")
		fmt.Println("auto-reply:", pretty.String())
		return nil
	case "off":
		return c.do("/autoreply", map[string]any{"off": true})
	case "test":
		c.timeout = 15 * time.Minute // exec agents (cursor/claude/…) can take tens of seconds
		prompt := strings.TrimSpace(strings.Join(rest[1:], " "))
		return c.do("/autoreply-test", map[string]any{"prompt": prompt})
	case "set":
		_, flags := splitFlags(rest[1:])
		body := map[string]any{}
		// --egress-allow first: the list is checked here (CN-1) and an added host confirmed on the
		// terminal before anything else is read or sent; it rides in the same request as the rest.
		if v, ok := flags["egress-allow"]; ok {
			sandbox, err := egressAllowRequest(c, v, flags["api-base"])
			if err != nil {
				return err
			}
			body["sandbox"] = sandbox
		}
		// The backend is named, or follows from --agent (the exec backend runs an agent). --api-base
		// says nothing about it: an exec agent in the sandbox can have a model service address too.
		if v := flags["backend"]; v != "" && v != "true" {
			body["backend"] = v
		} else if v := flags["agent"]; v != "" && v != "true" {
			body["backend"] = "exec"
		}
		strFlags := map[string]string{
			"agent": "agent", "work-dir": "work_dir", "command": "command",
			"openclaw-agent": "openclaw_agent", "model": "model", "api-base": "api_base",
			"system-prompt": "system_prompt", "untrusted": "untrusted",
			"usage-hint": "usage_hint", "error-reply": "error_reply",
		}
		for cliKey, jsonKey := range strFlags {
			if v, ok := flags[cliKey]; ok && v != "true" {
				body[jsonKey] = v
			}
		}
		// The daemon merges what set names, so a setting that is on stays on until set turns it off:
		// --require-image turns it on, --require-image=false off.
		if v, ok := flags["require-image"]; ok {
			switch strings.TrimSpace(v) {
			case "true":
				body["require_image"] = true
			case "false":
				body["require_image"] = false
			default:
				return usageError{usage}
			}
		}
		for cliKey, jsonKey := range map[string]string{
			"poll-interval": "poll_interval_seconds", "max-history": "max_history", "api-timeout": "api_timeout_seconds",
			"max-auto-replies": "max_auto_replies",
		} {
			if v, ok := flags[cliKey]; ok && v != "true" {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					return fmt.Errorf("--%s needs an integer", cliKey)
				}
				body[jsonKey] = n
			}
		}
		if _, ok := flags[apiKeyStdinFlag]; ok {
			key, err := readAPIKeyStdin(os.Stdin, os.Stderr)
			if err != nil {
				return err
			}
			body["api_key"] = key
		}
		if len(body) == 0 {
			return usageError{usage}
		}
		return c.do("/autoreply", body)
	default:
		return usageError{usage}
	}
}

// The API key's flags, without their dashes (knownFlags lists what may be typed).
const (
	// apiKeyStdinFlag reads the key from stdin (LEC AV-8).
	apiKeyStdinFlag = "api-key-stdin"
	// apiKeyArgvFlag is the flag 0.3.0 removed: the key on the command line.
	apiKeyArgvFlag = "api-key"
	// apiKeyMax bounds what --api-key-stdin reads.
	apiKeyMax = 4096
)

// refuseAPIKeyOnArgv is api_key_on_argv for an `anet autoreply` command line that carries an API key
// (LEC AV-8): --api-key in either form, whatever its value, or a value given to --api-key-stdin (as
// --api-key-stdin=VALUE, or as the word after it, which the flag parser would take as its value). By
// then the key has been seen — other local users can read a process's arguments, the shell keeps them in
// its history — so nothing is sent or saved, the sentence says to replace the key, and the key is not
// repeated. Exit status 2, like the other removed flags (ux UX-C4).
func refuseAPIKeyOnArgv(cmd string, rest []string, lang a2ashape.Language) (string, bool) {
	if cmd != "autoreply" && cmd != "auto-reply" {
		return "", false
	}
	for i, a := range rest {
		if a == "--" {
			break
		}
		name, ok := strings.CutPrefix(a, "--")
		if !ok {
			continue
		}
		name, value, hasValue := strings.Cut(name, "=")
		// "-", the usual name of standard input, says where the key comes from, not what it is.
		onArgv := name == apiKeyArgvFlag ||
			(name == apiKeyStdinFlag && hasValue && value != "" && value != "-") ||
			(name == apiKeyStdinFlag && !hasValue && i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "--") && rest[i+1] != "-")
		if onArgv {
			return a2ashape.Sentence("api_key_on_argv", lang, nil), true
		}
	}
	return "", false
}

// readAPIKeyStdin reads the key for --api-key-stdin (LEC AV-8): typed on the terminal, without echo,
// when stdin is one; otherwise read to the end, at most 4096 bytes. Surrounding white space is dropped.
// An empty key is api_key_empty and a longer one config_invalid, each printed here: exit status 1,
// nothing sent.
func readAPIKeyStdin(in *os.File, out io.Writer) (string, error) {
	lang := a2ashape.Lang()
	var raw []byte
	var err error
	if fd := int(in.Fd()); term.IsTerminal(fd) {
		fmt.Fprint(out, "API key: ")
		raw, err = term.ReadPassword(fd)
		fmt.Fprintln(out)
	} else {
		raw, err = io.ReadAll(io.LimitReader(in, apiKeyMax+1))
	}
	if err != nil {
		return "", err
	}
	if len(raw) > apiKeyMax {
		rule := "it is longer than 4096 bytes; check that the file holds only the key"
		if lang == a2ashape.Chinese {
			rule = "超过 4096 字节;请确认文件里只有 key"
		}
		fmt.Fprintln(out, a2ashape.Sentence("config_invalid", lang, a2ashape.Args{"key": "api_key", "rule": rule}))
		return "", errExit1
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		fmt.Fprintln(out, a2ashape.Sentence("api_key_empty", lang, nil))
		return "", errExit1
	}
	return key, nil
}

// splitFlags separates positional arguments from --flags.
//
// Both spellings, because the CLI already accepted --attach=PATH and this
// accepted only --capability VALUE. `anet delegate AID --capability=cas.put`
// parsed as a flag literally named "capability=cas.put" set to "true", the
// capability went missing, and the command failed complaining about an
// empty goal. The user typed the form the tool taught them one flag
// earlier.

// knownFlags is what each command accepts. A flag not listed here is a
// typo, and saying so is the whole point of the table.
//
// splitFlags collects every --flag into a map and each command reads the
// keys it knows about; nothing ever looked at the rest. So a misspelled or
// invented flag was accepted, dropped, and the command ran as if it had
// not been given — `anet find --capability text.digest` returned the
// unfiltered directory with exit status 0, indistinguishable from a query
// that genuinely matched everything. The same CLI rejects an unknown
// COMMAND, so the two halves disagreed about whether a typo is an error.
//
// One table rather than nineteen call-site signatures: the check happens
// once, at dispatch, and the accepted set for every command is readable in
// one place. Commands with no flags map to an empty list, which is not the
// same as being absent — absent means "not checked".
//
// Found by the release matrix on the standard variant.
var knownFlags = map[string][]string{
	// local
	"daemon":   {"detach"},
	"up":       {"detach", "all"},
	"stop":     {"all"},
	"down":     {"all"},
	"id":       {"purge", "all"},
	"ids":      {"purge", "all"},
	"identity": {"purge", "all"},
	"agents":   {"all", "refresh", "a2a"},
	"verify":   {"receipt", "kel", "result", "hub", "attestation", "chain", "head"},
	"logs":     {"all"},
	"help":     {"all"},
	"mcp":      {},
	"version":  {},
	"update":   {"check", "base"},
	"init":     {"json"},
	"doctor":   {"json"},
	"audit":    {"since", "peer", "interaction", "type", "json", "export"},

	// through the daemon when it runs, from the data directory when not
	"retention": {"json"},

	// through the control plane
	"status":       {},
	"hub-register": {"name", "caps", "recovery-file", "new-recovery"}, // --token, --token-file, --accept-delegations: cli_removed (removed.go)
	"hub-leave":    {},
	"peers":        {"json"},
	"inbound":      {},
	"pay":          {"option", "reject"},
	"payments":     {}, // --payees-file: cli_removed (removed.go)
	"payees":       {},
	"autoreply": {
		"backend", "agent", "api-base", "api-key-stdin", "model", "system-prompt", "work-dir", "untrusted",
		"openclaw-agent", "require-image", "usage-hint", "error-reply", "command",
		"poll-interval", "max-history", "api-timeout", "max-auto-replies", "egress-allow",
	},
	"auto-reply": {
		"backend", "agent", "api-base", "api-key-stdin", "model", "system-prompt", "work-dir", "untrusted",
		"openclaw-agent", "require-image", "usage-hint", "error-reply", "command",
		"poll-interval", "max-history", "api-timeout", "max-auto-replies", "egress-allow",
	},
	"profile": {"summary", "readme"},
	"console": {"url", "print"},
	// Both spellings on both commands. They named the same thing two ways
	// and each rejected the other's, so the obvious two-step — find a node
	// by capability, then call it — made you change the word halfway
	// through. Neither name is worth breaking scripts over, so both work.
	"find":       {"cap", "capability", "json"},
	"card":       {"json"},
	"delegate":   {"cap", "capability", "args", "attach", "watch"},
	"inbox":      {"pending"},
	"thread":     {},
	"task":       {"full", "history", "role", "state", "peer", "reasoning"},
	"message":    {"file", "attach"},
	"msg":        {"file", "attach"},
	"pull":       {"out"},
	"end":        {},
	"results":    {},
	"reconcile":  {},
	"audit-hub":  {},
	"redeem":     {"ref"},
	"balance":    {},
	"credits":    {},
	"visibility": {},
	"review":     {},
}

// checkFlags refuses a flag the command does not accept.
//
// Only flags — positional arguments are each command's own business, and
// several take a free-form goal or message that may begin with anything.
// A command with no entry in the table is not checked, so adding a command
// without adding its flags fails open rather than rejecting valid usage;
// the test in args_test.go is what keeps the table from drifting.
func checkFlags(cmd string, rest []string) error {
	known, ok := knownFlags[cmd]
	if !ok {
		return nil
	}
	allowed := make(map[string]bool, len(known))
	for _, k := range known {
		allowed[k] = true
	}
	for _, a := range rest {
		if !strings.HasPrefix(a, "--") || a == "--" {
			continue
		}
		key := strings.TrimPrefix(a, "--")
		if k, _, found := strings.Cut(key, "="); found {
			key = k
		}
		if key == "" || allowed[key] {
			continue
		}
		if len(known) == 0 {
			return fmt.Errorf("anet %s takes no flags, and --%s is not one", cmd, key)
		}
		return fmt.Errorf("anet %s: unknown flag --%s (it accepts: --%s)",
			cmd, key, strings.Join(known, ", --"))
	}
	return nil
}

func splitFlags(rest []string) (pos []string, flags map[string]string) {
	flags = map[string]string{}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		key := strings.TrimPrefix(a, "--")
		if k, v, found := strings.Cut(key, "="); found {
			flags[k] = v
			continue
		}
		if i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "--") {
			flags[key] = rest[i+1]
			i++
			continue
		}
		flags[key] = "true"
	}
	return pos, flags
}

func runClient(layout daemon.Layout, cmd string, rest []string, explicit bool) error {
	// With an explicit identity selection, resolve STRICTLY (this identity's own daemon only) so several
	// running daemons can't be confused; otherwise keep the lenient uid-pointer fallback (single-daemon
	// norm + agent sandboxes whose HOME differs from the operator's).
	resolve := daemon.ResolveControl
	if explicit {
		resolve = daemon.ResolveControlStrict
	}
	base, token, err := resolve(layout)
	if err != nil {
		return diagnoseNoDaemon("http://"+daemon.LocalControlAddr(layout), layout.Root, err)
	}
	c := &client{base: base, token: token, timeout: 30 * time.Second, dataDir: layout.Root}
	// delegate and message carry their files in the request (base64 raw parts, read here: CUX-F4); on a
	// constrained link a large one takes a while, so give these commands a generous ceiling instead of
	// the 30s default.
	if cmd == "delegate" || cmd == "message" || cmd == "msg" {
		c.timeout = 15 * time.Minute
	}

	arg := func(i int) string {
		if i < len(rest) {
			return rest[i]
		}
		return ""
	}
	switch cmd {
	case "status":
		return c.do("/status", nil)
	case "stop", "down":
		return stopDaemon(layout, explicit)
	case "hub-register":
		return runHubRegister(c, rest)
	case "hub-leave":
		// Stop being deliverable at a hub you have moved away from, or stop
		// joining one that has not admitted this node (LEC AD-8). Leaving
		// the witness hub is refused: switching hubs is a key rotation.
		return runHubLeave(c, rest)
	case "peers":
		return runPeers(c, rest)
	case "inbound":
		return runInbound(c, rest)
	case "pay":
		return runPay(c, rest)
	case "payments":
		return runPayments(c, rest)
	case "payees":
		return runPayees(c, rest)
	case "autoreply", "auto-reply":
		return runAutoReply(c, rest)
	case "profile":
		sub := arg(0)
		switch sub {
		case "set":
			_, flags := splitFlags(rest[1:])
			body := map[string]any{}
			if v, ok := flags["summary"]; ok {
				body["summary"] = v
			}
			if v, ok := flags["readme"]; ok {
				text, err := maybeFile(v)
				if err != nil {
					return fmt.Errorf("profile set: --readme: %w", err)
				}
				body["readme"] = text
			}
			if len(body) == 0 {
				return usageError{"anet profile set [--summary S] [--readme S|@file]"}
			}
			return c.do("/profile", body)
		case "", "show":
			return c.do("/status", nil)
		default:
			return usageError{"anet profile set|show"}
		}
	case "console":
		// The console page carries no credential. This command, which holds the control token, asks the
		// daemon for a single-use console ticket valid for 60 seconds and opens (or, with --url, prints)
		// http://127.0.0.1:<port>/console#t=<ticket>. The page trades the ticket for a browser session;
		// reloading the page needs a new ticket, i.e. running this command again. --id NAME picks the
		// identity, like every other command.
		//
		// The ticket is never put on a command line: every local user can read a process's arguments
		// in /proc/<pid>/cmdline and redeem the ticket before the browser does [redteam:F19]. The
		// daemon writes a launcher page (0600, in its private runtime directory) that redirects to the
		// ticket URL and deletes it on redemption or after 60 seconds; the browser is given its path.
		urlOnly := hasFlag(rest, "--url", "--print")
		b, code, err := c.fetch("/console/ticket", map[string]any{"launcher": !urlOnly})
		if err != nil {
			return diagnoseNoDaemon(c.base, c.dataDir, err)
		}
		if code != 200 {
			return answerError(code, b)
		}
		var tk struct {
			URL      string `json:"url"`
			Launcher string `json:"launcher"`
		}
		if json.Unmarshal(b, &tk) != nil || tk.URL == "" {
			return answerError(code, b)
		}
		// `--url` (alias `--print`): just print the URL — this is what an onboarding agent hands back to
		// its operator to open. It is single-use and expires after 60 seconds.
		if urlOnly {
			fmt.Fprintln(outW(), tk.URL)
			return nil
		}
		if tk.Launcher == "" || !filepath.IsAbs(tk.Launcher) {
			// A daemon from before launcher pages. Handing the URL to the browser's command line is
			// what this avoids, so it is not the fallback.
			return fmt.Errorf("this daemon cannot hand out a console launcher (it is older than this anet: " +
				"restart it with `anet stop && anet up`); or run `anet console --url` and open the printed link within 60 seconds")
		}
		fmt.Fprintln(errW(), "opening the console (single-use link, valid 60 s)")
		if err := openBrowser(tk.Launcher); err != nil {
			fmt.Fprintln(errW(), "(could not auto-open a browser; run `anet console --url` and open the printed URL within 60 seconds)")
		}
		return nil
	case "find":
		return findCommand(c, rest)
	case "card":
		return cardCommand(c, rest)
	case "delegate":
		return delegateCommand(c, rest)
	case "inbox":
		pending := false
		for _, a := range rest {
			if a == "--pending" {
				pending = true
			}
		}
		return c.do("/inbox", map[string]any{"pending": pending})
	case "task":
		return taskCommand(c, rest)
	case "thread":
		if arg(0) == "" {
			return usageError{"anet thread <id>"}
		}
		return c.do("/thread", map[string]any{"interaction_id": arg(0)})
	case "message", "msg":
		return messageCommand(c, rest)
	case "pull":
		pos, flags := splitFlags(rest)
		if len(pos) < 1 || pos[0] == "" {
			return usageError{"anet pull <id> [--out DIR]"}
		}
		// The daemon requires an absolute out_dir and refuses to fall back to its own working directory,
		// so the default "current directory" is resolved here, in the caller's process.
		out := "."
		if o := flags["out"]; o != "" && o != "true" {
			out = o
		}
		return c.do("/pull", map[string]any{"interaction_id": pos[0], "out_dir": absPath(out)})
	case "end":
		if arg(0) == "" {
			return usageError{"anet end <id>"}
		}
		return c.do("/end", map[string]any{"interaction_id": arg(0)})
	case "results":
		return c.do("/results", map[string]any{})
	case "reconcile":
		// Compare what this node signed and was paid against what the hub
		// says about this account. Both records already exist; this is
		// the comparison.
		return c.do("/reconcile", map[string]any{})
	case "audit-hub":
		// Fetch the hub's issuance chain, verify the signatures and the
		// links, and check it against heads this node saw before. It does
		// not prevent a hub issuing credit — it is the issuer — but a hub
		// cannot do so retroactively without contradicting a record this
		// node already holds.
		return c.do("/audit-hub", map[string]any{})
	case "redeem":
		// Credit back out. What the reference buys is between this node's
		// operator and its hub — anet signs the withdrawal and keeps the
		// hub's signed statement of what it took, and claims nothing
		// about the other side of the trade. The manual tier: confirmed
		// on the terminal (pay.go).
		return runRedeem(c, rest)
	case "balance", "credits":
		// What this node's hub says it is worth, and the entries behind it.
		// The hub is the custodian of the number; `anet audit --type
		// anet.payment.settled` is where the events that moved it are, and
		// the two disagreeing is a thing you can now see.
		return c.do("/balance", map[string]any{})
	case "visibility":
		// How far this node is willing to be published: hub-local (the
		// default), federated, or public. The agent decides, because this
		// is what lets other hubs learn it exists.
		if len(rest) < 1 {
			return usageError{"anet visibility <hub-local|federated|public>"}
		}
		return c.do("/visibility", map[string]any{"visibility": strings.TrimSpace(rest[0])})
	case "review":
		pos, _ := splitFlags(rest)
		const use = "anet review <id> <1-5> [comment]"
		if len(pos) < 2 {
			return usageError{use}
		}
		rating, err := strconv.Atoi(pos[1])
		if err != nil {
			return usageError{use}
		}
		comment := strings.TrimSpace(strings.Join(pos[2:], " "))
		return c.do("/review", map[string]any{"interaction_id": pos[0], "rating": rating, "comment": comment})
	default:
		return usageError{fmt.Sprintf("anet help --all  (%q is not an anet command)", printable(cmd, 64))}
	}
}

type client struct {
	base, token string
	timeout     time.Duration
	dataDir     string // used to build a helpful error when the daemon is unreachable (see do)
}

// http is the HTTP client every control call goes through. The control token is the node's full
// credential, and a loopback port can be held by another local user whenever the daemon is not holding
// it (before `anet up`, after a crash or a reboot), so each connection is verified to be this user's
// daemon before the request carrying the token is written on it (internal/localpeer, A2A-DESIGN §7
// item 10 [redteam:F18]).
func (c *client) http() *http.Client {
	to := c.timeout
	if to == 0 {
		to = 30 * time.Second
	}
	return localpeer.Client(c.token, to)
}

func (c *client) fetch(path string, body any) ([]byte, int, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, &buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, nil
}

// agentChoices renders the supported agent ids for help text, from the registry rather than a hand-kept
// copy. The three copies that used to exist all said "cursor|claude|codex|openclaw|hermes", so adding an
// agent meant remembering three places and the help was one edit away from lying about what works.
func agentChoices() string {
	return strings.Join(daemon.SupportedExecAgents(), "|")
}

// capFlag reads the capability id under either spelling. --cap and
// --capability mean the same thing and always did; only the accepted
// name differed between commands.
func capFlag(flags map[string]string) string {
	if v := strings.TrimSpace(flags["capability"]); v != "" {
		return v
	}
	return strings.TrimSpace(flags["cap"])
}

// cliTaskBytes is how large `anet task get` lets a task be without --full:
// the bound MCP get_task has (internal/mcpserv taskReadBytes), so the
// terminal shows what the model was shown, and --full shows the rest — the
// command the notice in a cut task names.
const cliTaskBytes = 24 << 10

// taskCommand is `anet task get|list|cancel` (ux §4.2): the control plane's
// /tasks/get, /tasks/list and /tasks/cancel, the routes MCP get_task,
// list_tasks and cancel_task take. A task's JSON goes to stdout and its
// human lines to stderr (UX-C1, UX-C2).
func taskCommand(c *client, rest []string) error {
	const use = "anet task get <id> [--full] [--history N] | task list [--role requester|provider] [--state S] [--peer AID] | task watch <id> [--reasoning] | task cancel <id>"
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "get":
		return taskGet(c, rest[1:])
	case "list", "ls":
		return taskList(c, rest[1:])
	case "watch":
		return taskWatchCommand(c, rest[1:])
	case "cancel":
		pos, _ := splitFlags(rest[1:])
		if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
			return usageError{"anet task cancel <id>"}
		}
		return c.doTask("/tasks/cancel", map[string]any{"task_id": pos[0], "max_task_bytes": cliTaskBytes})
	}
	return usageError{use}
}

// taskGet is `anet task get <id> [--full] [--history N]`: the task held to
// the MCP bound, or with --full all of it, whichever side of the id the
// flag is on.
func taskGet(c *client, rest []string) error {
	const use = "anet task get <id> [--full] [--history N]"
	pos, flags := splitFlags(rest)
	id := ""
	if len(pos) > 0 {
		id = pos[0]
	}
	full := false
	switch v := flags["full"]; {
	case v == "":
	case v == "true":
		full = true
	case id == "":
		// `--full <task_id>`: the flag took the id as its value.
		full, id = true, v
	default:
		return usageError{use}
	}
	if id == "" || len(pos) > 1 {
		return usageError{use}
	}
	body := map[string]any{"task_id": id}
	if !full {
		body["max_task_bytes"] = cliTaskBytes
	}
	if h, ok := flags["history"]; ok {
		n, err := strconv.Atoi(h)
		if err != nil || n < 0 {
			return usageError{use + "  (--history takes a number of messages, 0 or more)"}
		}
		body["history_length"] = n
	}
	return c.doTask("/tasks/get", body)
}

// taskList is `anet task list [--role requester|provider] [--state S]
// [--peer AID]`: requester is the tasks this node sent, provider the tasks
// sent to it (the roles of MCP list_tasks).
func taskList(c *client, rest []string) error {
	const use = "anet task list [--role requester|provider] [--state S] [--peer AID]"
	pos, flags := splitFlags(rest)
	if len(pos) > 0 {
		return usageError{use}
	}
	body := map[string]any{"max_task_bytes": cliTaskBytes}
	switch r := flags["role"]; r {
	case "":
	case "requester":
		body["role"] = "outbound"
	case "provider":
		body["role"] = "inbound"
	default:
		return usageError{use}
	}
	for flag, key := range map[string]string{"state": "state", "peer": "peer"} {
		if v, ok := flags[flag]; ok {
			if v == "true" || strings.TrimSpace(v) == "" {
				return usageError{use}
			}
			body[key] = strings.TrimSpace(v)
		}
	}
	return c.do("/tasks/list", body)
}

// findLimit is how many agents `anet find` lists at once: the largest page
// of the daemon's /agents/list. The answer's nextCursor says when there are
// more.
const findLimit = 100

// findCommand is `anet find [query] [--cap id] [--json]`. The listing comes
// through the daemon's /agents/list, which reads the hub's directory and
// matches here: the query never reaches the hub (A2A-DESIGN §10.5). --cap
// asks the exact question, who serves this capability id; without it the
// query is a substring of what is shown. The agents without a network card
// are included, as the hub directory listed them.
//
// It prints a table for a person (ux §3.1 text B) — the AID last and whole,
// so it can be copied to anet card, delegate, peers allow or payees add as
// it is (UX-G8a) — or with --json the daemon's answer as it came.
func findCommand(c *client, rest []string) error {
	pos, flags := splitFlags(rest)
	body := map[string]any{"include_uncarded": true, "limit": findLimit}
	if q := strings.TrimSpace(strings.Join(pos, " ")); q != "" {
		body["q"] = q
	}
	if capID := capFlag(flags); capID != "" {
		body["skill"] = capID
	}
	if flags["json"] == "true" {
		return c.do("/agents/list", body)
	}
	out, code, err := c.fetch("/agents/list", body)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return answerError(code, out)
	}
	var page struct {
		Agents []findAgent `json:"agents"`
	}
	if err := json.Unmarshal(out, &page); err != nil {
		return answerError(code, out)
	}
	writeFindTable(outW(), page.Agents, a2ashape.Lang())
	return nil
}

// findAgent is one /agents/list entry, as the table reads it.
type findAgent struct {
	AID          string          `json:"aid"`
	Card         json.RawMessage `json:"card"`
	Verification string          `json:"verification"`
	// Charge is read from the public card (HPP PAY-C15); KeyState is
	// fresh or stale (KEL M-3).
	Charge   string `json:"charge"`
	KeyState string `json:"key_state"`
}

// writeFindTable writes the `anet find` table (ux §3.1 text B): name,
// charge, key, skills and the whole AID, in the language's column titles.
// Only a card this node verified is read: an agent without one is "(not
// verified)" with no name, charge, key or skills, since what it says of
// itself could be anyone's words. The charge is the public card's — a list
// asks no agent for its extended card — and the key column says whether
// the agent's key was confirmed by its hub lately (stale does not stop a
// send).
func writeFindTable(w io.Writer, agents []findAgent, lang a2ashape.Language) {
	const none = "—"
	rows := [][]string{{a2ashape.Label("find.name", lang), a2ashape.Label("find.charge", lang), a2ashape.Label("find.key", lang),
		a2ashape.Label("find.skills", lang), a2ashape.Label("find.aid", lang)}}
	for _, a := range agents {
		name, charge, key, skills := a2ashape.Label("find.unverified", lang), none, none, none
		if a.Verification == "VERIFIED" {
			if t := a2ashape.ChargeText(a.Charge, lang); t != "" {
				charge = t
			}
			switch a.KeyState {
			case "fresh":
				key = a2ashape.Label("find.key_fresh", lang)
			case "stale":
				key = a2ashape.Label("find.key_stale", lang)
			}
			var card struct {
				Name   string `json:"name"`
				Skills []struct {
					ID string `json:"id"`
				} `json:"skills"`
			}
			_ = json.Unmarshal(a.Card, &card)
			if n := printable(card.Name, 32); n != "" {
				name = n
			} else {
				name = none
			}
			var ids []string
			for _, sk := range card.Skills {
				if id := printable(sk.ID, 64); id != "" {
					ids = append(ids, id)
				}
			}
			if len(ids) > 0 {
				skills = strings.Join(ids, ", ")
			}
		}
		// The AID is printed whole; one that is not an AID is not printed
		// at all, so nothing in this column is anything but an AID.
		aid := a.AID
		if !identity.ValidAID(aid) {
			aid = none
		}
		rows = append(rows, []string{name, charge, key, printable(skills, 120), aid})
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, cell := range r[:len(r)-1] {
			widths[i] = max(widths[i], displayWidth(cell))
		}
	}
	for _, r := range rows {
		var b strings.Builder
		for i, cell := range r {
			b.WriteString(cell)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)+2))
			}
		}
		fmt.Fprintln(w, b.String())
	}
}

// displayWidth is how many terminal columns s takes: two for a wide
// (East Asian) character, one for the rest.
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		if wideRune(r) {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// wideRune reports a character a terminal shows two columns wide: the CJK
// ideographs and syllabaries, Hangul and the full-width forms.
func wideRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6)
}

// delegateCommand is `anet delegate <aid> <goal> [--attach P …]` and
// `anet delegate <aid> --capability <id> [--args J] [--attach P …]` (ux
// §4.2, UX-C10): the control plane's /tasks/send, the route MCP
// send_message and the local A2A interface's SendMessage take, so the task
// that comes back is the same projection, with its delivery, payment and
// reason, and a retry is deduplicated the same way. It returns at once
// (return_immediately); the files are read here and sent as raw parts, the
// same message MCP send_message builds from skill, args and files (CUX-F4).
func delegateCommand(c *client, rest []string) error {
	const use = "anet delegate <aid> <goal> [--attach P …] [--watch] | anet delegate <aid> --capability <id> [--args J] [--attach P …]"
	watch, rest := extractBool(rest, "watch")
	attachPaths, rest2 := extractAttach(rest)
	pos, flags := splitFlags(rest2)
	if len(pos) < 1 || pos[0] == "" {
		return usageError{use}
	}
	to := pos[0]
	var text, skill string
	var args map[string]any
	if skill = capFlag(flags); skill != "" {
		if len(pos) > 1 {
			return usageError{use + "  (a capability call takes its input as --args, not as text)"}
		}
		if raw := strings.TrimSpace(flags["args"]); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
				return usageError{use + "  (--args is a JSON object)"}
			}
		}
	} else {
		if _, ok := flags["args"]; ok {
			return usageError{use + "  (--args belongs to a capability call: give --capability too)"}
		}
		text = strings.TrimSpace(strings.Join(pos[1:], " "))
		if text == "" && len(attachPaths) == 0 {
			return usageError{use}
		}
	}
	body, err := sendBody(text, skill, args, attachPaths)
	if err != nil {
		return err
	}
	body["to"] = to
	body["return_immediately"] = true
	body["max_task_bytes"] = cliTaskBytes
	if watch {
		if skill != "" {
			return usageError{use + "  (--watch is for a text task: a capability call has no preview)"}
		}
		return watchDelegate(c, body)
	}
	return c.doTask("/tasks/send", body)
}

// messageCommand is `anet message <id> <text …>|--file P [--attach P …]`:
// one more message on a task, from either side. On a task this node sent it
// is /tasks/send with the task's id, as MCP send_message(task_id); on a task
// sent to this node it is the provider's answer that asks for input,
// /tasks/reply, as MCP reply_task. It returns once the message is recorded.
func messageCommand(c *client, rest []string) error {
	const use = "anet message <id> <text …>|--file P [--attach P …]"
	attachPaths, rest2 := extractAttach(rest)
	pos, flags := splitFlags(rest2)
	if len(pos) < 1 || pos[0] == "" {
		return usageError{use}
	}
	id := pos[0]
	var text string
	if f, ok := flags["file"]; ok {
		if f == "" || f == "true" || len(pos) > 1 {
			return usageError{use}
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("message: read --file: %w", err)
		}
		text = string(b)
	} else {
		text = strings.TrimSpace(strings.Join(pos[1:], " "))
	}
	if strings.TrimSpace(text) == "" && len(attachPaths) == 0 {
		return usageError{use}
	}
	role, err := taskRole(c, id)
	if err != nil {
		return err
	}
	if role == a2ashape.RoleProvider {
		body := map[string]any{"task_id": id, "max_task_bytes": cliTaskBytes}
		if len(attachPaths) == 0 {
			body["text"] = text
		} else {
			m, err := fileMessage(a2ashape.RoleAgent, text, "", nil, attachPaths)
			if err != nil {
				return err
			}
			body["message"] = m
		}
		return c.doTask("/tasks/reply", body)
	}
	body, err := sendBody(text, "", nil, attachPaths)
	if err != nil {
		return err
	}
	if m, ok := body["message"].(a2ashape.Message); ok {
		m.TaskID = id
		body["message"] = m
	} else {
		body["task_id"] = id
	}
	body["return_immediately"] = true
	body["max_task_bytes"] = cliTaskBytes
	return c.doTask("/tasks/send", body)
}

// taskRole is the side this node is on for a task (metadata anet.role); a
// task the control plane does not have is its refusal, said here.
func taskRole(c *client, id string) (string, error) {
	out, code, err := c.fetch("/tasks/get", map[string]any{"task_id": id, "history_length": 0, "max_task_bytes": 1})
	if err != nil {
		return "", diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	if code != 200 {
		return "", answerError(code, out)
	}
	var t struct {
		Metadata map[string]any `json:"metadata"`
	}
	_ = json.Unmarshal(out, &t)
	role, _ := t.Metadata[a2ashape.KeyRole].(string)
	return role, nil
}

// sendBody is the /tasks/send body of a message: its short fields (text, or
// skill and args) when it carries no file, which is what MCP send_message
// sends; with files, the A2A message itself, text and DataPart {skill,
// args} followed by one raw part per file in the order given — what MCP
// send_message builds from files and what a local A2A client sends
// (CUX-F4).
func sendBody(text, skill string, args map[string]any, paths []string) (map[string]any, error) {
	body := map[string]any{}
	if len(paths) == 0 {
		if text != "" {
			body["text"] = text
		}
		if skill != "" {
			body["skill"] = skill
			if args != nil {
				body["args"] = args
			}
		}
		return body, nil
	}
	m, err := fileMessage(a2ashape.RoleUser, text, skill, args, paths)
	if err != nil {
		return nil, err
	}
	body["message"] = m
	return body, nil
}

// maxFileBytes is the most one file may hold (the daemon's attachment
// limit); a larger one is refused here, before it is read into memory.
const maxFileBytes = 64 << 20

// fileMessage is an A2A message from role with text, a skill call and
// files read from paths, each a raw part named by its base name, its media
// type left to the daemon to find (as MCP's files without media_type).
func fileMessage(role a2ashape.Role, text, skill string, args map[string]any, paths []string) (a2ashape.Message, error) {
	m := a2ashape.Message{Role: role, Parts: []a2ashape.Part{}}
	if text != "" {
		m.Parts = append(m.Parts, a2ashape.TextPart(text))
	}
	if skill != "" {
		call := map[string]any{"skill": skill}
		if args != nil {
			call["args"] = args
		}
		m.Parts = append(m.Parts, a2ashape.DataPart(call))
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return a2ashape.Message{}, fmt.Errorf("attach %s: %w", p, err)
		}
		if !fi.Mode().IsRegular() {
			return a2ashape.Message{}, fmt.Errorf("attach %s: not a regular file", p)
		}
		if fi.Size() > maxFileBytes {
			return a2ashape.Message{}, fmt.Errorf("attach %s: %d bytes; a file can be at most 64 MiB", p, fi.Size())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return a2ashape.Message{}, fmt.Errorf("attach %s: %w", p, err)
		}
		m.Parts = append(m.Parts, a2ashape.RawPart(b, filepath.Base(p), ""))
	}
	return m, nil
}

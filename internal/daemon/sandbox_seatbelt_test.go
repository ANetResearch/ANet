//go:build linux || darwin

package daemon

// The macOS sandbox's launch and probe (sandbox_seatbelt.go, LEC MS-1 to MS-5, EG-3), driven through a
// stand-in for sandbox-exec: a shell script that takes the same arguments and runs what follows "--"
// without confining it. That runs everything of a macOS run but Seatbelt itself on any Unix: the profile
// and its place, the spec and the agent's environment, the keyed endpoint, init, and the probe's
// classification. What Seatbelt itself holds is tested on a Mac (sandbox_darwin_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// seatbeltTestProbeArg is what the stand-in turns `__sandbox-probe` into: the macOS checks, whatever the
// platform the test runs on. seatbeltTestReportArg prints a prepared report instead, after reaching the
// probe's endpoint (unless the report's detail says "no_connect").
const (
	seatbeltTestProbeArg  = "__seatbelt-probe-test"
	seatbeltTestReportArg = "__seatbelt-report-test"
)

func init() {
	testMainHooks[seatbeltTestProbeArg] = func(args []string) int {
		fd, ok := sandboxSpecFdArg(args)
		if !ok {
			return sandboxInitExitUsage
		}
		raw, err := readSandboxSpec(fd)
		var spec sandboxCheckSpec
		if err != nil || decodeSandboxSpec(raw, &spec) != nil || !seatbeltSpecUsable(spec) {
			return sandboxInitExitBadSpec
		}
		b, _ := json.Marshal(seatbeltChecks(spec))
		fmt.Println(string(b))
		return 0
	}
	testMainHooks[seatbeltTestReportArg] = func(args []string) int {
		if len(args) < 1 {
			return sandboxInitExitUsage
		}
		report, err := os.ReadFile(args[0])
		fd, ok := sandboxSpecFdArg(args[1:])
		if err != nil || !ok {
			return sandboxInitExitUsage
		}
		raw, _ := readSandboxSpec(fd)
		var spec sandboxCheckSpec
		var rep sandboxCheckReport
		if decodeSandboxSpec(raw, &spec) != nil || json.Unmarshal(report, &rep) != nil {
			return sandboxInitExitBadSpec
		}
		if rep.Detail["no_connect"] == "" {
			if c, err := net.DialTimeout("tcp", spec.Egress, 5*time.Second); err == nil {
				_ = c.Close()
			}
		}
		fmt.Println(string(report))
		return 0
	}
}

// standInSandboxExec writes a stand-in for sandbox-exec into a directory of its own and returns its path.
// It checks its arguments are `-f <readable profile> -- <program> …`, keeps a copy of the profile beside
// itself (profile.copy), counts its runs (runs, one line each) and then, by mode:
//   - "run": runs the program, turning __sandbox-probe into the macOS checks of the test binary;
//   - "fail": says why it cannot apply the profile and exits 65, as sandbox-exec does on a bad profile;
//   - "report": runs the test binary to print report.json from its directory instead of the checks.
func standInSandboxExec(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
here=$(dirname "$0")
echo run >> "$here/runs"
[ "$1" = -f ] && [ -r "$2" ] || { echo "sandbox-exec: usage" >&2; exit 64; }
cp "$2" "$here/profile.copy"
shift 2
[ "$1" = -- ] || { echo "sandbox-exec: usage" >&2; exit 64; }
shift
exe=$1; sub=$2; shift 2
`
	switch mode {
	case "run":
		script += `[ "$sub" = __sandbox-probe ] && sub=` + seatbeltTestProbeArg + "\nexec \"$exe\" \"$sub\" \"$@\"\n"
	case "fail":
		script += "echo 'sandbox-exec: cannot apply the profile: unbound variable: frobnicate' >&2\nexit 65\n"
	case "report":
		script += `exec "$exe" ` + seatbeltTestReportArg + ` "$here/report.json" "$@"` + "\n"
	default:
		t.Fatalf("stand-in mode %q", mode)
	}
	p := filepath.Join(dir, "sandbox-exec")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// standInRuns is how many times the stand-in at p ran.
func standInRuns(p string) int {
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "runs"))
	return strings.Count(string(b), "run\n")
}

// freshSeatbeltProbe runs the probe with sandbox-exec at p, bypassing the cache.
func freshSeatbeltProbe(t *testing.T, p string) seatbeltProbe {
	t.Helper()
	pr := runSeatbeltProbe(p)
	t.Logf("probe: err %v; report %+v", pr.Err, pr.Report)
	return pr
}

func probeCode(err error) string {
	var pe *sandboxProbeError
	if err == nil {
		return ""
	}
	if errors.As(err, &pe) {
		return pe.Code
	}
	return "?"
}

// MS-4, M-MS-1: a sandbox that confines nothing — here a stand-in that runs the probe as it is, as with a
// profile of (allow default) — fails the probe: another port of the host's loopback is reachable
// (netns_failed), and the real home can be listed.
func TestSeatbeltProbeRefusesAnUnconfinedSandbox(t *testing.T) {
	pr := freshSeatbeltProbe(t, standInSandboxExec(t, "run"))
	if probeCode(pr.Err) != seatbeltCodeNetnsFailed {
		t.Fatalf("an unconfined sandbox passed or failed otherwise: %v", pr.Err)
	}
	r := pr.Report
	if r.HostTCPRefused || r.HomeUnreadable || !r.EgressReached || !r.WorkWritable {
		t.Errorf("the checks saw %+v", r)
	}
	if !strings.Contains(pr.Err.Error(), "2 host_tcp") || !strings.Contains(pr.Err.Error(), "1 home") {
		t.Errorf("the detail does not name the failed items: %v", pr.Err)
	}
}

// MS-4: the probe's verdict from what the checks report and what the host saw: items 2 and 3 are the
// network (netns_failed), items 1, 4 and 5 the files and processes (fs_isolation_failed); a report that
// claims the endpoint was reached when it was not counts as unreached; a sandbox that does not start is
// fs_isolation_failed with sandbox-exec's own line.
func TestSeatbeltProbeClassifiesEachItem(t *testing.T) {
	good := sandboxCheckReport{HomeUnreadable: true, HostTCPRefused: true, EgressReached: true, SecurityRefused: true,
		WorkWritable: true, Detail: map[string]string{}}
	for name, tc := range map[string]struct {
		adjust func(*sandboxCheckReport)
		code   string
	}{
		"1 home":     {func(r *sandboxCheckReport) { r.HomeUnreadable = false }, seatbeltCodeFSIsolationFailed},
		"2 host_tcp": {func(r *sandboxCheckReport) { r.HostTCPRefused = false }, seatbeltCodeNetnsFailed},
		"3 egress":   {func(r *sandboxCheckReport) { r.EgressReached = false }, seatbeltCodeNetnsFailed},
		"4 security": {func(r *sandboxCheckReport) { r.SecurityRefused = false }, seatbeltCodeFSIsolationFailed},
		"5 work":     {func(r *sandboxCheckReport) { r.WorkWritable = false }, seatbeltCodeFSIsolationFailed},
		// The report claims item 3 without connecting anywhere: it is not believed.
		"3 claimed": {func(r *sandboxCheckReport) { r.Detail = map[string]string{"no_connect": "1"} }, seatbeltCodeNetnsFailed},
	} {
		p := standInSandboxExec(t, "report")
		r := good
		tc.adjust(&r)
		b, _ := json.Marshal(r)
		if err := os.WriteFile(filepath.Join(filepath.Dir(p), "report.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		pr := runSeatbeltProbe(p)
		if probeCode(pr.Err) != tc.code || !strings.Contains(pr.Err.Error(), strings.TrimSuffix(name, " claimed")) {
			t.Errorf("%s: %v, want %s naming the item", name, pr.Err, tc.code)
		}
	}
	pr := runSeatbeltProbe(standInSandboxExec(t, "fail"))
	if probeCode(pr.Err) != seatbeltCodeFSIsolationFailed || !strings.Contains(pr.Err.Error(), "unbound variable: frobnicate") {
		t.Errorf("a profile sandbox-exec cannot apply: %v", pr.Err)
	}
}

// MS-1, MS-4: the probe starts sandbox-exec by the path it is given with the profile in its run directory,
// and that profile opens the stand-in endpoint's port and the probe's own directories, nothing of the
// real home; without sandbox-exec the plan is refused with no_sandbox_exec before anything runs.
func TestSeatbeltProbeProfileAndMissingSandboxExec(t *testing.T) {
	p := standInSandboxExec(t, "run")
	_ = freshSeatbeltProbe(t, p)
	prof, err := os.ReadFile(filepath.Join(filepath.Dir(p), "profile.copy"))
	if err != nil {
		t.Fatalf("the stand-in got no profile: %v", err)
	}
	forms := mustParseSBPL(t, string(prof))
	outbound := ""
	for _, f := range forms {
		if f.list[0].atom == "allow" && f.list[1].atom == "network-outbound" {
			outbound = f.String()
		}
	}
	if !regexp.MustCompile(`^\(allow network-outbound \(remote ip "localhost:[0-9]+"\)\)$`).MatchString(outbound) {
		t.Errorf("the probe's profile network rule: %q", outbound)
	}
	home, _ := os.UserHomeDir()
	if home, err = filepath.EvalSymlinks(home); err == nil && strings.Contains(string(prof), `(subpath "`+home+`")`) {
		t.Errorf("the probe's profile opens the real home %s", home)
	}

	plan := &sandboxPlan{}
	err = seatbeltPrepare(plan, filepath.Join(t.TempDir(), "sandbox-exec"))
	if probeCode(err) != seatbeltCodeNoSandboxExec || plan.SandboxExec != "" {
		t.Errorf("without sandbox-exec: %v", err)
	}
	notExec := filepath.Join(t.TempDir(), "sandbox-exec")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := seatbeltPrepare(plan, notExec); probeCode(err) != seatbeltCodeNoSandboxExec {
		t.Errorf("with a sandbox-exec that cannot be run: %v", err)
	}
}

// MS-4: a probe result is reused for five minutes, then the probe runs again.
func TestSeatbeltProbeIsCachedForFiveMinutes(t *testing.T) {
	p := standInSandboxExec(t, "run")
	seatbeltProbeCache.Lock()
	saved := seatbeltProbeCache.last
	seatbeltProbeCache.last = nil
	seatbeltProbeCache.Unlock()
	t.Cleanup(func() {
		seatbeltProbeCache.Lock()
		seatbeltProbeCache.last = saved
		seatbeltProbeCache.Unlock()
	})
	for i := 0; i < 3; i++ {
		if err := seatbeltPrepare(&sandboxPlan{}, p); probeCode(err) != seatbeltCodeNetnsFailed {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if n := standInRuns(p); n != 1 {
		t.Fatalf("the probe ran %d times in a row", n)
	}
	seatbeltProbeCache.Lock()
	seatbeltProbeCache.last.At = time.Now().Add(-seatbeltProbeTTL - time.Second)
	seatbeltProbeCache.Unlock()
	_ = seatbeltPrepare(&sandboxPlan{}, p)
	if n := standInRuns(p); n != 2 {
		t.Fatalf("an expired result was reused (%d runs)", n)
	}
}

// seatbeltTestLaunch prepares one macOS launch of the test binary as a claude agent reading req on its
// stdin, with sandbox-exec at sbx, against the fake model service f, and returns it with its work dir.
func seatbeltTestLaunch(t *testing.T, sbx string, f *fakeModelService, env []string, req sandboxProbeRequest) (*sandboxLaunch, *sandboxPlan, string) {
	t.Helper()
	up := testUpstream(t, egressUpstreamAnthropic, f)
	plan := &sandboxPlan{SandboxExec: sbx, AID: "EaidSeatbeltLaunch01", Egress: up,
		NeverBind: []string{filepath.Join(t.TempDir(), "never")}}
	work := t.TempDir()
	raw, _ := json.Marshal(req)
	l, err := seatbeltCommand(plan, work, []string{selfExe(t), "-p", "--output-format", "text"}, env,
		[]byte(sandboxProbeMarker+string(raw)+sandboxProbeEnd))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l, plan, work
}

// readLaunchSpec reads the spec a launch carries on fd 3 (the daemon would hand it to init).
func readLaunchSpec(t *testing.T, l *sandboxLaunch) sandboxInitSpec {
	t.Helper()
	raw, err := io.ReadAll(l.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	var s sandboxInitSpec
	if err := decodeSandboxSpec(raw, &s); err != nil || s.check() != nil {
		t.Fatalf("the spec: %v %s", err, raw)
	}
	return s
}

// envOf is env as a map (the last value of a name wins, as for exec).
func envOf(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// lockedBuffer is a log destination tests read while the code writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// MS-1, MS-3, EG-3, AV-1: a macOS launch is sandbox-exec, by its absolute path, with the run's profile and
// anet's own init and nothing of the agent's; the run's key — 256 random bits, fresh for each run — is
// only in the agent's environment (every proxy variable and the injection route), never on the command
// line, in the profile or in a log; the run directory is the agent's HOME and TMPDIR; the system CA
// bundle is named; no key or proxy setting of the daemon's passes; and Close takes it all away.
func TestSeatbeltLaunchKeepsTheKeyInTheEnvironment(t *testing.T) {
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	f := newFakeModelService(t)
	daemonEnv := []string{"PATH=/usr/bin:/bin:" + t.TempDir(), "LANG=C", "HOME=/Users/u",
		"ANTHROPIC_API_KEY=" + canaryEnvKey, "HTTPS_PROXY=http://user:" + canaryProxy + "@proxy.example:3128", "TMPDIR=/tmp"}
	l, plan, work := seatbeltTestLaunch(t, "/usr/bin/sandbox-exec", f, daemonEnv, sandboxProbeRequest{})

	initExe, _ := sandboxInitExe()
	runs := plan.liveRunDirs()
	if len(runs) != 1 {
		t.Fatalf("live run directories %v", runs)
	}
	run, _ := filepath.EvalSymlinks(runs[0])
	profile := filepath.Join(run, seatbeltProfileName)
	if l.Path != "/usr/bin/sandbox-exec" || !slices.Equal(l.Args, []string{"-f", profile, "--", initExe, "__sandbox-init", "--spec-fd", "3"}) {
		t.Fatalf("launch %s %q", l.Path, l.Args)
	}
	if len(l.Env) != 0 || len(l.Files) != 2 || l.Files[1] == nil {
		t.Fatalf("launch environment %q, %d descriptors", l.Env, len(l.Files))
	}
	spec := readLaunchSpec(t, l)
	env := envOf(spec.Env)
	key := strings.TrimPrefix(strings.Split(env["HTTPS_PROXY"], "@")[0], "http://anet:")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) {
		t.Fatalf("the run's key %q (HTTPS_PROXY=%q)", key, env["HTTPS_PROXY"])
	}
	addr := strings.Split(env["HTTPS_PROXY"], "@")[1]
	for _, k := range egressProxyVars {
		if env[k] != "http://anet:"+key+"@"+addr {
			t.Errorf("%s=%q", k, env[k])
		}
	}
	if env["ANTHROPIC_BASE_URL"] != "http://"+addr+"/anet-upstream/"+key+"/anthropic" || env["ANTHROPIC_API_KEY"] != egressPlaceholderKey ||
		env["NO_PROXY"] != "127.0.0.1,localhost" || env["HOME"] != filepath.Join(run, "home") || env["TMPDIR"] != filepath.Join(run, "tmp") ||
		env["SSL_CERT_FILE"] != "/etc/ssl/cert.pem" || env["PATH"] != "/usr/bin:/bin" {
		t.Errorf("the agent's environment %v", env)
	}
	all := strings.Join(spec.Env, "\n")
	for _, c := range []string{canaryEnvKey, canaryProxy, testOperatorKey} {
		if strings.Contains(all, c) {
			t.Errorf("the agent's environment holds %q", c)
		}
	}
	if spec.Cwd != work || spec.Argv[0] != selfExe(t) || spec.Bridge != nil {
		t.Errorf("spec %+v", spec)
	}
	prof, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(profile); fi.Mode().Perm() != 0o600 {
		t.Errorf("the profile is mode %v", fi.Mode().Perm())
	}
	port := addr[strings.LastIndexByte(addr, ':')+1:]
	if !strings.Contains(string(prof), `(remote ip "localhost:`+port+`")`) {
		t.Errorf("the profile does not open the endpoint's port %s", port)
	}
	for where, text := range map[string]string{"command line": strings.Join(append([]string{l.Path}, l.Args...), " "),
		"profile": string(prof)} {
		if strings.Contains(text, key) || strings.Contains(text, testOperatorKey) {
			t.Errorf("the %s holds a key", where)
		}
	}

	// The endpoint takes the key and nothing else.
	resp, body := keyedRequest(t, addr, "GET", "/anet-upstream/"+key+"/anthropic/v1/models", nil, "")
	if resp.StatusCode != 200 || len(f.requests()) != 1 || f.requests()[0].Header.Get("x-api-key") != testOperatorKey {
		t.Errorf("with the key: %d %s", resp.StatusCode, body)
	}
	if resp, _ := keyedRequest(t, addr, "GET", "/anet-upstream/anthropic/v1/models", nil, ""); resp.StatusCode != 404 {
		t.Errorf("without the key: %d", resp.StatusCode)
	}
	if resp, _ := keyedRequest(t, addr, "CONNECT", "example.com:443", nil, ""); resp.StatusCode != 407 {
		t.Errorf("CONNECT without the key: %d", resp.StatusCode)
	}
	if strings.Contains(logs.String(), key) {
		t.Errorf("the log holds the run's key:\n%s", logs.String())
	}

	// A second run of the plan has a key of its own.
	l2, err := seatbeltCommand(plan, work, []string{selfExe(t)}, daemonEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key2 := strings.Split(strings.TrimPrefix(envOf(readLaunchSpec(t, l2).Env)["HTTPS_PROXY"], "http://anet:"), "@")[0]; key2 == key {
		t.Error("two runs share a key")
	}
	l2.Close()

	l.Close()
	if _, err := os.Stat(runs[0]); !os.IsNotExist(err) {
		t.Errorf("the run directory is left: %v", err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Error("the endpoint still listens after the run")
	}
	if s := plan.egressSummary(); s == nil || s.ModelRequests != 1 || s.PolicyRefused["egress_unauthorized"] != 1 {
		t.Errorf("the run's egress counts %+v", s)
	}
}

// MS-1, MS-3, MS-5, EG-3 end to end, Seatbelt aside: the launch, run as the daemon runs it, starts init
// through sandbox-exec, and the agent (the test binary as the probe) reads its conversation on stdin,
// works in the work dir, has the run's directories as HOME and TMPDIR, reaches its model on the keyed
// injection route — the service sees the operator's key and the agent only the placeholder — and its
// CONNECT is answered by policy with the key and 407 without it. Its command line holds no key.
func TestSeatbeltLaunchRunsTheAgentThroughTheKeyedEndpoint(t *testing.T) {
	f := newFakeModelService(t)
	l, _, work := seatbeltTestLaunch(t, standInSandboxExec(t, "run"), f, []string{"PATH=/usr/bin:/bin", "LANG=C"},
		sandboxProbeRequest{Egress: true, HostURL: "http://192.0.2.1:9/", OperatorKey: testOperatorKey})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.Path, l.Args...)
	cmd.Env, cmd.ExtraFiles, cmd.Dir = l.Env, l.Files, work
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the run: %v\n%s", err, stderr.String())
	}
	var got sandboxProbeResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("probe reply %q: %v (%s)", stdout.String(), err, stderr.String())
	}
	t.Logf("the agent saw %+v", got)
	if got.Error != "" || got.RouteStatus != 200 || !got.KeyIsPlaceholder || got.EnvHasOperatorKey {
		t.Errorf("model call: status %d %s, placeholder %v, operator key in env %v (%s)", got.RouteStatus, got.RouteBody,
			got.KeyIsPlaceholder, got.EnvHasOperatorKey, got.Error)
	}
	if got.ConnectStatus != http.StatusForbidden || got.ConnectNoKeyStatus != http.StatusProxyAuthRequired {
		t.Errorf("CONNECT with the key %d, without %d", got.ConnectStatus, got.ConnectNoKeyStatus)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Header.Get("x-api-key") != testOperatorKey {
		t.Fatalf("the model service received %+v", reqs)
	}
	env := envOf(got.Environ)
	key := strings.Split(strings.TrimPrefix(env["HTTPS_PROXY"], "http://anet:"), "@")[0]
	if len(key) != 64 || strings.Contains(strings.Join(got.Argv, " "), key) || strings.Contains(got.Cmdline, key) {
		t.Errorf("the run's key %q on the agent's command line %q %q", key, got.Argv, got.Cmdline)
	}
	if got.StdinBytes == 0 || !strings.HasPrefix(env["HOME"], filepath.Dir(env["TMPDIR"])) ||
		filepath.Base(env["HOME"]) != "home" || filepath.Base(env["TMPDIR"]) != "tmp" {
		t.Errorf("stdin %d bytes, HOME %q, TMPDIR %q", got.StdinBytes, env["HOME"], env["TMPDIR"])
	}
}

// MS-3: the agent's search path keeps only the directories it may run programs from.
func TestEnvWithSeatbeltPath(t *testing.T) {
	got := envWithSeatbeltPath([]string{"A=1", "PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/Users/u/.local/bin:rel:/usr/sbin"},
		[]string{"/Users/u/.local/bin"})
	if !slices.Equal(got, []string{"A=1", "PATH=/usr/bin:/bin:/Users/u/.local/bin"}) {
		t.Errorf("%q", got)
	}
	if got := envWithValue([]string{"TMPDIR=/tmp", "B=2", "TMPDIR=/x"}, "TMPDIR", "/r/tmp"); !slices.Equal(got, []string{"B=2", "TMPDIR=/r/tmp"}) {
		t.Errorf("%q", got)
	}
}

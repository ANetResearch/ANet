//go:build linux || darwin

package autoreply

// runner_test.go — `anet runner serve` and the node's side of it (design v0.3.9 §C): who may connect, the
// job's round trip with a real agent program, cancellation, the protocol's limits, the agent's environment
// and the work directories.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/backendconn"
)

var (
	fakeAgentOnce sync.Once
	fakeAgentPath string
	fakeAgentErr  error
)

// fakeAgent is the path of testdata/fakeagent, built once for the run with the go command: a separate
// program the runner starts as hermes.
func fakeAgent(t testing.TB) string {
	t.Helper()
	fakeAgentOnce.Do(func() {
		gobin, err := exec.LookPath("go")
		if err != nil {
			gobin = filepath.Join(runtime.GOROOT(), "bin", "go")
		}
		dir, err := os.MkdirTemp("", "anet-fakeagent-")
		if err != nil {
			fakeAgentErr = err
			return
		}
		testCleanups = append(testCleanups, func() { _ = os.RemoveAll(dir) })
		out := filepath.Join(dir, "hermes")
		cmd := exec.Command(gobin, "build", "-o", out, "./testdata/fakeagent")
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
		if testGoCache != "" {
			cmd.Env = append(cmd.Env, "GOCACHE="+testGoCache)
		}
		if b, err := cmd.CombinedOutput(); err != nil {
			fakeAgentErr = fmt.Errorf("go build testdata/fakeagent: %v\n%s", err, b)
			return
		}
		fakeAgentPath = out
	})
	if fakeAgentErr != nil {
		if errors.Is(fakeAgentErr, exec.ErrNotFound) {
			t.Skip("no go command to build the fake agent")
		}
		t.Fatal(fakeAgentErr)
	}
	return fakeAgentPath
}

// testRunner is a runner served by this process for one test.
type testRunner struct {
	sock, root string
}

// startTestRunner serves a runner with o on a socket in a new short directory (a socket path is short),
// for this test's uid unless o says otherwise.
func startTestRunner(t *testing.T, o RunnerOptions) testRunner {
	t.Helper()
	quietLog(t)
	dir, err := os.MkdirTemp("", "anr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	o.Socket = filepath.Join(dir, "r.sock")
	if o.Root == "" {
		o.Root = filepath.Join(dir, "root")
	}
	if o.AllowUser == "" {
		o.AllowUser = strconv.Itoa(os.Getuid())
	}
	ready := make(chan struct{})
	o.Ready = func(string) { close(ready) }
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- RunnerServe(ctx, o) }()
	select {
	case <-ready:
	case err := <-errc:
		cancel()
		t.Fatalf("RunnerServe: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the runner did not start")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("RunnerServe: %v", err)
		}
	})
	return testRunner{sock: o.Socket, root: o.Root}
}

// selfRunner is the node's runner block for a runner of this test's uid.
func (r testRunner) cfg() *AutoReplyRunnerConfig {
	return &AutoReplyRunnerConfig{Socket: r.sock, ExpectedUser: strconv.Itoa(os.Getuid())}
}

func hermesJob(conversation string) runnerJob {
	return runnerJob{Agent: agentHermes, Conversation: conversation, TimeoutS: 60, Peer: "peerabcdef012345"}
}

// filesByName are a result's files by name.
func filesByName(l runnerLine) map[string]string {
	out := map[string]string{}
	for _, f := range l.Files {
		out[f.Name] = string(f.Data)
	}
	return out
}

// A job goes to the runner and back: the agent is the program the runner names, it reads the conversation
// from its file and the attachment from the input directory, works in the peer's directory, and its outbox
// comes back as files. Its environment is the runner's allowlisted one and the job's ANET_MODEL_* and
// ANET_TURN_* variables, nothing else: no ANET_* variable of the runner's, no unrelated secret.
func TestRunnerJobRoundTrip(t *testing.T) {
	t.Setenv("ANET_CONTROL_TOKEN_PROBE", "runner-own-anet-variable")
	t.Setenv("UNRELATED_SERVICE_SECRET", "runner-own-secret")
	r := startTestRunner(t, RunnerOptions{Agents: map[string]string{agentHermes: fakeAgent(t)}})
	job := hermesJob("Requester: hello\n\nRequester attachment: turn0-img0.png (image/png)")
	job.Files = []runnerFile{{Name: "turn0-img0.png", Mime: "image/png", Data: []byte("\x89PNG fake")}}
	job.Env = map[string]string{"ANET_MODEL_PROBE": "probe-value"}
	res, err := callRunner(context.Background(), r.cfg(), job)
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit != runnerExitOK || !strings.HasPrefix(res.Reply, "fake reply") {
		t.Fatalf("result %+v", res)
	}
	files := filesByName(res)
	if !strings.Contains(files["result.txt"], "input:turn0-img0.png") {
		t.Errorf("the agent did not find the attachment: %q", files["result.txt"])
	}
	env := files["env.txt"]
	for _, want := range []string{"ANET_MODEL_PROBE=probe-value", "ANET_OUTBOX=", "PATH="} {
		if !strings.Contains(env, want) {
			t.Errorf("the agent's environment lacks %s:\n%s", want, env)
		}
	}
	for _, never := range []string{"runner-own-anet-variable", "runner-own-secret", "ANET_CONTROL_TOKEN_PROBE"} {
		if strings.Contains(env, never) {
			t.Errorf("the agent's environment holds %s:\n%s", never, env)
		}
	}
	want := filepath.Join(r.root, "peerabcdef012345")
	if got := strings.TrimSpace(files["cwd.txt"]); !sameDir(got, want) {
		t.Errorf("the agent worked in %s, want the peer's directory %s", got, want)
	}
	// The peer's directory stays (per-peer); the job's input directory and outbox do not.
	ents, err := os.ReadDir(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		t.Errorf("left in the peer's directory after the job: %s", e.Name())
	}
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// A connection from an account --allow-user does not name is closed before anything of it is read or
// written: the runner answers nothing at all.
func TestRunnerRefusesAnotherAccount(t *testing.T) {
	r := startTestRunner(t, RunnerOptions{AllowUser: strconv.Itoa(os.Getuid() + 1), Agents: map[string]string{agentHermes: "/bin/false"}})
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = writeRunnerLine(c, hermesJob("hello"))
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, err := bufio.NewReader(c).ReadByte()
	if err == nil {
		t.Fatalf("the runner answered a connection of another account: %q", b)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the runner kept the connection of another account open")
	}
}

// The node checks the runner's socket as a service backend's: a listener of another account than
// expected_user is refused, and nothing is sent to it.
func TestRunnerNodeChecksTheListener(t *testing.T) {
	r := startTestRunner(t, RunnerOptions{Agents: map[string]string{agentHermes: "/bin/false"}})
	cfg := r.cfg()
	cfg.ExpectedUser = strconv.Itoa(os.Getuid() + 1)
	_, err := callRunner(context.Background(), cfg, hermesJob("hello"))
	var re *backendconn.RefusedError
	if !errors.As(err, &re) || re.Code != backendconn.CodePeerUIDMismatch {
		t.Fatalf("want peer_uid_mismatch, got %v", err)
	}
}

// Ending the node's side of a job — its context, which closes the connection — ends the agent and
// everything it started: the whole process group.
func TestRunnerCancellationEndsTheProcessGroup(t *testing.T) {
	r := startTestRunner(t, RunnerOptions{Agents: map[string]string{agentHermes: fakeAgent(t)}})
	pidFile := filepath.Join(t.TempDir(), "pids")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := callRunner(ctx, r.cfg(), hermesJob("MODE:sleep PIDS:"+pidFile))
		done <- err
	}()
	var pids []int
	deadline := time.Now().Add(30 * time.Second)
	for len(pids) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the agent did not start")
		}
		if b, err := os.ReadFile(pidFile); err == nil {
			for _, f := range strings.Fields(string(b)) {
				n, _ := strconv.Atoi(f)
				pids = append(pids, n)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("callRunner after cancel: %v", err)
	}
	for _, pid := range pids {
		deadline := time.Now().Add(10 * time.Second)
		for !processGone(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d of the cancelled job is still running", pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// processGone reports a process that no longer runs: none with the id, or a zombie waiting for its parent.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if i := strings.LastIndexByte(string(b), ')'); i > 0 && len(b) > i+2 {
			return b[i+2] == 'Z'
		}
	}
	return false
}

// An agent the runner does not run is refused with runner_agent_not_allowed; one that fails, prints past
// the output bound, or works in a per-run directory ends as it should.
func TestRunnerExits(t *testing.T) {
	bin := fakeAgent(t)
	r := startTestRunner(t, RunnerOptions{Agents: map[string]string{agentHermes: bin}})
	ctx := context.Background()

	job := hermesJob("hello")
	job.Agent = agentClaude
	if res, err := callRunner(ctx, r.cfg(), job); err != nil || res.Exit != runnerExitRefused || res.Code != "runner_agent_not_allowed" {
		t.Errorf("an agent not served: %+v %v", res, err)
	}
	if res, err := callRunner(ctx, r.cfg(), hermesJob("MODE:fail")); err != nil || res.Exit != runnerExitError ||
		!strings.Contains(res.Detail, "failing as asked") {
		t.Errorf("a failing agent: %+v %v", res, err)
	}
	if res, err := callRunner(ctx, r.cfg(), hermesJob("MODE:flood")); err != nil || res.Exit != runnerExitTooLarge || res.Reply != "" {
		t.Errorf("an agent over the output bound: exit %q, %d bytes of reply, %v", res.Exit, len(res.Reply), err)
	}
	bad := hermesJob("hello")
	bad.Env = map[string]string{"ANET_DATA_DIR": "/somewhere"}
	if res, err := callRunner(ctx, r.cfg(), bad); err != nil || res.Exit != runnerExitRefused || res.Code != "runner_job_invalid" {
		t.Errorf("a job with a variable outside ANET_TURN_* and ANET_MODEL_*: %+v %v", res, err)
	}

	runs := startTestRunner(t, RunnerOptions{Agents: map[string]string{agentHermes: bin}, Workspace: RunnerWorkspacePerRun})
	res, err := callRunner(ctx, runs.cfg(), hermesJob("hello"))
	if err != nil || res.Exit != runnerExitOK {
		t.Fatalf("per-run: %+v %v", res, err)
	}
	cwd := strings.TrimSpace(filesByName(res)["cwd.txt"])
	if !strings.Contains(cwd, string(filepath.Separator)+runnerRunsDir+string(filepath.Separator)) {
		t.Errorf("a per-run job worked in %s", cwd)
	}
	if _, err := os.Stat(cwd); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the per-run directory %s is still there after the job: %v", cwd, err)
	}
}

// --keep-days: a peer's directory whose last job is older is removed when the runner starts; a recent
// one stays.
func TestRunnerSweepsOldPeerDirectories(t *testing.T) {
	root := t.TempDir()
	old, recent := filepath.Join(root, "oldpeer0000000001"), filepath.Join(root, "newpeer0000000001")
	for _, d := range []string{old, recent} {
		if err := os.MkdirAll(filepath.Join(d, "result"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-20 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	startTestRunner(t, RunnerOptions{Root: root, KeepDays: 14, Agents: map[string]string{agentHermes: "/bin/false"}})
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a directory 20 days old is still there: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("a recent directory was removed: %v", err)
	}
}

// A second runner does not take the socket of one that is running.
func TestRunnerSocketIsLocked(t *testing.T) {
	r := startTestRunner(t, RunnerOptions{Agents: map[string]string{agentHermes: "/bin/false"}})
	err := RunnerServe(context.Background(), RunnerOptions{Socket: r.sock, AllowUser: strconv.Itoa(os.Getuid()), Root: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "another runner") {
		t.Fatalf("a second runner on the same socket: %v", err)
	}
}

// What the agent can see of the runner's account is its allowlisted environment, the outbox and the job's
// own two kinds of variable.
func TestRunnerAgentEnv(t *testing.T) {
	environ := []string{"PATH=/usr/bin:/bin", "HOME=/home/runner", "ANET_DATA_DIR=/home/runner/.anet", "ANET_TURN_URL=http://from-the-runner",
		"OPENAI_API_KEY=runner-key", "AWS_SECRET_ACCESS_KEY=x", "LC_ALL=C"}
	job := hermesJob("x")
	job.Env = map[string]string{"ANET_TURN_URL": "http://127.0.0.1:1/anet-turn/v1", "ANET_MODEL_KEY": "mpk-k"}
	got := strings.Join(runnerAgentEnv(environ, job, "/w/.anet-outbox-1"), "\n")
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/runner", "LC_ALL=C", "ANET_OUTBOX=/w/.anet-outbox-1",
		"ANET_TURN_URL=http://127.0.0.1:1/anet-turn/v1", "ANET_MODEL_KEY=mpk-k"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %s:\n%s", want, got)
		}
	}
	for _, never := range []string{"ANET_DATA_DIR", "http://from-the-runner", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(got, never) {
			t.Errorf("holds %s:\n%s", never, got)
		}
	}
}

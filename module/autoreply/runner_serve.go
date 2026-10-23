//go:build linux || darwin

package autoreply

// runner_serve.go — `anet runner serve` (design v0.3.9 §C): the exec backend's direct runs, started as
// another local account than the node's.
//
// The node keeps its identity keys, its control token, its service's database and the model keys; the
// agent, which a member can steer into running any program, runs as the runner's account and sees none of
// them. The runner listens on a Unix socket and takes a job only from the account --allow-user names:
// every connection is checked with the kernel's peer credentials (internal/peercred) before a byte of it
// is read, and one from anybody else is closed unread. The node checks the other way round that the
// account listening is the runner's (auto_reply.runner.expected_user, internal/backendconn).
//
// A job runs with the code of the exec backend (agents.go: the same argv, the same conversation channel,
// the same output bounds and process group) in a work directory of the runner's: per peer,
// <cache>/anet/runner/<aid16>/, kept --keep-days after its last job so that a member finds earlier
// results; or per run, removed when the job ends. Each job gets its own input directory and outbox in it.
// The agent's environment is the runner's own allowlisted one (execEnv) and exactly the job's
// ANET_TURN_* and ANET_MODEL_* variables. Closing the connection cancels the job, which kills the agent's
// process group.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/internal/fsperm"
	"github.com/ANetResearch/ANet/internal/peercred"
)

// RunnerOptions configures RunnerServe.
type RunnerOptions struct {
	// Socket is the absolute path of the socket to listen on.
	Socket string
	// AllowUser is the account (a name or a uid) whose connections are taken: the node's.
	AllowUser string
	// SocketGroup, when set, is the group the socket is given (a name or a gid), for a node account that
	// reaches the socket through that group.
	SocketGroup string
	// SocketMode is the socket's permission bits: 0660 when 0.
	SocketMode os.FileMode
	// Agents are the agents the runner runs, by id, each with its binary ("" finds it on PATH as the exec
	// backend does). Empty: every supported agent, found on PATH.
	Agents map[string]string
	// MaxJobs bounds the jobs that run at once (4 when 0); a job over it waits, queued.
	MaxJobs int
	// Workspace is RunnerWorkspacePerPeer (the default) or RunnerWorkspacePerRun.
	Workspace string
	// KeepDays is how long a per-peer work directory is kept after its last job (14 when unset, -1 never
	// removed).
	KeepDays int
	// Root is the work root; <user cache dir>/anet/runner when "".
	Root string
	// Ready, when set, is called with the listener's path once it accepts connections.
	Ready func(socket string)
}

// The values of RunnerOptions.Workspace.
const (
	RunnerWorkspacePerPeer = "per-peer"
	RunnerWorkspacePerRun  = "per-run"
)

// runnerJobReadTimeout bounds how long a connection may take to send its job.
const runnerJobReadTimeout = 30 * time.Second

// runnerSweepEvery is how often the per-peer work directories past --keep-days are removed.
const runnerSweepEvery = time.Hour

// runnerRunsDir is the per-run work directories' parent under the root; no peer is named so
// (runnerPeerName begins with a letter or a digit).
const runnerRunsDir = "_runs"

// RunnerServe serves jobs on o.Socket until ctx ends. It returns an error when it cannot start: the
// options are not usable, peer credentials cannot be read here (peer_cred_unsupported), another runner
// holds the socket, or the socket cannot be made.
func RunnerServe(ctx context.Context, o RunnerOptions) error {
	if !peercred.Supported() {
		return a2ashape.Refuse("runner_unsupported", nil)
	}
	s, err := newRunnerServer(o)
	if err != nil {
		return err
	}
	ln, unlock, err := s.listen()
	if err != nil {
		return err
	}
	defer unlock()
	defer ln.Close()
	s.sweep(true)
	go s.sweepLoop(ctx)
	log.Printf("anet runner: serving %s for uid %d (agents: %s; workspace %s; at most %d jobs at once)",
		o.Socket, s.allow, strings.Join(s.agentNames(), ", "), s.workspace, cap(s.slots))
	if o.Ready != nil {
		o.Ready(o.Socket)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	var jobs sync.WaitGroup
	defer jobs.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			s.serveConn(ctx, c)
		}()
	}
}

// runnerServer is one RunnerServe.
type runnerServer struct {
	o         RunnerOptions
	allow     int
	group     int // -1: none
	agents    map[string]string
	workspace string
	keep      time.Duration // 0: never removed
	root      string
	slots     chan struct{}

	mu     sync.Mutex
	active map[string]int // per-peer directories with a job in them
}

func newRunnerServer(o RunnerOptions) (*runnerServer, error) {
	s := &runnerServer{o: o, group: -1, active: map[string]int{}}
	if !filepath.IsAbs(o.Socket) || filepath.Clean(o.Socket) != o.Socket {
		return nil, fmt.Errorf("--socket must be a clean absolute path")
	}
	uid, err := lookupID(o.AllowUser, false)
	if err != nil {
		return nil, fmt.Errorf("--allow-user %q: %w", o.AllowUser, err)
	}
	s.allow = uid
	if o.SocketGroup != "" {
		if s.group, err = lookupID(o.SocketGroup, true); err != nil {
			return nil, fmt.Errorf("--socket-group %q: %w", o.SocketGroup, err)
		}
	}
	s.agents = map[string]string{}
	if len(o.Agents) == 0 {
		for _, id := range SupportedExecAgents() {
			s.agents[id] = ""
		}
	}
	for id, bin := range o.Agents {
		if _, err := lookupAgent(id); err != nil {
			return nil, fmt.Errorf("--agent: %w", err)
		}
		if bin != "" && !filepath.IsAbs(bin) {
			return nil, fmt.Errorf("--agent %s=%s: the binary must be an absolute path", id, bin)
		}
		s.agents[id] = bin
	}
	switch o.Workspace {
	case "", RunnerWorkspacePerPeer:
		s.workspace = RunnerWorkspacePerPeer
	case RunnerWorkspacePerRun:
		s.workspace = RunnerWorkspacePerRun
	default:
		return nil, fmt.Errorf("--workspace must be %s or %s", RunnerWorkspacePerPeer, RunnerWorkspacePerRun)
	}
	switch {
	case o.KeepDays == 0:
		s.keep = 14 * 24 * time.Hour
	case o.KeepDays > 0:
		s.keep = time.Duration(o.KeepDays) * 24 * time.Hour
	}
	n := o.MaxJobs
	if n <= 0 {
		n = 4
	}
	if n > runnerMaxJobs {
		return nil, fmt.Errorf("--max-jobs is at most %d", runnerMaxJobs)
	}
	s.slots = make(chan struct{}, n)
	s.root = o.Root
	if s.root == "" {
		c, err := os.UserCacheDir()
		if err != nil || c == "" {
			return nil, errors.New(errWorkRootUnknown)
		}
		s.root = filepath.Join(c, "anet", "runner")
	}
	if err := mkdirPrivate(s.root); err != nil {
		return nil, err
	}
	return s, nil
}

// lookupID is a decimal uid or gid, or the id of the user or group named.
func lookupID(s string, group bool) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("it is required")
	}
	if strings.Trim(s, "0123456789") == "" {
		n, err := strconv.ParseUint(s, 10, 31)
		return int(n), err
	}
	var id string
	if group {
		g, err := user.LookupGroup(s)
		if err != nil {
			return 0, err
		}
		id = g.Gid
	} else {
		u, err := user.Lookup(s)
		if err != nil {
			return 0, err
		}
		id = u.Uid
	}
	return strconv.Atoi(id)
}

func (s *runnerServer) agentNames() []string {
	names := make([]string, 0, len(s.agents))
	for id := range s.agents {
		names = append(names, id)
	}
	sort.Strings(names)
	return names
}

// listen takes the socket: a lock file beside it keeps a second runner from replacing the socket of one
// that is running, a stale socket of an earlier one is removed, and the new one gets its mode and group.
func (s *runnerServer) listen() (net.Listener, func(), error) {
	sock := s.o.Socket
	lock, err := fsperm.OpenNoFollow(sock+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("runner lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, nil, fmt.Errorf("another runner serves %s", sock)
	}
	unlock := func() { _ = os.Remove(sock); _ = lock.Close() }
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode().Type() != os.ModeSocket {
			unlock()
			return nil, nil, fmt.Errorf("%s exists and is not a socket", sock)
		}
		_ = os.Remove(sock) // the lock is ours: nobody serves it
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	mode := s.o.SocketMode
	if mode == 0 {
		mode = 0o660
	}
	if err := os.Chmod(sock, mode&0o777); err != nil {
		_ = ln.Close()
		unlock()
		return nil, nil, err
	}
	if s.group >= 0 {
		if err := os.Lchown(sock, -1, s.group); err != nil {
			_ = ln.Close()
			unlock()
			return nil, nil, fmt.Errorf("--socket-group: %w", err)
		}
	}
	return ln, unlock, nil
}

// serveConn takes one connection: from the allowed account only, one job, then closed.
func (s *runnerServer) serveConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	cred, err := peercred.Of(c)
	if err != nil {
		log.Printf("anet runner: refused a connection: %v", err)
		return
	}
	if cred.UID != s.allow {
		log.Printf("anet runner: refused a connection from uid %d (only uid %d is served)", cred.UID, s.allow)
		return
	}
	br := bufio.NewReaderSize(c, 64<<10)
	_ = c.SetReadDeadline(time.Now().Add(runnerJobReadTimeout))
	var job runnerJob
	if err := readRunnerLine(br, runnerJobMax, &job); err != nil {
		s.answer(c, runnerLine{Exit: runnerExitRefused, Code: "runner_job_invalid", Detail: logText(err.Error(), 200)})
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if err := job.check(); err != nil {
		s.answer(c, runnerLine{Exit: runnerExitRefused, Code: "runner_job_invalid", Detail: logText(err.Error(), 200)})
		return
	}
	bin, ok := s.agents[job.Agent]
	if !ok {
		log.Printf("anet runner: refused a job for agent %s (not served here)", logText(job.Agent, 40))
		s.answer(c, runnerLine{Exit: runnerExitRefused, Code: "runner_agent_not_allowed"})
		return
	}
	// The node writes nothing after its job: anything that arrives, or the end of the connection, cancels it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		var b [1]byte
		_, _ = br.Read(b[:])
		cancel()
	}()
	select {
	case s.slots <- struct{}{}:
	default:
		if s.answer(c, runnerLine{Progress: "queued"}) != nil {
			return
		}
		select {
		case s.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
	defer func() { <-s.slots }()
	if s.answer(c, runnerLine{Progress: "started"}) != nil {
		return
	}
	start := time.Now()
	res := s.run(ctx, job, bin)
	if ctx.Err() != nil && res.Exit != runnerExitTimeout {
		log.Printf("anet runner: job for peer %s: agent %s: canceled after %s", job.Peer, job.Agent, time.Since(start).Round(time.Millisecond))
		return
	}
	log.Printf("anet runner: job for peer %s: agent %s: exit %s (%s, %d file(s))", job.Peer, job.Agent, res.Exit,
		time.Since(start).Round(time.Millisecond), len(res.Files))
	_ = s.answer(c, res)
}

// answer writes one line, within a bound of time.
func (s *runnerServer) answer(c net.Conn, l runnerLine) error {
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	return writeRunnerLine(c, l)
}

// run runs one job and returns its final line.
func (s *runnerServer) run(ctx context.Context, job runnerJob, bin string) runnerLine {
	fail := func(err error) runnerLine {
		return runnerLine{Exit: runnerExitError, Detail: logText(err.Error(), 400)}
	}
	workDir, done, err := s.workDir(job.Peer)
	if err != nil {
		return fail(fmt.Errorf("work dir: %w", err))
	}
	defer done()
	inputDir, err := mkdirTempPrivate(workDir, ".anet-in-")
	if err != nil {
		return fail(err)
	}
	defer execwork.RemoveTree(inputDir)
	outbox, err := mkdirTempPrivate(workDir, ".anet-outbox-")
	if err != nil {
		return fail(err)
	}
	defer execwork.RemoveTree(outbox)
	conversation := job.Conversation
	if len(job.Files) > 0 {
		for _, f := range job.Files {
			if err := writeNewPrivate(filepath.Join(inputDir, f.Name), f.Data); err != nil {
				return fail(err)
			}
		}
		// The conversation names its attachments; where they are is the runner's to say.
		conversation += "\n\nThe attachments named above are in the directory " + inputDir + "."
	}
	timeout := autoReplyDefaultAPITimeout
	if job.TimeoutS > 0 {
		timeout = time.Duration(job.TimeoutS) * time.Second
	}
	reply, err := InvokeAgent(ctx, execInvokeOpts{
		AgentID:      job.Agent,
		Instructions: job.Instructions,
		Conversation: conversation,
		InputDir:     inputDir,
		WorkDir:      workDir,
		Model:        job.Model,
		Command:      bin,
		Env:          runnerAgentEnv(os.Environ(), job, outbox),
		Timeout:      timeout,
	})
	switch {
	case errors.Is(err, errAgentOutputTooLarge):
		return runnerLine{Exit: runnerExitTooLarge}
	case errors.Is(err, context.DeadlineExceeded):
		return runnerLine{Exit: runnerExitTimeout, Detail: logText(err.Error(), 400)}
	case err != nil:
		return fail(err)
	}
	atts, err := collectOutbox(outbox)
	if err != nil {
		return fail(fmt.Errorf("outbox: %w", err))
	}
	out := runnerLine{Exit: runnerExitOK, Reply: reply}
	for _, a := range atts {
		out.Files = append(out.Files, runnerFile{Name: a.Name, Mime: a.Mime, Data: a.Data})
	}
	return out
}

// runnerAgentEnv is the agent's whole environment in the runner: the runner's own, through the exec
// backend's allowlist (no ANET_* variable of the runner's passes), ANET_OUTBOX, and the job's ANET_TURN_*
// and ANET_MODEL_* variables, which job.check let through.
func runnerAgentEnv(environ []string, job runnerJob, outbox string) []string {
	env := execEnvFrom(environ, AutoReplyConfig{Agent: job.Agent}, nil, outbox)
	keys := make([]string, 0, len(job.Env))
	for k := range job.Env {
		if runnerEnvName(k) && runnerEnvValue(job.Env[k]) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+job.Env[k])
	}
	return env
}

// workDir is the job's work directory and what ends its use: the peer's own directory, marked in use
// until the job ends and then touched so that --keep-days counts from the last job; or a fresh per-run
// directory, removed when the job ends.
func (s *runnerServer) workDir(peer string) (string, func(), error) {
	if s.workspace == RunnerWorkspacePerRun {
		parent := filepath.Join(s.root, runnerRunsDir)
		if err := mkdirPrivate(parent); err != nil {
			return "", nil, err
		}
		dir, err := mkdirTempPrivate(parent, "run-")
		if err != nil {
			return "", nil, err
		}
		return dir, func() { execwork.RemoveTree(dir) }, nil
	}
	dir := filepath.Join(s.root, peer)
	s.mu.Lock()
	s.active[peer]++
	err := mkdirPrivate(dir)
	s.mu.Unlock()
	release := func() {
		now := time.Now()
		_ = os.Chtimes(dir, now, now)
		s.mu.Lock()
		if s.active[peer]--; s.active[peer] <= 0 {
			delete(s.active, peer)
		}
		s.mu.Unlock()
	}
	if err != nil {
		release()
		return "", nil, err
	}
	return dir, release, nil
}

func (s *runnerServer) sweepLoop(ctx context.Context) {
	t := time.NewTicker(runnerSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(false)
		}
	}
}

// sweep removes the per-peer work directories whose last job ended more than --keep-days ago, and, at
// start, the per-run directories an earlier runner left when it was stopped in the middle of a job.
func (s *runnerServer) sweep(atStart bool) {
	if atStart {
		execwork.RemoveTree(filepath.Join(s.root, runnerRunsDir))
	}
	if s.keep <= 0 {
		return
	}
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	cut := time.Now().Add(-s.keep)
	for _, e := range ents {
		name := e.Name()
		if !e.IsDir() || name == runnerRunsDir || !runnerPeerName(name) {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.ModTime().After(cut) {
			continue
		}
		// Under the lock: a job that starts for this peer waits until its directory is gone, and then
		// makes a new one.
		s.mu.Lock()
		if s.active[name] == 0 {
			execwork.RemoveTree(filepath.Join(s.root, name))
		}
		s.mu.Unlock()
	}
}

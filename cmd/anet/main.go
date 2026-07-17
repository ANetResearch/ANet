// Command anet is the anet v0.1 daemon + CLI. `anet daemon` runs the long-lived process (identity +
// local delegation store + Hub relay client + local control plane); the other verbs are thin clients
// that drive the running daemon over its control API. v0.1 is centralized: all traffic flows through the
// official Hub. See internal/daemon.
package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/ANetResearch/ANet/internal/daemon"
)

func main() {
	// --id <name> is a GLOBAL selector (may appear anywhere) picking which named identity to act on; it is
	// pulled out before verb dispatch. See internal/daemon identities.go for the full selection precedence.
	idFlag, args := extractGlobalID(os.Args[1:])
	layout := daemon.ResolveLayout(idFlag)
	explicit := daemon.SelectionIsExplicit(idFlag)
	if len(args) == 0 {
		guide(layout)
		return
	}
	cmd, rest := args[0], args[1:]
	fail := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
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
		fmt.Println("anet", daemon.Version)
	case "logs":
		printLogs(layout, rest)
	case "install":
		fail(runInstall(rest))
	case "help", "-h", "--help":
		if len(rest) > 0 && (rest[0] == "--all" || rest[0] == "all") {
			usageAll()
		} else {
			guide(layout)
		}
	default:
		fail(runClient(layout, cmd, rest, explicit))
	}
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

// cmdDoc is one verb in the help registry: its invocation form + a one-line description.
type cmdDoc struct{ use, desc string }

var grpNetwork = []cmdDoc{
	{"hub-register <url> [--name N] [--caps a,b] [--guest-messages N] [--accept-delegations true|false]", "在官方 Hub 上注册你的 agent(提交 AID; 访客试玩默认 5 条, 0 关闭)"},
	{"accept <on|off>", "是否接收别人委派来的任务(默认 on; 关闭后仍在 find 可见, 但委派会被丢弃)"},
	{"autoreply set --backend exec --agent <cursor|claude|…>", "全自动接单: 收到委派就拉起本机编码 agent 撰写回复(热生效, 无需重启)"},
	{"autoreply set --backend openai --api-base URL --model M", "全自动接单: 用你的 OpenAI 兼容 API 回复(可加 --require-image 等)"},
	{"autoreply test [\"问题\"]", "本地验证自动回复(不经过 Hub, 不建身份, 零污染)"},
	{"autoreply show|off", "查看 / 关闭内置自动回复循环"},
	{"profile set [--summary S] [--readme S|@file] [--pricing S]", "由你的 agent 自述能力与收费(仅展示), 发布到 Hub"},
	{"console [--url]", "打开本地控制台(浏览+一键 find/delegate/review); --url 只打印网址(交给操作者在浏览器打开)"},
	{"find [query]", "在 Hub 上搜索 agent(按 AID/名字/能力/自述子串; 空 query 列全部)"},
	{"delegate <provider-aid> <goal> [--attach PATH …]", "把任务经 Hub 中继排队给对方(立即返回 interaction_id, 对方可离线; --attach 附带图片/媒体/压缩包)"},
	{"inbox [--pending]", "列出别人委派给我的任务(--pending 只看未结束)"},
	{"thread <id>", "读一次交互的完整对话(多轮消息 + 附件清单 + 结束协商状态)"},
	{"message <id> <text…>|--file PATH [--attach PATH …]", "在一次委派里发消息(多轮对话, 任一方都可发; --attach 发送图片/媒体/压缩包, 单个 ≤64 MiB)"},
	{"pull <id> [--out DIR]", "把收到的附件(图片/媒体/压缩包)保存到本地目录(默认当前目录)"},
	{"end <id>", "提议结束任务(对方也点 end 即达成一致; 双方一致后由委派方评价)"},
	{"accept-end <id>", "同意对方的结束提议(与 end 等价, 语义更明确)"},
	{"results", "拉取我委派、现在已结束的任务对话记录(含对方回执)"},
	{"review <id> <rating 1-5> [comment]", "为已结束的委派签署评价并上传到 Hub"},
}

func printSection(title string, cmds []cmdDoc) {
	if len(cmds) == 0 {
		return
	}
	fmt.Printf("%s\n", title)
	for _, c := range cmds {
		fmt.Printf("  anet %-52s %s\n", c.use, c.desc)
	}
	fmt.Println()
}

const nextPrefix = "  → "

func printNext(steps [][2]string) {
	if len(steps) == 0 {
		return
	}
	fmt.Println("Next:")
	for _, s := range steps {
		fmt.Printf("%s%-46s — %s\n", nextPrefix, s[0], s[1])
	}
	fmt.Println()
}

func shortAID(aid string) string {
	if aid == "" {
		return "(no identity yet)"
	}
	if len(aid) > 20 {
		return aid[:20] + "…"
	}
	return aid
}

// guide prints a STATE-AWARE introduction: it queries the running daemon and shows what to do next in
// the centralized v0.1 model — register on the Hub, find agents, delegate, chat, end, review.
func guide(layout daemon.Layout) {
	fmt.Printf("anet %s — the agent collaboration network (v0.1, centralized via the official Hub)\n", daemon.Version)
	fmt.Println("register · find · delegate · chat · end · review — anet moves signed tasks; your agent does the work")
	fmt.Println()

	var st map[string]any
	running := false
	if base, token, err := daemon.ResolveControl(layout); err == nil {
		c := &client{base: base, token: token, timeout: 3 * time.Second}
		if b, code, e := c.fetch("/status", nil); e == nil && code == 200 {
			running = true
			_ = json.Unmarshal(b, &st)
		}
	}

	if !running {
		fmt.Println("The daemon isn't running — it's your gateway to the network.")
		fmt.Printf("Data dir: %s", layout.Root)
		if fileExists(layout.IdentityPath()) {
			fmt.Print("  (an identity already exists here — starting the daemon REUSES it)")
		}
		fmt.Println()
		fmt.Println("Tip: to join as a brand-new identity (e.g. a fresh test), point ANET_DATA_DIR at an empty dir first:")
		fmt.Println("       export ANET_DATA_DIR=~/.anet-test   # then: anet daemon --detach")
		fmt.Println()
		printSection("Start here", []cmdDoc{{"daemon --detach", "run the node in the background so it OUTLIVES this shell (then re-run `anet`)"}})
		printNext([][2]string{{"anet daemon --detach", "start your node detached (alias: `anet up`), then run `anet` again"}})
		return
	}

	aid, _ := st["aid"].(string)
	hub, _ := st["hub_url"].(string)
	dataDir, _ := st["data_dir"].(string)
	fmt.Printf("You are agent %s\n", shortAID(aid))
	if dataDir != "" {
		fmt.Printf("Data dir: %s\n", dataDir)
	}
	if hub == "" {
		fmt.Println("You're not registered with a Hub yet.")
		fmt.Println()
		printSection("Join the network", grpNetwork)
		printNext([][2]string{
			{"anet hub-register <url> --name <you>", "register so others can find you (submit your AID)"},
			{`anet profile set --summary "..."`, "if you provide a service, describe it"},
			{"anet console", "open the local web console to browse + act"},
		})
		return
	}
	fmt.Printf("Registered with Hub %s\n", hub)
	fmt.Println()
	printSection("Collaborate through the Hub", grpNetwork)
	printNext([][2]string{
		{"anet console", "open the local web console (browse + one-click actions)"},
		{`anet delegate <aid> "<goal>"`, "queue a task for a provider"},
		{"anet inbox --pending", "see tasks others delegated to you"},
	})
	fmt.Println("Full reference: anet help --all")
}

func usageAll() {
	fmt.Print(`anet ` + daemon.Version + ` — anet daemon + CLI (v0.1, centralized)

  anet daemon                 run the daemon in the FOREGROUND (identity + local store + Hub relay client + control plane)
  anet up [name] [--all]      start a node detached so it OUTLIVES this shell (alias: anet daemon --detach) — recommended
  anet stop [name] [--all]    gracefully stop a running daemon (alias: anet down) — no kill/PID needed
  anet status                 show daemon identity + data dir + Hub registration + profile
  anet logs [N|--all]         show the daemon log

 Identities (run several personas on one machine — e.g. a "coder" and a "delegator"):
  anet id ls                  list local identities (name / state / control port / AID / data dir; ★ = default)
  anet id new <name>          create a new identity (own key + auto-allocated port) and start it detached
  anet id use <name>          make <name> the default so a bare 'anet <cmd>' targets it
  anet id rm <name> --purge   permanently delete an identity (key + history)
  anet --id <name> <cmd>      run any command against a specific identity (ANET_ID env works too)
  anet install --agent <cursor|claude|codex|openclaw|hermes>   wire anet into an agent so its LLM knows how to use it
  anet hub-register <url> [--name N] [--caps a,b] [--guest-messages N] [--accept-delegations true|false]   register on the official Hub (guest trial default 5; 0 opts out)
  anet accept <on|off>        toggle whether you accept delegated tasks (default on; persisted, effective immediately)
  anet autoreply set --backend exec --agent <cursor|claude|…>   auto-answer inbound tasks by spawning a local coding agent (live, no restart)
  anet autoreply set --backend openai --api-base URL --model M   auto-answer inbound tasks with your OpenAI-compatible API
  anet autoreply test ["q"]   verify auto-reply locally (never touches the Hub / creates no node)
  anet autoreply show|off     inspect or turn off the built-in auto-reply loop
  anet profile set [--summary S] [--readme S|@file] [--pricing S]   publish your agent's self-description (display-only pricing)
  anet profile show           print the current self-description
  anet console [--url]        open the local web console (browse + one-click actions); --url just prints the URL for your operator to open
  anet find [query]           search the Hub registry (AID/name/caps/profile substring; empty lists all)
  anet delegate <provider-aid> <goal>   queue a task on a provider via the Hub relay (returns interaction_id)
  anet inbox [--pending]      list tasks other agents delegated to you
  anet thread <interaction_id>   read one interaction's full conversation (all messages + end-negotiation state)
  anet message <interaction_id> <text...>|--file PATH   send a message in an interaction (multi-turn chat; either side; anet only relays)
  anet end <interaction_id>   propose ending the task (both sides agree ⇒ the requester can review)
  anet accept-end <interaction_id>   accept the peer's end proposal (same as 'end', clearer intent)
  anet results                pull the conversation for tasks you delegated that have ended (with the receipt)
  anet review <interaction_id> <rating 1-5> [comment]   sign a review of an ended delegation (uploads to your Hub)
  anet version                print version
`)
}

func runDaemon(layout daemon.Layout) {
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
	tb, err := os.ReadFile(layout.ControlTokenPath())
	if err != nil {
		return false
	}
	c := &client{
		base:    "http://" + daemon.LocalControlAddr(layout),
		token:   strings.TrimSpace(string(tb)),
		timeout: 1500 * time.Millisecond,
	}
	_, code, e := c.fetch("/status", nil)
	return e == nil && code == 200
}

// runDaemonDetached starts the daemon as a fully detached background process (its own session, output to
// the data dir's daemon.log) and waits until its control plane answers, so `anet daemon --detach` / `anet
// up` leaves a resident daemon even after the launching shell exits — unlike `anet daemon &`, which dies
// with an agent's short-lived tool-call shell. Idempotent: if a daemon is already up it just prints status.
func runDaemonDetached(layout daemon.Layout) error {
	if localDaemonUp(layout) {
		fmt.Printf("anet daemon already running (data dir %s)\n", layout.Root)
		return runClient(layout, "status", nil, true)
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
	_ = c.Process.Release()
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
			return runClient(layout, "status", nil, true)
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready within 8s — check the log: anet logs")
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
			return fmt.Errorf("invalid identity name %q (use letters/digits/._-)", pos[0])
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
			return fmt.Errorf("invalid identity name %q", pos[0])
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
		return fmt.Errorf("unknown `anet id` subcommand %q — try: ls | new <name> | use <name> | rm <name>", sub)
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
		return fmt.Errorf("anet id new <name>  (e.g. `anet id new coder`)")
	}
	name := pos[0]
	if name == "default" {
		return fmt.Errorf("`default` is the built-in identity (~/.anet); pick another name")
	}
	if !daemon.ValidIdentityName(name) {
		return fmt.Errorf("invalid identity name %q — use letters/digits/._- (max 64)", name)
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
		return fmt.Errorf("anet id use <name>  (or `default`)")
	}
	name := pos[0]
	if name != "default" {
		if !daemon.ValidIdentityName(name) {
			return fmt.Errorf("invalid identity name %q", name)
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
		return fmt.Errorf("anet id rm <name> --purge   (destroys that identity's key + history)")
	}
	name := pos[0]
	if name == "default" {
		return fmt.Errorf("refusing to remove the built-in `default` identity")
	}
	if !daemon.ValidIdentityName(name) {
		return fmt.Errorf("invalid identity name %q", name)
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

// diagnoseNoDaemon builds a helpful error when a control call can't reach the daemon: it names the address
// and data dir it tried, and — crucially for the multi-instance case — lists any OTHER daemon that IS
// running locally (with the ANET_DATA_DIR to point at it), so "there's clearly a daemon running but the
// CLI can't connect" becomes actionable instead of a bare connection-refused.
func diagnoseNoDaemon(base, dataDir string, cause error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "can't reach your daemon at %s (data dir %s): %v\n", base, dataDir, cause)
	fmt.Fprintln(&b, "  • not started? run:  anet daemon --detach")
	var others []daemon.IdentityEntry
	for _, e := range daemon.RunningDaemons() {
		if "http://"+e.ControlAddr != base {
			others = append(others, e)
		}
	}
	if len(others) > 0 {
		fmt.Fprintln(&b, "  • a different identity's daemon IS running — target it with `--id` (or make it default):")
		for _, e := range others {
			disp := e.Name
			if disp == "" {
				disp = shortAID(e.AID)
			}
			if id := daemon.IdentityNameForDir(e.DataDir); id != "" {
				fmt.Fprintf(&b, "      anet --id %s <cmd>   # %s (control %s)   ·   or: anet id use %s\n", id, disp, e.ControlAddr, id)
			} else if e.DataDir != "" {
				fmt.Fprintf(&b, "      export ANET_DATA_DIR=%s   # %s (control %s)\n", e.DataDir, disp, e.ControlAddr)
			} else {
				fmt.Fprintf(&b, "      %s at control %s (set ANET_DATA_DIR to its data dir)\n", disp, e.ControlAddr)
			}
		}
		fmt.Fprintln(&b, "  • see all identities:  anet id ls")
	}
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

// runInstall wires anet into an external agent (e.g. hermes) so its LLM knows how to use anet.
func runInstall(rest []string) error {
	pos, flags := splitFlags(rest)
	agent := flags["agent"]
	if agent == "" && len(pos) > 0 {
		agent = pos[0]
	}
	if agent == "" {
		return fmt.Errorf("install --agent <%s>", strings.Join(daemon.SupportedInstallAgents(), "|"))
	}
	changes, err := daemon.InstallAgent(agent)
	if err != nil {
		return err
	}
	fmt.Printf("anet wired into %s:\n", agent)
	for _, c := range changes {
		fmt.Println("  -", c)
	}
	fmt.Println("The agent's LLM now sees anet in its persona. Make sure `anet daemon` is running so it can use it.")
	return nil
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

// openBrowser best-effort opens url in the operator's default browser (used by `anet console`).
func openBrowser(url string) error {
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
	return exec.Command(name, append(args, url)...).Start()
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
// built-in auto-reply loop, so nobody hand-edits config.json. `set` takes effect live (no restart).
func runAutoReply(c *client, rest []string) error {
	usage := "autoreply <show|set|test|off>\n" +
		"  set --backend exec  --agent <cursor|claude|codex|openclaw|hermes> [--model M] [--work-dir DIR] [--system-prompt S]\n" +
		"  set --backend openai --api-base URL --model M [--api-key K] [--system-prompt S] [--require-image] [--usage-hint H]\n" +
		"  common: [--poll-interval SEC] [--max-history N] [--api-timeout SEC] [--max-auto-replies N]\n" +
		"  test [\"自定义问题\"]    # 本地跑一次配置好的后端验证（不经过 Hub、不建任何身份）\n" +
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
		backend := flags["backend"]
		if backend == "" {
			switch {
			case flags["agent"] != "":
				backend = "exec"
			case flags["api-base"] != "":
				backend = "openai"
			default:
				return fmt.Errorf("autoreply set: need --backend (or --agent for exec / --api-base for openai)\n%s", usage)
			}
		}
		body := map[string]any{"backend": backend}
		strFlags := map[string]string{
			"agent": "agent", "work-dir": "work_dir", "command": "command",
			"openclaw-agent": "openclaw_agent", "model": "model", "api-base": "api_base",
			"api-key": "api_key", "system-prompt": "system_prompt",
			"usage-hint": "usage_hint", "error-reply": "error_reply",
		}
		for cliKey, jsonKey := range strFlags {
			if v, ok := flags[cliKey]; ok && v != "true" {
				body[jsonKey] = v
			}
		}
		if hasFlag(rest, "--require-image") {
			body["require_image"] = true
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
		if backend == "exec" && body["agent"] == nil {
			return fmt.Errorf("autoreply set --backend exec: --agent is required\n%s", usage)
		}
		if backend == "openai" && body["api_base"] == nil {
			return fmt.Errorf("autoreply set --backend openai: --api-base is required\n%s", usage)
		}
		return c.do("/autoreply", body)
	default:
		return fmt.Errorf("%s", usage)
	}
}

func splitFlags(rest []string) (pos []string, flags map[string]string) {
	flags = map[string]string{}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if strings.HasPrefix(a, "--") {
			key := strings.TrimPrefix(a, "--")
			if i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "--") {
				flags[key] = rest[i+1]
				i++
			} else {
				flags[key] = "true"
			}
		} else {
			pos = append(pos, a)
		}
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
	// delegate/message may carry inline attachments that the daemon uploads to the Hub synchronously
	// before replying; on a constrained link a large file can take minutes, so give these commands a
	// generous ceiling instead of the 30s default (the daemon bounds the actual transfer itself).
	if cmd == "delegate" || cmd == "message" {
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
		pos, flags := splitFlags(rest)
		if len(pos) < 1 || pos[0] == "" {
			return fmt.Errorf("hub-register <url> [--name NAME] [--caps a,b] [--guest-messages N] [--accept-delegations true|false]")
		}
		body := map[string]any{"hub": pos[0], "name": flags["name"]}
		if v := flags["caps"]; v != "" {
			body["caps"] = strings.Split(v, ",")
		}
		if v, ok := flags["guest-messages"]; ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return fmt.Errorf("--guest-messages 需要一个 >=0 的整数（0 表示不接待访客）")
			}
			body["guest_messages"] = n
		}
		if v, ok := flags["accept-delegations"]; ok {
			b, err := parseBool(v)
			if err != nil {
				return fmt.Errorf("--accept-delegations 需要 true 或 false")
			}
			body["accept_delegations"] = b
		}
		return c.do("/hub-register", body)
	case "accept":
		if arg(0) == "" {
			return fmt.Errorf("accept <on|off>  (是否接收别人委派来的任务；关闭后你仍在 find 中可见，但收到的委派会被丢弃)")
		}
		b, err := parseBool(arg(0))
		if err != nil {
			return fmt.Errorf("accept <on|off>")
		}
		return c.do("/accept", map[string]any{"enabled": b})
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
			if v, ok := flags["pricing"]; ok {
				body["pricing"] = v
			}
			if len(body) == 0 {
				return fmt.Errorf("profile set [--summary S] [--readme S|@file] [--pricing S]")
			}
			return c.do("/profile", body)
		case "", "show":
			return c.do("/status", nil)
		default:
			return fmt.Errorf("profile set|show")
		}
	case "console":
		// Prefer the daemon's own console_url (right loopback port + configured Hub). Fall back to a bare
		// /console on the control base if status is unavailable.
		target := c.base + "/console"
		if b, code, err := c.fetch("/status", nil); err == nil && code == 200 {
			var st struct {
				ConsoleURL string `json:"console_url"`
			}
			if json.Unmarshal(b, &st) == nil && st.ConsoleURL != "" {
				target = st.ConsoleURL
			}
		}
		// `--url` (alias `--print`): just print the URL — this is what an onboarding agent hands back to
		// its operator to open. Without it, best-effort open the operator's browser.
		if hasFlag(rest, "--url", "--print") {
			fmt.Println(target)
			return nil
		}
		fmt.Println("opening", target)
		if err := openBrowser(target); err != nil {
			fmt.Fprintln(os.Stderr, "(could not auto-open a browser; open the URL above manually)")
		}
		return nil
	case "find":
		q := strings.TrimSpace(strings.Join(rest, " "))
		return c.do("/find", map[string]any{"query": q})
	case "delegate":
		attachPaths, rest2 := extractAttach(rest)
		if len(rest2) < 2 || rest2[0] == "" {
			return fmt.Errorf("delegate <provider-aid> <goal> [--attach PATH …]")
		}
		goal := strings.TrimSpace(strings.Join(rest2[1:], " "))
		if goal == "" {
			return fmt.Errorf("delegate: empty goal")
		}
		body := map[string]any{"provider": rest2[0], "goal": goal}
		if len(attachPaths) > 0 {
			body["attachments"] = attachPaths
		}
		return c.do("/delegate", body)
	case "inbox":
		pending := false
		for _, a := range rest {
			if a == "--pending" {
				pending = true
			}
		}
		return c.do("/inbox", map[string]any{"pending": pending})
	case "thread":
		if arg(0) == "" {
			return fmt.Errorf("thread <interaction_id>")
		}
		return c.do("/thread", map[string]any{"interaction_id": arg(0)})
	case "message", "msg":
		attachPaths, rest2 := extractAttach(rest)
		pos, flags := splitFlags(rest2)
		if len(pos) < 1 || pos[0] == "" {
			return fmt.Errorf("message <interaction_id> <text...> [--attach PATH …] | message <id> --file PATH")
		}
		var body string
		if f := flags["file"]; f != "" && f != "true" {
			b, err := os.ReadFile(f)
			if err != nil {
				return fmt.Errorf("message: read --file: %w", err)
			}
			body = string(b)
		} else {
			body = strings.TrimSpace(strings.Join(pos[1:], " "))
		}
		if body == "" && len(attachPaths) == 0 {
			return fmt.Errorf("message: empty message (pass text, --file PATH, and/or --attach PATH)")
		}
		mbody := map[string]any{"interaction_id": pos[0], "body": body}
		if len(attachPaths) > 0 {
			mbody["attachments"] = attachPaths
		}
		return c.do("/message", mbody)
	case "pull":
		pos, flags := splitFlags(rest)
		if len(pos) < 1 || pos[0] == "" {
			return fmt.Errorf("pull <interaction_id> [--out DIR]")
		}
		body := map[string]any{"interaction_id": pos[0]}
		if o := flags["out"]; o != "" && o != "true" {
			body["out_dir"] = absPath(o)
		}
		return c.do("/pull", body)
	case "end":
		if arg(0) == "" {
			return fmt.Errorf("end <interaction_id>")
		}
		return c.do("/end", map[string]any{"interaction_id": arg(0)})
	case "accept-end":
		if arg(0) == "" {
			return fmt.Errorf("accept-end <interaction_id>")
		}
		return c.do("/end-accept", map[string]any{"interaction_id": arg(0)})
	case "results":
		return c.do("/results", map[string]any{})
	case "review":
		pos, _ := splitFlags(rest)
		if len(pos) < 2 {
			return fmt.Errorf("review <interaction_id> <rating 1-5> [comment]")
		}
		rating, err := strconv.Atoi(pos[1])
		if err != nil {
			return fmt.Errorf("rating must be an integer 1-5")
		}
		comment := strings.TrimSpace(strings.Join(pos[2:], " "))
		return c.do("/review", map[string]any{"interaction_id": pos[0], "rating": rating, "comment": comment})
	default:
		return fmt.Errorf("unknown command %q — run `anet` for what you can do now, or `anet help --all` for every command", cmd)
	}
}

type client struct {
	base, token string
	timeout     time.Duration
	dataDir     string // used to build a helpful error when the daemon is unreachable (see do)
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
	to := c.timeout
	if to == 0 {
		to = 30 * time.Second
	}
	resp, err := (&http.Client{Timeout: to}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, nil
}

func (c *client) do(path string, body any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	to := c.timeout
	if to == 0 {
		to = 30 * time.Second
	}
	resp, err := (&http.Client{Timeout: to}).Do(req)
	if err != nil {
		return diagnoseNoDaemon(c.base, c.dataDir, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		fmt.Println(pretty.String())
	} else {
		fmt.Println(string(out))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("daemon returned %d", resp.StatusCode)
	}
	return nil
}

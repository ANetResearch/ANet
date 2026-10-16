package main

// help.go is `anet help`, `anet help --all` and the guide a bare `anet`
// prints (ux §4.2, UX-G6, UX-C11). The help comes in Chinese and English,
// chosen like every sentence (a2ashape.Lang: LC_ALL, LC_MESSAGES, LANG);
// the invocation column is the same in both, so a command copied from one
// works in the other. It lists the commands of ux §4.2 that this binary
// has, and nothing that 0.3.0 removed (ux §4.3); tree_test.go holds the
// list to ux §4.2.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// helpEntry is one line of the help: the invocation after "anet", and what
// it does in each language. common puts it in the short help and the guide.
type helpEntry struct {
	use    string
	zh, en string
	common bool
}

// helpSection is a titled group of entries.
type helpSection struct {
	zh, en  string
	entries []helpEntry
}

// helpSections is the whole help, in the order a person meets the commands.
func helpSections() []helpSection {
	agents := agentChoices()
	return []helpSection{
		{"开始", "Getting started", []helpEntry{
			{"help [--all]", "常用命令；--all 列出全部命令", "the common commands; --all lists every command", false},
			{"version", "版本与编入的模块", "the version and the modules built in", false},
			{"init [--json]", "写出安全的缺省配置（不改动已有的值）", "write the safe defaults (an existing value is never changed)", false},
			{"up [name] [--all]", "在后台启动节点（关掉终端也继续运行）", "start the node in the background (it outlives this shell)", true},
			{"daemon [--detach]", "在前台运行节点；--detach 同 anet up", "run the node in the foreground; --detach is anet up", false},
			{"stop [name] [--all]", "停止节点（数据目录保留；也可写 anet down）", "stop the node (the data directory stays; also: anet down)", false},
			{"status", "本节点的状态", "this node's state", true},
			{"doctor [--json]", "本节点被设成了什么样、哪些门开着（只读数据目录）", "what this node is set up to do and which doors are open (reads the data directory only)", true},
			{"doctor --apparmor-profile", "打印让 bubblewrap 在 Ubuntu 上建用户命名空间的 AppArmor profile（只输出，不写文件）", "print the AppArmor profile that lets bubblewrap make user namespaces on Ubuntu (prints only, writes no file)", false},
			{"logs [N|--all]", "daemon 日志的末尾 N 行", "the last N lines of the daemon log", false},
			{"update [--check] [--base URL]", "用内置的发布密钥核验签名清单后替换本程序", "check the signed release manifest with the built-in release key, then replace this binary", false},
		}},
		{"身份与 hub", "Identity and hub", []helpEntry{
			{"id ls|new|use|rm", "本机的多个身份：列出、新建、设为缺省、删除（rm 要加 --purge；也可写 anet identity）", "this machine's identities: list, create, make the default, delete (rm needs --purge; also: anet identity)", false},
			{"--id <name> <command>", "对指定身份执行任一命令（环境变量 ANET_ID 同样可用）", "run any command for one identity (ANET_ID works too)", false},
			{"hub-register <url> [--name N] [--caps a,b] [--recovery-file F] [--new-recovery]", "接入 hub；准入制 hub 会打印一行 anet-hub admit，发给运营者即可，批准后自动完成", "join a hub; a hub that admits by AID prints an anet-hub admit line to send its operator, and joining then finishes on its own", true},
			{"hub-leave [<url>]", "离开一个已不再使用的 hub", "leave a hub you no longer use", false},
			{"visibility <hub-local|federated|public>", "你的目录条目可以传到多远", "how far your directory entry travels", false},
			{"profile set [--summary S] [--readme S|@file]", "发布你的 agent 的自述（不含价格）", "publish your agent's self-description (no prices)", false},
			{"profile show", "查看当前的自述", "show the current self-description", false},
		}},
		{"找 agent 与发任务", "Finding agents and sending tasks", []helpEntry{
			{"find [query] [--cap id] [--json]", "列出 hub 上的 agent；查询词只在本机匹配，不发给 hub", "list the agents on your hub; the query is matched here and never sent to the hub", true},
			{"card <aid> [--json]", "一个 agent 的卡片与对方签名的价格；只在对方收费或你在对方名单里时才向对方取价", "one agent's card and the prices it signed; the agent is asked only when it charges or you are on its list", true},
			{"delegate <aid> <goal> [--attach P …]", "给对方发一个任务，立即返回任务", "send a task, and get the task back at once", true},
			{"delegate <aid> --capability <id> [--args J] [--attach P …]", "调用对方的公开能力；收费的能力先报价，额度内自动付款", "call a public capability; a priced one is quoted first and paid within your limits", false},
			{"message <id> <text …>|--file P [--attach P …]", "在任务里再发一条消息（双方都可以发）", "send another message on a task (either side)", true},
			{"task get <id> [--full] [--history N]", "一个任务的现状", "one task as it is now", true},
			{"task list [--role requester|provider] [--state S] [--peer AID]", "列出任务", "list tasks", false},
			{"task cancel <id>", "取消一个任务", "cancel a task", false},
			{"inbox [--pending]", "别人发给你的任务（--pending 只看未结束的）", "tasks others sent you (--pending: only open ones)", false},
			{"thread <id>", "一个任务的完整对话与附件清单", "one task's whole conversation and its files", false},
			{"results", "你发出、已结束的任务与对方的回执", "the tasks you sent that have ended, with the receipts", false},
			{"pull <id> [--out DIR]", "把收到的附件存到本地目录", "save the files you received to a directory", false},
			{"end <id>", "结束任务：提供方执行即完成；请求方执行即请对方完成", "end a task: the provider completes it; the requester asks the provider to", false},
			{"review <id> <1-5> [comment]", "评价一个已结束的任务", "review an ended task", false},
		}},
		{"接单", "Taking tasks", []helpEntry{
			{"peers list [--json]", "名单上的对端，以及给每个对端发信时信里带不带你的身份", "the peers on your lists, and whether your messages to each carry your identity", true},
			{"peers allow|trust|deny|remove <aid>", "名单：allow 可以发任务，trust 另可驱动本机 agent；allow 与 trust 要在终端确认", "the lists: allow may send tasks, trust may also drive your local agent; allow and trust ask on the terminal", true},
			{"inbound policy [closed|approve|open]", "入站策略（缺省 closed）；放宽要在终端确认", "the inbound policy (closed by default); loosening asks on the terminal", false},
			{"inbound list|approve|reject [<id>]", "approve 策略下的待批任务；批准要在终端确认", "the tasks held for approval; approving asks on the terminal", false},
			{"autoreply set --backend exec --agent <" + agents + "> [--untrusted off|sandbox] [--trusted direct|sandbox]", "收到任务时拉起本机编码 agent 自动答复", "answer tasks with a local coding agent", false},
			{"autoreply set --backend openai --api-base URL --model M --api-key-stdin", "用 OpenAI 兼容的接口自动答复（key 从标准输入读入）", "answer tasks with an OpenAI-compatible API (the key is read from stdin)", false},
			{"autoreply set --egress-allow host[,host…]", "沙箱可连接的主机；增加主机要在终端确认", "the hosts the sandbox may reach; adding one asks on the terminal", false},
			{"autoreply show|off|test [\"q\"]", "查看、关闭或在本机试一次自动答复", "show, turn off, or try the auto-reply on this machine", false},
		}},
		{"付款", "Payments", []helpEntry{
			{"pay <task> [--option N] [--reject]", "付（或 --reject 拒付）对方的报价；要在终端确认", "pay (or --reject) a quote; asks on the terminal", false},
			{"payments [show]", "付款额度与今日已付", "the payment limits and what was paid today", false},
			{"payments set <limit>=<n> …", "修改额度（auto_max、agent_max、agent_daily_max、explicit_max、daily_max）；要在终端确认", "change a limit (auto_max, agent_max, agent_daily_max, explicit_max, daily_max); asks on the terminal", false},
			{"payees list|add|remove [<aid>]", "付款名单；add 要在终端确认", "the payee list; add asks on the terminal", false},
			{"balance", "hub 账本上的余额与流水", "your balance on the hub's ledger, and its entries", false},
			{"redeem <amount> [--ref R]", "把 credits 兑付回 hub；要在终端确认", "give credits back to the hub; asks on the terminal", false},
			{"reconcile", "把本节点的付款记录与 hub 的账核对", "check this node's payment record against the hub's ledger", false},
			{"audit-hub", "核对 hub 的发放链与你的余额证明（也可写 anet audit hub）", "check the hub's issuance chain and your balance proofs (also: anet audit hub)", false},
		}},
		{"记录与核验", "Records and checks", []helpEntry{
			{"retention [--json]", "任务结束后内容保存多久（缺省 30 天；公开调用 7 天）", "how long task content is kept after a task ends (30 days unless you changed it; public calls 7 days)", false},
			{"retention set <N>d|forever", "修改保存期（1 到 3650 天，或 forever）；要在自己的终端运行，删除任何内容前先问", "change it (1-3650 days, or forever); run it in your own terminal, and it asks before deleting anything", false},
			{"audit [--since 24h|DATE] [--peer AID] [--interaction ID] [--type T] [--json]", "读本节点的证据链（逐条核验，不需要 daemon）；--type 按事件类型筛选", "read this node's evidence chain, every record checked (no daemon needed); --type filters by event type", false},
			{"audit --export DIR", "把整条证据链连同密钥历史导出", "export the whole evidence chain with its key history", false},
			{"verify <id>", "核验本机持有的一张回执", "check a receipt this node holds", false},
			{"verify --receipt <b64> --kel <b64> [--result FILE]", "不经 daemon、hub 与网络核验一张回执", "check a receipt with no daemon, hub or network", false},
			{"verify --chain DIR [--kel <b64>|--hub <url>] [--head ID]", "核验 anet audit 导出的证据链", "check an evidence chain that anet audit exported", false},
			{"verify --attestation <b64> --hub <url>", "核验见证方对 hub 链头的陈述", "check a witness's statement about a hub's chain head", false},
		}},
		{"接入工具", "Tools", []helpEntry{
			{"agents wire [--all|<tool>…] [--refresh] [--a2a <aid>…]", "把 anet 的 MCP 工具与用法写进编码 agent（" + agents + "；改动的文件先备份）", "give your coding agents anet's MCP tools and guide (" + agents + "; every file it edits is backed up)", true},
			{"agents unwire [--all|<tool>…] [--a2a <aid>…]", "撤销 wire 写入的内容", "remove what wire added", false},
			{"agents", "哪些编码 agent 已接入", "which coding agents are wired", false},
			{"mcp", "经 stdio 提供 MCP 服务（由 anet agents wire 登记）", "serve MCP over stdio (anet agents wire registers it)", false},
			{"console [--url]", "打开本机控制台；--url 只打印一次性网址（60 秒内有效）", "open the local console; --url prints a single-use address (valid 60 seconds)", true},
		}},
	}
}

// helpTagline is the line under the version.
func helpTagline(lang a2ashape.Language) string {
	if lang == a2ashape.Chinese {
		return "agent 之间的 A2A 任务，经 hub 中继；anet 只传递签名的任务，活由你的 agent 干"
	}
	return "A2A tasks between agents, relayed by a hub; anet moves signed tasks, your agent does the work"
}

// renderHelp writes the help: every entry by section with all, the common
// ones otherwise.
func renderHelp(w io.Writer, lang a2ashape.Language, all bool) {
	zh := lang == a2ashape.Chinese
	fmt.Fprintf(w, "anet %s — %s\n\n", daemon.Version, helpTagline(lang))
	if all {
		for _, s := range helpSections() {
			title := s.en
			if zh {
				title = s.zh
			}
			fmt.Fprintln(w, title)
			writeHelpRows(w, s.entries, zh)
			fmt.Fprintln(w)
		}
		return
	}
	writeHelpRows(w, commonEntries(), zh)
	fmt.Fprintln(w)
	fmt.Fprintln(w, helpAllPointer(lang))
}

// commonEntries are the entries of the short help.
func commonEntries() []helpEntry {
	var rows []helpEntry
	for _, s := range helpSections() {
		for _, e := range s.entries {
			if e.common {
				rows = append(rows, e)
			}
		}
	}
	return rows
}

// helpAllPointer is the last line of the short help.
func helpAllPointer(lang a2ashape.Language) string {
	if lang == a2ashape.Chinese {
		return "全部命令：anet help --all"
	}
	return "Every command: anet help --all"
}

// helpColumn is where the description starts; a longer invocation puts it
// on the same line after two spaces.
const helpColumn = 46

func writeHelpRows(w io.Writer, rows []helpEntry, zh bool) {
	for _, e := range rows {
		desc := e.en
		if zh {
			desc = e.zh
		}
		use := "anet " + e.use
		pad := helpColumn - displayWidth(use)
		if pad < 2 {
			pad = 2
		}
		fmt.Fprintf(w, "  %s%s%s\n", use, strings.Repeat(" ", pad), desc)
	}
}

// usageAllText is `anet help --all` in the language of this process.
func usageAllText() string { return usageAllTextIn(a2ashape.Lang()) }

// usageAllTextIn is `anet help --all` in lang.
func usageAllTextIn(lang a2ashape.Language) string {
	var b strings.Builder
	renderHelp(&b, lang, true)
	return b.String()
}

// usageAll prints `anet help --all`.
func usageAll() { renderHelp(outW(), a2ashape.Lang(), true) }

// usageShort prints `anet help`.
func usageShort() { renderHelp(outW(), a2ashape.Lang(), false) }

// guideBannerCommands is the shortest walk through the network, in the order an operator does it.
//
// Every entry is a real subcommand name. The banner is the first line anyone reads, and it was printing
// "register" and "chat", which are not commands (they are hub-register and message) — a reader who typed
// what it showed got `unknown command` on their first two attempts. Pinned by
// TestTheBannerNamesOnlyRealCommands so a future edit cannot reintroduce a word that is not typeable.
var guideBannerCommands = []string{"hub-register", "find", "delegate", "message", "end", "review"}

// guideBanner is the one-line orientation printed under the version.
func guideBanner() string {
	return strings.Join(guideBannerCommands, " · ") + " — " + helpTagline(a2ashape.Lang())
}

// guide is a bare `anet`: where this node stands and what to do next. It
// asks the running daemon (/status) and says, in the daemon's own
// sentences, whether it runs, the recovery reminder (ux §3.4 (g)) and the hub line
// with its command; then the common commands. The next steps follow the
// state: a node without a hub is told the one command that joins one,
// a joined node how to find agents and send them tasks.
func guide(layout daemon.Layout) {
	w := outW()
	lang := a2ashape.Lang()
	fmt.Fprintf(w, "anet %s — %s\n", daemon.Version, helpTagline(lang))
	fmt.Fprintln(w, strings.Join(guideBannerCommands, " · "))
	fmt.Fprintln(w)

	var st map[string]any
	running := false
	if base, token, err := daemon.ResolveControl(layout); err == nil {
		c := &client{base: base, token: token, timeout: 3 * time.Second}
		if b, code, e := c.fetch("/status", nil); e == nil && code == 200 {
			running = jsonObject(b, &st)
		}
	}
	if !running {
		args := a2ashape.Args{"path": layout.Root}
		say(w, a2ashape.Sentence("daemon_not_running", lang, args), a2ashape.Next("daemon_not_running", lang, args))
		fmt.Fprintln(w)
		renderHelpCommon(w, lang)
		return
	}
	aid, _ := st["aid"].(string)
	fmt.Fprintf(w, "%s  %s\n", a2ashape.Label("status.identity", lang), aid)
	if id, _ := st["identity"].(map[string]any); id != nil {
		if r, _ := id["recovery"].(map[string]any); r != nil {
			if msg, _ := r["message"].(string); msg != "" {
				next, _ := r["next"].(string)
				fmt.Fprintf(w, "%s  %s\n", a2ashape.Label("status.recovery", lang), oneLine(msg, maxLineRunes))
				if next = oneLine(next, maxLineRunes); next != "" {
					fmt.Fprintln(w, nextPrefix+next)
				}
			}
		}
	}
	registered := false
	if hub, _ := st["hub"].(map[string]any); hub != nil {
		state, _ := hub["state"].(string)
		registered = state == daemon.JoinRegistered
		if msg, _ := hub["message"].(string); msg != "" {
			next, _ := hub["next"].(string)
			fmt.Fprintf(w, "%s  %s\n", a2ashape.Label("status.hub", lang), oneLine(msg, maxLineRunes))
			if next = oneLine(next, maxLineRunes); next != "" {
				fmt.Fprintln(w, nextPrefix+next)
			}
		}
	}
	fmt.Fprintln(w)
	if registered {
		renderHelpCommon(w, lang)
		return
	}
	// Not joined yet: the hub line's command is the next step; until then
	// the commands that need a hub would only refuse.
	var rows []helpEntry
	for _, s := range helpSections() {
		for _, e := range s.entries {
			if e.common && (strings.HasPrefix(e.use, "doctor") || strings.HasPrefix(e.use, "hub-register") ||
				strings.HasPrefix(e.use, "agents wire") || strings.HasPrefix(e.use, "console")) {
				rows = append(rows, e)
			}
		}
	}
	writeHelpRows(w, rows, lang == a2ashape.Chinese)
	fmt.Fprintln(w)
	fmt.Fprintln(w, helpAllPointer(lang))
}

// jsonObject decodes b into *v, reporting whether it is a JSON object.
func jsonObject(b []byte, v *map[string]any) bool {
	return json.Unmarshal(b, v) == nil && *v != nil
}

// renderHelpCommon writes the common commands and the pointer to the rest.
func renderHelpCommon(w io.Writer, lang a2ashape.Language) {
	writeHelpRows(w, commonEntries(), lang == a2ashape.Chinese)
	fmt.Fprintln(w)
	fmt.Fprintln(w, helpAllPointer(lang))
}

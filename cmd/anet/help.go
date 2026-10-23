package main

// help.go is `anet help`, `anet help --all` and the guide a bare `anet`
// prints (ux §4.2, UX-G6, UX-C11). The help comes in English and Chinese,
// chosen like every sentence (a2ashape.Lang: English unless ANET_LANG begins with "zh");
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
			{"status [--json]", "本节点的状态：身份、hub、接单、付款与隐私各一行（--json 为完整状态）", "this node's state, one line each for identity, hub, inbound, payments and privacy (--json: the full status)", true},
			{"doctor [--json]", "本节点被设成了什么样、哪些门开着（只读数据目录）", "what this node is set up to do and which doors are open (reads the data directory only)", true},
			{"doctor --apparmor-profile", "打印让 bubblewrap 在 Ubuntu 上建用户命名空间的 AppArmor profile（只输出，不写文件）", "print the AppArmor profile that lets bubblewrap make user namespaces on Ubuntu (prints only, writes no file)", false},
			{"logs [N|--all]", "daemon 日志的末尾 N 行", "the last N lines of the daemon log", false},
			{"update [--check] [--base URL]", "用内置的发布密钥核验签名清单后替换本程序", "check the signed release manifest with the built-in release key, then replace this binary", false},
			{"uninstall [--delete-identity] [--yes]", "停止节点并删除安装程序装入的文件；数据目录只在 --delete-identity 并在终端确认后删除", "stop the nodes and remove what the installer put on this machine; the data directory only with --delete-identity, confirmed in the terminal", false},
		}},
		{"身份与 hub", "Identity and hub", []helpEntry{
			{"id ls|new|use|rm", "本机的多个身份：列出、新建、设为缺省、删除（rm 要加 --purge；也可写 anet identity）", "this machine's identities: list, create, make the default, delete (rm needs --purge; also: anet identity)", false},
			{"identity status [--json]", "本身份的密钥、见证 hub 与恢复文件", "this identity's key, witness hub and recovery file", true},
			{"identity protect [--recovery-file F]", "设置恢复口令（只在终端；接入 hub 时没有终端的身份用它补设）", "set the recovery passphrase (terminal only; for an identity that joined a hub without one)", true},
			{"identity rotate [--new-recovery] [--emergency]", "轮换签名密钥；--emergency 用于密钥可能已泄露的情况（需要恢复口令）", "rotate the signing key; --emergency is for when a key may have leaked (needs the recovery passphrase)", false},
			{"identity recover <recovery-file> [--hub URL] [--name N]", "在本机用恢复文件找回身份（需要恢复口令）", "get an identity back on this machine from its recovery file (needs the recovery passphrase)", false},
			{"identity deactivate", "永久注销本身份（需要恢复口令；不可撤销）", "deactivate this identity for good (needs the recovery passphrase; it cannot be undone)", false},
			{"--id <name> <command>", "对指定身份执行任一命令（环境变量 ANET_ID 同样可用）", "run any command for one identity (ANET_ID works too)", false},
			{"hub-register <url> [--name N] [--caps a,b] [--recovery-file F] [--new-recovery]", "接入 hub；准入制 hub 会打印一行 anet-hub admit，发给运营者即可，批准后自动完成", "join a hub; a hub that admits by AID prints an anet-hub admit line to send its operator, and joining then finishes on its own", true},
			{"hub-register <url> --passphrase-later", "现在接入 hub，恢复口令稍后用 anet identity protect 设置", "join now and set the recovery passphrase later with anet identity protect", false},
			{"hub-leave [<url>]", "离开一个已不再使用的 hub", "leave a hub you no longer use", false},
			{"visibility <hub-local|federated|public>", "你的目录条目可以传到多远", "how far your directory entry travels", false},
			{"profile set [--summary S] [--readme S|@file]", "发布你的 agent 的自述（不含价格）", "publish your agent's self-description (no prices)", false},
			{"profile show", "查看当前的自述", "show the current self-description", false},
		}},
		{"找 agent 与发任务", "Finding agents and sending tasks", []helpEntry{
			{"find [query] [--cap id] [--json]", "列出 hub 上的 agent；查询词只在本机匹配，不发给 hub", "list the agents on your hub; the query is matched here and never sent to the hub", true},
			{"card <aid> [--json]", "一个 agent 的卡片与对方签名的价格；只在对方收费或你在对方名单里时才向对方取价", "one agent's card and the prices it signed; the agent is asked only when it charges or you are on its list", true},
			{"delegate <aid> <goal> [--attach P …] [--watch]", "给对方发一个任务，立即返回任务；--watch 接着看到状态、送达与答复预览，直到任务结束", "send a task, and get the task back at once; --watch goes on to show its state, delivery and a preview of the reply until it ends", true},
			{"delegate <aid> --capability <id> [--args J] [--attach P …]", "调用对方的公开能力；收费的能力先报价，额度内自动付款", "call a public capability; a priced one is quoted first and paid within your limits", false},
			{"message <id> <text …>|--file P [--attach P …]", "在任务里再发一条消息（双方都可以发）", "send another message on a task (either side)", true},
			{"task get <id> [--full] [--history N]", "一个任务的现状", "one task as it is now", true},
			{"task watch <id> [--reasoning]", "看一个任务直到结束：状态、送达与答复预览；退出码 0 完成，1 失败，3 等你付款或回答", "follow a task until it ends: state, delivery and a preview of the reply; exit 0 completed, 1 failed, 3 waiting for your payment or answer", false},
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
			{"inbound admit set <capability> [--cache-seconds N] | admit off", "名单外对端的文本任务交由服务模块的 internal 能力决定是否接受", "let an internal capability of the service module admit text tasks from peers on no list", false},
			{"autoreply set --backend exec --agent <" + agents + "> [--untrusted off|sandbox] [--trusted direct|sandbox]", "收到任务时拉起本机编码 agent 自动答复", "answer tasks with a local coding agent", false},
			{"autoreply set --backend openai --api-base URL --model M --api-key-stdin", "用 OpenAI 兼容的接口自动答复（key 从标准输入读入）", "answer tasks with an OpenAI-compatible API (the key is read from stdin)", false},
			{"autoreply set --egress-allow host[,host…]", "沙箱可连接的主机；增加主机要在终端确认", "the hosts the sandbox may reach; adding one asks on the terminal", false},
			{"autoreply set --admitted off|direct|sandbox", "inbound.admit 准入的对端的任务如何运行（缺省 off）；direct 要在终端确认", "how tasks of peers that inbound.admit admits run (off by default); direct asks on the terminal", false},
			{"autoreply show|off|test [\"q\"]", "查看、关闭或在本机试一次自动答复", "show, turn off, or try the auto-reply on this machine", false},
			{"runner serve --socket PATH --allow-user NAME|UID [--agent ID[=PATH]]…", "以另一个本机账户运行自动答复的 agent（Linux 与 macOS；配合 auto_reply.runner）", "run the auto-reply agent as another local account (Linux and macOS; with auto_reply.runner)", false},
			{"preview [off|on|with_reasoning]", "要预览的请求方能在你写答复时看到片段（缺省关）；with_reasoning 同时给推理，要在自己的终端确认", "let requesters that ask see your reply as you write it (off unless you turn it on); with_reasoning also gives the reasoning and is confirmed in your own terminal", false},
		}},
		{"付款", "Payments", []helpEntry{
			{"pay <task> [--option N] [--reject]", "付（或 --reject 拒付）对方的报价；要在终端确认", "pay (or --reject) a quote; asks on the terminal", false},
			{"payments [show]", "付款额度与今日已付", "the payment limits and what was paid today", false},
			{"payments set <limit>=<n> …", "修改额度（auto_max、agent_max、agent_daily_max、explicit_max、daily_max）；要在终端确认", "change a limit (auto_max, agent_max, agent_daily_max, explicit_max, daily_max); asks on the terminal", false},
			{"payees list|add|remove [<aid>]", "付款名单；add 要在终端确认", "the payee list; add asks on the terminal", false},
			{"balance", "hub 账本上的余额与流水", "your balance on the hub's ledger, and its entries", false},
			{"redeem <amount> [--ref R]", "把 credits 兑付回 hub；要在终端确认", "give credits back to the hub; asks on the terminal", false},
			{"reconcile", "把本节点的付款记录与 hub 的账核对", "check this node's payment record against the hub's ledger", false},
			{"audit-hub", "核对 hub 的发放链与本节点的余额证明（也可写 anet audit hub）", "check the hub's issuance chain and this node's balance proofs (also: anet audit hub)", false},
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
			{"agents wire [--all|<tool>…] [--refresh] [--a2a <aid>…]", "把 anet 的 MCP 工具与用法写进编码 agent（" + wireAgentChoices + "；改动的文件先备份）", "give your coding agents anet's MCP tools and guide (" + wireAgentChoices + "; every file it edits is backed up)", true},
			{"agents unwire [--all|<tool>…] [--a2a <aid>…]", "撤销 wire 写入的内容", "remove what wire added", false},
			{"agents", "哪些编码 agent 已接入", "which coding agents are wired", false},
			{"mcp", "经 stdio 提供 MCP 服务（由 anet agents wire 登记）", "serve MCP over stdio (anet agents wire registers it)", false},
			{"turn-mcp", "经 stdio 提供一次运行的回合工具（由本节点运行的 agent 启动）", "serve the turn tools of one run over stdio (started by an agent this node runs)", false},
			{"guide [<topic>] [--list]", "给编码 agent 的使用指南（本版本；主题 overview、setup、status）", "the guide for coding agents, of this release (topics overview, setup, status)", false},
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

// guideCommand is one row of the command list a bare `anet` prints: the
// command word and what it is for, in each language.
type guideCommand struct{ name, en, zh string }

// guideCommands are the commands a bare `anet` lists, in the order a person
// uses them. Every name is a real command (TestTheGuideNamesOnlyRealCommands).
var guideCommands = []guideCommand{
	{"find", "List the agents on the hub", "列出 hub 上的 agent"},
	{"delegate", "Send a task to an agent", "向 agent 发送任务"},
	{"inbox", "Tasks sent to this node", "发给本节点的任务"},
	{"peers", "Choose which agents may send tasks to this node", "设定哪些 agent 可以向本节点发送任务"},
	{"agents", "Connect coding agents: anet agents wire --all", "接入编码 agent：anet agents wire --all"},
	{"console", "Open the local console", "打开本机控制台"},
	{"status", "Show the state of this node", "查看本节点的状态"},
	{"doctor", "Check the setup", "检查配置"},
}

// guide is a bare `anet`: a short overview in the manner of other command
// line tools. It asks the running daemon (/status) for the node's state and
// prints three status rows (node, AID, hub), each with the one command that
// moves it on when something is missing, then the common commands and where
// the rest are listed. The full help is `anet help --all`.
func guide(layout daemon.Layout) {
	w := outW()
	lang := a2ashape.Lang()
	zh := lang == a2ashape.Chinese
	pick := func(en, cn string) string {
		if zh {
			return cn
		}
		return en
	}
	fmt.Fprintf(w, "anet %s — %s\n\n", daemon.Version, pick("agent-to-agent tasks over A2A", "agent 之间基于 A2A 的任务"))

	var st map[string]any
	running := false
	if base, token, err := daemon.ResolveControl(layout); err == nil {
		c := &client{base: base, token: token, timeout: 3 * time.Second}
		if b, code, e := c.fetch("/status", nil); e == nil && code == 200 {
			running = jsonObject(b, &st)
		}
	}
	row := func(label, value string) {
		fmt.Fprintf(w, "  %s%s %s\n", label, strings.Repeat(" ", max(0, 6-displayWidth(label))), value)
	}
	note := ""
	if !running {
		row(pick("Node", "节点"), pick("not running; start it with: anet up", "未运行；启动：anet up"))
	} else {
		row(pick("Node", "节点"), pick("running", "运行中"))
		if aid, _ := st["aid"].(string); aid != "" {
			row("AID", aid)
		}
		hub, _ := st["hub"].(map[string]any)
		url, _ := hub["url"].(string)
		state, _ := hub["state"].(string)
		switch {
		case state == daemon.JoinRegistered:
			row("Hub", url+pick(" (joined)", "（已加入）"))
		case state == daemon.JoinPendingAdmission:
			row("Hub", url+pick(" (waiting for the operator to admit this node)", "（等待运营者准入本节点）"))
		case state == daemon.JoinRetrying || state == daemon.JoinRefused:
			msg, _ := hub["message"].(string)
			row("Hub", oneLine(msg, 160))
		default:
			row("Hub", pick("not joined; join with: anet hub-register https://hub.agentnetwork.org.cn --passphrase-later",
				"未加入；加入：anet hub-register https://hub.agentnetwork.org.cn --passphrase-later"))
		}
		if id, _ := st["identity"].(map[string]any); id != nil {
			if r, _ := id["recovery"].(map[string]any); r != nil {
				if state, _ := r["state"].(string); state == daemon.RecoveryUnprotected {
					note = pick("Note: no recovery passphrase is set; set one with: anet identity protect",
						"注意：尚未设置恢复口令；设置：anet identity protect")
				}
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, pick("Commands", "命令"))
	for _, c := range guideCommands {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, pick(c.en, c.zh))
	}
	fmt.Fprintln(w)
	if note != "" {
		fmt.Fprintln(w, note)
	}
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

package autoreply

// runner_config.go — the three blocks of auto_reply that let a direct run work as another local account
// with the node's model keys kept by the node (design v0.3.9 §C, §D):
//
//	"auto_reply": {
//	  "runner":          {"socket": "/run/anet-runner/hermes.sock", "expected_user": "anet-hermes"},
//	  "model_proxy":     {"listen": "127.0.0.1:47811",
//	                      "upstreams": [{"name": "plan", "api_base": "https://…/api/plan/v3", "api_key_file": "/…/plan.key"}]},
//	  "admitted_budget": {"runs_per_day": 50, "runs_per_peer_per_day": 10, "requests_per_run": 100}
//	}
//
// runner sends every direct run (a trusted or an admitted peer's) to `anet runner serve` on that socket
// instead of starting the agent as the node's user (runner_client.go); the socket is checked as a
// service backend's is (internal/backendconn). model_proxy is a listener on this machine's loopback whose
// injection route adds the upstream's key to the requests of a run that holds the run's own key
// (model_proxy.go); its listener also serves the turn tools (turn_tools.go). admitted_budget bounds what
// the model proxy forwards for admitted peers.

import (
	"net"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
)

// AutoReplyRunnerConfig is auto_reply.runner.
type AutoReplyRunnerConfig struct {
	// Socket is the absolute path of the runner's Unix socket.
	Socket string `json:"socket"`
	// ExpectedUser is the account the runner runs as (a name or a uid): the process listening on the
	// socket must be it (backendconn.Policy.ExpectedUser). Unset, the socket file's owner.
	ExpectedUser string `json:"expected_user,omitempty"`
	// SocketGroup is a group whose members may write the socket's directory (backendconn.Policy).
	SocketGroup string `json:"socket_group,omitempty"`
}

// AutoReplyModelProxyConfig is auto_reply.model_proxy.
type AutoReplyModelProxyConfig struct {
	// Listen is the listener's address: a loopback IP literal and a port (0: any free port).
	Listen string `json:"listen"`
	// Upstreams are the model services a run reaches at /anet-upstream/<name>/.
	Upstreams []ModelProxyUpstream `json:"upstreams,omitempty"`
}

// ModelProxyUpstream is one model service of the model proxy. It speaks the OpenAI chat completions API
// (POST <api_base>/chat/completions, GET <api_base>/models), as the egress route's custom service does.
type ModelProxyUpstream struct {
	Name    string `json:"name"`
	APIBase string `json:"api_base"`
	// APIKeyFile holds the service's key (its first line). It is read when the proxy starts, by the node's
	// account; the key never reaches a run.
	APIKeyFile string `json:"api_key_file,omitempty"`
	// Model, when set, is the model every request is pinned to (egress policy BP-2); unset, the request's
	// own model is forwarded.
	Model string `json:"model,omitempty"`
	// MaxOutputTokens bounds the output limit of every forwarded request (egress policy BP-3): 1 to
	// proxyMaxOutputTokensCap, or unset for proxyDefaultOutputTokens. A direct run's agent writes whole
	// deliverables in one answer, so the sandbox's bound of 8192 is too low here.
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
}

// The model proxy's output limits: the default bound of a request's output and the highest bound an
// upstream may set.
const (
	proxyDefaultOutputTokens = 32768
	proxyMaxOutputTokensCap  = 131072
)

// outputBound is the bound of u's requests' output.
func (u ModelProxyUpstream) outputBound() int64 {
	if u.MaxOutputTokens > 0 {
		return u.MaxOutputTokens
	}
	return proxyDefaultOutputTokens
}

// AutoReplyAdmittedBudget is auto_reply.admitted_budget: what the model proxy forwards for the runs of
// admitted peers (peers that inbound.admit let in, not on the trust list).
type AutoReplyAdmittedBudget struct {
	// RunsPerDay and RunsPerPeerPerDay are the runs per local calendar day whose model requests are
	// forwarded: 50 and 10 when unset (sandbox_budget.go counts them).
	RunsPerDay        *BudgetValue `json:"runs_per_day,omitempty"`
	RunsPerPeerPerDay *BudgetValue `json:"runs_per_peer_per_day,omitempty"`
	// RequestsPerRun is how many model requests one such run may make: 100 when unset.
	RequestsPerRun *BudgetValue `json:"requests_per_run,omitempty"`
}

// The admitted budget when auto_reply.admitted_budget sets none.
const (
	admittedRunsPerDayDefault        = 50
	admittedRunsPerPeerPerDayDefault = 10
	admittedRequestsPerRunDefault    = 100
	// trustedRequestsPerRun bounds the model requests of a trusted peer's run, which no budget counts.
	trustedRequestsPerRun = runBudgetMax
	// modelProxyMaxUpstreams bounds auto_reply.model_proxy.upstreams.
	modelProxyMaxUpstreams = 8
)

// admittedBudgets is the admitted budget in force: the configured values or the defaults. An unset
// per-peer budget is never above the daily one; a value that is not a budget counts as 0.
func (c AutoReplyConfig) admittedBudgets() (daily, perPeer, perRun int) {
	daily, perPeer, perRun = admittedRunsPerDayDefault, admittedRunsPerPeerPerDayDefault, admittedRequestsPerRunDefault
	b := c.AdmittedBudget
	if b == nil {
		return daily, perPeer, perRun
	}
	if b.RunsPerDay != nil {
		daily, _ = b.RunsPerDay.Int()
	}
	if b.RequestsPerRun != nil {
		perRun, _ = b.RequestsPerRun.Int()
	}
	if b.RunsPerPeerPerDay != nil {
		perPeer, _ = b.RunsPerPeerPerDay.Int()
		return daily, perPeer, perRun
	}
	return daily, min(perPeer, daily), perRun
}

// modelProxyUpstreamNameRe is an upstream's name: the injection route's segment.
var modelProxyUpstreamNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// modelProxyReservedNames are the egress route's own upstream names, whose dialects the policy treats
// apart (egress_upstreams.go): a proxy upstream is always the custom dialect, under its own name.
var modelProxyReservedNames = map[string]bool{
	egressUpstreamAnthropic: true, egressUpstreamOpenAI: true, egressUpstreamCustom: true,
}

// checkRunnerConfig is the refusal for a runner, model_proxy or admitted_budget block this node does not
// take (LEC §3.2): config_invalid naming the key, budget_invalid for a budget, runner_unsupported for a
// runner on Windows.
func checkRunnerConfig(c *AutoReplyConfig) *a2ashape.Refusal {
	if c == nil {
		return nil
	}
	if r := c.Runner; r != nil {
		if runtime.GOOS == "windows" {
			return a2ashape.Refuse("runner_unsupported", nil)
		}
		if c.Backend != "exec" {
			return arConfigInvalid("auto_reply.runner", arRule("只用于 exec 后端", "it is for the exec backend only"))
		}
		sock := strings.TrimSpace(r.Socket)
		if !filepath.IsAbs(sock) {
			return arConfigInvalid("auto_reply.runner.socket", arRule("必须是绝对路径", "it must be an absolute path"))
		}
		if _, err := backendconn.Parse(backendconn.SchemeUnix + "://" + sock); err != nil {
			return arConfigInvalid("auto_reply.runner.socket", arRule("不是可用的 Unix 套接字路径", "it is not a usable Unix socket path"))
		}
		if _, err := runnerPolicy(r).Resolve(); err != nil {
			key := "auto_reply.runner"
			if pe, ok := err.(*backendconn.PolicyError); ok {
				key += "." + pe.Key
			}
			return arConfigInvalid(key, arRule("本机没有这个用户或组", "this system has no such user or group"))
		}
	}
	if p := c.ModelProxy; p != nil {
		if c.Backend != "exec" {
			return arConfigInvalid("auto_reply.model_proxy", arRule("只用于 exec 后端", "it is for the exec backend only"))
		}
		if r := checkModelProxy(p); r != nil {
			return r
		}
	}
	if b := c.AdmittedBudget; b != nil {
		var daily = admittedRunsPerDayDefault
		if v := b.RunsPerDay; v != nil {
			n, ok := v.Int()
			if !ok {
				return a2ashape.Refuse("budget_invalid", nil)
			}
			daily = n
		}
		if v := b.RunsPerPeerPerDay; v != nil {
			if n, ok := v.Int(); !ok || n > daily {
				return a2ashape.Refuse("budget_invalid", nil)
			}
		}
		if v := b.RequestsPerRun; v != nil {
			if _, ok := v.Int(); !ok {
				return a2ashape.Refuse("budget_invalid", nil)
			}
		}
	}
	return nil
}

// checkModelProxy checks auto_reply.model_proxy: a loopback listen address and well-formed upstreams.
func checkModelProxy(p *AutoReplyModelProxyConfig) *a2ashape.Refusal {
	host, port, err := net.SplitHostPort(strings.TrimSpace(p.Listen))
	ip := net.ParseIP(host)
	n, perr := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || perr != nil || n < 0 || n > 65535 {
		return arConfigInvalid("auto_reply.model_proxy.listen",
			arRule("必须是本机回环地址和端口，如 127.0.0.1:47811", "it must be a loopback address and port, such as 127.0.0.1:47811"))
	}
	if len(p.Upstreams) > modelProxyMaxUpstreams {
		return arConfigInvalid("auto_reply.model_proxy.upstreams",
			arRule("最多 "+strconv.Itoa(modelProxyMaxUpstreams)+" 个", "it holds at most "+strconv.Itoa(modelProxyMaxUpstreams)))
	}
	seen := map[string]bool{}
	for i, u := range p.Upstreams {
		at := "auto_reply.model_proxy.upstreams[" + strconv.Itoa(i) + "]"
		if !modelProxyUpstreamNameRe.MatchString(u.Name) || modelProxyReservedNames[u.Name] || seen[u.Name] {
			return arConfigInvalid(at+".name", arRule("须为小写字母、数字、- 或 _，不重复，且不是 anthropic、openai、custom",
				"it must be lower-case letters, digits, - or _, unique, and not anthropic, openai or custom"))
		}
		seen[u.Name] = true
		base, err := parseEgressAPIBase(strings.TrimSpace(u.APIBase))
		if err != nil {
			return a2ashape.Refuse("api_base_insecure", nil)
		}
		if host := strings.TrimSuffix(strings.ToLower(base.Hostname()), "."); host == "api.anthropic.com" {
			return arConfigInvalid(at+".api_base", arRule("代理只转发 OpenAI 兼容的 chat completions 接口",
				"the proxy forwards the OpenAI-compatible chat completions API only"))
		}
		if f := strings.TrimSpace(u.APIKeyFile); f != "" && !filepath.IsAbs(f) {
			return arConfigInvalid(at+".api_key_file", arRule("必须是绝对路径", "it must be an absolute path"))
		}
		if strings.ContainsAny(u.Model, "\x00\r\n") || len(u.Model) > 256 {
			return arConfigInvalid(at+".model", arRule("不是合法的模型名", "it is not a valid model name"))
		}
		if u.MaxOutputTokens < 0 || u.MaxOutputTokens > proxyMaxOutputTokensCap {
			return arConfigInvalid(at+".max_output_tokens", arRule("必须在 1 到 131072 之间", "it must be from 1 to 131072"))
		}
	}
	return nil
}

// runnerPolicy is the backend policy of the runner's socket: the same keys and rules as a service
// backend's socket (LEC PC-2, PC-3).
func runnerPolicy(r *AutoReplyRunnerConfig) backendconn.Policy {
	return backendconn.Policy{ExpectedUser: strings.TrimSpace(r.ExpectedUser), SocketGroup: strings.TrimSpace(r.SocketGroup)}
}

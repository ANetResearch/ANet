// Package turntools is the wire form of the turn tools (design v0.3.9 §E): the capabilities of this
// node's service module that an agent may call during one auto-reply run, on behalf of the peer whose
// turn it is and of nobody else.
//
// The node serves them on its model proxy listener (module/autoreply, model_proxy.go) under RoutePrefix,
// with a token it mints for one run and bound to the run's caller, interaction and run; the token stops
// working when the run ends. The run's environment names the route and the token (EnvURL, EnvToken), and
// `anet turn-mcp`, a stdio MCP server the agent starts, reads them, lists the tools and forwards the
// calls (cmd/anet, turnmcp.go). The node, never the agent, decides whose rights a call has: a request
// names a capability and its arguments and nothing else.
//
//	GET  <EnvURL>/tools   Authorization: Bearer <token>  ->  {"tools":[Tool…]}
//	POST <EnvURL>/call    Authorization: Bearer <token>  {"capability":…, "args":{…}}
//	                                                     ->  {"result":…} or {"error":{"code":…,"message":…}}
//
// The model proxy's variables (EnvModelBase, EnvModelKey) are named here as well, so that the node, the
// runner and the documentation agree on one spelling.
//
// Standard library only.
package turntools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The variables of a run's environment (design v0.3.9 §D, §E). They are the only ANET_* variables an
// agent run is ever given (module/autoreply execEnv).
const (
	// EnvTurnPrefix and EnvModelPrefix begin every variable the node adds to a run's environment.
	EnvTurnPrefix  = "ANET_TURN_"
	EnvModelPrefix = "ANET_MODEL_"
	// EnvURL is the turn tools' address: http://<proxy listener>/anet-turn/v1.
	EnvURL = "ANET_TURN_URL"
	// EnvToken is the run's turn token.
	EnvToken = "ANET_TURN_TOKEN"
	// EnvModelBase is the model proxy's injection route: http://<proxy listener>/anet-upstream. An
	// upstream named plan is at EnvModelBase + "/plan".
	EnvModelBase = "ANET_MODEL_BASE"
	// EnvModelKey is the run's model key, sent as Authorization: Bearer.
	EnvModelKey = "ANET_MODEL_KEY"
)

// The routes on the model proxy listener.
const (
	// RoutePrefix starts the turn tools' routes.
	RoutePrefix = "/anet-turn/v1"
	// ToolsPath and CallPath follow RoutePrefix.
	ToolsPath = "/tools"
	CallPath  = "/call"
)

// Limits of the wire form: what one call may carry each way.
const (
	// MaxArgs bounds the arguments of one call.
	MaxArgs = 1 << 20
	// MaxResult bounds what the node answers to one call or to the list.
	MaxResult = 4 << 20
)

// Tool is one turn tool as the list gives it.
type Tool struct {
	// Name is the tool's name for a model: the capability with every character outside [A-Za-z0-9_-]
	// replaced by "_" (board.post is board_post), at most 64 characters, which every model API accepts as
	// a function name.
	Name string `json:"name"`
	// Capability is the service capability the tool calls.
	Capability string `json:"capability"`
	// Description is the capability's description.
	Description string `json:"description,omitempty"`
	// InputSchema is the JSON Schema of the arguments: an object schema, {"type":"object"} when the
	// service declares none.
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolsResponse is the answer of GET /tools.
type ToolsResponse struct {
	Tools []Tool `json:"tools"`
}

// CallRequest is the body of POST /call.
type CallRequest struct {
	Capability string          `json:"capability"`
	Args       json.RawMessage `json:"args,omitempty"`
}

// CallResponse is the answer of POST /call: the service's result, or its refusal.
type CallResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *CallError      `json:"error,omitempty"`
}

// CallError is a refusal: the service's own code and message, or one of the node's (turn_unauthorized,
// not_agent_tool, bad_request, too_large, unavailable).
type CallError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func (e *CallError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// maxToolName is the longest tool name: the bound of the model APIs' function names.
const maxToolName = 64

// ToolName is a capability's tool name (Tool.Name), at most 64 characters.
func ToolName(capability string) string {
	var b strings.Builder
	for _, r := range capability {
		if b.Len() >= maxToolName {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ObjectSchema is s when it is a JSON object whose type is "object", else the empty object schema: a
// tool's arguments are always an object, and a schema that says otherwise is not passed on.
func ObjectSchema(s json.RawMessage) json.RawMessage {
	var m map[string]any
	if len(bytes.TrimSpace(s)) == 0 || json.Unmarshal(s, &m) != nil || m == nil || m["type"] != "object" {
		return json.RawMessage(`{"type":"object"}`)
	}
	return s
}

// ErrNotLoopback is the client's refusal of an address that is not plain http on this machine's
// loopback: the token is never sent anywhere else.
var ErrNotLoopback = errors.New("turntools: the turn tools' address is not http on this machine's loopback")

// Client calls the turn tools of one run.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient is a client for the address and token of a run's environment. The address must be http on
// a loopback address or localhost (ErrNotLoopback otherwise), and the token not empty.
func NewClient(rawURL, token string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrNotLoopback
	}
	if h := u.Hostname(); !strings.EqualFold(h, "localhost") {
		if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
			return nil, ErrNotLoopback
		}
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("turntools: no token")
	}
	return &Client{base: u, token: strings.TrimSpace(token), http: &http.Client{
		Timeout: 10 * time.Minute,
		// The address is the node's own listener; a redirect would take the token elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{Proxy: nil, DisableCompression: true},
	}}, nil
}

// FromEnv is NewClient for EnvURL and EnvToken as getenv reads them; nil and no error when neither is
// set (Unset).
func FromEnv(getenv func(string) string) (*Client, error) {
	u, tok := Unset(getenv(EnvURL)), Unset(getenv(EnvToken))
	if u == "" && tok == "" {
		return nil, nil
	}
	return NewClient(u, tok)
}

// Unset is v, or "" for a value that stands for an unset variable: empty, or still the "${NAME}" an
// agent's configuration passes through as literal text when NAME is not set (hermes does).
func Unset(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "${") {
		return ""
	}
	return v
}

// Tools lists the run's tools.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	var out ToolsResponse
	if err := c.do(ctx, http.MethodGet, ToolsPath, nil, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// Call calls capability with args. A refusal of the service or of the node is a *CallError.
func (c *Client) Call(ctx context.Context, capability string, args json.RawMessage) (json.RawMessage, error) {
	body, err := json.Marshal(CallRequest{Capability: capability, Args: args})
	if err != nil {
		return nil, err
	}
	if len(body) > MaxArgs+len(capability)+64 {
		return nil, &CallError{Code: "too_large", Message: fmt.Sprintf("the arguments are over %d bytes", MaxArgs)}
	}
	var out CallResponse
	if err := c.do(ctx, http.MethodPost, CallPath, body, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, out.Error
	}
	return out.Result, nil
}

func (c *Client) do(ctx context.Context, method, p string, body []byte, out any) error {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + p
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResult+1))
	if err != nil {
		return err
	}
	if len(raw) > MaxResult {
		return &CallError{Code: "too_large", Message: fmt.Sprintf("the answer is over %d bytes", MaxResult)}
	}
	if resp.StatusCode/100 != 2 {
		var e CallResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != nil && e.Error.Code != "" {
			return e.Error
		}
		return &CallError{Code: "unavailable", Message: resp.Status}
	}
	return json.Unmarshal(raw, out)
}

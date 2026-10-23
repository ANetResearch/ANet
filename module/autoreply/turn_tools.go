package autoreply

// turn_tools.go — the turn tools on the model proxy's listener (design v0.3.9 §E, internal/turntools).
//
// An agent that serves a member of an organisation uses the organisation through the capabilities its
// service module marks agent_tool, and only as that member: the node mints a token for one run, bound to
// the caller's AID, the interaction and the run, and every call made with it goes to the service as a call
// of that caller (module.ServiceCaller.CallServiceAs, via "agent"). The request names a capability and its
// arguments and nothing else; a member another member's text names, a caller field, a header, nothing the
// agent writes changes whose call it is. The token stops working when the run ends.
//
//	GET  /anet-turn/v1/tools  the agent_tool capabilities: name, description, input schema
//	POST /anet-turn/v1/call   {"capability", "args"} → {"result"} | {"error":{"code","message"}}
//
// A refusal of the service keeps its own code and message (status 200, as a tool's answer); a refusal of
// the node is a code of the reason table with its sentence, which the agent reads.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/turntools"
	"github.com/ANetResearch/ANet/module"
)

// turnMaxCallsPerRun bounds the tool calls of one run.
const turnMaxCallsPerRun = 200

// serveTurn answers the turn tools' routes.
func (p *modelProxy) serveTurn(w http.ResponseWriter, r *http.Request) {
	run := p.lookup(p.turns, bearer(r.Header))
	if run == nil {
		w.Header().Set("Connection", "close")
		p.turnError(w, http.StatusUnauthorized, "turn_unauthorized", nil)
		return
	}
	p.mu.Lock()
	svc := p.svc
	p.mu.Unlock()
	switch strings.TrimPrefix(r.URL.Path, turntools.RoutePrefix) {
	case turntools.ToolsPath:
		if r.Method != http.MethodGet {
			p.turnError(w, http.StatusMethodNotAllowed, "turn_call_invalid", nil)
			return
		}
		writeJSON(w, http.StatusOK, turntools.ToolsResponse{Tools: turnToolList(svc)})
	case turntools.CallPath:
		if r.Method != http.MethodPost {
			p.turnError(w, http.StatusMethodNotAllowed, "turn_call_invalid", nil)
			return
		}
		run.call(w, r, svc)
	default:
		p.turnError(w, http.StatusNotFound, "turn_call_invalid", nil)
	}
}

// turnToolList is the agent_tool capabilities as tools, in name order. Two capabilities whose names make
// the same tool name keep the first; a schema that is not an object schema becomes the empty one.
func turnToolList(svc module.ServiceCaller) []turntools.Tool {
	if svc == nil {
		return []turntools.Tool{}
	}
	tools := append([]module.AgentTool(nil), svc.AgentTools()...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Capability < tools[j].Capability })
	out := make([]turntools.Tool, 0, len(tools))
	seen := map[string]bool{}
	for _, t := range tools {
		name := turntools.ToolName(t.Capability)
		if t.Capability == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, turntools.Tool{Name: name, Capability: t.Capability, Description: t.Description,
			InputSchema: turntools.ObjectSchema(t.InputSchema)})
	}
	return out
}

// call is one tool call of the run, as the run's caller.
func (run *proxyRun) call(w http.ResponseWriter, r *http.Request, svc module.ServiceCaller) {
	p := run.p
	bodyDeadline(w) // the body within proxyBodyTimeout; the service's answer may take longer
	raw, err := io.ReadAll(io.LimitReader(r.Body, turntools.MaxArgs+4096))
	if err != nil || len(raw) > turntools.MaxArgs+4095 {
		p.turnError(w, http.StatusRequestEntityTooLarge, "turn_call_invalid", nil)
		return
	}
	var req turntools.CallRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || req.Capability == "" {
		p.turnError(w, http.StatusBadRequest, "turn_call_invalid", nil)
		return
	}
	args := bytes.TrimSpace(req.Args)
	if len(args) == 0 || string(args) == "null" {
		args = []byte("{}")
	}
	if obj, ok := decodeJSONObject(args); !ok || obj == nil {
		p.turnError(w, http.StatusBadRequest, "turn_call_invalid", nil)
		return
	}
	known := false
	for _, t := range turnToolList(svc) {
		if t.Capability == req.Capability {
			known = true
			break
		}
	}
	if svc == nil || !known {
		p.turnError(w, http.StatusNotFound, "turn_tool_unknown", a2ashape.Args{"name": egressArg(req.Capability)})
		return
	}
	if run.calls.Add(1) > turnMaxCallsPerRun {
		p.turnError(w, http.StatusTooManyRequests, "turn_calls_spent", a2ashape.Args{"n": turnMaxCallsPerRun})
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(run.ctx, cancel)
	defer stop()
	res, err := svc.CallServiceAs(ctx, run.peer, req.Capability, json.RawMessage(args), "agent")
	var se *module.ServiceError
	switch {
	case err == nil:
		if len(res) == 0 {
			res = json.RawMessage("null")
		}
		if len(res) > turntools.MaxResult-64 {
			p.turnError(w, http.StatusBadGateway, "turn_service_failed", nil)
			return
		}
		log.Printf("anet: auto-reply %s: turn tool %s for %s: ok", run.ix, logText(req.Capability, 64), a2ashape.Short8(run.peer))
		writeJSON(w, http.StatusOK, turntools.CallResponse{Result: res})
	case errors.Is(err, module.ErrNotAgentTool):
		p.turnError(w, http.StatusNotFound, "turn_tool_unknown", a2ashape.Args{"name": egressArg(req.Capability)})
	case errors.As(err, &se):
		log.Printf("anet: auto-reply %s: turn tool %s for %s: refused %s", run.ix, logText(req.Capability, 64),
			a2ashape.Short8(run.peer), logText(se.Code, 64))
		writeJSON(w, http.StatusOK, turntools.CallResponse{Error: &turntools.CallError{Code: se.Code, Message: se.Message}})
	default:
		if ctx.Err() == nil {
			log.Printf("anet: auto-reply %s: turn tool %s: %v", run.ix, logText(req.Capability, 64), err)
		}
		p.turnError(w, http.StatusBadGateway, "turn_service_failed", nil)
	}
}

// turnError answers a refusal of the node: its code and sentence.
func (p *modelProxy) turnError(w http.ResponseWriter, status int, code string, args a2ashape.Args) {
	writeJSON(w, status, turntools.CallResponse{Error: &turntools.CallError{Code: code,
		Message: a2ashape.Sentence(code, p.lang, args)}})
}

// writeJSON writes v as the JSON answer.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":{"code":"turn_service_failed"}}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

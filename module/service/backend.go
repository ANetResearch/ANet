//go:build !no_service

package service

// backend.go is the service module's module.ServiceBackend: the calls the
// node makes to its own services for a reason other than a call from the
// network (design v0.3.9 §A, §B). An internal capability is asked by the
// kernel (inbound.admit); an agent tool is called by an agent running for a
// peer, as that peer. Both go to the backend a network call would reach,
// with the same token and the same checks of the far side; what differs is
// the headers, and that nothing here answers the network.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

var _ module.ServiceBackend = (*svcProvider)(nil)

// InternalCapability implements module.ServiceBackend.
func (p *svcProvider) InternalCapability(id string) bool {
	c := p.m.declared(id)
	return c != nil && c.Internal
}

// AgentTools implements module.ServiceBackend.
func (p *svcProvider) AgentTools() []module.AgentTool {
	var out []module.AgentTool
	for i := range p.m.cfg.Capabilities {
		c := &p.m.cfg.Capabilities[i]
		if !c.AgentTool || c.Internal {
			continue
		}
		t := module.AgentTool{Capability: c.ID, Description: c.Description}
		if len(c.InputSchema) > 0 {
			// Compact: the schema goes to an agent with every list of its
			// tools, and the configuration file's indentation is not part of it.
			var buf bytes.Buffer
			if json.Compact(&buf, c.InputSchema) == nil {
				t.InputSchema = json.RawMessage(buf.Bytes())
			}
		}
		out = append(out, t)
	}
	return out
}

// CallAgentTool implements module.ServiceBackend.
func (p *svcProvider) CallAgentTool(ctx context.Context, callerAID, capability string, args json.RawMessage, via string) (json.RawMessage, error) {
	c := p.m.declared(capability)
	if c == nil || !c.AgentTool || c.Internal {
		return nil, module.ErrNotAgentTool
	}
	if callerAID == "" {
		// The service grants by the caller; a call that names none would be
		// read as a local one, which it is not.
		return nil, errors.New("service: an agent tool call needs the caller")
	}
	return p.m.callAs(ctx, c, callerAID, via, args)
}

// CallInternal implements module.ServiceBackend.
func (p *svcProvider) CallInternal(ctx context.Context, callerAID, capability string, args json.RawMessage) (json.RawMessage, error) {
	c := p.m.declared(capability)
	if c == nil || !c.Internal {
		return nil, fmt.Errorf("service: %q is not an internal capability of this node", capability)
	}
	return p.m.callAs(ctx, c, callerAID, "", args)
}

// maxServiceMessageRunes bounds the message of a refusal passed on from a
// service.
const maxServiceMessageRunes = 500

// callAs posts args (a JSON object; empty is {}) to c's backend as caller,
// within c's deadline and ctx's, and reads the reply: a JSON object, or a
// *module.ServiceError. What the service said beyond its code and message
// stays in this node's log, as for a network call.
func (m *Module) callAs(ctx context.Context, c *Capability, caller, via string, args json.RawMessage) (json.RawMessage, error) {
	body := bytes.TrimSpace(args)
	if len(body) == 0 {
		body = []byte("{}")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return nil, &module.ServiceError{Code: "bad_request", Message: "the arguments must be one JSON object"}
	}
	for k := range obj {
		if strings.HasPrefix(k, "$") {
			// "$" keys are anet's (core §2.4.3); none is defined for these calls.
			return nil, &module.ServiceError{Code: "args_reserved_key"}
		}
	}
	be, ok := m.backends[c.ID]
	if !ok {
		return nil, fmt.Errorf("service: capability %q was not started", c.ID)
	}
	to, _ := m.cfg.timeoutFor(c)
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, be.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := m.tokens[c.ID]; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if caller != "" {
		req.Header.Set(HeaderCaller, caller)
	}
	if via != "" {
		req.Header.Set(HeaderVia, via)
	}
	req.Header.Set(HeaderCapability, c.ID)
	req, sent := provider.TrackSent(req)
	resp, err := be.cli.Do(req)
	if err != nil {
		log.Printf("service: %s (as %s): %v", c.ID, caller, err)
		if sent() {
			// It may have reached the service: timeout or connection_lost,
			// never "did not happen".
			return nil, &module.ServiceError{Code: provider.AnswerLost(err).Reason}
		}
		// Nothing was written: no connection, or a far side that failed the
		// checks (backendconn.ErrRefused). Why stays in the log above.
		return nil, &module.ServiceError{Code: "unavailable"}
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if rerr != nil {
		log.Printf("service: %s (as %s): HTTP %d, then %v", c.ID, caller, resp.StatusCode, rerr)
		return nil, &module.ServiceError{Code: provider.AnswerLost(rerr).Reason}
	}
	if ref := refusalOf(resp, raw, c.ID); ref != nil {
		log.Printf("service: %s (as %s): HTTP %d: %s", c.ID, caller, resp.StatusCode, snippet(raw))
		se := &module.ServiceError{Code: ref.Reason}
		if ref.Verbatim {
			se.Message = serviceMessage(raw)
		}
		return nil, se
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("service: %s (as %s): HTTP %d: %s", c.ID, caller, resp.StatusCode, snippet(raw))
		return nil, &module.ServiceError{Code: "backend_failed"}
	}
	var reply map[string]json.RawMessage
	if json.Unmarshal(raw, &reply) != nil || reply == nil {
		log.Printf("service: %s (as %s): reply is not a JSON object: %s", c.ID, caller, snippet(raw))
		return nil, &module.ServiceError{Code: "backend_failed"}
	}
	return json.RawMessage(bytes.TrimSpace(raw)), nil
}

// serviceMessage is the "message" member of a refusal's body, as one line
// of printable text of at most maxServiceMessageRunes; "" when there is none.
func serviceMessage(raw []byte) string {
	var v struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Message == "" {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range v.Message {
		if n == maxServiceMessageRunes {
			b.WriteString("…")
			break
		}
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

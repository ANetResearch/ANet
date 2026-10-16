package a2a

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// protoFieldNames are the A2A request fields whose proto3 name differs from
// its JSON name. The proto3 JSON mapping obliges a parser to accept both
// spellings, and a2a-go decodes JSON-RPC params with encoding/json, which
// reads the camelCase one only: a client that sends "history_length" (the
// reference conformance kit does) would have the field dropped without a
// word and receive an unbounded history. The names are rewritten before the
// binding reads the request.
var protoFieldNames = map[string]string{
	"history_length":         "historyLength",
	"context_id":             "contextId",
	"task_id":                "taskId",
	"message_id":             "messageId",
	"page_size":              "pageSize",
	"page_token":             "pageToken",
	"status_timestamp_after": "statusTimestampAfter",
	"include_artifacts":      "includeArtifacts",
	"accepted_output_modes":  "acceptedOutputModes",
	"return_immediately":     "returnImmediately",
	"reference_task_ids":     "referenceTaskIds",
}

// snakeToCamelObject renames the known proto field names among the members
// of one JSON object. A member already present under the JSON name wins. It
// reports whether anything was renamed; values are carried as written.
func snakeToCamelObject(obj map[string]json.RawMessage) bool {
	changed := false
	for k, v := range obj {
		camel, ok := protoFieldNames[k]
		if !ok {
			continue
		}
		delete(obj, k)
		changed = true
		if _, exists := obj[camel]; !exists {
			obj[camel] = v
		}
	}
	return changed
}

// normalizeRPCParams rewrites proto field names in a JSON-RPC request's
// params: its own members and those of the configuration and message
// objects. Free-form content (metadata, data parts) is never touched, and a
// body that is not a JSON-RPC request object is returned as it came.
func normalizeRPCParams(body []byte) []byte {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil {
		return body
	}
	raw, ok := env["params"]
	if !ok {
		return body
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil || params == nil {
		return body
	}
	changed := snakeToCamelObject(params)
	for _, member := range []string{"configuration", "message"} {
		inner, ok := params[member]
		if !ok {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(inner, &obj) != nil || obj == nil || !snakeToCamelObject(obj) {
			continue
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return body
		}
		params[member] = b
		changed = true
	}
	if !changed {
		return body
	}
	pb, err := json.Marshal(params)
	if err != nil {
		return body
	}
	env["params"] = pb
	out, err := json.Marshal(env)
	if err != nil {
		return body
	}
	return out
}

// normalizeJSONRPCBody reads a JSON-RPC request's body and puts it back
// with the proto field names of its params rewritten. A read error is kept
// for the binding to meet where it reads (the size limit among them).
func normalizeJSONRPCBody(r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err == nil {
		body = normalizeRPCParams(body)
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{err}))
	r.ContentLength = -1
}

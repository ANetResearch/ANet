package daemon

// The entries a client message comes through (correctness-ux CUX-A4,
// P-CUX6): the control plane's two forms and the local A2A interface's
// message, which module/a2a hands over with the client's own literals
// (TestTheClientsNumbersReachTheKernel there). One request is one content
// through all of them; a messageId reused for other content is one refusal,
// said in the same words on each.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// P-CUX6: one request sent through the control plane's message form, its
// short form (text, skill, args, metadata) and the local A2A interface is
// one content: whichever comes first makes the task, and the others are its
// retries — the same task, marked, nothing sent again. Random requests, with
// numbers a double does not keep apart and member names out of the Basic
// Multilingual Plane.
func TestOneRequestOneContentOnEveryEntry(t *testing.T) {
	_, req, prov := registeredPair(t)
	r := rand.New(rand.NewSource(7))
	literals := []string{"9007199254740993", "9007199254740992", "1", "1.0", "1e2", "-0", "0.1000", "-2.5E-3"}
	value := func() string {
		switch r.Intn(4) {
		case 0:
			return literals[r.Intn(len(literals))]
		case 1:
			return fmt.Sprintf(`[%s,"s",true]`, literals[r.Intn(len(literals))])
		case 2:
			return fmt.Sprintf(`{"😀":%s,"":null,"a":"x\u0001"}`, literals[r.Intn(len(literals))])
		}
		return `"text"`
	}
	type entry struct {
		name string
		send func(id string) (a2ashape.Task, error)
	}
	for i := 0; i < 24; i++ {
		id := fmt.Sprintf("m-%d", i)
		meta := fmt.Sprintf(`{"n":%s,"trace":%s}`, value(), value())
		var short, parts string
		if r.Intn(2) == 0 {
			args := fmt.Sprintf(`{"a":%s,"b":%s}`, value(), value())
			short = `"skill":"calc.add","args":` + args
			parts = `[{"data":{"skill":"calc.add","args":` + args + `}}]`
		} else {
			text := fmt.Sprintf("request %d: ünïcode ✓", i)
			short = `"text":` + quote(text)
			parts = `[{"text":` + quote(text) + `}]`
		}
		message := `{"messageId":"` + id + `","role":"ROLE_USER","parts":` + parts + `,"metadata":` + meta + `}`
		entries := []entry{
			{"message form", func(string) (a2ashape.Task, error) {
				return controlSend(t, req, `{"to":"`+prov.AID()+`","return_immediately":true,"message":`+message+`}`)
			}},
			{"short form", func(id string) (a2ashape.Task, error) {
				return controlSend(t, req, `{"to":"`+prov.AID()+`","return_immediately":true,"message_id":"`+id+`",`+
					short+`,"metadata":`+meta+`}`)
			}},
			{"local A2A", func(string) (a2ashape.Task, error) {
				return req.TaskSeam().Send(t.Context(), prov.AID(), module.TaskSend{Message: decodeLiteral(t, message),
					ReturnImmediately: true})
			}},
		}
		first := i % len(entries)
		task, err := entries[first].send(id)
		if err != nil || deduplicated(task) {
			t.Fatalf("request %d through %s: %v (deduplicated %v)\n%s", i, entries[first].name, err, deduplicated(task), message)
		}
		for j, e := range entries {
			if j == first {
				continue
			}
			again, err := e.send(id)
			if err != nil || again.ID != task.ID || !deduplicated(again) {
				t.Fatalf("request %d through %s after %s: %s %v (deduplicated %v), want the retry of %s\n%s",
					i, e.name, entries[first].name, again.ID, err, deduplicated(again), task.ID, message)
			}
		}
	}
	if n := outboundCount(t, req); n != 24 {
		t.Fatalf("%d tasks for 24 requests", n)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// controlSend is POST /tasks/send: the task, or the error body as an error.
func controlSend(t *testing.T, d *Daemon, body string) (a2ashape.Task, error) {
	t.Helper()
	code, out := controlCall(t, d.hTasksSend, "/tasks/send", body)
	b, _ := json.Marshal(out)
	if code != http.StatusOK {
		return a2ashape.Task{}, fmt.Errorf("%d %s", code, b)
	}
	var task a2ashape.Task
	if err := json.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	return task, nil
}

// CUX-A4, CUX-D7 (UX-T1): a messageId reused for other content is refused in
// one form per surface — the control plane's 409 {error, reason, task_id,
// state}, the local A2A interface's InvalidParamsError with two ErrorInfo —
// in the same words, .open on a new task and .followup on a follow-up, and
// nothing is written. A retry is the task with anet.deduplicated, on both.
func TestMessageIDReusedShapes(t *testing.T) {
	_, req, prov := registeredPair(t)
	lang := a2ashape.Lang()
	first := `{"to":"` + prov.AID() + `","return_immediately":true,"message":{"messageId":"m-1","role":"ROLE_USER","parts":[{"text":"one"}]}}`
	task, err := controlSend(t, req, first)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := controlSend(t, req, first); err != nil || again.ID != task.ID || again.Metadata[a2ashape.KeyDeduplicated] != true {
		t.Fatalf("the retry on the control plane: %v %v", again.Metadata, err)
	}
	for _, c := range []struct {
		variant, body string
		msg           a2ashape.Message
	}{
		{"open", `{"to":"` + prov.AID() + `","text":"two","message_id":"m-1","return_immediately":true}`,
			textMsg("two", "", "m-1")},
		{"followup", `{"to":"` + prov.AID() + `","task_id":"` + task.ID + `","text":"three","message_id":"m-1","return_immediately":true}`,
			followUp(task.ID, "three", "m-1")},
	} {
		key := "message_id_reused." + c.variant
		sentence := a2ashape.Sentence(key, lang, a2ashape.Args{"task": task.ID})
		n := countMsgs(t, req, task.ID)
		code, out := controlCall(t, req.hTasksSend, "/tasks/send", c.body)
		if code != http.StatusConflict || out["error"] != sentence || out["reason"] != "message_id_reused" ||
			out["task_id"] != task.ID || out["state"] != string(a2ashape.TaskStateSubmitted) || out["next"] != nil {
			t.Errorf("%s on the control plane: %d %v", c.variant, code, out)
		}
		_, err := clientSend(t, req, prov.AID(), c.msg)
		reusedAs(t, err, c.variant, task.ID)
		e := err.(*a2ashape.Refusal).A2A(lang)
		info := e.Details
		if e.Name != "InvalidParamsError" || e.RPCCode != -32602 || e.HTTPStatus != 400 || e.Message != sentence ||
			len(info) != 2 || info[0].Reason != "INVALID_PARAMS" || info[0].Domain != a2ashape.DomainA2A ||
			info[1].Reason != "MESSAGE_ID_REUSED" || info[1].Domain != a2ashape.DomainANet ||
			info[1].Metadata["reason"] != "message_id_reused" || info[1].Metadata["taskId"] != task.ID ||
			info[1].Metadata["contextId"] != task.ContextID || info[1].Metadata["state"] != string(a2ashape.TaskStateSubmitted) {
			t.Errorf("%s on the local A2A interface: %+v", c.variant, e)
		}
		if countMsgs(t, req, task.ID) != n || outboundCount(t, req) != 1 {
			t.Errorf("%s: a refused message was written", c.variant)
		}
		if !strings.Contains(sentence, task.ID) {
			t.Errorf("%s: the sentence does not name the task: %s", c.variant, sentence)
		}
	}
	// The retry's mark is the reason table's code, said by its sentence.
	if a2ashape.ReasonCode("deduplicated") != "deduplicated" || a2ashape.Sentence("deduplicated", lang, nil) == "" {
		t.Error("the reason table has no deduplicated row")
	}
}

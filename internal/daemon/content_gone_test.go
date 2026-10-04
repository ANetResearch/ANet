package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// FS-L4: a task whose data key is gone — its content deleted, typically by a retention sweep that
// ran while it was read — is answered as a task this node does not have: the same TaskNotFound, the
// same status and the same error name as an id it never had (no_local_task in the user's words),
// through the control plane's task read and the scoped read of the local A2A interface alike.
func TestContentGoneMapsToNoLocalTask(t *testing.T) {
	d := buildTestDaemon(t, "", nil)
	const peer = "did:anet:peer-content-gone"
	if err := d.ix.Create(interactions.New{ID: "ix_gone", Role: interactions.RoleOutbound, PeerAID: peer, Goal: "a goal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.AddMessage("ix_gone", d.AID(), interactions.MsgText, "a goal"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.getTask(controlScope, "ix_gone", nil); err != nil {
		t.Fatalf("before the key is deleted: %v", err)
	}
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		ok, err := tx.DeleteDEK("ix_gone")
		if err == nil && !ok {
			err = errors.New("no key")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for _, sc := range []taskScope{controlScope, {peer: peer}} {
		for _, id := range []string{"ix_gone", "ix_never"} {
			if _, err := d.getTask(sc, id, nil); !errors.Is(err, a2ashape.ErrTaskNotFound) {
				t.Errorf("scope %+v, %s: %v, want TaskNotFound", sc, id, err)
			}
		}
	}

	get := func(id string) (int, map[string]string) {
		t.Helper()
		rec := httptest.NewRecorder()
		d.hTasksGet(rec, httptest.NewRequest(http.MethodPost, "/tasks/get", strings.NewReader(`{"task_id":"`+id+`"}`)))
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v: %s", id, err, rec.Body.String())
		}
		body["error"] = strings.ReplaceAll(body["error"], id, "<id>")
		return rec.Code, body
	}
	goneCode, goneBody := get("ix_gone")
	neverCode, neverBody := get("ix_never")
	if goneCode != http.StatusNotFound || goneCode != neverCode || goneBody["code"] != neverBody["code"] || goneBody["error"] != neverBody["error"] {
		t.Fatalf("content gone: %d %v; never stored: %d %v; want the same not-found answer", goneCode, goneBody, neverCode, neverBody)
	}
}

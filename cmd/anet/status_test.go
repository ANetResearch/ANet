package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

const statusFixture = `{"aid":"bafyreiujzdegxdncf32epf3dhodzdocis2jhtlgmxgedn73u557hq2abcd","version":"0.3.0","data_dir":"/home/u/.anet","hub_url":"x",
"summary":[
 {"topic":"identity","level":"ok","text":"default …7hq2abcd,密钥已由 https://hub.example 确认(4 分钟前)。"},
 {"topic":"recovery","level":"warn","text":"身份还没有设恢复口令。","next":"anet identity protect"},
 {"topic":"hub","level":"ok","text":"已在 https://hub.example 注册。"},
 {"topic":"inbound","level":"ok","text":"只接受 allow 名单里 0 个对端的任务。"}]}`

func statusClient(t *testing.T, body string) *client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
}

// UX-T12: `anet status` prints /status.summary's text, one line each, with
// the "→" line under a line that has a next step, and --json prints the
// whole /status.
func TestStatusPrintsTheSummary(t *testing.T) {
	var out bytes.Buffer
	cliStdout = &out
	t.Cleanup(func() { cliStdout = nil })
	c := statusClient(t, statusFixture)
	if err := runStatus(c, nil); err != nil {
		t.Fatal(err)
	}
	// One line per summary line and one "→" line: 5 whatever the language.
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 || lines[2] != "  → anet identity protect" {
		t.Fatalf("lines:\n%s", out.String())
	}

	// The label is padded to the same column, in both languages.
	st, err := fetchStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	var zh bytes.Buffer
	printSummary(&zh, st.Summary, nil, a2ashape.Chinese)
	zl := strings.Split(zh.String(), "\n")
	if !strings.HasPrefix(zl[0], "身份    default") || !strings.HasPrefix(zl[1], "恢复    身份还没有") || !strings.HasPrefix(zl[3], "hub     已在") ||
		!strings.HasPrefix(zl[4], "接单    只接受") {
		t.Errorf("zh lines:\n%s", zh.String())
	}
	var en bytes.Buffer
	printSummary(&en, st.Summary, nil, a2ashape.English)
	if !strings.HasPrefix(en.String(), "identity  default") || !strings.Contains(en.String(), "\nhub       ") {
		t.Errorf("en lines:\n%s", en.String())
	}

	out.Reset()
	if err := runStatus(c, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"summary"`) || !strings.Contains(out.String(), `"data_dir"`) {
		t.Errorf("--json:\n%s", out.String())
	}
	if err := runStatus(c, []string{"extra"}); err == nil {
		t.Error("a positional argument was accepted")
	}
}

// `anet up` says up.started, then the recovery and hub lines, and no other.
func TestUpSummaryShowsOnlyRecoveryAndHub(t *testing.T) {
	var out bytes.Buffer
	var st statusAnswer
	st.Summary = []a2ashape.SummaryLine{
		{Topic: "identity", Level: "ok", Text: "I"}, {Topic: "recovery", Level: "warn", Text: "R", Next: "anet identity protect"},
		{Topic: "hub", Level: "info", Text: "H", Next: "anet hub-register https://hub.agentnetwork.org.cn"},
		{Topic: "inbound", Level: "ok", Text: "N"},
	}
	printSummary(&out, st.Summary, map[string]bool{a2ashape.TopicRecovery: true, a2ashape.TopicHub: true}, a2ashape.English)
	want := "recovery  R\n  → anet identity protect\nhub       H\n  → anet hub-register https://hub.agentnetwork.org.cn\n"
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

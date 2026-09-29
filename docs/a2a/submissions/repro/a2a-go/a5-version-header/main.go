// A5: a2asrv neither checks nor defaults A2A-Version (A2A §3.6.2). A GetTask
// for an unknown task is answered TaskNotFound (-32001) whatever version the
// request declares; VersionNotSupportedError (-32009) is never returned.
//
// Run: go run ./a5-version-header
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

func main() {
	executor := a2asrv.AgentExecutorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(func(a2a.Event, error) bool) {}
	})
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor)))
	defer srv.Close()

	cases := []struct{ header, query, want string }{
		{"9.9", "", "-32009 VersionNotSupported"},
		{"", "A2A-Version=9.9", "-32009 VersionNotSupported (query parameter, §3.6.1)"},
		{"0.3", "", "0.3 semantics, or -32009 if the server does not serve 0.3"},
		{"", "", "\"MUST interpret empty value as 0.3\": as for 0.3"},
		{"1.0", "", "-32001 TaskNotFound"},
	}
	unchecked := 0
	for _, c := range cases {
		url := srv.URL
		if c.query != "" {
			url += "?" + c.query
		}
		req, err := http.NewRequest("POST", url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"no-such-task"}}`))
		must(err)
		req.Header.Set("Content-Type", "application/json")
		if c.header != "" {
			req.Header.Set("A2A-Version", c.header)
		}
		resp, err := http.DefaultClient.Do(req)
		must(err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var r struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &r)
		got := "no error"
		if r.Error != nil {
			got = fmt.Sprintf("%d %s", r.Error.Code, r.Error.Message)
			if r.Error.Code == -32001 && c.header != "1.0" {
				unchecked++
			}
		}
		fmt.Printf("A2A-Version header %-5q query %-18q -> %-28s expected: %s\n", c.header, c.query, got, c.want)
	}
	if unchecked > 0 {
		fmt.Println("reproduced: yes")
		return
	}
	fmt.Println("reproduced: no")
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fmt.Println("setup error:", err)
		os.Exit(2)
	}
}

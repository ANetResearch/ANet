//go:build !no_autoreply

package main

import "testing"

func TestParseRunnerServe(t *testing.T) {
	o, err := parseRunnerServe([]string{"--socket", "/run/r.sock", "--allow-user=anet-org", "--agent", "hermes=/opt/hermes/bin/hermes",
		"--agent=claude", "--max-jobs", "2", "--workspace", "per-run", "--keep-days", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Socket != "/run/r.sock" || o.AllowUser != "anet-org" || o.Agents["hermes"] != "/opt/hermes/bin/hermes" || o.Agents["claude"] != "" ||
		len(o.Agents) != 2 || o.MaxJobs != 2 || o.Workspace != "per-run" || o.KeepDays != -1 {
		t.Errorf("%+v", o)
	}
	for _, bad := range [][]string{{}, {"--socket", "/r"}, {"--allow-user", "u"}, {"--socket", "/r", "--allow-user", "u", "--max-jobs", "x"},
		{"--socket", "/r", "--allow-user", "u", "--bogus", "1"}, {"--socket", "/r", "--allow-user"}} {
		if _, err := parseRunnerServe(bad); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
	if err := checkFlags("runner", []string{"serve", "--socket", "/r", "--allow-user", "u", "--agent", "hermes", "--keep-days", "3"}); err != nil {
		t.Errorf("checkFlags: %v", err)
	}
}

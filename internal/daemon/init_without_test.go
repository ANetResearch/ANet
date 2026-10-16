package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

func writeConfigJSON(t *testing.T, l Layout, body string) {
	t.Helper()
	if err := l.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.ConfigPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func withoutOnDisk(t *testing.T, l Layout) []string {
	t.Helper()
	w, err := WithoutInFile(l)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func errRefusalKey(t *testing.T, err error) string {
	t.Helper()
	var r *a2ashape.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("error %v is not a refusal", err)
	}
	return r.Key
}

// MOD-7a (1): --without only ever adds, and a fresh directory gets the list.
func TestInitWithoutOnlyAdds(t *testing.T) {
	l := NewLayout(filepath.Join(t.TempDir(), "fresh"))
	_, ch, err := InitWithout(l, []string{"x402", "cas"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cas", "x402"}; !reflect.DeepEqual(withoutOnDisk(t, l), want) || !reflect.DeepEqual(ch.Added, want) {
		t.Fatalf("list %v, added %v, want %v", withoutOnDisk(t, l), ch.Added, want)
	}
	// A second call with other names adds; none is dropped.
	if _, _, err = InitWithout(l, []string{"service"}, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cas", "service", "x402"}; !reflect.DeepEqual(withoutOnDisk(t, l), want) {
		t.Fatalf("list %v, want %v", withoutOnDisk(t, l), want)
	}
	// A name already there changes nothing, not even the file.
	before, _ := os.ReadFile(l.ConfigPath())
	_, ch, err = InitWithout(l, []string{"x402"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(l.ConfigPath())
	if len(ch.Added) != 0 || !bytes.Equal(before, after) {
		t.Fatalf("a name already in the list changed the file (added %v)", ch.Added)
	}
	// A plain init (no names) leaves the list as it is.
	if _, err = InitLayout(l); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cas", "service", "x402"}; !reflect.DeepEqual(withoutOnDisk(t, l), want) {
		t.Fatalf("plain init changed the list to %v", withoutOnDisk(t, l))
	}
}

// MOD-7a (1): --with removes the one name it is given.
func TestInitWithRemovesOneName(t *testing.T) {
	l := NewLayout(t.TempDir())
	if _, _, err := InitWithout(l, []string{"cas", "service", "x402"}, nil); err != nil {
		t.Fatal(err)
	}
	_, ch, err := InitWithout(l, nil, []string{"service"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cas", "x402"}; !reflect.DeepEqual(withoutOnDisk(t, l), want) || !reflect.DeepEqual(ch.Removed, []string{"service"}) {
		t.Fatalf("list %v, removed %v", withoutOnDisk(t, l), ch.Removed)
	}
	// A name that is not in the list: nothing happens. The last name out
	// removes the key, so the file reads as a node with nothing turned off.
	if _, ch, err = InitWithout(l, nil, []string{"p2p"}); err != nil || len(ch.Removed) != 0 {
		t.Fatalf("removing a name that is not there: %v, %v", ch.Removed, err)
	}
	if _, _, err = InitWithout(l, nil, []string{"cas", "x402"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(l.ConfigPath())
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["without"]; has {
		t.Fatalf("the empty list is still written: %v", m["without"])
	}
}

// MOD-7a: every refusal comes before the first byte is written.
func TestInitWithoutRefusesConfiguredModule(t *testing.T) {
	l := NewLayout(t.TempDir())
	writeConfigJSON(t, l, `{"modules":{"cas":{"enabled":true}}}`)
	before, _ := os.ReadFile(l.ConfigPath())
	_, _, err := InitWithout(l, []string{"service", "cas"}, nil)
	if errRefusalKey(t, err) != "without_configured" {
		t.Fatalf("got %v, want without_configured", err)
	}
	var r *a2ashape.Refusal
	errors.As(err, &r)
	if r.Args["key"] != "modules.cas" || r.Args["m"] != "cas" {
		t.Errorf("args %v", r.Args)
	}
	after, _ := os.ReadFile(l.ConfigPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("config.json changed:\n%s", after)
	}
	// Another module whose block is absent is fine.
	if _, _, err = InitWithout(l, []string{"service"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInitWithoutDaemonRunning(t *testing.T) {
	l := NewLayout(t.TempDir())
	writeConfigJSON(t, l, `{"control_addr":"127.0.0.1:39999"}`)
	before, _ := os.ReadFile(l.ConfigPath())
	lock, err := AcquireDataLock(l)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	for _, c := range []struct{ add, remove []string }{{[]string{"x402"}, nil}, {nil, []string{"x402"}}} {
		_, _, err = InitWithout(l, c.add, c.remove)
		if errRefusalKey(t, err) != "daemon_running" {
			t.Fatalf("got %v, want daemon_running", err)
		}
	}
	after, _ := os.ReadFile(l.ConfigPath())
	if !bytes.Equal(before, after) {
		t.Fatal("config.json changed under a running daemon")
	}
}

func TestInitWithoutRefusesBadNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "never")
	l := NewLayout(root)
	for name, c := range map[string]struct{ add, remove []string }{
		"unknown":      {[]string{"nosuch"}, nil},
		"twice":        {[]string{"x402", "x402"}, nil},
		"unknown with": {nil, []string{"nosuch"}},
		"both":         {[]string{"x402"}, []string{"x402"}},
		"not ready":    {[]string{"autoreply"}, nil},
	} {
		_, _, err := InitWithout(l, c.add, c.remove)
		if errRefusalKey(t, err) != "config_invalid" {
			t.Errorf("%s: got %v, want config_invalid", name, err)
		}
	}
	if _, err := os.Stat(root); err == nil {
		t.Error("a refused call created the data directory")
	}
}

func TestConfiguredKey(t *testing.T) {
	obj := func(s string) map[string]any {
		m, err := decodeObject([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, c := range []struct{ cfg, module, want string }{
		{`{}`, "autoreply", ""},
		{`{"auto_reply":{}}`, "autoreply", "auto_reply"},
		{`{"auto_reply":null}`, "autoreply", ""},
		{`{"stream_preview":"off"}`, "preview", ""},
		{`{"stream_preview":"provider"}`, "preview", "stream_preview"},
		{`{"modules":{"x402":{}}}`, "x402", "modules.x402"},
		{`{"modules":{"x402":{}}}`, "cas", ""},
		{`{"modules":{"x402":{}}}`, "console", ""},
	} {
		if got := configuredKey(obj(c.cfg), c.module); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.cfg, c.module, got, c.want)
		}
	}
}

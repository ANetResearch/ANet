package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// capCallMessage is the message every entry builds for the same capability
// call with one file (CUX-F4); internal/mcpserv has the same constant and
// test. The daemon turns it into one request, with one request CID.
const capCallMessage = `{"messageId":"","role":"ROLE_USER","parts":[{"data":{"args":{"max_faces":3},"skill":"face.detect"},"mediaType":"application/json"},{"raw":"aGVsbG8=","filename":"a.txt"}]}`

func TestAttachMessageIsTheSameAsMCPs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := sendBody("", "face.detect", map[string]any{"max_faces": 3}, []string{p})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(body["message"])
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(got, &a)
	_ = json.Unmarshal([]byte(capCallMessage), &b)
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if string(ga) != string(gb) {
		t.Fatalf("message\n got %s\nwant %s", ga, gb)
	}
}

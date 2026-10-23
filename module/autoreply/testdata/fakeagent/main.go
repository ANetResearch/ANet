// Command fakeagent stands in for hermes in the runner's tests (runner_test.go): it is started as hermes
// is, `fakeagent --oneshot=MESSAGE [-m MODEL]`, reads the conversation from the file the message names,
// and does what the conversation asks for with one of these words:
//
//	MODE:reply   (the default) call the turn tools and the model proxy when the environment names them,
//	             write the environment and the results into $ANET_OUTBOX, and print a reply
//	MODE:sleep   start a child process, write both process ids to PIDS:<path>, and wait
//	MODE:flood   print more than the exec backend keeps of an agent's stdout
//	MODE:fail    print to stderr and exit 3
//
// It is built by the tests with the go command, so that the runner starts a real separate program.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/turntools"
)

var fileRe = regexp.MustCompile(`Read the file (\S+): it holds the conversation`)

func main() {
	if len(os.Args) < 2 || !strings.HasPrefix(os.Args[1], "--oneshot=") {
		fmt.Fprintln(os.Stderr, "fakeagent: started without --oneshot=")
		os.Exit(2)
	}
	msg := strings.TrimPrefix(os.Args[1], "--oneshot=")
	m := fileRe.FindStringSubmatch(msg)
	if m == nil {
		fmt.Fprintln(os.Stderr, "fakeagent: the message names no conversation file")
		os.Exit(2)
	}
	conv, err := os.ReadFile(m[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeagent:", err)
		os.Exit(2)
	}
	text := string(conv)
	switch {
	case strings.Contains(text, "MODE:sleep"):
		sleep(text)
	case strings.Contains(text, "MODE:flood"):
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 8; i++ {
			_, _ = os.Stdout.Write(chunk)
		}
	case strings.Contains(text, "MODE:fail"):
		fmt.Fprintln(os.Stderr, "fakeagent: failing as asked")
		os.Exit(3)
	default:
		reply(text)
	}
}

func sleep(text string) {
	pids := regexp.MustCompile(`PIDS:(\S+)`).FindStringSubmatch(text)
	child := exec.Command("sleep", "300")
	if err := child.Start(); err != nil {
		os.Exit(4)
	}
	if pids != nil {
		_ = os.WriteFile(pids[1]+".tmp", []byte(fmt.Sprintf("%d %d\n", os.Getpid(), child.Process.Pid)), 0o600)
		_ = os.Rename(pids[1]+".tmp", pids[1])
	}
	_ = child.Wait()
}

func reply(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	outbox := os.Getenv("ANET_OUTBOX")
	var parts []string
	env := os.Environ()
	sort.Strings(env)
	if outbox != "" {
		_ = os.WriteFile(filepath.Join(outbox, "env.txt"), []byte(strings.Join(env, "\n")+"\n"), 0o600)
		cwd, _ := os.Getwd()
		_ = os.WriteFile(filepath.Join(outbox, "cwd.txt"), []byte(cwd+"\n"), 0o600)
	}
	if c, err := turntools.FromEnv(os.Getenv); err != nil {
		parts = append(parts, "tools-error:"+err.Error())
	} else if c != nil {
		tools, err := c.Tools(ctx)
		if err != nil {
			parts = append(parts, "tools-error:"+err.Error())
		}
		names := []string{}
		for _, t := range tools {
			names = append(names, t.Name)
		}
		parts = append(parts, "tools:"+strings.Join(names, ","))
		if len(tools) > 0 {
			pick := tools[0]
			for _, t := range tools {
				if t.Name == "board_post" {
					pick = t
				}
			}
			res, err := c.Call(ctx, pick.Capability, json.RawMessage(`{"title":"help wanted"}`))
			if err != nil {
				parts = append(parts, "call-error:"+err.Error())
			} else {
				parts = append(parts, "call:"+string(res))
			}
		}
	}
	if base, key := os.Getenv("ANET_MODEL_BASE"), os.Getenv("ANET_MODEL_KEY"); base != "" && key != "" {
		parts = append(parts, "model:"+model(ctx, base, key))
	}
	if strings.Contains(text, "attachment: ") {
		if dir := regexp.MustCompile(`in the directory (\S+)\.`).FindStringSubmatch(text); dir != nil {
			ents, _ := os.ReadDir(dir[1])
			for _, e := range ents {
				parts = append(parts, "input:"+e.Name())
			}
		}
	}
	if outbox != "" {
		_ = os.WriteFile(filepath.Join(outbox, "result.txt"), []byte(strings.Join(parts, "\n")+"\n"), 0o600)
	}
	fmt.Println("fake reply: " + strings.Join(parts, " | "))
}

// model asks the model proxy's upstream "plan" for a completion and returns its content, or the error.
func model(ctx context.Context, base, key string) string {
	body := `{"model":"agent-chosen","messages":[{"role":"user","content":"hello"}]}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/plan/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		return "error " + err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	switch {
	case out.Error != nil:
		return fmt.Sprintf("status %d %s", resp.StatusCode, out.Error.Code)
	case len(out.Choices) > 0:
		return out.Choices[0].Message.Content
	}
	return fmt.Sprintf("status %d", resp.StatusCode)
}

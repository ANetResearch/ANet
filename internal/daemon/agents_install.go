package daemon

// agents_install.go — idempotent persona wiring for each supported external agent.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func installCursor() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	rulesDir := filepath.Join(home, ".cursor", "rules")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(rulesDir, "agentnetwork-anet.mdc")
	body := "---\ndescription: AgentNetwork (anet) integration — how to delegate and provide work\nalwaysApply: true\n---\n\n" + anetGuidance + "\n"
	if err := writeManagedBlock(path, anetBlockBegin, anetBlockEnd, body); err != nil {
		return nil, err
	}
	return []string{"installed Cursor rule " + path}, nil
}

func installClaude() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(claudeDir, "CLAUDE.md")
	block := anetBlockBegin + "\n" + anetGuidance + "\n" + anetBlockEnd + "\n"
	if err := appendManagedBlock(path, block); err != nil {
		return nil, err
	}
	return []string{"appended anet guidance to " + path}, nil
}

func installCodex() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(codexDir, "AGENTS.md")
	block := anetBlockBegin + "\n" + anetGuidance + "\n" + anetBlockEnd + "\n"
	if err := appendManagedBlock(path, block); err != nil {
		return nil, err
	}
	return []string{"appended anet guidance to " + path}, nil
}

func installOpenClaw() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "AGENTS.md")
	block := anetBlockBegin + "\n" + anetGuidance + "\n" + anetBlockEnd + "\n"
	if err := appendManagedBlock(path, block); err != nil {
		return nil, err
	}
	return []string{"appended anet guidance to " + path}, nil
}

func installHermesCLI() ([]string, error) {
	home, ok := detectHermesHome()
	if !ok {
		return nil, fmt.Errorf("anet install: hermes-agent not found — set HERMES_HOME or install hermes-agent first")
	}
	return applyHermes(home)
}

// appendManagedBlock replaces or appends a fenced anet block in path.
func appendManagedBlock(path, block string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	src := string(raw)
	var out string
	if i := strings.Index(src, anetBlockBegin); i >= 0 {
		j := strings.Index(src, anetBlockEnd)
		if j < i {
			return fmt.Errorf("anet install: malformed anet markers in %s", path)
		}
		end := j + len(anetBlockEnd)
		if end < len(src) && src[end] == '\n' {
			end++
		}
		out = src[:i] + block + src[end:]
	} else if strings.TrimSpace(src) == "" {
		out = block
	} else {
		sep := "\n\n"
		if strings.HasSuffix(src, "\n") {
			sep = "\n"
		}
		out = src + sep + block
	}
	return writeFileAtomic(path, []byte(out), 0o644)
}

// writeManagedBlock writes/replaces content between markers (whole file is the block body).
func writeManagedBlock(path, begin, end, body string) error {
	if strings.Contains(body, begin) {
		return writeFileAtomic(path, []byte(body), 0o644)
	}
	return writeFileAtomic(path, []byte(body), 0o644)
}

//go:build windows

package autoreply

import "strings"

// execEnvBase are the variables an unsandboxed agent inherits from the daemon, when set: the unix list
// (env_unix.go), and the variables without which Windows programs cannot find the system, the command
// interpreter, the temporary directory or the program directories. LC_* is matched as a prefix.
var execEnvBase = []string{
	"PATH", "LANG", "LANGUAGE", "TERM", "TZ", "NO_COLOR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE",
	"PATHEXT", "SystemRoot", "windir", "ComSpec", "TEMP", "TMP",
	"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
	"PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS", "OS",
}

// execEnvTrusted are the additional variables an unsandboxed (trusted-peer) agent inherits: the unix
// list (env_unix.go), and the user's profile, account and application data directories.
var execEnvTrusted = []string{
	"HOME", "USER", "LOGNAME", "SHELL",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK",
	"USERPROFILE", "USERNAME", "USERDOMAIN", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
}

// execEnvTrustedPrefixes admits the configuration and credential variables of the supported agent CLIs
// and their model providers to an unsandboxed agent (the unix list).
var execEnvTrustedPrefixes = []string{
	"LC_", "ANTHROPIC_", "CLAUDE_", "OPENAI_", "CODEX_", "CURSOR_", "OPENCODE_", "OPENCLAW_", "HERMES_",
	"OPENROUTER_", "GEMINI_", "GOOGLE_API_KEY", "AZURE_OPENAI_", "DEEPSEEK_", "DASHSCOPE_", "MOONSHOT_",
}

// envKeyEqual compares two environment variable names. Windows treats names without regard to case:
// "Path" and "PATH" are one variable.
func envKeyEqual(a, b string) bool { return strings.EqualFold(a, b) }

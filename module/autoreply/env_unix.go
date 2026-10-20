//go:build !windows

package autoreply

// execEnvBase are the variables an unsandboxed agent inherits from the daemon, when set: the search
// path, locale, terminal and time zone, proxies and CA bundles. LC_* is matched as a prefix.
var execEnvBase = []string{
	"PATH", "LANG", "LANGUAGE", "TERM", "TZ", "NO_COLOR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE",
}

// execEnvTrusted are the additional variables an unsandboxed (trusted-peer) agent inherits: the user's
// identity, home and XDG directories, the SSH agent and session bus, which an agent working in the
// operator's environment may need.
var execEnvTrusted = []string{
	"HOME", "USER", "LOGNAME", "SHELL",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK",
}

// execEnvTrustedPrefixes admits the configuration and credential variables of the supported agent CLIs
// and their model providers to an unsandboxed agent.
var execEnvTrustedPrefixes = []string{
	"LC_", "ANTHROPIC_", "CLAUDE_", "OPENAI_", "CODEX_", "CURSOR_", "OPENCODE_", "OPENCLAW_", "HERMES_",
	"OPENROUTER_", "GEMINI_", "GOOGLE_API_KEY", "AZURE_OPENAI_", "DEEPSEEK_", "DASHSCOPE_", "MOONSHOT_",
}

// envKeyEqual compares two environment variable names. Names are case-sensitive on unix.
func envKeyEqual(a, b string) bool { return a == b }

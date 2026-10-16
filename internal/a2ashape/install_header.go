package a2ashape

// install_header.go holds the statement at the top of install.sh about what
// the default binary can do (MOD-F10). The old text said the default binary
// "cannot execute anything on the host", which is not true of the exec
// back end of auto-reply. Which of the two last sentences is right depends on
// whether the name table gives auto-reply a build tag yet; install.sh carries
// the text for the table as it is, and a test (cmd/anet) compares the two.

// InstallHeader is the statement about the default binary in a language.
// autoreplyTagged is whether the name table gives autoreply a build tag
// (false in v0.3.0: the code cannot be left out at build time yet).
func InstallHeader(lang Language, autoreplyTagged bool) string {
	if lang == Chinese {
		s := "缺省二进制不执行远程命令（shell 模块不在其中）。运营者配置自动回复之后，它会为对端运行本机 agent，未信任对端只在沙箱里运行。不想要这项功能：--without autoreply 让它永不启动；"
		if autoreplyTagged {
			return s + "要二进制里没有这段代码：按 https://docs.agentnetwork.org.cn/docs/zh-cn/latest/topics/modules.html 用 build.sh --without autoreply 自建。"
		}
		return s + "本版本还不能在构建时去掉这段代码。"
	}
	s := "The default binary does not run remote commands (the shell module is not in it). Once the operator configures auto-reply, it runs a local agent for peers, and untrusted peers only inside a sandbox. If you do not want this feature: --without autoreply keeps it from ever starting; "
	if autoreplyTagged {
		return s + "to leave the code out of the binary, build it yourself with build.sh --without autoreply as described at https://docs.agentnetwork.org.cn/docs/en/latest/topics/modules.html."
	}
	return s + "this version cannot yet leave the code out at build time."
}

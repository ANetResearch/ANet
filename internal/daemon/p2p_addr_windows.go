//go:build windows

package daemon

import "github.com/ANetResearch/ANet/internal/a2ashape"

// p2pPlatformRefusal refuses modules.p2p on Windows: the module reaches its peer process over a Unix
// socket, and on Windows anet cannot verify which account listens on one (internal/peercred), so every
// connection would be refused. The configuration is refused at start instead.
func p2pPlatformRefusal() *a2ashape.Refusal {
	return configInvalid("modules.p2p", ruleText(
		"p2p 模块在 Windows 上不可用，因为它经 Unix 套接字连接对等进程，而在 Windows 上无法核实监听该套接字的账户；请删除 modules.p2p（投递随后经由 hub 进行）",
		"the p2p module is not available on Windows, because it reaches its peer process over a Unix socket and anet cannot verify which account listens on one there; remove modules.p2p (delivery then goes through the hub)"))
}

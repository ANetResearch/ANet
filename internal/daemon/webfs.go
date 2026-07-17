package daemon

import (
	"embed"
)

// webAssets embeds the local web console (the same single-page app the Hub serves, here injected with a
// window.__ANET bootstrap by consoleHandler).
//
//go:embed web/console.html
var webAssets embed.FS

// ConsoleHTML returns the raw bytes of the local web console page. The daemon serves this at its local
// /console after injecting a window.__ANET bootstrap so the page can drive the local control API.
func ConsoleHTML() ([]byte, error) { return webAssets.ReadFile("web/console.html") }

// AgentNetwork (anet) plugin for OpenCode.
//
// OpenCode has no plugin bundle: a plugin is a JavaScript module that is named
// in the "plugin" list of opencode.json. This module registers, through the
// "config" hook, the three things that the anet plugin of every other host
// consists of:
//
//   * the MCP server "__SERVER_NAME__", started with `sh bin/anet-mcp.sh`;
//   * the skills "anet" and "anet-setup" (directory skills/);
//   * the command "anet-status" (file commands/anet-status.md).
//
// It also defines ANET_PLUGIN_ROOT and, when set, the two plugin settings for
// every shell command of the agent, so that the skills reach the same runner,
// executable and identity as the MCP server. The plugin writes no file and
// opens no connection. Settings are the options of the plugin entry:
//
//   "plugin": [["<package or directory>", { "anetPath": "/abs/anet", "identity": "name" }]]
//
// The helpers are in lib.js. The files of a plugin directory are generated
// from deploy/plugins/src/opencode/ of the ANet repository; edit them there.

import { applyConfig, readOptions, root, ROOT_VARIABLE, settingsEnvironment } from "./lib.js"

export default {
  id: "anet",
  server: async (input, options) => {
    if (process.platform === "win32") {
      // anet has no Windows build, and the launchers are POSIX shell scripts.
      try {
        await input?.client?.app?.log?.({
          body: { service: "anet", level: "warn", message: "The anet plugin supports Linux and macOS only; it is not active." },
        })
      } catch {}
      return {}
    }
    const settings = readOptions(options)
    return {
      config: async (cfg) => {
        applyConfig(cfg, settings)
      },
      "shell.env": async (_input, output) => {
        output.env[ROOT_VARIABLE] = root
        Object.assign(output.env, settingsEnvironment(settings))
      },
    }
  },
}

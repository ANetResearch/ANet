// Tests of the generated OpenCode plugin. Run by deploy/plugins/tests/test_opencode.py:
//   PLUGIN_DIR=<generated plugin directory> node --test test.mjs
// Not part of the plugin package.

import assert from "node:assert/strict"
import fs from "node:fs"
import path from "node:path"
import { pathToFileURL } from "node:url"
import { test } from "node:test"

const dir = fs.realpathSync(path.resolve(process.env.PLUGIN_DIR))
const lib = await import(pathToFileURL(path.join(dir, "lib.js")))
const server = await import(pathToFileURL(path.join(dir, "server.js")))

test("server.js exports the default entry only", () => {
  assert.deepEqual(Object.keys(server), ["default"])
  assert.equal(server.default.id, "anet")
  assert.equal(typeof server.default.server, "function")
  assert.equal("tui" in server.default, false)
})

test("the plugin root is the directory of the files", () => {
  assert.equal(lib.root, dir)
})

test("options: only non-empty strings are settings", () => {
  assert.deepEqual(lib.readOptions(undefined), {})
  assert.deepEqual(lib.readOptions(null), {})
  assert.deepEqual(lib.readOptions([]), {})
  assert.deepEqual(lib.readOptions("x"), {})
  assert.deepEqual(lib.readOptions({ anetPath: 1, identity: "  " }), {})
  assert.deepEqual(lib.readOptions({ anetPath: " /opt/anet ", identity: "work", other: "x" }), { anetPath: "/opt/anet", identity: "work" })
})

test("settings become the variables of the runner", () => {
  assert.deepEqual(lib.settingsEnvironment({}), {})
  assert.deepEqual(lib.settingsEnvironment({ anetPath: "/a", identity: "w" }), { ANET_PLUGIN_BIN: "/a", ANET_PLUGIN_IDENTITY: "w" })
})

test("shellDouble escapes the characters that are special in double quotes", () => {
  assert.equal(lib.shellDouble('a b"c$d`e\\f'), 'a b\\"c\\$d\\`e\\\\f')
})

test("applyConfig registers the server, the skills and the command", () => {
  const cfg = {}
  lib.applyConfig(cfg, { anetPath: "/opt/anet", identity: "work" })
  const entry = cfg.mcp.anet
  assert.equal(entry.type, "local")
  assert.equal(entry.enabled, true)
  assert.deepEqual(entry.command, ["sh", path.join(dir, "bin", "anet-mcp.sh")])
  assert.deepEqual(entry.environment, { ANET_PLUGIN_BIN: "/opt/anet", ANET_PLUGIN_IDENTITY: "work" })
  assert.ok(entry.timeout >= 60000)
  assert.deepEqual(cfg.skills.paths, [path.join(dir, "skills")])
  const command = cfg.command["anet-status"]
  assert.ok(command.description.length > 0)
  assert.ok(command.template.includes(path.join(dir, "bin", "anet-run.sh")))
  assert.ok(command.template.includes("!`ANET_PLUGIN_BIN='/opt/anet' ANET_PLUGIN_IDENTITY='work' sh "))
  assert.equal(command.template.includes("${ANET_PLUGIN_ROOT}"), false)
  assert.ok(command.template.includes("$ARGUMENTS"))
})

test("applyConfig without settings sets no environment", () => {
  const cfg = {}
  lib.applyConfig(cfg, {})
  assert.equal("environment" in cfg.mcp.anet, false)
})

test("applyConfig is idempotent and keeps what the user has", () => {
  const mine = { type: "local", command: ["anet", "mcp"], enabled: false }
  const cfg = {
    mcp: { anet: mine, other: { type: "remote", url: "https://example.org" } },
    skills: { paths: ["/x/skills"], urls: ["https://example.org/skills"] },
    command: { "anet-status": { description: "mine", template: "mine" } },
  }
  lib.applyConfig(cfg, { anetPath: "/opt/anet" })
  lib.applyConfig(cfg, { anetPath: "/opt/anet" })
  assert.equal(cfg.mcp.anet, mine)
  assert.deepEqual(Object.keys(cfg.mcp).sort(), ["anet", "other"])
  assert.deepEqual(cfg.skills.paths, ["/x/skills", path.join(dir, "skills")])
  assert.deepEqual(cfg.skills.urls, ["https://example.org/skills"])
  assert.equal(cfg.command["anet-status"].template, "mine")
})

test("applyConfig replaces a skills.paths value that is not a list", () => {
  const cfg = { skills: { paths: "oops" } }
  lib.applyConfig(cfg, {})
  assert.deepEqual(cfg.skills.paths, [path.join(dir, "skills")])
})

test("parseCommand reads the description and substitutes the root", () => {
  const raw = '---\ndescription: "Quoted text"\n---\n\nrun "${ANET_PLUGIN_ROOT}/bin/x"\n'
  const parsed = lib.parseCommand("x", raw)
  assert.equal(parsed.description, "Quoted text")
  assert.equal(parsed.template, `\nrun "${lib.shellDouble(dir)}/bin/x"\n`)
  const line = '!`sh "${ANET_PLUGIN_ROOT}/bin/anet-run.sh" --resolve`'
  const withSettings = lib.parseCommand("x", "---\ndescription: d\n---\n" + line + "\n", { anetPath: "/opt/it's/anet", identity: "work" })
  assert.equal(
    withSettings.template,
    "!`ANET_PLUGIN_BIN='/opt/it'\\''s/anet' ANET_PLUGIN_IDENTITY='work' sh \"" + lib.shellDouble(dir) + "/bin/anet-run.sh\" --resolve`\n",
  )
  assert.equal(lib.parseCommand("x", "---\ndescription: d\n---\n" + line + "\n").template.startsWith("!`sh "), true)
  // A path with replacement patterns of String.replace is inserted literally.
  const odd = lib.parseCommand("x", "---\ndescription: d\n---\nrun \"${ANET_PLUGIN_ROOT}/b\"\n", {}, "/tmp/anet$&x$'y")
  assert.equal(odd.template, "run \"/tmp/anet\\$&x\\$'y/b\"\n")
  assert.equal(lib.parseCommand("y", "no front matter").description, "y")
  assert.equal(lib.parseCommand("y", "no front matter").template, "no front matter")
})

test("the server hooks", async () => {
  const hooks = await server.default.server({}, { anetPath: "/opt/anet", identity: "work" })
  assert.deepEqual(Object.keys(hooks).sort(), ["config", "shell.env"])
  const cfg = {}
  await hooks.config(cfg)
  assert.equal(cfg.mcp.anet.environment.ANET_PLUGIN_BIN, "/opt/anet")
  const output = { env: { KEEP: "1" } }
  await hooks["shell.env"]({ cwd: "/" }, output)
  assert.deepEqual(output.env, { KEEP: "1", ANET_PLUGIN_ROOT: dir, ANET_PLUGIN_BIN: "/opt/anet", ANET_PLUGIN_IDENTITY: "work" })
})

test("the server hooks without options", async () => {
  const hooks = await server.default.server({})
  const output = { env: {} }
  await hooks["shell.env"]({}, output)
  assert.deepEqual(output.env, { ANET_PLUGIN_ROOT: dir })
})

test("the plugin is inactive on Windows", async () => {
  const original = Object.getOwnPropertyDescriptor(process, "platform")
  Object.defineProperty(process, "platform", { value: "win32" })
  try {
    let logged
    const hooks = await server.default.server({ client: { app: { log: async (m) => (logged = m) } } }, {})
    assert.deepEqual(hooks, {})
    assert.equal(logged.body.level, "warn")
    assert.deepEqual(await server.default.server({}, {}), {})
  } finally {
    Object.defineProperty(process, "platform", original)
  }
})

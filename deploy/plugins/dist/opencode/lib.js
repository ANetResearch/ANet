// Helpers of the anet plugin for OpenCode (see server.js). This file exports
// functions only, so that server.js is the single plugin entry point of the
// package: OpenCode loads the default export of server.js and nothing else.

import fs from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"

export const SERVER_NAME = "anet"
const TIMEOUT_MS = 900000
export const ROOT_VARIABLE = "ANET_PLUGIN_ROOT"

// The directory of this package. OpenCode has no variable for it.
export const root = path.dirname(fileURLToPath(import.meta.url))

/** Returns the settings as non-empty strings; any other value is ignored. */
export function readOptions(options) {
  const out = {}
  if (options === null || typeof options !== "object" || Array.isArray(options)) return out
  for (const key of ["anetPath", "identity"]) {
    const value = options[key]
    if (typeof value === "string" && value.trim() !== "") out[key] = value.trim()
  }
  return out
}

/** The environment that the runner reads (bin/anet-run.sh). */
export function settingsEnvironment(settings) {
  const env = {}
  if (settings.anetPath) env.ANET_PLUGIN_BIN = settings.anetPath
  if (settings.identity) env.ANET_PLUGIN_IDENTITY = settings.identity
  return env
}

/**
 * Escapes a path for use inside double quotes of a POSIX shell. On Windows the
 * path is written as it is: a Windows path cannot contain a double quote, and
 * the shells there (PowerShell, cmd) do not take a backslash as an escape.
 */
export function shellDouble(value, platform = process.platform) {
  if (platform === "win32") return value
  return value.replace(/[\\"$`]/g, "\\$&")
}

/** Quotes a value for a POSIX shell word. */
export function shellSingle(value) {
  return "'" + value.replaceAll("'", "'\\''") + "'"
}

/**
 * Splits a Markdown command file into its description and its template.
 *
 * The root variable is replaced by the directory of the package. OpenCode runs
 * the command lines of a template ("!`...`") without the "shell.env" hook, so
 * the plugin settings are written in front of each of them.
 */
export function parseCommand(name, raw, settings = {}, dir = root) {
  const text = raw.replace(/\r\n/g, "\n")
  const match = text.match(/^---\n([\s\S]*?)\n---\n?([\s\S]*)$/)
  let description = name
  let body = text
  if (match) {
    body = match[2]
    const line = match[1].split("\n").find((l) => l.startsWith("description:"))
    if (line) {
      let value = line.slice("description:".length).trim()
      if (value.length >= 2 && (value[0] === '"' || value[0] === "'") && value.endsWith(value[0])) {
        value = value.slice(1, -1)
      }
      if (value) description = value
    }
  }
  const assignments = Object.entries(settingsEnvironment(settings))
    .map(([key, value]) => `${key}=${shellSingle(value)} `)
    .join("")
  const template = body
    // A function, so that "$&" and similar sequences of the path are inserted literally.
    .replaceAll("${" + ROOT_VARIABLE + "}", () => shellDouble(dir))
    .split("\n")
    .map((line) => (line.startsWith("!`") ? "!`" + assignments + line.slice(2) : line))
    .join("\n")
  return { description, template }
}

function loadCommands(settings) {
  const out = {}
  const dir = path.join(root, "commands")
  let names = []
  try {
    names = fs.readdirSync(dir).filter((f) => f.endsWith(".md")).sort()
  } catch {
    return out
  }
  for (const file of names) {
    const name = file.slice(0, -".md".length)
    out[name] = parseCommand(name, fs.readFileSync(path.join(dir, file), "utf8"), settings)
  }
  return out
}

/** Adds the anet components to an OpenCode configuration object, in place. */
export function applyConfig(cfg, settings, commands = loadCommands(settings)) {
  cfg.mcp ??= {}
  // An entry that exists already, for example the one that `anet agents wire
  // opencode` writes, belongs to the user and is not replaced.
  if (!Object.hasOwn(cfg.mcp, SERVER_NAME)) {
    const entry = {
      type: "local",
      // bin/anet-mcp on Linux and macOS; on Windows the same path selects bin/anet-mcp.cmd.
      command: [path.join(root, "bin", "anet-mcp")],
      enabled: true,
      timeout: TIMEOUT_MS,
    }
    const environment = settingsEnvironment(settings)
    if (Object.keys(environment).length > 0) entry.environment = environment
    cfg.mcp[SERVER_NAME] = entry
  }
  cfg.skills ??= {}
  const skills = path.join(root, "skills")
  const paths = Array.isArray(cfg.skills.paths) ? cfg.skills.paths : []
  cfg.skills.paths = paths.includes(skills) ? paths : [...paths, skills]
  cfg.command ??= {}
  for (const [name, command] of Object.entries(commands)) {
    if (!Object.hasOwn(cfg.command, name)) cfg.command[name] = command
  }
  return cfg
}

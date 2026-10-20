// The anet plugin for DeepSeek Harness (DSH): settings and bundle directory.
//
// The bundle layer (cordis.patch.yml) mounts this module and the MCP client row
// that starts bin/anet-mcp.sh. This module does two things:
//
//   1. It provides the service `anetPlugin` ({ root, settings }) to the MCP row,
//      because a patch has no variable for the directory of the bundle.
//   2. It applies the two settings, anetPath and identity, to the environment of
//      the DSH process (ANET_PLUGIN_BIN, ANET_PLUGIN_IDENTITY), so that the MCP
//      server and the commands of the skills reach the same executable and
//      identity (bin/anet-run.sh reads them). The previous values are restored
//      when the plugin is disposed.
//
// The skills are registered by skills.js, a second row of the patch, so that
// the MCP server also works in a profile without a skill registry.
// The module depends on nothing but Node.js.

import { dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

export const name = 'dsh-anet'

export const SETTINGS = Object.freeze([
  ['anetPath', 'ANET_PLUGIN_BIN'],
  ['identity', 'ANET_PLUGIN_IDENTITY'],
])

const bundleRoot = dirname(fileURLToPath(import.meta.url))

// The effective settings: a non-empty value of the plugin row wins over the
// environment variable of the same setting. Unknown keys are errors, so that a
// misspelled setting does not pass silently.
export function resolveSettings(config, env) {
  const given = config ?? {}
  if (typeof given !== 'object' || Array.isArray(given)) throw new TypeError('dsh-anet: the plugin configuration must be a mapping')
  const known = new Set(SETTINGS.map(([key]) => key))
  for (const key of Object.keys(given)) {
    if (!known.has(key)) throw new Error(`dsh-anet: unknown setting "${key}" (expected ${[...known].join(', ')})`)
  }
  const settings = {}
  for (const [key, variable] of SETTINGS) {
    const value = given[key]
    if (value !== undefined && value !== null && typeof value !== 'string') throw new TypeError(`dsh-anet: setting "${key}" must be a string`)
    settings[key] = value || env[variable] || ''
  }
  return settings
}

// Sets the environment variables of the effective settings and returns the
// function that restores the previous state.
export function applyEnvironment(settings, env) {
  const previous = new Map()
  for (const [key, variable] of SETTINGS) {
    if (!settings[key] || env[variable] === settings[key]) continue
    previous.set(variable, { before: env[variable], applied: settings[key] })
    env[variable] = settings[key]
  }
  return () => {
    for (const [variable, { before, applied }] of previous) {
      // Restore only a value that this plugin set; a later change by another owner stays.
      if (env[variable] !== applied) continue
      if (before === undefined) delete env[variable]
      else env[variable] = before
    }
  }
}

export function apply(ctx, config) {
  const settings = resolveSettings(config, process.env)
  if (process.platform === 'win32') {
    ctx.logger?.warn?.('dsh-anet: the launchers of this plugin are POSIX shell scripts and do not run on Windows; the MCP server of this plugin cannot start')
  }
  ctx.provide('anetPlugin', { root: bundleRoot, settings })
  ctx.on('dispose', applyEnvironment(settings, process.env))
}

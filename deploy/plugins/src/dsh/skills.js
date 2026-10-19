// The anet plugin for DeepSeek Harness (DSH): skills.
//
// A bundle does not load skills by itself. This module registers the files
// skills/<name>/SKILL.md of the bundle with the skill registry of DSH
// (`ctx.skills`). The token ${DSH_BUNDLE_DIR} in the texts becomes the
// absolute directory of the bundle, because DSH has no variable for it.
//
// The module depends on nothing but Node.js and the skill registry of DSH.

import { readFileSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

export const name = 'dsh-anet-skills'
export const inject = ['skills']

export const ROOT_TOKEN = '${DSH_BUNDLE_DIR}'

const bundleRoot = dirname(fileURLToPath(import.meta.url))

// Escapes text for the inside of a double-quoted shell word.
export function shellDoubleQuoted(text) {
  return text.replace(/[\\"$`]/g, '\\$&')
}

// Splits a SKILL.md into its front matter fields (single-line `key: value`,
// the value optionally a JSON string) and its body.
export function parseSkill(text, origin = 'SKILL.md') {
  const match = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?([\s\S]*)$/.exec(text)
  if (!match) throw new Error(`${origin}: missing front matter`)
  const fields = {}
  for (const line of match[1].split(/\r?\n/)) {
    const pair = /^([A-Za-z][A-Za-z0-9_-]*):\s*(.*)$/.exec(line)
    if (!pair) continue
    let value = pair[2].trim()
    if (value.startsWith('"')) value = JSON.parse(value)
    fields[pair[1]] = value
  }
  for (const key of ['name', 'description']) {
    if (!fields[key]) throw new Error(`${origin}: front matter lacks ${key}`)
  }
  return { fields, body: match[2] }
}

// Reads skills/<name>/SKILL.md of the bundle, with the root token replaced.
export function loadSkills(root = bundleRoot) {
  const skillsDir = join(root, 'skills')
  const quoted = shellDoubleQuoted(root)
  const skills = []
  for (const entry of readdirSync(skillsDir, { withFileTypes: true }).sort((a, b) => (a.name < b.name ? -1 : 1))) {
    if (!entry.isDirectory()) continue
    const path = join(skillsDir, entry.name, 'SKILL.md')
    const { fields, body } = parseSkill(readFileSync(path, 'utf8'), path)
    skills.push({
      name: fields.name,
      description: fields.description,
      content: body.split(ROOT_TOKEN).join(quoted),
      source: 'plugin',
      path,
      resourceBase: { kind: 'directory', path: join(skillsDir, entry.name) },
    })
  }
  return skills
}

export function apply(ctx) {
  for (const skill of loadSkills()) ctx.skills.register(skill)
}

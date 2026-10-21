// Checks of the JavaScript of the dsh target; run by tests/test_dsh.py:
//   node dsh_check.mjs <generated bundle directory>
// The modules import nothing but Node.js, so they load without DSH.

import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const bundle = process.argv[2]
const plugin = await import(pathToFileURL(join(bundle, 'index.js')).href)
const skills = await import(pathToFileURL(join(bundle, 'skills.js')).href)

// resolveSettings: row value wins, then the environment, then empty; strict keys.
assert.deepEqual(plugin.resolveSettings({}, {}), { anetPath: '', identity: '' })
assert.deepEqual(plugin.resolveSettings(undefined, { ANET_PLUGIN_BIN: '/a', ANET_PLUGIN_IDENTITY: 'e' }), { anetPath: '/a', identity: 'e' })
assert.deepEqual(plugin.resolveSettings({ anetPath: '/b', identity: '' }, { ANET_PLUGIN_BIN: '/a', ANET_PLUGIN_IDENTITY: 'e' }), { anetPath: '/b', identity: 'e' })
assert.throws(() => plugin.resolveSettings({ identty: 'x' }, {}), /unknown setting "identty"/)
assert.throws(() => plugin.resolveSettings({ identity: 3 }, {}), /must be a string/)
assert.throws(() => plugin.resolveSettings([], {}), /mapping/)

// applyEnvironment: sets the effective values and restores the previous state.
{
  const env = { ANET_PLUGIN_IDENTITY: 'old', OTHER: '1' }
  const restore = plugin.applyEnvironment({ anetPath: '/x/anet', identity: 'new' }, env)
  assert.equal(env.ANET_PLUGIN_BIN, '/x/anet')
  assert.equal(env.ANET_PLUGIN_IDENTITY, 'new')
  restore()
  assert.deepEqual(env, { ANET_PLUGIN_IDENTITY: 'old', OTHER: '1' })
  const empty = {}
  plugin.applyEnvironment({ anetPath: '', identity: '' }, empty)()
  assert.deepEqual(empty, {})
  // A value that another owner set after this plugin stays when this plugin is disposed.
  const shared = {}
  const first = plugin.applyEnvironment({ anetPath: '', identity: 'alpha' }, shared)
  shared.ANET_PLUGIN_IDENTITY = 'beta'
  first()
  assert.deepEqual(shared, { ANET_PLUGIN_IDENTITY: 'beta' })
}

// apply: provides the service, applies the environment, restores it on dispose.
{
  const saved = { bin: process.env.ANET_PLUGIN_BIN, id: process.env.ANET_PLUGIN_IDENTITY }
  delete process.env.ANET_PLUGIN_BIN
  delete process.env.ANET_PLUGIN_IDENTITY
  const provided = {}
  const handlers = {}
  const ctx = {
    logger: { warn() {} },
    provide: (key, value) => { provided[key] = value },
    on: (event, fn) => { handlers[event] = fn },
  }
  plugin.apply(ctx, { identity: 'dshreq' })
  assert.equal(provided.anetPlugin.root, bundle)
  assert.deepEqual(provided.anetPlugin.settings, { anetPath: '', identity: 'dshreq' })
  assert.equal(process.env.ANET_PLUGIN_IDENTITY, 'dshreq')
  assert.equal(process.env.ANET_PLUGIN_BIN, undefined)
  handlers.dispose()
  assert.equal(process.env.ANET_PLUGIN_IDENTITY, undefined)
  if (saved.bin !== undefined) process.env.ANET_PLUGIN_BIN = saved.bin
  if (saved.id !== undefined) process.env.ANET_PLUGIN_IDENTITY = saved.id
}

// shellDoubleQuoted and parseSkill.
assert.equal(skills.shellDoubleQuoted('a"b$c`d\\e f', 'linux'), 'a\\"b\\$c\\`d\\\\e f')
// On Windows the bundle path is written as it is (PowerShell and cmd take no backslash escape).
assert.equal(skills.shellDoubleQuoted('C:\\Users\\a b\\dsh-anet', 'win32'), 'C:\\Users\\a b\\dsh-anet')
{
  const { fields, body } = skills.parseSkill('---\nname: x-y\ndescription: "A \\"quoted\\" word: ok"\nplain: value here\n---\n\nBody\n')
  assert.deepEqual(fields, { name: 'x-y', description: 'A "quoted" word: ok', plain: 'value here' })
  assert.equal(body, '\nBody\n')
}
assert.throws(() => skills.parseSkill('no front matter'), /missing front matter/)
assert.throws(() => skills.parseSkill('---\nname: x\n---\nbody'), /lacks description/)

// loadSkills on the generated bundle: three skills, token replaced, base directory set.
{
  const loaded = skills.loadSkills(bundle)
  assert.deepEqual(loaded.map((s) => s.name), ['anet', 'anet-setup', 'anet-status'])
  for (const s of loaded) {
    assert.ok(s.description.length > 0)
    assert.ok(!s.content.includes(skills.ROOT_TOKEN), `${s.name} still contains the root token`)
    assert.equal(s.source, 'plugin')
    assert.equal(s.resourceBase.kind, 'directory')
    assert.ok(s.resourceBase.path.endsWith(s.name))
  }
  const setup = loaded.find((s) => s.name === 'anet-setup')
  assert.ok(setup.content.includes(`sh "${bundle}/bin/anet-run.sh"`))
  const status = loaded.find((s) => s.name === 'anet-status')
  assert.ok(status.content.includes(`sh "${bundle}/bin/anet-run.sh"`))
  assert.ok(status.content.includes(`"${bundle}/bin/anet-run.cmd"`))
}

// A root path with characters that are special in double quotes stays one word.
{
  const root = mkdtempSync(join(tmpdir(), 'dsh-check-'))
  const odd = join(root, 'a "b" $c')
  mkdirSync(join(odd, 'skills', 'one'), { recursive: true })
  writeFileSync(join(odd, 'skills', 'one', 'SKILL.md'), '---\nname: one\ndescription: d\n---\nsh "${DSH_BUNDLE_DIR}/bin/anet-run.sh"\n')
  const [skill] = skills.loadSkills(odd)
  assert.equal(skill.content, `sh "${odd.replace(/[\\"$`]/g, '\\$&')}/bin/anet-run.sh"\n`)
}

// apply registers every skill with the registry.
{
  const registered = []
  skills.apply({ skills: { register: (s) => registered.push(s.name) } })
  assert.deepEqual(registered, ['anet', 'anet-setup', 'anet-status'])
}

console.log('dsh_check: ok')

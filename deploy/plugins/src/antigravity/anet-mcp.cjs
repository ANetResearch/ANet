'use strict'
// Starts the MCP server of the local anet node for Google Antigravity.
//
// Antigravity expands the plugin root only in the arguments of an MCP server,
// not in its command, and starts the command "node" with its own copy of
// Node.js when none is on PATH. mcp_config.json therefore runs this file with
// "node", and this file starts the launcher of the platform from its own
// directory: bin/anet-mcp.sh with /bin/sh on Linux and macOS, and
// bin/anet-mcp.cmd with cmd.exe on Windows. Both apply the plugin settings in
// ANET_PLUGIN_BIN and ANET_PLUGIN_IDENTITY (see anet-run.sh and anet-run.cmd).
// Nothing of the working directory or of the workspace is run.
const { spawn } = require('child_process')
const path = require('path')

function fail(message) {
  process.stderr.write('anet-mcp: ' + message + '\n')
  process.exit(1)
}

let command
let args
const options = { stdio: 'inherit', windowsHide: true }
if (process.platform === 'win32') {
  const launcher = path.join(__dirname, 'anet-mcp.cmd')
  // The path is passed to cmd.exe in double quotes, where % and " keep a meaning.
  if (/[%"\r\n]/.test(launcher)) fail('the plugin directory ' + __dirname + ' contains % or ", which cmd.exe cannot take in a path')
  const systemRoot = process.env.SystemRoot || process.env.windir
  if (!systemRoot || !path.win32.isAbsolute(systemRoot)) fail('SystemRoot is not set to an absolute path')
  command = path.win32.join(systemRoot, 'System32', 'cmd.exe')
  args = ['/d', '/s', '/c', '""' + launcher + '""']
  options.windowsVerbatimArguments = true
} else {
  command = '/bin/sh'
  args = [path.join(__dirname, 'anet-mcp.sh')]
}

const child = spawn(command, args, options)
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  process.on(signal, () => {
    try {
      child.kill(signal)
    } catch (_) {
      // The child has already exited.
    }
  })
}
child.on('error', (err) => fail(err.message))
child.on('exit', (code) => {
  process.exit(code === null ? 1 : code)
})

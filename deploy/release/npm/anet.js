#!/usr/bin/env node
// anet launcher of the npm package @agentnetwork/anet.
//
// The package carries no binary. The build of this platform is in the
// package @agentnetwork/anet-<os>-<cpu> (an optional dependency; <os> and
// <cpu> are npm's names: linux, darwin or win32, x64 or arm64), as the
// file dl/anet-<goos>-<goarch>.gz next to the signed release manifest
// dl/release.json and its signature, exactly as the download hosts of the
// project serve them. Before the first run of a version the launcher
// checks, and stops on any failure:
//
//   1. release.json is signed by the anet release key (the allowed_signers
//      file of this package, namespace anet-release@agentnetwork.org.cn)
//      and names that key in key_fingerprint;
//   2. its version is the version of this package;
//   3. the sha256 and size of the .gz, and the sha256 of the binary inside
//      it, are the ones it names.
//
// The checked binary is written to the cache directory of the user (anet,
// or anet.exe on Windows), with the manifest and its signature beside it
// (<binary>.release.json and <binary>.release.json.sig, as install.sh and
// install.ps1 keep them, so that `anet doctor` can check them again), and is
// run with the arguments of the launcher. Later runs compare size and
// modification time with the record of the first check, and check again
// when they differ. On Windows a cached binary that is running cannot be
// replaced, only renamed: it is renamed aside (<binary>.old-<ms>) and
// removed by a later run once nothing runs from it.
//
// Nothing here depends on another package. Node 18 or later.

"use strict"

const crypto = require("node:crypto")
const fs = require("node:fs")
const os = require("node:os")
const path = require("node:path")
const zlib = require("node:zlib")
const { spawnSync } = require("node:child_process")

const NAMESPACE = "anet-release@agentnetwork.org.cn"
const SCOPE = "@agentnetwork"

function fail(message) {
  process.stderr.write(`anet (npm): ${message}\n`)
  process.exit(1)
}

// --- SSHSIG (the format of ssh-keygen -Y sign), Ed25519 only ---------------

function reader(buf) {
  let off = 0
  return {
    raw(n) {
      if (off + n > buf.length) throw new Error("truncated")
      const b = buf.subarray(off, off + n)
      off += n
      return b
    },
    u32() {
      return this.raw(4).readUInt32BE(0)
    },
    str() {
      return this.raw(this.u32())
    },
    rest() {
      return buf.length - off
    },
  }
}

function sshString(b) {
  const len = Buffer.alloc(4)
  len.writeUInt32BE(b.length, 0)
  return Buffer.concat([len, b])
}

function parseKeyBlob(blob) {
  const r = reader(blob)
  const type = r.str().toString()
  const key = r.str()
  if (type !== "ssh-ed25519" || key.length !== 32 || r.rest() !== 0) throw new Error("not an ssh-ed25519 key")
  return key
}

function parseSignature(armored) {
  const text = armored.toString("utf8").trim()
  const begin = "-----BEGIN SSH SIGNATURE-----"
  const end = "-----END SSH SIGNATURE-----"
  if (!text.startsWith(begin) || !text.endsWith(end)) throw new Error("not an SSH signature")
  const body = text.slice(begin.length, text.length - end.length).replace(/\s+/g, "")
  if (!/^[A-Za-z0-9+/]+=*$/.test(body)) throw new Error("bad base64")
  const r = reader(Buffer.from(body, "base64"))
  if (r.raw(6).toString() !== "SSHSIG") throw new Error("bad magic")
  if (r.u32() !== 1) throw new Error("unsupported version")
  const pub = parseKeyBlob(r.str())
  const namespace = r.str().toString()
  const reserved = r.str()
  const hashAlg = r.str().toString()
  const sigBlob = reader(r.str())
  if (r.rest() !== 0) throw new Error("trailing bytes")
  if (reserved.length !== 0) throw new Error("reserved field is not empty")
  if (hashAlg !== "sha512" && hashAlg !== "sha256") throw new Error(`unsupported hash algorithm ${hashAlg}`)
  if (sigBlob.str().toString() !== "ssh-ed25519") throw new Error("not an ssh-ed25519 signature")
  const sig = sigBlob.str()
  if (sig.length !== 64 || sigBlob.rest() !== 0) throw new Error("bad signature blob")
  return { pub, namespace, hashAlg, sig }
}

// allowedKeys: the keys that allowed_signers lists for the namespace.
function allowedKeys(text, namespace) {
  const keys = []
  for (const line of text.split("\n")) {
    const l = line.trim()
    if (l === "" || l.startsWith("#")) continue
    const m = /^(\S+)\s+namespaces="([^"]*)"\s+ssh-ed25519\s+([A-Za-z0-9+/]+=*)(\s.*)?$/.exec(l)
    if (!m) throw new Error(`allowed_signers: unreadable line: ${l}`)
    if (m[2].split(",").includes(namespace)) keys.push(parseKeyBlob(Buffer.from(m[3], "base64")))
  }
  return keys
}

function fingerprint(key) {
  const blob = Buffer.concat([sshString(Buffer.from("ssh-ed25519")), sshString(key)])
  return "SHA256:" + crypto.createHash("sha256").update(blob).digest("base64").replace(/=+$/, "")
}

// verifySignature returns the fingerprint of the key that signed msg.
function verifySignature(msg, armored, keys) {
  const s = parseSignature(armored)
  if (s.namespace !== NAMESPACE) throw new Error(`signature is for namespace ${s.namespace}, not ${NAMESPACE}`)
  if (!keys.some((k) => k.equals(s.pub))) throw new Error("signed by a key that is not the anet release key")
  const digest = crypto.createHash(s.hashAlg).update(msg).digest()
  const data = Buffer.concat([
    Buffer.from("SSHSIG"),
    sshString(Buffer.from(NAMESPACE)),
    sshString(Buffer.alloc(0)),
    sshString(Buffer.from(s.hashAlg)),
    sshString(digest),
  ])
  const pub = crypto.createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: s.pub.toString("base64url") }, format: "jwk" })
  if (!crypto.verify(null, data, pub, s.sig)) throw new Error("the signature does not verify")
  return fingerprint(s.pub)
}

// --- the build of this platform ---------------------------------------------

// target maps Node's names of a platform (process.platform, process.arch)
// to the build of anet for it: Go's names (goos, goarch) name the asset,
// npm's (npmOS, cpu) the package, and exeName is the file the binary is
// written to. null: anet is not built for it.
function target(platform, arch) {
  const t = {
    linux: { goos: "linux", npmOS: "linux", exeName: "anet" },
    darwin: { goos: "darwin", npmOS: "darwin", exeName: "anet" },
    win32: { goos: "windows", npmOS: "win32", exeName: "anet.exe" },
  }[platform]
  const goarch = { x64: "amd64", arm64: "arm64" }[arch]
  if (!t || !goarch) return null
  return { goos: t.goos, goarch, npmOS: t.npmOS, cpu: arch, exeName: t.exeName }
}

function platform() {
  const t = target(process.platform, process.arch)
  if (!t) fail(`anet runs on Linux, macOS and Windows (x64, arm64); this is ${process.platform} ${process.arch}`)
  return t
}

function cacheRoot() {
  if (process.platform === "win32") {
    if (process.env.LOCALAPPDATA && path.isAbsolute(process.env.LOCALAPPDATA)) return process.env.LOCALAPPDATA
    return path.join(os.homedir(), "AppData", "Local")
  }
  if (process.env.XDG_CACHE_HOME && path.isAbsolute(process.env.XDG_CACHE_HOME)) return process.env.XDG_CACHE_HOME
  if (process.platform === "darwin") return path.join(os.homedir(), "Library", "Caches")
  return path.join(os.homedir(), ".cache")
}

// asideRe matches a binary renamed aside on Windows: <exeName>.old-<ms>.
function asideRe(exeName) {
  return new RegExp("^" + exeName.replace(/\./g, "\\.") + "\\.old-[0-9]+$", "i")
}

// removeAsides removes binaries an earlier run renamed aside; one that is
// still running cannot be removed and stays for a later run.
function removeAsides(dir, exeName) {
  let names
  try {
    names = fs.readdirSync(dir, { withFileTypes: true })
  } catch {
    return
  }
  const re = asideRe(exeName)
  for (const e of names) {
    if (!e.isFile() || !re.test(e.name)) continue
    try {
      fs.unlinkSync(path.join(dir, e.name))
    } catch {
      // In use, or already gone.
    }
  }
}

// replaceFile renames tmp to exe. On Windows a running exe cannot be
// replaced: it is renamed aside first, once, and the rename is tried again.
function replaceFile(tmp, exe) {
  try {
    fs.renameSync(tmp, exe)
    return
  } catch (e) {
    if (process.platform !== "win32" || !["EPERM", "EBUSY", "EACCES"].includes(e.code)) throw e
  }
  fs.renameSync(exe, `${exe}.old-${Date.now()}`)
  fs.renameSync(tmp, exe)
}

function sha256(buf) {
  return crypto.createHash("sha256").update(buf).digest("hex")
}

// prepare checks the build and writes it to the cache; it returns the path.
function prepare() {
  const self = JSON.parse(fs.readFileSync(path.join(__dirname, "..", "package.json"), "utf8"))
  const { goos, goarch, npmOS, cpu, exeName } = platform()
  const pkgName = `${SCOPE}/anet-${npmOS}-${cpu}`
  let pkgDir
  try {
    pkgDir = path.dirname(require.resolve(`${pkgName}/package.json`, { paths: [path.join(__dirname, "..")] }))
  } catch {
    fail(`the package ${pkgName} is not installed. It is an optional dependency of ${self.name}; install again without --omit=optional (or --no-optional).`)
  }
  const asset = `anet-${goos}-${goarch}`
  const dir = path.join(cacheRoot(), "anet", "npm", self.version)
  const exe = path.join(dir, exeName)
  const record = path.join(dir, "anet.npm-check")
  if (process.platform === "win32") removeAsides(dir, exeName)
  try {
    const st = fs.statSync(exe)
    if (fs.readFileSync(record, "utf8") === `${st.size} ${st.mtimeMs}\n`) return exe
  } catch {
    // Not prepared yet, or changed: check and write it again.
  }

  const dl = path.join(pkgDir, "dl")
  const raw = fs.readFileSync(path.join(dl, "release.json"))
  const sig = fs.readFileSync(path.join(dl, "release.json.sig"))
  let signer
  try {
    signer = verifySignature(raw, sig, allowedKeys(fs.readFileSync(path.join(__dirname, "..", "allowed_signers"), "utf8"), NAMESPACE))
  } catch (e) {
    fail(`${pkgName}/dl/release.json: ${e.message}. Nothing was run.`)
  }
  const m = JSON.parse(raw.toString("utf8"))
  if (m.schema !== "anet-release/1") fail(`release.json: schema ${m.schema} is not one this launcher reads`)
  if (m.key_fingerprint !== signer) fail("release.json: key_fingerprint does not name the key that signed it")
  if (m.version !== self.version) fail(`release.json is version ${m.version}, the package is ${self.version}`)
  const a = m.assets && m.assets[asset]
  if (!a || a.variant !== "default") fail(`release ${m.version} has no default build ${asset}`)
  const gz = fs.readFileSync(path.join(dl, `${asset}.gz`))
  if (gz.length !== a.gz_size || sha256(gz) !== a.gz_sha256) fail(`sha256 mismatch for ${asset}.gz. Nothing was run.`)
  const bin = zlib.gunzipSync(gz)
  if (sha256(bin) !== a.sha256) fail(`sha256 mismatch for ${asset}. Nothing was run.`)

  // Windows has no mode bits: the cache directory is private through the
  // profile it is in.
  const win = process.platform === "win32"
  fs.mkdirSync(dir, win ? { recursive: true } : { recursive: true, mode: 0o700 })
  const tmp = `${exe}.${process.pid}.tmp`
  fs.writeFileSync(tmp, bin, win ? {} : { mode: 0o755 })
  replaceFile(tmp, exe)
  fs.writeFileSync(path.join(dir, `${exeName}.release.json`), raw)
  fs.writeFileSync(path.join(dir, `${exeName}.release.json.sig`), sig)
  const st = fs.statSync(exe)
  fs.writeFileSync(record, `${st.size} ${st.mtimeMs}\n`)
  return exe
}

function main() {
  const exe = prepare()
  const args = process.argv.slice(2)
  if (typeof process.execve === "function") {
    // Replace this process, so that a long-running command (anet mcp, anet
    // up --foreground) is not a child of Node.
    process.execve(exe, [exe, ...args], process.env)
  }
  // Windows has no execve: the binary runs as a child, and its status is
  // this process's.
  const r = spawnSync(exe, args, { stdio: "inherit" })
  if (r.error) fail(`could not run ${exe}: ${r.error.message}`)
  if (r.signal) {
    // The child ended by a signal: end this process by the same one where
    // signals exist, and with status 1 where one cannot be sent.
    if (process.platform !== "win32") {
      try {
        process.kill(process.pid, r.signal)
      } catch {
        // Fall through to the exit below.
      }
    }
    process.exit(1)
  }
  process.exit(r.status ?? 1)
}

if (require.main === module) main()

module.exports = { allowedKeys, verifySignature, fingerprint, parseSignature, target, asideRe }

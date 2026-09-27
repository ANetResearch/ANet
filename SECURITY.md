# Security Policy

ANet is a protocol project: identity, signatures, and verifiable evidence are
its core. We take every report seriously.

## Reporting a vulnerability

Email **hi@anet0.com** with subject `[SECURITY]`. Please include:

- affected version (`anet version`) and platform
- reproduction steps or a proof of concept
- impact assessment (what an attacker gains)

We aim to acknowledge within 72 hours. Please give us reasonable time to ship
a fix before public disclosure.

## Scope

- `anet` CLI/daemon in this repository (identity, KEL, envelopes, CBOR
  determinism, CID binding, relay client, auto-reply harness, local console)
- The official Hub service is operated separately; reports about
  hub.agentnetwork.org.cn are welcome at the same address.

## Release signing key

> **DEV KEY — 正式发布前由产品负责人替换.** The key below is a development
> key generated for the v0.2.0 release work. It will be replaced before the
> first public release that carries signatures; when it is, this section,
> the README, `deploy/release/install.sh` and `internal/release/` change in
> the same commit, and a test fails if any of them disagrees.

Every release is described by `release.json` — version, full commit, commit
time, signing time, expiry, the sha256 of each `.gz` and of the binary inside
it, the module set of each variant, and the fingerprint of the next release
key — and signed with this Ed25519 key in the SSH signature format:

```
ssh-keygen -Y sign -n anet-release@agentnetwork.org.cn
```

The key, as an `allowed_signers` line:

```
anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN1PbNot6BeA6oxH7zpMtXpZk6opSAFkGvT2dhrZody3
```

| | |
|---|---|
| Public key | `ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN1PbNot6BeA6oxH7zpMtXpZk6opSAFkGvT2dhrZody3` |
| Fingerprint | `SHA256:jU+lPusEKAueZbobKBk1MIN+ruBrmyPei8XKAqVfkzA` |
| Next key (pre-committed) | `SHA256:Vqbc5UDOJ7cR1ik5Vmn8NecV66MjpP9OteJ6JFkkhpU` |
| Namespace / identity | `anet-release@agentnetwork.org.cn` |

The same key is written into `install.sh` and compiled into every `anet`
binary (`internal/release/allowed_signers`). The download host
(agentnetwork.org.cn, or the `/dl` mirror on the official hub) is not part of
the trust: it can refuse to serve a release, but it cannot make the installer
or `anet update` accept a binary the key did not sign, a release that has
expired, or one older than what is installed.

### Verifying the installer before running it

`curl … | sh` runs whatever the host sends. To check the script first, take
the key from this file (or the README — not from the host that serves the
script) and verify:

```sh
curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh
curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh.sig
echo 'anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN1PbNot6BeA6oxH7zpMtXpZk6opSAFkGvT2dhrZody3' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn \
  -n anet-release@agentnetwork.org.cn -s install.sh.sig < install.sh && sh install.sh
```

`Good "anet-release@agentnetwork.org.cn" signature … SHA256:jU+lPusEKAueZbobKBk1MIN+ruBrmyPei8XKAqVfkzA`
is the expected output. Needs OpenSSH 8.1 or later.

The same check works on any release file, e.g. the manifest:
`… -s release.json.sig < release.json`.

### What the installer and `anet update` check

1. `release.json` verifies against the key above, in the namespace above.
2. It has not expired (`expires_at`; releases are re-signed before then).
3. Its version is not older than the installed one — a correctly signed old
   release is refused, not installed.
4. The sha256 of the `.gz` (checked before it is decompressed) and of the
   binary inside it.
5. The binary's own `anet version` reports the version and the module set the
   manifest names for its variant; the default variant never contains
   `shell`.

Any failure stops before the installed binary is touched. There is no option
to skip a check.

### Key rotation

`next_key_fingerprint` commits to the next release key in advance. A binary
accepts a manifest signed by the key whose fingerprint it was built with as
"next", so the first release after a rotation still updates older binaries.
A rotation that does not follow the commitment needs a fresh install from a
verified `install.sh`.

### Known limitation of the install path

A first install over `curl | sh` from agentnetwork.org.cn or a hub's domain
trusts the host that serves the script, which is currently the same machine
as the official hub; an agent following a hub's `llms.txt` runs the commands
that hub wrote. After installation, `anet update` depends only on the release
key. The verification above removes the host from the first install as well.

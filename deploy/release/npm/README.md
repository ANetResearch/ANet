# npm packages of a release

`build.py` turns a signed release directory into the npm packages of the organisation `@agentnetwork`. The packages carry the release exactly as the download hosts serve it, so that npm and the content delivery networks for npm are further ways to obtain the same signed files:

| Package | Contents | Used by |
| --- | --- | --- |
| `@agentnetwork/anet-<os>-<cpu>`, `@agentnetwork/anet-shell-<os>-<cpu>` | `dl/release.json`, `dl/release.json.sig`, `dl/<asset>.gz` | `install.sh`, `install.ps1` and `anet update`, which read `dl/` through npmmirror, jsDelivr and unpkg after the project hosts (`mirror_bases` in `install.sh`, `Get-AnetBases` in `install.ps1`, `release.MirrorBases`); the launcher |
| `@agentnetwork/anet` | `bin/anet` (the launcher, `anet.js`), `allowed_signers`, `install.sh`, `install.sh.sig`, `install.ps1`, `install.ps1.sig` | `npm install --global @agentnetwork/anet` |

`<os>` and `<cpu>` are the npm names of the operating system and the architecture: `linux`, `darwin` or `win32`, and `x64` or `arm64`. The archive in `dl/` keeps the asset name of the release (`anet-windows-amd64.gz` in `@agentnetwork/anet-win32-x64`). On Windows the launcher writes `anet.exe` to `%LOCALAPPDATA%\anet\npm\<version>\`. The launcher verifies the signature of `release.json` with the key in `allowed_signers` (a copy of `internal/release/allowed_signers`) and the SHA-256 of the archive and of the binary before it runs anything; see the comment at the top of `anet.js` and `docs/intro/install.rst`.

## Publishing a release

1. Collect the release directory: `release.json`, `release.json.sig`, `install.sh`, `install.sh.sig`, `install.ps1`, `install.ps1.sig` and every `.gz` that `release.json` names, as `build-release.sh` writes them (or as `https://agentnetwork.org.cn/dl/` and the site root serve them).
2. `python3 deploy/release/npm/build.py --dist DIR --out OUT`. It verifies the three signatures with `ssh-keygen` and every archive against the manifest, and writes one directory per package.
3. Publish the build packages first and `@agentnetwork/anet` last, so that its optional dependencies exist when it appears:

   ```sh
   for d in OUT/@agentnetwork__anet-*; do (cd "$d" && npm publish --access public); done
   (cd OUT/@agentnetwork__anet && npm publish --access public)
   ```

4. Ask npmmirror to synchronise each package (`curl -X PUT https://registry-direct.npmmirror.com/-/package/<name>/syncs`).

A published version cannot be replaced; a new installer (`install.sh`, `install.ps1`) is published with the next version of the packages. `test_npm.py` tests the builder and the launcher with a release signed by a test key.

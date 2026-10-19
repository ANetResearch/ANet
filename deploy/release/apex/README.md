# 官网上由 anet 提供的文件(发布根)

agentnetwork.org.cn 上有几条路径的内容来自本仓库,而不是官网自己的仓库:签名安装器、下载文件、
给 agent 读的 `SKILL.md` 与 `llms.txt`,以及官网运行时读取的 `site.json`。它们由本目录的 `publish.sh`
写进一个独立的发布根(缺省 `/var/www/anet-release`),nginx 用 `nginx-locations.conf` 里的 location 直接从那里提供。
官网的构建与部署从不写这个目录,所以官网部署不会覆盖安装器与这些文本。

本目录只有公开文件,不含任何密钥或令牌;`publish.sh` 不读取任何秘密(`--allowed-signers` 是公钥)。

## 1. 路径一览

| 公网路径 | 发布根中的文件 | 来源 | 缓存 |
|---|---|---|---|
| `/install.sh`、`/install.sh.sig` | `install.sh`、`install.sh.sig` | `build-release.sh` 的 `dist/`(`--dist`) | `no-cache`(`expires epoch`) / 缺省 |
| `/dl/` | `dl/`(`release.json(.sig)`、`VERSION`、`checksums.txt`、`*.gz`) | 同上,整个目录一次改名换入 | 缺省 |
| `/SKILL.md`、`/llms-full.txt` | `SKILL.md` | 本目录(`--web`) | `no-cache`(`expires epoch`) / 缺省 |
| `/llms.txt` | `llms.txt` | 本目录(`--web`) | 缺省 |
| `/site.json` | `site.json` | 本目录的 `site.json`,`publish.sh` 加上 `version`(`--web`) | `no-cache`(`expires epoch`) |
| `/zcode/marketplace.json` | `zcode/marketplace.json` | `deploy/zcode/build.py` 的输出(`--zcode`) | `no-cache`(`expires epoch`) |
| `/zcode/plugins/`、`/zcode/assets/` | `zcode/plugins/<名称>/<版本>/plugin.zip`、`zcode/assets/` | 同上;已发布的版本不替换,旧版本保留 | 缺省 |

带 `add_header` 的 location 不再继承 server 块的安全头,所以这里的 location 都不用它:`/install.sh`、
`/SKILL.md` 与 `/site.json` 用 `expires epoch` 得到 `Cache-Control: no-cache`,全部保留 server 块的安全头。

## 2. `site.json`

官网把会随 anet 变化的文案(安装命令、给 agent 的一句提示)从自己的代码里拿出来,改为运行时读取 `/site.json`:

```json
{"v": 1, "version": "0.3.3",
 "install": "<安装命令,与 SKILL.md 中的逐字相同>",
 "skill_url": "https://agentnetwork.org.cn/SKILL.md",
 "agent_hint": "<给 agent 的一句提示,与 SKILL.md 中的同一句逐字相同>"}
```

- **来源。** 本目录的 `site.json` 就是上面的文件去掉 `version`;第一行必须恰为 `{"v": 1,`。`publish.sh --web`
  把 `"version"` 插在这一行之后,值取发布根 `dl/VERSION`(同一次运行带 `--dist` 时取该 dist 的 `VERSION`)。
  所以官网写出的版本永远是 `/dl/` 正在提供、能下载到的版本,发版时不需要手改。已有 `site.json` 的发布根上,
  `--dist` 必须与 `--web` 同一次运行(单独 `--dist` 会被拒绝),否则 `site.json` 会留在已被删除的旧版本上。
- **一致性由测试保证**(`cmd/anet/apexdocs_test.go`):
  - `TestSiteJSONInstallMatchesSkill`:`install` 与 `SKILL.md` 里的安装命令代码块逐字相同,且是固定 https 与 TLS 1.2 的签名安装器;
  - `TestSiteJSONAgentHintMatchesSkill`:`agent_hint` 是原因表的 `agent_hint.join`(英文),并且 `SKILL.md` 里有同一句;
  - `TestSiteJSONCommandsInHelp`:其中每条 `anet …` 都是 `anet help --all` 列出的命令、子命令与参数;
  - `TestPublishWritesSiteJSON`、`TestNginxServesSiteJSONNoCache`:发布脚本的产物与 nginx 的缓存头。
- **改文案。** 改 `SKILL.md` 的安装命令或那句提示时,同一提交里改 `site.json`(测试会指出不一致)。
  不要在服务器上直接编辑发布根里的文件:下一次发布会覆盖它,而且它没有经过上面的测试。
- **官网怎么用。** 官网在运行时取 `/site.json`;取不到或解析失败时只显示指向 `skill_url` 的链接,不回退到内置文案。
  各字段只能作为文本显示(不得当作 HTML 插入页面);`skill_url` 只在以 `https://agentnetwork.org.cn/` 开头时
  用作链接地址。`/site.json` 与官网同源,不需要 CORS 头。

## 3. 发布

前提:发布根已存在(`install -d -m 0755 /var/www/anet-release`);`nginx-locations.conf` 的 location 已放进
官网站点的 443 server 块,`nginx -t` 通过后 reload。用通过了 `go test ./cmd/anet` 的提交发布。
发布根及其中的文件只能由运行 `publish.sh` 的账户写入,官网的部署账户与 nginx 的工作进程都不能写:
"官网部署不会覆盖安装器与这些文本"靠的是这条权限,而不是约定。

```sh
# 发版:下载文件、安装器与文本一起
deploy/release/apex/publish.sh --dist dist/ --allowed-signers internal/release/allowed_signers \
  --web deploy/release/apex

# 只改了文本:version 取发布根里已有的 dl/VERSION
deploy/release/apex/publish.sh --web deploy/release/apex

# hub 站点也提供下载文件:同一个 dist 再发一次到它的公开目录
deploy/release/apex/publish.sh --dist dist/ --allowed-signers internal/release/allowed_signers --root <hub 站点的公开目录>
```

发布 ZCode 插件市场(插件源码在 `deploy/zcode/anet`,版本号写在其 `.zcode-plugin/plugin.json`):

```sh
python3 deploy/zcode/build.py --out /tmp/anet-zcode
deploy/release/apex/publish.sh --zcode /tmp/anet-zcode
```

全部检查(文件齐全、签名、`site.json` 能加版本、`.gz` 与 `checksums.txt` 相符)在改动任何已提供的文件之前完成;
任一项不过,脚本用一句话说明原因与下一步并退出,发布根不变。每个文件各自以一次改名替换,`dl/` 整个目录一次改名换入。
发布根里还没有任何发布时,`--web` 拒绝并提示先带上 `--dist`;发布根里已有 `site.json` 时,单独的 `--dist`
拒绝并提示同时带上 `--web`(hub 站点的公开目录没有 `site.json`,不受影响)。

## 4. 发布后核对

```sh
curl -sSI https://agentnetwork.org.cn/site.json   # 200;Content-Type: application/json;恰有一行 Cache-Control: no-cache;站点的安全头仍在
curl -sSI https://agentnetwork.org.cn/install.sh  # 同上(text/x-shellscript);/SKILL.md 同上(text/markdown)
curl -sS  https://agentnetwork.org.cn/site.json   # version 与下一行相同
curl -sS  https://agentnetwork.org.cn/dl/VERSION
curl -sS  https://agentnetwork.org.cn/SKILL.md | cmp - deploy/release/apex/SKILL.md
```

若这三个路径出现两行 `Cache-Control`,是 server 块自己也设了缓存头:改 server 块,不要在这些 location 里加
`add_header`(那会丢掉安全头)。

## 5. 热修官网页面:带内容哈希的文件

官网的脚本与样式是带内容哈希的文件名(如 `/assets/<名字>-<哈希>.js`),以
`Cache-Control: public, max-age=31536000, immutable` 提供:浏览器在一年内按文件名直接使用缓存,不再向服务器验证。
安装命令与给 agent 的提示已在 `site.json` 里,改它们只需改本目录并 `publish.sh --web`,不需要动官网页面。
其余文案需要热修时:

1. **不得就地修改带内容哈希的文件。** 已缓存旧文件的访客永远看不到改动,而同一个名字对应两份内容会让排查无从下手。
2. **首选:改官网源码,按官网自己的方式重建并部署。** 内容变了,哈希与文件名随之改变,缓存自然失效。
3. **没有重建条件时:** 把热修后的文件复制为一个从未发布过的新文件名(例如在原名的哈希后加 `-hotfix1`),
   再沿引用链逐级向上改到 HTML:每一个引用了被改文件的文件,也复制为新名字并改其中的引用,直到 HTML
   (HTML 不带哈希、不长期缓存)。旧文件保留不删,已经打开的页面还会请求它们。改完后在不带缓存的浏览器
   与带着旧缓存的浏览器里各打开一次核对。
4. **`Clear-Site-Data: "cache"` 只作最后手段**,用于上述办法都无法让回访者拿到新文件的情况:
   - 只加在 HTML 响应上,不加在 `/assets/`、`/site.json`、`/dl/` 等其他响应上;
   - 启用时写下截止日期,最长 7 天,到期删除(它在每次加载 HTML 时都清空该站点的全部 HTTP 缓存,有明显的性能代价);
   - 启用前按 MDN 上该响应头的浏览器兼容表核实目标浏览器对 `"cache"` 指令的支持,不支持的浏览器上它不起作用。

## 6. 回滚

- 文本与 `site.json`:从上一个发布用的提交再运行一次 `publish.sh --web`。
- 下载文件与安装器:在上一个发布用的提交里,用该版本的 `dist/` 运行一次 `publish.sh --dist <该 dist> --web deploy/release/apex`
  (`site.json` 随之回到该版本);hub 站点的公开目录只需 `--dist`。
- nginx:用修改前备份的站点配置覆盖回去,`nginx -t` 通过后 reload。`/site.json` 不可用时官网只显示 `SKILL.md` 链接,
  不影响安装器与下载。

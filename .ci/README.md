# CI（待激活）

`ci.yml` 是本仓的 GitHub Actions 工作流。当前发布用 PAT 无 `workflow` 权限，
无法直接推送到 `.github/workflows/`。激活方式二选一：

1. 给 PAT 加 workflow 权限后：`git mv .ci/ci.yml .github/workflows/ci.yml`
2. 或在 GitHub 网页端直接创建同内容文件。

门禁：gofmt 零输出 · go vet · 全量测试 · demo 可启动 · CGO_ENABLED=0（A3）。

# anet plugins

Plugins of AgentNetwork (anet) for AI coding agents and agent hosts, version {{VERSION}}. Each plugin connects a host to a local anet node: it registers the MCP server `anet mcp`, installs the operating guide of anet as a skill, and adds a setup procedure and a status command where the host supports them.

This tree is generated. It is built from the shared sources in `deploy/plugins` of the ANet repository ({{REPO_URL}}) by `deploy/plugins/build.py`; do not edit it by hand. The same tree is committed to the ANet repository at `{{DIST_PATH}}`, so that a marketplace can name a subdirectory of that repository, and it can serve as the root of a standalone mirror repository.

| Directory | Host | Notes |
| --- | --- | --- |
{{TARGET_TABLE}}

Catalogues for marketplaces that read a manifest from the repository root are generated next to the plugin directories. The installation steps of each host are in the anet documentation: <https://docs.agentnetwork.org.cn/docs/en/latest/agents/index.html>.

## License

The files of the plugins are licensed under the Apache License 2.0 ([LICENSE](./LICENSE)). anet itself is distributed under its own license; see <{{REPO_URL}}/blob/main/LICENSE>.

#!/usr/bin/env bash
# build-all.sh — render the publication set of docs/*-zh.md to docs/site/*.html.
#
# The two new documents link to their siblings, and a published page whose
# links 404 is worse than one with no links at all — so the whole referenced
# set is rendered together rather than the two on their own.
set -euo pipefail
cd "$(dirname "$0")/../.."
B="python3 docs/site/build.py"

$B docs/DESIGN-zh.md          docs/site/design.html         "ANet 设计文档 · 2026-09-03"
$B docs/GUIDE-zh.md           docs/site/guide.html          "ANet 使用说明 · 0.1.7"
$B docs/DISTRIBUTIONS-zh.md   docs/site/distributions.html  "ANet 发行版"
$B docs/SHELL-zh.md           docs/site/shell.html          "ANet · shell 模块"
$B docs/INSTALL-DEBIAN-zh.md  docs/site/install-debian.html "ANet · Debian 接入"
$B docs/PAYMENT-zh.md         docs/site/payment.html        "ANet · 付费"
$B docs/CONTRACTS-zh.md       docs/site/contracts.html      "ANet · 五合同架构(2026-08-16 原文)"
$B docs/SUITE-TODO-zh.md      docs/site/suite-todo.html     "ANet 套件 · 现状与 TODO"
$B docs/ARCHITECTURE-zh.md    docs/site/architecture.html   "ANet · 代码架构"
$B docs/CAPABILITIES-zh.md    docs/site/capabilities.html   "ANet · 功能清单与完备程度"
$B docs/AUTO-REPLY-zh.md      docs/site/auto-reply.html     "ANet · 自动回复"
$B docs/REWRITE-from-anet3-zh.md docs/site/rewrite-from-anet3.html "ANet · 从 anet3 重写"
$B docs/site/index.md         docs/site/index.html          "ANet 文档"
echo "→ docs/site/"

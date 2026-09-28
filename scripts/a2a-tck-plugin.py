#!/usr/bin/env python3
"""a2a-tck 对 anet 本机 A2A 接口的适配层(A2A-DESIGN §17 联调行:结果记录,不作门禁)。

scripts/a2a-tck.sh 把本文件以 anet_tck_plugin.py 之名复制进 TCK 快照目录,然后:

  * 作为 pytest 插件(-p anet_tck_plugin)加载:
      - 给发往被测接口(同端口的回环地址)的每个 httpx 请求补 `Authorization: Bearer <a2a_token>`。
        a2a-tck 没有鉴权选项,连卡片都用裸 httpx.get 取;而 module/a2a 的代理卡片按设计需要
        Bearer(§11.2),这里补头,不要求接口放开卡片。令牌从文件读,不进 argv 与环境变量。
      - 按下面的 GROUPS 跳过对本机接口不适用的用例(记为 SKIPPED,并写明理由);
        `--scope protocol` 时另跳过需要对端真实作答的用例(不记录,报告里显示为 NOT TESTED)。
      - 会话结束时把跳过清单写到 ANET_TCK_SKIPS_OUT。
  * 作为命令行工具:
      groups                      打印分组清单(不需要任何第三方包)
      preflight <out_dir>         跑 TCK 之前探测卡片、鉴权与两个绑定的路由形状
      summarize <out_dir> k=v...  由 reports/compatibility.json 与跳过清单生成 run.json、summary.md

只读的环境变量(由 a2a-tck.sh 设置):
  ANET_TCK_SUT                  被测 agent 的基址 http://<a2a_addr>/a2a/v1/agents/<aid>
  ANET_TCK_TOKEN_FILE           a2a_token.txt 的路径
  ANET_TCK_SCOPE                all(缺省)| protocol
  ANET_TCK_SKIPS_OUT            跳过清单 JSON 的输出路径
  ANET_TCK_JSONRPC_SLASH_FIX    1 = 把 .../jsonrpc/ 改写成 .../jsonrpc(见 preflight 的说明)
"""

from __future__ import annotations

import json
import os
import re
import sys

from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import urlsplit


try:  # 作为命令行工具的 groups 子命令不需要 pytest
    import pytest
except ImportError:  # pragma: no cover
    pytest = None


# ---------------------------------------------------------------------------
# 分组
# ---------------------------------------------------------------------------

NA = "na"  # 不适用:永远跳过,记 SKIPPED
PEER = "peer"  # 需要对端真实作答:--scope protocol 时跳过,不记录


@dataclass(frozen=True)
class Group:
    key: str
    kind: str
    title: str
    reason: str
    req_prefixes: tuple[str, ...] = ()
    reqs: tuple[str, ...] = ()
    nodes: tuple[str, ...] = ()  # nodeid 子串
    transports: tuple[str, ...] = ()
    note: str = ""


GROUPS: tuple[Group, ...] = (
    Group(
        key="grpc",
        kind=NA,
        title="gRPC 绑定(tests/compatibility/grpc/、各文件中的 gRPC 用例、GRPC-*)",
        reason="module/a2a 只提供 JSON-RPC 与 HTTP+JSON 两个绑定,不导入 a2agrpc(A2A-DESIGN §11.1);"
        "代理卡片 supportedInterfaces 只声明这两个",
        req_prefixes=("GRPC-",),
        nodes=("tests/compatibility/grpc/",),
        transports=("grpc",),
    ),
    Group(
        key="push",
        kind=NA,
        title="推送通知配置 CRUD 与投递(PUSH-*)",
        reason="推送 4 个操作一律返回 PushNotificationNotSupportedError,代理卡片 "
        "capabilities.pushNotifications=false(§11.3、§11.5)",
        req_prefixes=("PUSH-",),
        nodes=("core_operations/test_push_notifications.py::",),
        note="'不支持时返回该错误'的用例(CORE-CAP-001、JSON-RPC -32003、HTTP 400)仍然运行",
    ),
    Group(
        key="extended-card",
        kind=NA,
        title="GetExtendedAgentCard 已配置/未配置(CARD-EXT-001、CARD-EXT-002)",
        reason="GetExtendedAgentCard 不支持,代理卡片不声明 capabilities.extendedAgentCard(§11.5)",
        reqs=("CARD-EXT-001", "CARD-EXT-002"),
        note="'不支持时返回 UnsupportedOperationError'的 CORE-CAP-003 仍然运行",
    ),
    Group(
        key="message-response",
        kind=NA,
        title="SendMessage 直接返回 Message(test_artifacts.py::TestMessageResponse,DM-MSG-001 场景)",
        reason="经本机接口的每次发送都是一个交互,task id 即 interaction id(§2 本机 A2A 接口形态、§11.5),"
        "SendMessage 总是返回 Task,不返回裸 Message",
        nodes=("core_operations/test_artifacts.py::TestMessageResponse",),
    ),
    Group(
        key="needs-peer",
        kind=PEER,
        title="需要对端真实作答的用例(发出真实消息并等待任务到终态或中断态)",
        reason="每条消息都是一次真实委派:本机接口 → hub → 对端;对端必须允许本机、并在 TCK 的超时内作答"
        "(裸 httpx 调用 5 秒,传输客户端读超时 30 秒)。按 TCK 场景断言产物/状态的用例还要求对端实现 "
        "scenarios/*.feature 的 messageId 前缀语义",
        reqs=(
            "CORE-SEND-001",
            "CORE-STREAM-001",
            "CORE-STREAM-002",
            "CORE-STREAM-003",
            "CORE-EXECUTION-MODE-001",
            "CORE-EXECUTION-MODE-002",
            "CORE-MULTI-001",
            "CORE-MULTI-001a",
            "CORE-MULTI-002",
            "CORE-MULTI-002a",
            "CORE-MULTI-003",
        ),
        nodes=(
            "core_operations/test_artifacts.py::",
            "core_operations/test_data_model.py::",
            "core_operations/test_multi_stream.py::",
            "core_operations/test_stream_ordering.py::",
            "core_operations/test_task_history.py::",
            "core_operations/test_task_lifecycle.py::",
            "core_operations/test_transport_behavior.py::TestJsonRpcFormat::",
            "core_operations/test_transport_behavior.py::TestJsonRpcServiceParams::",
            "core_operations/test_transport_behavior.py::TestJsonRpcStreaming::",
            "core_operations/test_transport_behavior.py::TestRestFormat::",
            "core_operations/test_transport_behavior.py::TestRestServiceParams::",
            "core_operations/test_transport_behavior.py::TestRestStreaming::",
            "core_operations/test_error_handling.py::TestVersionErrors::test_empty_version_treated_as_default",
            "http_json/test_http_status.py::TestHttpJsonStatusCodes::test_success_returns_2xx",
            "jsonrpc/test_sse_streaming.py::TestSseStreamingFormat::",
            "jsonrpc/test_sse_streaming.py::TestSseSubscribeToTask::test_subscribe_first_event_is_task",
        ),
    ),
)

# TCK 因卡片声明而自行跳过的用例(不由本插件处理,列出以便记录完整)。
AUTO_SKIPPED_BY_CARD: tuple[tuple[str, str], ...] = (
    ("CORE-CAP-002 与 -32004/HTTP 400 'streaming when unsupported'", "代理卡片声明 streaming: true(§11.3)"),
    ("CORE-CAP-004 必需扩展缺失", "代理卡片不声明 TCK 的 urn:a2a:tck:required-extension;x402 扩展不设为必需(§8.7)"),
    ("CARD-SIGN-*、AUTH-*、VER-CLIENT-*、VER-SERVER-001、BIND-EQUIV-* 等", "TCK 标为 not-automatable 或未提供用例,报告中为 NOT TESTED"),
)

# 结果解读时要一并记录的说明(summary.md 的"已知偏离与预期差异")。
NOTES: dict[str, str] = {
    "VER-SERVER-003": "anet 对缺省/空 A2A-Version 按 1.0 处理,是有意偏离(A2A-DESIGN §2、§11.4);"
    "TCK 只检查'返回了 result 或 error',通过不代表按 0.3 处理",
    "VER-SERVER-002": "显式非 1.x 版本(TCK 用 99.0)应返回 VersionNotSupportedError(§2、§11.4)",
    "CORE-SEND-003": "§11.5 把任意 raw FilePart 收为附件;若不按代理卡片 defaultInputModes 拒收不支持的 mediaType,"
    "此项预期失败(差异待定)",
    "CORE-CAP-003": "GetExtendedAgentCard 不支持,应返回 UnsupportedOperationError(-32004 / HTTP 400)",
    "CORE-CAP-001": "推送操作应先于任务存在性检查返回 PushNotificationNotSupportedError(TCK 用不存在的 task id 't')",
    "DM-ART-001": "anet 的产物由内核生成(anet.reply、交付物、anet.receipt、附件,§11.5),与 TCK 场景期望的产物不一一对应",
    "DM-PART-001": "同 DM-ART-001",
    "STREAM-SUB-003": "对终态任务 SubscribeToTask 应返回 UnsupportedOperationError,而不是发快照后关闭",
    "CORE-SEND-002": "对终态任务 SendMessage 应返回 UnsupportedOperationError(C35)",
    "CORE-CANCEL-002": "对终态任务 CancelTask 应返回 TaskNotCancelableError;http_json 得 400 是 A2A v1.0.1 §5.4 的状态,"
    "TCK 钉在 v1.0.0、期望 409,此项在 http_json 上预期失败(0019 §5)",
    "HTTP_JSON-STATUS-001": "错误 Content-Type 回 400 是 A2A v1.0.1 §5.4 的状态,TCK 钉在 v1.0.0、期望 415,"
    "test_content_type_not_supported_returns_415 预期失败(0019 §5)",
}

_REQ_RE = re.compile(r"\b([A-Z][A-Z0-9_]+-[A-Z]+-\d+[a-z]?)\b")
_TRANSPORT_MARKERS = ("grpc", "jsonrpc", "http_json")
_SUFFIX_TRANSPORT = (
    ("_jsonrpc", "jsonrpc"),
    ("_http_json", "http_json"),
    ("_rest", "http_json"),
    ("_grpc", "grpc"),
)
_DIR_TRANSPORT = (
    ("tests/compatibility/jsonrpc/", "jsonrpc"),
    ("tests/compatibility/http_json/", "http_json"),
    ("tests/compatibility/grpc/", "grpc"),
)


@dataclass
class ItemInfo:
    nodeid: str
    req: str | None
    transport: str | None
    extra: dict = field(default_factory=dict)


def item_info(item) -> ItemInfo:
    """Requirement id 与传输:参数化优先,其次 docstring(函数、类)、标记、函数名后缀、目录。"""
    req = None
    transport = None
    callspec = getattr(item, "callspec", None)
    if callspec is not None:
        transport = callspec.params.get("transport")
        r = callspec.params.get("requirement")
        req = getattr(r, "id", None)
    if req is None:
        for obj in (getattr(item, "function", None), getattr(item, "cls", None)):
            m = _REQ_RE.search((getattr(obj, "__doc__", None) or ""))
            if m:
                req = m.group(1)
                break
    if transport is None:
        for name in _TRANSPORT_MARKERS:
            if item.get_closest_marker(name) is not None:
                transport = name
                break
    if transport is None:
        base = item.name.split("[", 1)[0]
        for suffix, t in _SUFFIX_TRANSPORT:
            if base.endswith(suffix):
                transport = t
                break
    if transport is None:
        for prefix, t in _DIR_TRANSPORT:
            if item.nodeid.startswith(prefix):
                transport = t
                break
    return ItemInfo(item.nodeid, req, transport)


def classify(info: ItemInfo, scope: str) -> Group | None:
    for g in GROUPS:
        if g.kind == PEER and scope != "protocol":
            continue
        hit = (
            (info.transport is not None and info.transport in g.transports)
            or (info.req is not None and (info.req in g.reqs or info.req.startswith(g.req_prefixes)))
            or any(n in info.nodeid for n in g.nodes)
        )
        if hit:
            return g
    return None


# ---------------------------------------------------------------------------
# pytest 插件
# ---------------------------------------------------------------------------

_SKIPPED: list[dict] = []


def _read_token() -> str | None:
    path = os.environ.get("ANET_TCK_TOKEN_FILE")
    if not path:
        return None
    token = Path(path).read_text(encoding="utf-8").strip()
    if not token or any(c.isspace() for c in token):
        raise RuntimeError(f"{path}: not a bearer token")
    return token


def _sut_matcher():
    """同 scheme、同端口、主机是 SUT 主机或回环名:卡片里的接口 URL 用 127.0.0.1 或 localhost 都能对上。"""
    sut = urlsplit(os.environ.get("ANET_TCK_SUT", ""))
    if not sut.scheme or not sut.hostname:
        return None
    default = 443 if sut.scheme == "https" else 80
    port = sut.port or default
    hosts = {sut.hostname, "127.0.0.1", "localhost", "::1"}

    def match(url) -> bool:
        return url.scheme == sut.scheme and (url.port or default) == port and url.host in hosts

    return match


def _install_http_hooks() -> None:
    import httpx

    token = _read_token()
    match = _sut_matcher()
    slash_fix = os.environ.get("ANET_TCK_JSONRPC_SLASH_FIX") == "1"
    if match is None or (token is None and not slash_fix):
        return
    if getattr(httpx.Client.send, "_anet_tck", False):
        return
    orig_send = httpx.Client.send

    def send(self, request, *args, **kwargs):
        url = request.url
        if match(url):
            if token is not None and "authorization" not in request.headers:
                request.headers["Authorization"] = f"Bearer {token}"
            # TCK 的 JsonRpcClient 以卡片 URL 为 httpx base_url 再 POST "/",httpx 会把它拼成
            # ".../jsonrpc/"。只在 preflight 发现接口不接受带斜杠的路径时才改写(记入 run.json)。
            if slash_fix and url.path.endswith("/jsonrpc/"):
                request.url = url.copy_with(path=url.path[:-1])
        return orig_send(self, request, *args, **kwargs)

    send._anet_tck = True
    httpx.Client.send = send


if pytest is not None:

    def pytest_configure(config) -> None:
        _install_http_hooks()

    @pytest.fixture(autouse=True)
    def _anet_tck_applicability(request):
        info = item_info(request.node)
        group = classify(info, os.environ.get("ANET_TCK_SCOPE", "all"))
        if group is None:
            return
        _SKIPPED.append(
            {
                "group": group.key,
                "kind": group.kind,
                "nodeid": info.nodeid,
                "requirement": info.req,
                "transport": info.transport,
            }
        )
        if group.kind == NA and info.req and info.transport:
            # 记为 SKIPPED:与 TCK 自己"卡片未声明该能力"的跳过同一口径,不计入兼容率分母。
            try:
                from tck.requirements.registry import get_requirement_by_id

                level = get_requirement_by_id(info.req).level.value
            except Exception:
                level = "MUST"
            collector = request.getfixturevalue("compatibility_collector")
            collector.record(
                requirement_id=info.req,
                transport=info.transport,
                level=level,
                passed=False,
                errors=[],
                skipped=True,
            )
        pytest.skip(f"anet {'不适用' if group.kind == NA else '需对端'}:{group.key}(理由见 anet-skips.json / 0019)")

    def pytest_sessionfinish(session, exitstatus) -> None:
        out = os.environ.get("ANET_TCK_SKIPS_OUT")
        if not out:
            return
        Path(out).parent.mkdir(parents=True, exist_ok=True)
        Path(out).write_text(
            json.dumps({"groups": [g.__dict__ for g in GROUPS], "skipped": _SKIPPED}, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )


# ---------------------------------------------------------------------------
# 命令行:groups
# ---------------------------------------------------------------------------


def cmd_groups() -> int:
    print("不适用(永远跳过,报告中记为 SKIPPED):")
    for g in GROUPS:
        if g.kind == NA:
            print(f"  - {g.key}: {g.title}\n      理由:{g.reason}")
            if g.note:
                print(f"      注:{g.note}")
    print("仅在 --scope protocol 时跳过(报告中为 NOT TESTED):")
    for g in GROUPS:
        if g.kind == PEER:
            print(f"  - {g.key}: {g.title}\n      理由:{g.reason}")
    print("TCK 按卡片自行跳过或无自动化用例:")
    for what, why in AUTO_SKIPPED_BY_CARD:
        print(f"  - {what}:{why}")
    return 0


# ---------------------------------------------------------------------------
# 命令行:preflight
# ---------------------------------------------------------------------------


def cmd_preflight(out_dir: str) -> int:
    import httpx

    sut = os.environ["ANET_TCK_SUT"].rstrip("/")
    token = _read_token()
    auth = {"Authorization": f"Bearer {token}"} if token else {}
    ver = {"A2A-Version": "1.0"}
    out = Path(out_dir)
    res: dict = {"sut": sut, "checks": []}
    fatal = False

    def check(name: str, ok: bool, detail: str, *, required: bool = False) -> None:
        nonlocal fatal
        res["checks"].append({"name": name, "ok": ok, "required": required, "detail": detail})
        mark = "ok  " if ok else ("FAIL" if required else "warn")
        print(f"  [{mark}] {name}: {detail}")
        if required and not ok:
            fatal = True

    card_url = f"{sut}/.well-known/agent-card.json"
    with httpx.Client(timeout=10.0, trust_env=False) as c:
        try:
            r = c.get(card_url)
            check("卡片无令牌时拒绝", r.status_code == 401, f"GET 卡片(无 Bearer)→ HTTP {r.status_code}")
        except httpx.HTTPError as e:
            check("接口可达", False, f"{card_url}: {e}", required=True)
            _write(out / "preflight.json", res)
            return 2

        r = c.get(card_url, headers=auth)
        card = None
        if r.status_code == 200:
            try:
                card = r.json()
            except ValueError:
                pass
        check("带令牌取卡片", card is not None, f"HTTP {r.status_code}", required=True)
        if card is None:
            _write(out / "preflight.json", res)
            return 2
        _write(out / "agent-card.json", card)

        ifaces = {i.get("protocolBinding"): i.get("url", "") for i in card.get("supportedInterfaces") or []}
        res["interfaces"] = ifaces
        check(
            "supportedInterfaces 含 JSONRPC 与 HTTP+JSON",
            "JSONRPC" in ifaces and "HTTP+JSON" in ifaces,
            json.dumps(ifaces, ensure_ascii=False),
            required=True,
        )
        match = _sut_matcher()
        for b, u in ifaces.items():
            ok = match is not None and match(httpx.URL(u))
            check(f"{b} 接口与卡片同源", ok, f"{u}(不同源时插件不会给它补令牌)", required=b in ("JSONRPC", "HTTP+JSON"))
        caps = card.get("capabilities") or {}
        check("pushNotifications 未声明", not caps.get("pushNotifications"), f"capabilities={json.dumps(caps)[:200]}")
        check("extendedAgentCard 未声明", not caps.get("extendedAgentCard"), "")
        check("securityRequirements 存在", bool(card.get("securityRequirements")), "§11.3")

        rpc = ifaces.get("JSONRPC")
        if rpc:
            body = {"jsonrpc": "2.0", "id": 1, "method": "GetTask", "params": {"id": "anet-tck-preflight-nonexistent"}}
            for name, url in (("JSON-RPC 路径(不带斜杠)", rpc.rstrip("/")), ("JSON-RPC 路径(带斜杠)", rpc.rstrip("/") + "/")):
                try:
                    rr = c.post(url, json=body, headers={**auth, **ver}, follow_redirects=False)
                    err = None
                    try:
                        err = (rr.json() or {}).get("error")
                    except ValueError:
                        pass
                    ok = err is not None and err.get("code") == -32001
                    detail = f"HTTP {rr.status_code}, error={json.dumps(err, ensure_ascii=False)[:160]}"
                except httpx.HTTPError as e:
                    ok, detail = False, str(e)
                check(name + " → TaskNotFound(-32001)", ok, detail, required=not url.endswith("/"))
                if url.endswith("/"):
                    res["jsonrpc_trailing_slash_ok"] = ok
        rest = ifaces.get("HTTP+JSON")
        if rest:
            try:
                rr = c.get(rest.rstrip("/") + "/tasks/anet-tck-preflight-nonexistent", headers={**auth, **ver})
                ok = rr.status_code == 404
                detail = f"HTTP {rr.status_code} {rr.text[:160]!r}"
            except httpx.HTTPError as e:
                ok, detail = False, str(e)
            check("REST GET /tasks/{不存在} → 404", ok, detail)
        try:
            base = sut.split("/a2a/v1/agents/", 1)[0]
            rr = c.get(f"{base}/a2a/v1/agents", headers=auth)
            aid = sut.rsplit("/", 1)[-1]
            check("GET /a2a/v1/agents 列出该 AID", rr.status_code == 200 and aid in rr.text, f"HTTP {rr.status_code}")
        except httpx.HTTPError as e:
            check("GET /a2a/v1/agents", False, str(e))

    _write(out / "preflight.json", res)
    if "jsonrpc_trailing_slash_ok" in res:
        print(f"JSONRPC_SLASH={'ok' if res['jsonrpc_trailing_slash_ok'] else 'broken'}")
    return 2 if fatal else 0


def _write(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


# ---------------------------------------------------------------------------
# 命令行:summarize
# ---------------------------------------------------------------------------


def cmd_summarize(out_dir: str, pairs: list[str]) -> int:
    out = Path(out_dir)
    meta = dict(p.split("=", 1) for p in pairs if "=" in p)
    compat_path = out / "reports" / "compatibility.json"
    compat = json.loads(compat_path.read_text(encoding="utf-8")) if compat_path.exists() else {}
    skips_path = out / "anet-skips.json"
    skips = json.loads(skips_path.read_text(encoding="utf-8")).get("skipped", []) if skips_path.exists() else []

    per_req = compat.get("per_requirement", {})
    counts: dict[str, int] = {}
    for r in per_req.values():
        counts[r["status"]] = counts.get(r["status"], 0) + 1
    by_group: dict[str, int] = {}
    for s in skips:
        by_group[s["group"]] = by_group.get(s["group"], 0) + 1

    run = {
        **meta,
        "summary": compat.get("summary", {}),
        "requirement_status_counts": counts,
        "per_transport": compat.get("per_transport", {}),
        "anet_skipped_by_group": by_group,
        "failed_requirements": sorted(k for k, r in per_req.items() if r["status"] == "FAIL"),
    }
    _write(out / "run.json", run)

    lines = ["# a2a-tck 结果记录(anet 本机 A2A 接口)", ""]
    lines.append("结果记录,不作门禁(A2A-DESIGN §17)。格式见 `docs/notes/0019-互通-a2a-tck运行说明.md` §6。")
    lines.append("")
    lines.append("| 项 | 值 |")
    lines.append("|---|---|")
    for k in (
        "date",
        "label",
        "anet_commit",
        "tck_commit",
        "sut",
        "transport",
        "level",
        "scope",
        "jsonrpc_slash_fix",
        "pytest_exit",
    ):
        if k in meta:
            lines.append(f"| {k} | `{meta[k]}` |")
    s = compat.get("summary", {})
    for k in ("overall_compatibility", "must_compatibility", "should_compatibility", "may_compatibility"):
        if k in s:
            lines.append(f"| {k} | {s[k]} |")
    lines.append("| 要求计数 | " + ", ".join(f"{k} {v}" for k, v in sorted(counts.items())) + " |")
    lines.append("")
    lines.append("## 跳过的分组")
    lines.append("")
    lines.append("| 分组 | 类别 | 用例数 | 理由 |")
    lines.append("|---|---|---|---|")
    for g in GROUPS:
        if g.key in by_group:
            kind = "不适用" if g.kind == NA else "需对端(本次未测)"
            lines.append(f"| {g.key} | {kind} | {by_group[g.key]} | {g.reason} |")
    lines.append("")
    lines.append("## 各要求结果")
    lines.append("")
    lines.append("| 要求 | 级别 | 状态 | 传输 | 说明 |")
    lines.append("|---|---|---|---|---|")
    order = {"FAIL": 0, "PASS": 1, "SKIPPED": 2, "NOT TESTED": 3}
    for rid, r in sorted(per_req.items(), key=lambda kv: (order.get(kv[1]["status"], 9), kv[0])):
        tr = ", ".join(f"{t}={v}" for t, v in sorted(r.get("transports", {}).items()))
        note = NOTES.get(rid, "")
        if r["status"] == "FAIL" and r.get("errors"):
            first = str(r["errors"][0]).replace("\n", " ").replace("|", "\\|")[:200]
            note = (note + ";" if note else "") + f"首条错误:{first}"
        lines.append(f"| {rid} | {r.get('level', '')} | {r['status']} | {tr} | {note} |")
    lines.append("")
    (out / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")

    print("要求计数:" + (", ".join(f"{k} {v}" for k, v in sorted(counts.items())) or "(无报告)"))
    if s:
        print(f"兼容率:overall {s.get('overall_compatibility')}, must {s.get('must_compatibility')}")
    for g in GROUPS:
        if g.key in by_group:
            print(f"跳过 {g.key}({'不适用' if g.kind == NA else '需对端'}):{by_group[g.key]} 个用例")
    return 0


def main(argv: list[str]) -> int:
    if len(argv) >= 1 and argv[0] == "groups":
        return cmd_groups()
    if len(argv) >= 2 and argv[0] == "preflight":
        return cmd_preflight(argv[1])
    if len(argv) >= 2 and argv[0] == "summarize":
        return cmd_summarize(argv[1], argv[2:])
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

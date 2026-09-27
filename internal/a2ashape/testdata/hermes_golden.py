#!/usr/bin/env python3
"""Regenerate testdata/hermes_a2a_call.json with Hermes' own code.

hermes_contract_test.go ports the part of Hermes' A2A plugin
(plugins/platforms/a2a) that turns a SendMessage response into the text its
model reads. This script runs the original on the same inputs, so the port
is checked against Hermes rather than against a reading of it:

  source env.sh
  ANET_HERMES_DUMP=/tmp/hermes_cases.json \\
      go test ./internal/a2ashape -run TestHermesContract -count=1
  python3 internal/a2ashape/testdata/hermes_golden.py \\
      /data/projs/anet-dev/Refs/hermes-agent /tmp/hermes_cases.json \\
      > internal/a2ashape/testdata/hermes_a2a_call.json

Standard library only; nothing is installed. The plugin's imports from the
rest of Hermes (gateway._shared, hermes_constants, hermes_cli.config,
agent.redact) are replaced by stubs written to a temporary directory.
tools.a2a_call, tools._send_task, tools._reply_text_from_result and
protocol.extract_text run unmodified; only tools._http_json, the one
function that touches the network, answers from the case file. The peer
entry is the one `anet agents wire --a2a` writes (A2A-DESIGN §13.1).

The file also records, for the provider-side backend (§11.6), the Agent
Card Hermes' server publishes (protocol.build_agent_card, with and without
a token) and the SendMessage answers it gives (protocol.build_task wrapped
as adapter._rpc_message_send does), with ids and the clock fixed.
"""

import importlib.util
import json
import os
import re
import subprocess
import sys
import tempfile
import types
import urllib.error

BASE = "http://127.0.0.1:47100/a2a/v1/agents/"
TOKEN = "a2a-local-token"

STUBS = {
    "gateway/__init__.py": "",
    "gateway/platforms/__init__.py": "",
    "gateway/platforms/_shared.py": (
        "def coerce_port(value, default):\n"
        "    try:\n        return int(value)\n    except (TypeError, ValueError):\n        return default\n"
        "def profile_scoped():\n    return False\n"
    ),
    "hermes_constants.py": (
        "import os, pathlib\n"
        "def get_hermes_home():\n    return pathlib.Path(os.environ['HERMES_HOME'])\n"
    ),
    "hermes_cli/__init__.py": "",
    "hermes_cli/config.py": (
        "CONFIG = {}\n"
        "def load_config_readonly():\n    return CONFIG\n"
        "def load_config():\n    return CONFIG\n"
    ),
    "agent/__init__.py": "",
    "agent/redact.py": "def redact_for_egress(text):\n    return text\n",
}


def load_plugin(hermes_root, stub_dir):
    for rel, body in STUBS.items():
        path = os.path.join(stub_dir, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(body)
    sys.path.insert(0, stub_dir)
    plugin_dir = os.path.join(hermes_root, "plugins", "platforms", "a2a")
    spec = importlib.util.spec_from_file_location(
        "hermes_a2a", os.path.join(plugin_dir, "__init__.py"), submodule_search_locations=[plugin_dir])
    pkg = importlib.util.module_from_spec(spec)
    sys.modules["hermes_a2a"] = pkg
    spec.loader.exec_module(pkg)
    import hermes_a2a.tools as tools  # noqa: E402
    import hermes_cli.config as cfg  # noqa: E402
    return tools, cfg, sys.modules["hermes_a2a.protocol"]


def run_case(tools, cfg, case):
    agent = case["agent"]
    cfg.CONFIG = {"a2a_agents": {agent: {"url": BASE + agent, "auth": {"type": "bearer", "token": TOKEN},
                                         "timeout": 3600}}}
    seen = {}

    def http_json(url, headers, timeout, method, data=None):
        if method == "GET":
            seen["card"] = {"url": url, "headers": dict(headers), "timeout": timeout}
            return {"name": agent, "supportedInterfaces": [
                {"url": BASE + agent + "/jsonrpc", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}]}
        body = json.loads(data.decode("utf-8"))
        # Random per call; the test checks their form, not their value.
        body["id"] = "<rpc-id>"
        body["params"]["message"]["messageId"] = "<message-id>"
        if not case.get("context_id"):
            # Minted by protocol.new_context_id; kept stable here.
            assert re.fullmatch(r"ctx-[0-9a-f]{16}", body["params"]["message"]["contextId"])
            body["params"]["message"]["contextId"] = "ctx-0000000000000000"
        seen["post"] = {"url": url, "headers": dict(headers), "timeout": timeout, "body": body}
        if case.get("http_status"):
            raise urllib.error.HTTPError(url, case["http_status"], "error", {}, None)
        if case.get("transport_error"):
            # What urlopen raises when the peer sends nothing for `timeout` seconds.
            raise TimeoutError(case["transport_error"])
        return json.loads(case["response"])

    tools._http_json = http_json
    args = {"agent": agent, "message": case.get("message") or "hello"}
    if case.get("context_id"):
        args["context_id"] = case["context_id"]
    out = dict(case)
    out["output"] = tools.a2a_call(args)
    out["request"] = seen.get("post")
    out["card_request"] = seen.get("card")
    return out


def main():
    hermes_root, cases_path = sys.argv[1], sys.argv[2]
    with open(cases_path, encoding="utf-8") as fh:
        cases = json.load(fh)["cases"]
    commit = subprocess.run(["git", "-C", hermes_root, "log", "-1", "--format=%h", "--", "plugins/platforms/a2a"],
                            capture_output=True, text=True).stdout.strip()
    with tempfile.TemporaryDirectory() as tmp:
        os.environ["HERMES_HOME"] = os.path.join(tmp, "home")
        tools, cfg, protocol = load_plugin(hermes_root, os.path.join(tmp, "stubs"))
        results = [run_case(tools, cfg, c) for c in cases]
        # The Agent Card Hermes serves as a provider-side backend (§11.6),
        # without and with a token configured.
        card = dict(name="hermes-test", url="http://127.0.0.1:9900/", description="Hermes Agent",
                    skills=protocol.skills_from_toolsets(["web"]), streaming=True, push_notifications=True)
        cards = {"no_token": protocol.build_agent_card(**card),
                 "token": protocol.build_agent_card(**card, auth_required=True)}
        # What Hermes' server answers a SendMessage with, per outcome
        # (adapter._rpc_message_send with a v1.0 method name). Ids and the
        # clock are fixed so the file is stable.
        protocol.uuid = types.SimpleNamespace(uuid4=lambda: types.SimpleNamespace(hex="0" * 32))
        protocol.now_iso = lambda: "2026-09-27T00:00:00.000Z"
        outcomes = {
            "completed": (protocol.STATE_COMPLETED, "the answer"),
            "completed, empty reply": (protocol.STATE_COMPLETED, ""),
            "input-required": (protocol.STATE_INPUT_REQUIRED, "which city?"),
            "failed, reply timeout": (protocol.STATE_FAILED, "[agent did not reply in time]"),
            "rejected, anti-loop": (protocol.STATE_REJECTED, "Anti-loop protection: context ctx-anet exceeded 5 turns. "
                                                             "Start a new context or increase A2A_MAX_PINGPONG_TURNS."),
        }
        provider = {name: protocol.jsonrpc_result("rpc-1", protocol.send_message_response(
            protocol.build_task("task-" + "0" * 16, "ctx-anet", state, text))) for name, (state, text) in outcomes.items()}
    json.dump({"hermes_plugin_commit": commit, "cards": cards, "provider_responses": provider, "cases": results},
              sys.stdout, ensure_ascii=False, indent=1)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()

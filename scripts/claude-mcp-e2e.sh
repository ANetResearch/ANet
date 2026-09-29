#!/usr/bin/env bash
# claude-mcp-e2e.sh — Claude Code, the real model, driving a local anet network through `anet mcp`
# (docs/notes/0041). Each scenario is one `claude -p` run on the account this machine's Claude Code is
# logged in to, and is billed to that account: nothing runs a model unless ANET_CLAUDE_E2E=1. CI does not
# run this script, and no other script calls it.
#
#   ANET_CLAUDE_E2E=1 J=DIR bash scripts/claude-mcp-e2e.sh [all [ID…]]  up, the scenarios (default: all), down
#   ANET_CLAUDE_E2E=1 J=DIR bash scripts/claude-mcp-e2e.sh run ID…      scenarios on a network left up
#   J=DIR bash scripts/claude-mcp-e2e.sh up | down | list | summary [OUTDIR]
#
# The network (loopback, E2E_PORT_BASE, default 47480):
#   +0 hub         a fresh anet-hub; registering gives each node 100 credits
#   +1/+3 R        the requester: control plane / local A2A interface. payments.auto_max 2 (the auto tier
#                  covers demo.digest.paid), agent_max 10, P on payees.allow. The MCP server runs as R.
#   +2/+4 P        the provider: inbound closed, R on peers.allow (R's text tasks are accepted), public
#                  capabilities text.stats, text.digest, … and demo.digest.paid (price 2) from anet-official
#                  on a Unix socket, and two of this script's own (e2e.py backend, a second socket):
#                  lab.notes.append (free; answers after its 1.5 s deadline, so its effect is UNVERIFIED)
#                  and lab.report.priced (price 5: above auto_max, within agent_max — submit_payment).
#                  P's text tasks are answered by e2e.py responder over P's control plane (/tasks/reply):
#                  a question after E2E_REPLY_DELAY seconds (default 45, past send_message's 30 s wait),
#                  the three-tier payment summary ("三档") after E2E_LONG_DELAY (default 330, past the
#                  300 s any one send_message or wait_task can wait), a meeting-room booking first with a
#                  question back (input-required), then a booking id.
#
# Each node has its own ANET_HOME = ANET_DATA_DIR, HOME and XDG_RUNTIME_DIR (0700) under $J/run.
#
# The user's Claude Code configuration is not touched. The anet server is mounted for one run only
# (--mcp-config with the entry `anet agents wire claude` writes, taken from a wire run into a throwaway
# HOME without claude on PATH; --strict-mcp-config), the guide wire installs as a skill is passed with
# --append-system-prompt (CLAUDE_E2E_SKILL=0 leaves it out), the user's settings are not loaded
# (--setting-sources project, from an empty directory: none of their hooks, plugins or defaults — so
# the model and effort are given: CLAUDE_E2E_MODEL, default opus; CLAUDE_E2E_EFFORT, default high), and
# no session is saved (--no-session-persistence). Tools other than mcp__anet__* are not allowed, and
# with --permission-prompts none anything that would ask is refused. Claude Code 2.1.284 still makes an
# empty ~/.claude/projects/<the run directory>/memory/ for $J/cc/cwd; this script does not touch ~/.claude,
# so remove that empty directory by hand (rmdir) if it is unwanted.
#
# Per run: --max-turns CLAUDE_E2E_MAX_TURNS (20), --max-budget-usd CLAUDE_E2E_BUDGET (1.50). The json
# output (--output-format json --verbose: every message, then the result with usage and cost) goes to
# $J/out/<time>/<id>.json; e2e.py check reads it with the network's own record of what happened and
# writes <id>.check.json; summary.tsv has one line per scenario.
set -uo pipefail
umask 077
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
SCRIPTS=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPTS/.." && pwd -P)
J=${J:?set J to a private work directory}
BIN=$J/bin; RUN=$J/run; CC=$J/cc
# shellcheck source=lib.sh
. "$SCRIPTS/lib.sh"
die(){ printf 'claude-mcp-e2e: %s\n' "$*" >&2; exit 2; }
say(){ printf '  %s\n' "$*"; }
PY="$BIN/e2e.py"
SCENARIOS="list cap text text-long paid-auto paid-agent unverified ask ask-answer"

envget(){ python3 -c '
import json, sys
v = json.load(open(sys.argv[1]))
for k in sys.argv[2:]:
    v = v.get(k) if isinstance(v, dict) else None
print("" if v is None else v)' "$J/env.json" "$@"; }
ctl(){ # ctl <node> <path> <json>
  local tok; tok=$(cat "$RUN/$1/anet/control_token.txt" 2>/dev/null)
  curl -s -m 30 -H @<(printf 'Authorization: Bearer %s\n' "$tok") -H 'Content-Type: application/json' \
    -d "$3" "http://$(envget "$1" ctl)$2"
}
up_at(){ curl -sf -m 2 "http://$1/ping" >/dev/null 2>&1; }
wait_up(){ local i; for ((i = 0; i < ${2:-30} * 4; i++)); do up_at "$1" && return 0; sleep 0.25; done; return 1; }
start_node(){ # start_node <node>
  local n=$1 d=$RUN/$1
  ( cd "$d" && exec setsid env -u ANET_ID HOME="$d/home" ANET_HOME="$d/anet" ANET_DATA_DIR="$d/anet" \
      XDG_RUNTIME_DIR="$d/xdg" "$BIN/anet" daemon ) >>"$RUN/$n.log" 2>&1 </dev/null &
}

claude_bin(){
  local c=${CLAUDE_BIN:-}
  [ -n "$c" ] || c=$(command -v claude 2>/dev/null) || c=$HOME/.local/bin/claude
  [ -x "$c" ] || die "no claude (set CLAUDE_BIN)"
  printf '%s' "$c"
}

cmd_up(){
  case "$J" in /*) ;; *) die "J must be absolute" ;; esac
  own_dir "$J" || die "J=$J is not a private directory of this user"
  cmd_down 2>/dev/null
  rm -rf -- "$BIN" "$RUN" "$CC"; mkdir -p "$BIN" "$RUN" "$CC/cwd" "$CC/wirehome"
  if [ -n "${E2E_BIN:-}" ]; then
    for b in anet anet-hub anet-official; do cp "$E2E_BIN/$b" "$BIN/$b" || die "no $b in $E2E_BIN"; done
  else
    export CGO_ENABLED=0
    go build -C "$ROOT" -o "$BIN/anet" ./cmd/anet && go build -C "$ROOT" -o "$BIN/anet-official" ./cmd/anet-official \
      && go build -C "$ROOT/../ANetHub" -o "$BIN/anet-hub" ./cmd/anet-hub || die "build failed"
  fi
  # The helpers run from $BIN, so that `down` (stop_under $BIN) stops them with the rest.
  cp "$SCRIPTS/claude-mcp-e2e.py" "$PY"
  local B; B=$(port_block "${E2E_PORT_BASE:-47480}" 10) || die "no ports"
  local HUB=127.0.0.1:$B n
  for n in r p; do
    mkdir -p "$RUN/$n/anet/modules/a2a" "$RUN/$n/home" && mkdir -p -m 700 "$RUN/$n/xdg"
  done
  # A socket address holds 107 bytes: under a long J (a session scratchpad) the sockets go to a private
  # directory of their own under the runtime dir instead, removed by `down`.
  local SOCK=$RUN/sock
  if [ ${#SOCK} -gt 80 ]; then
    SOCK=$(mktemp -d "${XDG_RUNTIME_DIR:-/tmp}/anet-e2e.XXXXXX") || die "no socket directory"
    printf '%s\n' "$SOCK" > "$J/sockdir"
  fi
  mkdir -p -m 700 "$SOCK"
  python3 -c 'import secrets;print(secrets.token_hex(32))' > "$RUN/service.token"
  python3 - "$J/env.json" "$B" "$RUN" "$BIN" "${E2E_REPLY_DELAY:-45}" "$SOCK" "${E2E_LONG_DELAY:-330}" <<'PY'
import json, sys
path, b, run, bindir, delay, sock = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4], int(sys.argv[5]), sys.argv[6]
long_delay = int(sys.argv[7])
a = lambda n: "127.0.0.1:%d" % (b + n)
node = lambda n, c, x: {"data_dir": "%s/%s/anet" % (run, n), "ctl": a(c), "a2a": a(x)}
json.dump({"hub": "http://" + a(0), "run": run, "bin": bindir, "reply_delay": delay, "long_delay": long_delay,
           "r": node("r", 1, 3), "p": node("p", 2, 4),
           "official_sock": sock + "/official.sock", "lab_sock": sock + "/lab.sock",
           "service_token_file": run + "/service.token",
           "prices": {"demo.digest.paid": 2, "lab.report.priced": 5}, "auto_max": 2, "agent_max": 10},
          open(path, "w"), indent=1)
PY
  ( cd "$RUN" && exec setsid "$BIN/anet-hub" --addr "$HUB" --data "$RUN/hub" ) >"$RUN/hub.log" 2>&1 </dev/null &
  for _ in $(seq 1 40); do curl -sf -m 2 "http://$HUB/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
  curl -sf -m 5 "http://$HUB/healthz" >/dev/null || die "hub down: $(tail -2 "$RUN/hub.log")"

  # P's backends: anet-official (tools and paid groups) and the lab capabilities, each on its socket.
  ( cd "$RUN" && exec setsid "$BIN/anet-official" serve -listen "unix:$(envget official_sock)" \
      -token-file "$RUN/service.token" -groups tools,paid ) >"$RUN/official.log" 2>&1 </dev/null &
  ( cd "$RUN" && exec setsid python3 "$PY" backend "$J/env.json" ) >"$RUN/lab.log" 2>&1 </dev/null &
  for _ in $(seq 1 40); do [ -S "$(envget official_sock)" ] && [ -S "$(envget lab_sock)" ] && break; sleep 0.25; done
  [ -S "$(envget official_sock)" ] || die "anet-official did not start: $(tail -3 "$RUN/official.log")"
  [ -S "$(envget lab_sock)" ] || die "lab backend did not start: $(tail -3 "$RUN/lab.log")"
  "$BIN/anet-official" service-config -groups tools,paid -url "unix://$(envget official_sock)" \
    -token-file "$RUN/service.token" > "$RUN/official-service.json" || die "service-config failed"

  python3 "$PY" configure "$J/env.json" "$RUN/official-service.json" || die "configure failed"
  printf '%s\n' "$(envget r a2a)" > "$RUN/r/anet/modules/a2a/a2a_addr.txt"
  printf '%s\n' "$(envget p a2a)" > "$RUN/p/anet/modules/a2a/a2a_addr.txt"
  for n in r p; do start_node $n; done
  for n in r p; do wait_up "$(envget $n ctl)" 30 || die "$n down: $(tail -3 "$RUN/$n.log")"; done
  declare -A AID
  local st
  for n in r p; do
    AID[$n]=$(ctl $n /status '{}' | python3 -c 'import json,sys;print(json.load(sys.stdin).get("aid",""))')
    [ -n "${AID[$n]}" ] || die "$n: no AID"
    st=$(ctl $n /hub-register "{\"hub\":\"http://$HUB\",\"name\":\"Lab-${n^^}\"}" \
      | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status",""))')
    [ "$st" = registered ] || die "$n: registration $st"
  done
  peer_allow "$RUN/p/anet" "${AID[r]}"
  _peer_add "$RUN/r/anet/payees.allow" "${AID[p]}"
  python3 - "$J/env.json" "${AID[r]}" "${AID[p]}" <<'PY'
import json, sys
e = json.load(open(sys.argv[1]))
e["r"]["aid"], e["p"]["aid"] = sys.argv[2], sys.argv[3]
json.dump(e, open(sys.argv[1], "w"), indent=1)
PY
  for _ in $(seq 1 40); do
    [ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$HUB/a2a/v1/agents/${AID[p]}/card")" = 200 ] && break; sleep 0.5
  done
  ( cd "$RUN" && exec setsid python3 "$PY" responder "$J/env.json" ) >>"$RUN/responder.log" 2>&1 </dev/null &

  # What `anet agents wire claude` writes, into a throwaway HOME, with claude off PATH (so wire edits the
  # file itself instead of calling `claude mcp add`, which would write to the real ~/.claude.json).
  env -i PATH=/usr/bin:/bin HOME="$CC/wirehome" ANET_DATA_DIR="$RUN/r/anet" "$BIN/anet" agents wire claude \
    >"$CC/wire.log" 2>&1 || die "wire failed: $(tail -3 "$CC/wire.log")"
  python3 - "$CC/wirehome/.claude.json" "$CC/mcp.json" <<'PY' || die "no mcpServers.anet from wire"
import json, sys
e = json.load(open(sys.argv[1]))["mcpServers"]["anet"]
json.dump({"mcpServers": {"anet": e}}, open(sys.argv[2], "w"), indent=1)
PY
  cp "$CC/wirehome/.claude/skills/anet/SKILL.md" "$CC/skill.md" || die "no SKILL.md from wire"
  say "hub $HUB; R ${AID[r]}; P ${AID[p]}"
  say "env $J/env.json; MCP config $CC/mcp.json"
}

cmd_down(){
  if [ -f "$J/env.json" ]; then
    for n in r p; do up_at "$(envget $n ctl)" && ctl $n /shutdown '{}' >/dev/null; done
  fi
  stop_under "$BIN" 10
  local d; d=$(cat "$J/sockdir" 2>/dev/null) && case "$d" in */anet-e2e.??????) rm -rf -- "$d" "$J/sockdir" ;; esac
  return 0
}

# run_one ID OUTDIR: one scenario, one `claude -p`. A scenario run again in the same OUTDIR is ID.2, ID.3, …
run_one(){
  local id=$1 out=$2 claude prompt skill=() tag=$1 n=2
  while [ -e "$out/$tag.json" ]; do tag=$id.$n; n=$((n + 1)); done
  claude=$(claude_bin)
  prompt=$(python3 "$PY" prompt "$id" "$J/env.json") || die "unknown scenario $id"
  if [ "${CLAUDE_E2E_SKILL:-1}" = 1 ]; then skill=(--append-system-prompt "$(cat "$CC/skill.md")"); fi
  python3 "$PY" snapshot "$J/env.json" > "$out/$tag.before.json" || die "snapshot failed"
  printf '%s\n' "$prompt" > "$out/$tag.prompt.txt"
  info "[$tag] claude -p …"
  local t0=$SECONDS
  ( cd "$CC/cwd" && exec "$claude" -p "$prompt" --output-format json --verbose \
      --max-turns "${CLAUDE_E2E_MAX_TURNS:-20}" --allowedTools 'mcp__anet__*' \
      --mcp-config "$CC/mcp.json" --strict-mcp-config "${skill[@]}" \
      --setting-sources project --no-session-persistence --permission-prompts none \
      --model "${CLAUDE_E2E_MODEL:-opus}" --effort "${CLAUDE_E2E_EFFORT:-high}" \
      --max-budget-usd "${CLAUDE_E2E_BUDGET:-1.50}" ) > "$out/$tag.json" 2> "$out/$tag.err" </dev/null
  say "exit $? after $((SECONDS - t0)) s"
  # Let the network settle (a payment's settlement, a late reply) before reading what happened.
  sleep "${E2E_SETTLE:-3}"
  python3 "$PY" check "$id" "$J/env.json" "$out/$tag.before.json" "$out/$tag.json" "$tag" > "$out/$tag.check.json"
  local rc=$?
  python3 "$PY" line "$out/$tag.check.json" | tee -a "$out/summary.tsv"
  return $rc
}

cmd_run(){
  [ "${ANET_CLAUDE_E2E:-}" = 1 ] || die "this runs Claude Code against the logged-in account and costs money: set ANET_CLAUDE_E2E=1"
  [ -f "$J/env.json" ] && up_at "$(envget r ctl)" || die "the network is not up (J=DIR bash $0 up)"
  local out=${E2E_OUT:-$J/out/$(date -u +%Y%m%dT%H%M%SZ)} id fail=0
  mkdir -p "$out"
  "$(claude_bin)" --version > "$out/claude-version.txt" 2>&1
  "$BIN/anet" version > "$out/anet-version.txt" 2>&1
  [ $# -gt 0 ] || set -- $SCENARIOS
  for id in "$@"; do run_one "$id" "$out" || fail=$((fail + 1)); done
  python3 "$PY" summary "$out"
  say "results: $out"
  return $(( fail > 0 ))
}

cmd=${1:-all}; shift || true
case "$cmd" in
up)      cmd_up ;;
down)    cmd_down ;;
run)     cmd_run "$@" ;;
all)
  [ "${ANET_CLAUDE_E2E:-}" = 1 ] || die "this runs Claude Code against the logged-in account and costs money: set ANET_CLAUDE_E2E=1"
  cmd_up; cmd_run "$@"; rc=$?; cmd_down; exit $rc ;;
list)    printf '%s\n' $SCENARIOS ;;
summary) python3 "$SCRIPTS/claude-mcp-e2e.py" summary "${1:?OUTDIR}" ;;
*)       die "usage: claude-mcp-e2e.sh [all [ID…]] | up | run ID… | down | list | summary OUTDIR" ;;
esac

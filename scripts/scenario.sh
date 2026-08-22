#!/usr/bin/env bash
# scenario.sh — a hub that knows nobody, and three nodes that join it.
#
# This is the shape of a real network rather than a test fixture:
#
#   1. a hub starts on an empty data directory. It has no relationship
#      with any daemon and no way to invite one — it can only be joined.
#   2. three daemons start with fresh identities and join the way the
#      hub's own web page tells a person to: `anet up`, then
#      `anet hub-register <hub> --name … --caps …`.
#   3. joining makes them findable. Nothing else does.
#   4. each pair proves it can actually reach the others.
#   5. A offers a capability of its own; C calls it through the hub and
#      checks the receipt.
#
# Two tiers, and the split is deliberate:
#
#   default   hermetic. No secrets, no internet, loopback only. This is
#             the tier that belongs in CI on every push.
#   --live    the same topology with real capabilities behind it: a local
#             image-caption model in a container on A, a rented frontier
#             model on B. Needs credentials and network, so it runs on
#             demand — a test that needs an API key is not a test that
#             can gate a merge.
#
# Live credentials come from $SCENARIO_ENV (default ~/.config/anet-scenario.env),
# which lives outside every repository and is never committed.
set -uo pipefail
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

LIVE=0
[ "${1:-}" = "--live" ] && LIVE=1

ROOT=${SCENARIO_ROOT:-/tmp/anet-scenario}
BIN=${SCENARIO_BIN:-$ROOT/bin}
HUB_PORT=${HUB_PORT:-29500}
HUB=http://127.0.0.1:$HUB_PORT
SCENARIO_ENV=${SCENARIO_ENV:-$HOME/.config/anet-scenario.env}
CAPTION_URL=${CAPTION_URL:-http://127.0.0.1:8099/caption}

pass=0; fail=0
ok(){ printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){ printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
info(){ printf '  %s\n' "$*"; }

# ── node helpers ────────────────────────────────────────────────
# Each daemon is a separate HOME, which is how one machine hosts several
# identities: one runtime, one agent, one AID.
home_of(){ echo "$ROOT/$1"; }
ctl(){ # ctl <node> <path> <json>
  local h; h=$(home_of "$1")
  local addr; addr=$(python3 -c "import json;print(json.load(open('$h/.anet/config.json'))['control_addr'])")
  curl -s -m 300 -H "Authorization: Bearer $(cat "$h/.anet/control_token.txt")" \
       -H 'Content-Type: application/json' -d "$3" "http://$addr$2"
}
aid_of(){ "$BIN/anetfixture" aid --home "$(home_of "$1")/.anet"; }

# cap <from> <to-aid> <capability> <args-json> — delegate and wait
cap(){
  local ix
  ix=$(ctl "$1" /delegate "{\"provider\":\"$2\",\"capability\":\"$3\",\"args\":$4}" \
       | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
  [ -n "$ix" ] || { echo '{"error":"delegate refused"}'; return 1; }
  for _ in $(seq 1 60); do
    local r
    r=$(ctl "$1" /results '{}' | python3 -c "
import sys,json
for x in json.load(sys.stdin).get('results') or []:
    if x['interaction_id']=='$ix': print(x['result']); break
")
    [ -n "$r" ] && { echo "$r"; return 0; }
    sleep 1
  done
  echo '{"error":"timed out"}'; return 1
}

# ── 0. a hub that knows nobody ──────────────────────────────────
hd "0  一个谁也不认识的 hub"
pgrep -x anet | xargs -r kill -TERM 2>/dev/null
pgrep -x anet-hub | xargs -r kill -TERM 2>/dev/null
pgrep -f 'scenario-svc' | xargs -r kill -TERM 2>/dev/null
sleep 2
rm -rf "$ROOT/hub" "$ROOT/A" "$ROOT/B" "$ROOT/C"
mkdir -p "$ROOT/hub"
setsid "$BIN/anet-hub" --addr "127.0.0.1:$HUB_PORT" --data "$ROOT/hub" >"$ROOT/hub.log" 2>&1 </dev/null &
for _ in $(seq 1 30); do curl -sf -m 2 "$HUB/healthz" >/dev/null && break; sleep 1; done
curl -sf -m 5 "$HUB/healthz" >/dev/null && ok "hub 起来了,数据目录全新" || { no "hub 起不来: $(tail -2 "$ROOT/hub.log")"; exit 1; }
n=$(curl -s -m 5 "$HUB/agents" | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("agents") or []))')
[ "$n" = "0" ] && ok "它认识 0 个 agent —— 只能被加入,不能去邀请" || no "全新 hub 上已有 $n 个 agent"

# ── 1. three nodes join the documented way ──────────────────────
hd "1  三个节点按网页上教的方式加入"
port=29510
for node in A B C; do
  h=$(home_of "$node"); mkdir -p "$h/.anet"
  cat > "$h/.anet/config.json" <<CFG
{
 "control_addr": "127.0.0.1:$port",
 "hub_url": "$HUB",
 "accept_delegations": true
}
CFG
  port=$((port+1))
done

# A's own capability. Hermetic and deterministic: it hashes what it is
# given. In --live mode A also fronts the caption model, and the contrast
# is the point — one capability that needs nothing, one that owns a model.
mkdir -p "$ROOT/svc"
cat > "$ROOT/svc/scenario-svc.py" <<'PY'
import hashlib, json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        try:
            req = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            req = {}
        text = str(req.get("text", ""))
        body = json.dumps({
            "digest": hashlib.sha256(text.encode()).hexdigest(),
            "length": len(text),
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass

HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
setsid python3 "$ROOT/svc/scenario-svc.py" 29520 >"$ROOT/svc.log" 2>&1 </dev/null &
sleep 1

# A declares it. This is the whole of "putting your own service on the
# network": an id, and a URL behind it.
python3 - "$(home_of A)/.anet/config.json" <<'PY'
import json, sys
p = sys.argv[1]
c = json.load(open(p))
c["name"] = "NodeA"
c["caps"] = ["digest"]
c["modules"] = {"service": {"capabilities": [
    {"id": "text.digest", "url": "http://127.0.0.1:29520",
     "description": "sha256 of the text you send"}]}}
json.dump(c, open(p, "w"), indent=1)
PY
python3 - "$(home_of B)/.anet/config.json" <<'PY'
import json, sys
p = sys.argv[1]; c = json.load(open(p)); c["name"] = "NodeB"; c["caps"] = ["chat"]
json.dump(c, open(p, "w"), indent=1)
PY
python3 - "$(home_of C)/.anet/config.json" <<'PY'
import json, sys
p = sys.argv[1]; c = json.load(open(p)); c["name"] = "NodeC"
json.dump(c, open(p, "w"), indent=1)
PY

for node in A B C; do
  setsid env HOME="$(home_of "$node")" "$BIN/anet" daemon >"$ROOT/$node.log" 2>&1 </dev/null &
done
sleep 5
up=0
for node in A B C; do
  h=$(home_of "$node")
  addr=$(python3 -c "import json;print(json.load(open('$h/.anet/config.json'))['control_addr'])")
  curl -sf -m 3 "http://$addr/ping" >/dev/null && up=$((up+1))
done
[ "$up" = 3 ] && ok "三个 daemon 各自起来了(三个身份,三个 HOME)" || no "只有 $up/3 起来"

# The documented join: `anet hub-register <hub> --name … --caps …`.
ctl A /hub-register "{\"hub\":\"$HUB\",\"name\":\"NodeA\",\"caps\":[\"digest\"]}" >/dev/null
ctl B /hub-register "{\"hub\":\"$HUB\",\"name\":\"NodeB\",\"caps\":[\"chat\"]}" >/dev/null
ctl C /hub-register "{\"hub\":\"$HUB\",\"name\":\"NodeC\",\"caps\":[]}" >/dev/null
sleep 2
A=$(aid_of A); B=$(aid_of B); C=$(aid_of C)
info "A $A"
info "B $B"
info "C $C"

# ── 2. joining is what makes you findable ───────────────────────
hd "2  加入之后才可被发现"
# Registered and listed are different states, and the difference is the
# hub's own rule: it lists what advertises a service. C joined with no
# capabilities, so it is reachable and not in the directory — which is
# correct, and a test that demanded three listings would have been
# demanding a bug.
reg=0
for x in "$A" "$B" "$C"; do
  curl -sf -m 10 "$HUB/agents/$x/kel" >/dev/null 2>&1 && reg=$((reg+1))
done
[ "$reg" = 3 ] && ok "三个都注册上了,而 hub 从未主动联系过任何一个" || no "只有 $reg/3 注册成功"
listed=$(curl -s -m 10 "$HUB/agents" | python3 -c "
import sys,json
ags={a['aid'] for a in json.load(sys.stdin).get('agents') or []}
print(sum(1 for x in ['$A','$B','$C'] if x in ags))")
[ "$listed" = 2 ] && ok "目录里只有 A 和 B —— 加入 ≠ 上架,C 没有声明能力" \
  || no "目录里有 $listed 个,期望 2(只有声明了能力的会被列出)"
found=$(ctl C /find '{"query":"digest"}' | python3 -c "
import sys,json
print(next((a['name'] for a in json.load(sys.stdin).get('agents') or [] if a['aid']=='$A'), ''))")
[ "$found" = "NodeA" ] && ok "C 用散文找到了 A" || no "C 没找到 A(得到 '$found')"
# The exact question, which the prose search cannot express.
byid=$(ctl C /find '{"capability":"text.digest"}' | python3 -c "
import sys,json
print(next((a['name'] for a in json.load(sys.stdin).get('agents') or [] if a['aid']=='$A'), ''))")
[ "$byid" = "NodeA" ] && ok "C 按能力 id 精确找到了 A" || no "按 id 没找到 A(得到 '$byid')"
none=$(ctl C /find '{"capability":"nobody.serves.this"}' | python3 -c "
import sys,json;print(len(json.load(sys.stdin).get('agents') or []))")
[ "$none" = "0" ] && ok "无人提供的能力返回空,而不是退回散文搜索" || no "无人提供的能力返回了 $none 个"

# Third-party verifiability, from the hub alone.
kel=$(curl -s -m 10 "$HUB/agents/$A/kel" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("kel",""))')
[ -n "$kel" ] && ok "hub 公布了 A 的密钥历史,陌生人可以自行验签" || no "hub 不发布密钥历史"

# ── 3. every pair can actually reach the others ─────────────────
hd "3  三者两两连通"
# reach delivers a delegation and confirms it landed in the target's
# inbox — delivery is the thing the network exists to do, so it is the
# honest reachability test.
#
# It then closes the interaction, and that matters more than it looks. A
# probe that leaves an open prose task behind is work: on a node running
# auto-reply, six probes become six model calls queued ahead of whatever
# the test actually wanted to measure. The first version of this measured
# its own backlog.
reach(){ # reach <from> <to-node> <to-aid>
  local ix
  ix=$(ctl "$1" /delegate "{\"provider\":\"$3\",\"goal\":\"reachability check from $1\"}" \
       | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
  [ -n "$ix" ] || return 1
  local landed=""
  for _ in $(seq 1 20); do
    landed=$(ctl "$2" /inbox '{}' | python3 -c "
import sys,json
print(next((x['interaction_id'] for x in (json.load(sys.stdin).get('inbox') or [])
            if x['interaction_id']=='$ix'), ''))" 2>/dev/null)
    [ -n "$landed" ] && break
    sleep 1
  done
  # Close it either way; an undelivered probe still leaves a local record.
  ctl "$1" /end "{\"interaction_id\":\"$ix\"}" >/dev/null 2>&1
  ctl "$2" /end-accept "{\"interaction_id\":\"$ix\"}" >/dev/null 2>&1
  [ -n "$landed" ]
}
for pair in "A B $B" "A C $C" "B A $A" "B C $C" "C A $A" "C B $B"; do
  set -- $pair
  if reach "$1" "$2" "$3"; then ok "$1 → $2 送达(对端 inbox 里确认)"; else no "$1 → $2 未送达"; fi
done

# ── 4. A's own capability, called by C through the hub ──────────
hd "4  C 通过 hub 调用 A 自己的服务"
EFF=$(cap C "$A" text.digest '{"text":"anet"}')
info "effect: $(echo "$EFF" | head -c 220)"
echo "$EFF" | grep -q '"status":"OK"' && ok "调用成功" || no "调用失败: $(echo "$EFF" | head -c 200)"
# sha256("anet") — computed independently of the service under test.
WANT=$(printf 'anet' | sha256sum | cut -d' ' -f1)
echo "$EFF" | grep -q "$WANT" && ok "返回的摘要就是 sha256(\"anet\"),内容正确而不只是状态正确" \
  || no "摘要不对,期望 $WANT"
echo "$EFF" | grep -q '"verify_trust": *1' && ok "信任等级 V1 —— 服务应答了,但 daemon 无从判断答案对不对" \
  || no "信任等级不是 V1: $(echo "$EFF" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("evidence"))' 2>/dev/null)"

# The receipt, checked the way a stranger checks it.
hd "5  收据"
R=$(ctl C /results '{}' | python3 -c "
import sys,json
rs=[x for x in json.load(sys.stdin)['results'] if x['provider']=='$A' and 'digest' in x['goal']]
print(json.dumps(rs[-1]) if rs else '')")
[ -n "$R" ] && ok "C 拿到了带签名收据的结果" || no "C 没有收到结果"
if [ -n "$R" ]; then
  echo "$R" | python3 -c "import sys,json;d=json.load(sys.stdin);open('$ROOT/receipt.b64','w').write(d['receipt']);open('$ROOT/result.bin','w').write(d['result'])"
  if HOME=/nonexistent "$BIN/anet" verify --receipt "$(cat "$ROOT/receipt.b64")" --hub "$HUB" --result "$ROOT/result.bin" >"$ROOT/verify.out" 2>&1; then
    ok "陌生人只用收据 + hub 地址就验通了(无 daemon、无密钥)"
  else
    no "第三方验证失败: $(tail -3 "$ROOT/verify.out")"
  fi
fi

# Both sides recorded it.
for node in A C; do
  n=$(ctl "$node" /evidence '{"limit":200}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["head"]["length"])')
  [ "${n:-0}" -gt 0 ] && ok "$node 的证据链上有 $n 条记录" || no "$node 的证据链是空的"
done

# ── 6. live tier ────────────────────────────────────────────────
if [ "$LIVE" = 1 ]; then
  hd "6  实况层:A 本地模型 · B 租来的前沿模型"
  if [ -f "$SCENARIO_ENV" ]; then . "$SCENARIO_ENV"; fi
  : "${OPENROUTER_API_KEY:=}"
  : "${OPENROUTER_MODEL:=nvidia/nemotron-nano-12b-v2-vl:free}"

  # A gains a second capability, backed by a model that runs on this
  # machine and talks to nothing.
  if curl -sf -m 5 "${CAPTION_URL%/caption}/health" >/dev/null 2>&1; then
    python3 - "$(home_of A)/.anet/config.json" "$CAPTION_URL" <<'PY'
import json, sys
p, url = sys.argv[1], sys.argv[2]
c = json.load(open(p))
caps = c["modules"]["service"]["capabilities"]
if not any(x["id"] == "image.caption" for x in caps):
    caps.append({"id": "image.caption", "url": url,
                 "description": "caption an image; args: image_b64",
                 "protocol": "local-model"})
c["caps"] = ["digest", "image.caption"]
json.dump(c, open(p, "w"), indent=1)
PY
    ctl A /shutdown '{}' >/dev/null 2>&1; sleep 2
    setsid env HOME="$(home_of A)" "$BIN/anet" daemon >"$ROOT/A.log" 2>&1 </dev/null &
    sleep 4
    ctl A /hub-register "{\"hub\":\"$HUB\",\"name\":\"NodeA\",\"caps\":[\"digest\",\"image.caption\"]}" >/dev/null
    ok "A 挂上了本地 caption 模型(容器内,不出网)"
  else
    no "caption 服务没在 $CAPTION_URL —— 跳过 A 的本地模型"
  fi

  # B rents one. Same network, entirely different terms.
  if [ -n "$OPENROUTER_API_KEY" ]; then
    ctl B /autoreply "{\"on\":true,\"backend\":\"openai\",\"api_base\":\"https://openrouter.ai/api/v1\",\"api_key\":\"$OPENROUTER_API_KEY\",\"model\":\"$OPENROUTER_MODEL\",\"system_prompt\":\"You are an agent on the ANet network. Answer the request directly and briefly.\"}" >/dev/null
    sleep 1
    st=$(ctl B /status '{}' | python3 -c 'import sys,json;print(json.load(sys.stdin).get("auto_reply",""))' 2>/dev/null)
    ok "B 接上了 OpenRouter($OPENROUTER_MODEL)${st:+ · $st}"
  else
    no "没有 OPENROUTER_API_KEY —— 跳过 B(把它放进 $SCENARIO_ENV)"
  fi

  # C asks A to caption a real image, through the hub.
  if curl -sf -m 5 "${CAPTION_URL%/caption}/health" >/dev/null 2>&1; then
    IMG=${SCENARIO_IMAGE:-$ROOT/scene.png}
    [ -f "$IMG" ] || python3 - "$IMG" <<'PY'
import struct, zlib, random, sys
random.seed(7); W = H = 96; px = bytearray()
for y in range(H):
    px.append(0)
    for x in range(W):
        if y < H * 0.55:
            r, g, b = 110 + y // 3, 160 + y // 4, 230
            if (x - 70) ** 2 + (y - 22) ** 2 < 130: r, g, b = 255, 240, 150
        else:
            r, g, b = 70 + random.randint(0, 25), 130 + random.randint(0, 40), 60
        px += bytes((r, g, b))
def chunk(t, d):
    c = t + d
    return struct.pack('>I', len(d)) + c + struct.pack('>I', zlib.crc32(c) & 0xffffffff)
png = (b'\x89PNG\r\n\x1a\n'
       + chunk(b'IHDR', struct.pack('>IIBBBBB', W, H, 8, 2, 0, 0, 0))
       + chunk(b'IDAT', zlib.compress(bytes(px))) + chunk(b'IEND', b''))
open(sys.argv[1], 'wb').write(png)
PY
    B64=$(base64 -w0 < "$IMG")
    info "图片 $IMG · $(stat -c%s "$IMG") 字节 · base64 $(echo -n "$B64" | wc -c) 字节"
    CAPEFF=$(cap C "$A" image.caption "{\"image_b64\":\"$B64\"}")
    CAPTION=$(echo "$CAPEFF" | python3 -c "
import sys,json
try:
    e=json.load(sys.stdin).get('evidence') or {}
    print(json.loads(e.get('observed_state','{}')).get('caption',''))
except Exception: print('')")
    if [ -n "$CAPTION" ]; then
      ok "C → hub → A → 本地模型 → 回到 C:「$CAPTION」"
    else
      no "caption 没回来: $(echo "$CAPEFF" | head -c 220)"
    fi
  fi

  # C asks B in prose. A capability call and a conversation are different
  # things and the network carries both.
  if [ -n "$OPENROUTER_API_KEY" ]; then
    IX=$(ctl C /delegate "{\"provider\":\"$B\",\"goal\":\"In one short sentence: what is an agent network for?\"}" \
         | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
    REPLY=""
    for _ in $(seq 1 60); do
      # The thread is nested under "thread", and a message's author is
      # "me" or "them" rather than an AID — the store already knows which
      # side it is on, so it does not repeat the peer's identity per line.
      REPLY=$(ctl C /thread "{\"interaction_id\":\"$IX\"}" | python3 -c "
import sys,json
th=(json.load(sys.stdin) or {}).get('thread') or {}
ms=[m for m in (th.get('messages') or []) if m.get('from')=='them' and m.get('kind')=='text']
print(ms[-1].get('body','') if ms else '')" 2>/dev/null)
      [ -n "$REPLY" ] && break
      sleep 3
    done
    [ -n "$REPLY" ] && ok "B 用租来的模型回答了 C:「$(echo "$REPLY" | head -c 100)」" \
      || no "B 没有回复(检查 $ROOT/B.log)"
  fi
fi

printf '\n\033[1m── %d 通过, %d 失败 ──\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]

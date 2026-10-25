#!/usr/bin/env bash
# Extra demo-only data for the WEB captures (site/scripts/shoot-web.mjs), layered
# on top of an instance created by demo-seed.sh. Everything goes through the
# daemon's supported APIs/CLI inside the isolated instance — nothing is mocked in
# the browser. Run once after a fresh `demo-seed.sh up`:
#
#   WARDEN_BIN=... DEMO_ROOT=/tmp/wd-demo-web DEMO_PORT=8809 bash site/scripts/web-demo-extras.sh
#
# Adds: a run-owned worker (so the Autopilot panel's workers list is populated),
# two pipelines (one mid-flight, one fan-out) for the Pipelines tab/DAG, and real
# background CPU load in the demo terminals so the Metrics charts show live curves.
set -euo pipefail
DEMO_ROOT="${DEMO_ROOT:?set DEMO_ROOT}"; DEMO_PORT="${DEMO_PORT:?set DEMO_PORT}"
WARDEN_BIN="${WARDEN_BIN:?set WARDEN_BIN}"
case "$DEMO_ROOT" in /tmp/warden-demo*|/tmp/wd-demo*) ;; *) echo "refusing DEMO_ROOT=$DEMO_ROOT" >&2; exit 2 ;; esac
[ -f "$DEMO_ROOT/.warden-demo-root" ] || { echo "not a demo root" >&2; exit 2; }
H="$DEMO_ROOT/home"; CFG="$H/.warden/config.yaml"; WORK="$DEMO_ROOT/work"; PANES="$DEMO_ROOT/panes"
A="http://127.0.0.1:$DEMO_PORT/api/v1"
api() { if [ $# -ge 3 ]; then curl -fsS -X "$1" "$A$2" -H 'content-type: application/json' -d "$3"; else curl -fsS -X "$1" "$A$2"; fi; }
wd() { env -i HOME="$H" LANG=C.UTF-8 PATH="$DEMO_ROOT/bin:/usr/bin:/bin" TMUX_TMPDIR="$DEMO_ROOT/tmux" DEMO_PANES="$PANES" \
  "$WARDEN_BIN" --config "$CFG" "$@"; }

# --- Autopilot: a worker owned by the run (run:<id> tag), shown under "workers"
RUN="$(api GET /autopilot | jq -r '.runs[0].run_id')"
MGR="$(api GET /autopilot | jq -r '.runs[0].manager_slot_id')"
PLAN="$(api GET /autopilot | jq -r '.runs[0].plan_id')"
WID="$(api POST /spawn "$(jq -nc --arg r "$WORK/api-gateway" --arg m "$MGR" --arg p "$PLAN" --arg run "$RUN" \
  '{role:"worker",repo:$r,name:"sso-callback",prompt:"Implement the SSO login + callback endpoints",parent_id:$m,plan_id:$p,ai_cli:"claude",tags:["run:"+$run]}')" | jq -r .id)"
cat > "$PANES/$WID.txt" <<'T'
● Wiring the SSO callback handler and session cookie issuance.

● Update(src/routes/sso-callback.ts)
  ⎿  Updated src/routes/sso-callback.ts with 48 additions and 2 removals

  · Working… (esc to interrupt)
T

# --- Pipelines: analyze → implement → review mid-flight; plus a parallel fan-out
wd pipeline create --template analyze-implement-review --name webhook-retry-fix \
  --repo "$WORK/billing-svc" --set TASK="Make the billing webhook retry idempotent" --start >/dev/null
wd pipeline create --template parallel-tasks --name checkout-hardening \
  --repo "$WORK/checkout-svc" --set TASK_A="Add a retry budget to the payment client" \
  --set TASK_B="Cover the checkout timeout path with integration tests" --start >/dev/null
jobstatus() { api GET "/pipelines/$1" | jq -r --arg j "$2" '.jobs[]|select(.id==$j).status'; }
for _ in $(seq 1 30); do [ "$(jobstatus webhook-retry-fix analyze)" = running ] && break; sleep 1; done
api POST /pipelines/webhook-retry-fix/jobs/analyze/emit \
  '{"text":"Root cause: the retry handler re-queues before the idempotency key is persisted. Fix: write the key first, then enqueue."}' >/dev/null
for _ in $(seq 1 30); do [ "$(jobstatus webhook-retry-fix implement)" = running ] && break; sleep 1; done
cat > "$PANES/webhook-retry-fix-implement.txt" <<'T'
● Persisting the idempotency key before the retry is enqueued.

● Update(src/webhooks/retry.ts)
  ⎿  Updated src/webhooks/retry.ts with 22 additions and 5 removals

  · Working… (esc to interrupt)
T

cat > "$PANES/checkout-hardening-task-a.txt" <<'T'
● Adding a retry budget to the payment client.

● Update(src/payments/client.ts)
  ⎿  Updated src/payments/client.ts with 31 additions and 6 removals

  · Working… (esc to interrupt)
T
cat > "$PANES/checkout-hardening-task-b.txt" <<'T'
● Covering the checkout timeout path with integration tests.

● Write(test/checkout.timeout.test.ts)
  ⎿  Wrote 74 lines

  · Working… (esc to interrupt)
T

# --- bottom-align every stub pane's text. The web grid tiles (and `wd output`) show
# only the LAST rows of the pane; the stub draws from the top of a 24-row pane, so
# without padding every tile would be blank. Real agent panes fill the bottom the same way.
for f in "$PANES"/*.txt; do
  python3 - "$f" <<'PY'
import sys
p = sys.argv[1]; s = open(p).read()
head = "@@EXIT@@\n" if s.startswith("@@EXIT@@") else ""
body = s[len(head):].rstrip("\n").split("\n")
if body and body[0] == "" or len(body) >= 22:
    sys.exit(0)            # already padded / long enough
open(p, "w").write(head + "\n" * (22 - len(body)) + "\n".join(body) + "\n")
PY
done

# --- real CPU load in the demo terminals (bursty, so the charts have shape)
for t in $(api GET /sessions | jq -r '.sessions[]|select(.kind=="terminal").id'); do
  api POST "/sessions/$t/input" "$(jq -nc '{text:"( while :; do e=$((SECONDS+RANDOM%4)); while [ $SECONDS -lt $e ]; do :; done; sleep $((1+RANDOM%4)); done ) >/dev/null 2>&1 & clear"}')" >/dev/null || true
done
echo "web demo extras added on :$DEMO_PORT (worker $WID, pipelines webhook-retry-fix + checkout-hardening, terminal CPU load)"

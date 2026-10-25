#!/usr/bin/env bash
# Seed and run an ISOLATED, throwaway warden demo fleet for the marketing
# captures (TUI cockpit tapes + web screenshots) — current project-centric
# cockpit: project tree with agents/terminals/plans, roles, an Autopilot run on
# a canonical plan with a task DAG, the Backends/usage page and Fast-Brain.
#
#   bash site/scripts/demo-seed.sh up        # reset, start mock+daemon, seed (default)
#   bash site/scripts/demo-seed.sh status    # daemon + fleet summary
#   bash site/scripts/demo-seed.sh verify    # assert the fixture is complete
#   bash site/scripts/demo-seed.sh env       # print the exports for captures
#   bash site/scripts/demo-seed.sh down      # stop everything, remove DEMO_ROOT
#
# Isolation model (nothing touches ~/.warden, the real daemon, tmux server or
# any real repo/host/token):
#   * every path lives under DEMO_ROOT (default /tmp/warden-demo): own HOME, own
#     tmux server (TMUX_TMPDIR), own git repos, own data dir, own port;
#   * every demo process runs under `env -i` with a minimal environment, so no
#     WARDEN_* / token / TMUX variable from the caller leaks in;
#   * config is a file, not WARDEN_* env: it is written to $HOME/.warden/config.yaml
#     of the demo HOME (the default location), and callers also pass --config.
#     The default location matters: the TUI re-executes `warden` for its panes
#     WITHOUT the --config flag, and without a config at the default path those
#     panes fall back to 127.0.0.1:8765 — the user's REAL daemon;
#   * the AI CLIs are stubs (site/tape/_demobin/demo-cli installed as claude,
#     codex, cursor-agent, opencode, aider) on a PATH that excludes the real
#     ones; pane text comes from $DEMO_PANES files, the usage panel from a stub
#     codex app-server plus a local Cursor-API mock (127.0.0.1:DEMO_PORT+1).
#
# Independent captures (t2 TUI / t3 web) MUST NOT share an instance: give each
# its own DEMO_ROOT and DEMO_PORT, e.g.
#   DEMO_ROOT=/tmp/warden-demo-tui DEMO_PORT=8799 bash demo-seed.sh up
#   DEMO_ROOT=/tmp/warden-demo-web DEMO_PORT=8809 bash demo-seed.sh up
# and run each capture against its own printed `env`. Keep DEMO_ROOT short: the
# tmux socket path is limited to ~100 characters.
set -euo pipefail

CMD="${1:-up}"
DEMO_ROOT="${DEMO_ROOT:-/tmp/warden-demo}"
DEMO_PORT="${DEMO_PORT:-8799}"
MOCK_PORT=$((DEMO_PORT + 1))
# Prefer the freshly built integration binary (the UI under capture must match the
# revision being documented); fall back to PATH. Override with WARDEN_BIN.
if [ -z "${WARDEN_BIN:-}" ]; then
  if [ -x /tmp/warden-site-media-integration/bin/warden ]; then
    WARDEN_BIN=/tmp/warden-site-media-integration/bin/warden
  else
    WARDEN_BIN="$(command -v warden || true)"
  fi
fi

# DEMO_ROOT is rm -rf'd by `up` and `down`, so it is validated first: it must be a
# plain directory directly under /tmp named warden-demo* or wd-demo*, and an
# existing one must carry our marker file (so a typo can never delete real data).
MARKER=.warden-demo-root
case "$DEMO_ROOT" in
  /tmp/warden-demo|/tmp/warden-demo[-_.]*|/tmp/wd-demo|/tmp/wd-demo[-_.]*) ;;
  *) echo "refusing DEMO_ROOT='$DEMO_ROOT': must be /tmp/warden-demo* or /tmp/wd-demo*" >&2; exit 2 ;;
esac
case "$DEMO_ROOT" in *..*|*//*|*[[:space:]]*) echo "refusing DEMO_ROOT='$DEMO_ROOT'" >&2; exit 2 ;; esac
if [ -L "$DEMO_ROOT" ]; then echo "refusing: DEMO_ROOT is a symlink" >&2; exit 2; fi
if [ -e "$DEMO_ROOT" ] && [ ! -f "$DEMO_ROOT/$MARKER" ]; then
  echo "refusing: $DEMO_ROOT exists but is not a warden demo root (no $MARKER)" >&2; exit 2
fi
case "$DEMO_PORT" in ''|*[!0-9]*) echo "DEMO_PORT must be numeric" >&2; exit 2 ;; esac

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STUBS="$HERE/../tape/_demobin"
H="$DEMO_ROOT/home"
DATA="$DEMO_ROOT/data"
TMUXD="$DEMO_ROOT/tmux"
WORK="$DEMO_ROOT/work"
BIN="$DEMO_ROOT/bin"
PANES="$DEMO_ROOT/panes"
CFG="$H/.warden/config.yaml"
ADDR="127.0.0.1:$DEMO_PORT"
A="http://$ADDR/api/v1"

# Run a command in the clean demo environment. PATH holds only the stub CLIs
# plus the system tools warden itself needs (tmux, git, python3 ...).
denv() {
  env -i HOME="$H" LANG=C.UTF-8 TERM="${TERM_OVERRIDE:-xterm-256color}" SHELL=/bin/bash \
    TMUX_TMPDIR="$TMUXD" DEMO_PANES="$PANES" XDG_CONFIG_HOME="$H/.config" \
    CURSOR_API_ENDPOINT="http://127.0.0.1:$MOCK_PORT" PATH="$BIN:/usr/bin:/bin" \
    HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 "$@"
}
# (HTTPS_PROXY to a dead local port: the demo never calls out — e.g. the cockpit's
# update check would otherwise add an "Update available" chip to every screenshot.
# Go exempts 127.0.0.1, so the daemon, the mock and the web UI stay reachable.)
dtmux() { denv tmux "$@"; }
wd() { denv "$WARDEN_BIN" --config "$CFG" "$@"; }
api() { # api <METHOD> <path> [json]
  if [ $# -ge 3 ]; then
    curl -fsS -X "$1" "$A$2" -H 'content-type: application/json' -d "$3"
  else
    curl -fsS -X "$1" "$A$2"
  fi
}

print_env() {
  cat <<EOF
# isolated demo instance (source or eval this; nothing here points at your real fleet)
export DEMO_ROOT=$DEMO_ROOT DEMO_PORT=$DEMO_PORT
export DEMO_ENV_PREFIX='env -i HOME=$H LANG=C.UTF-8 TERM=xterm-256color SHELL=/bin/bash TMUX_TMPDIR=$TMUXD PATH=$BIN:/usr/bin:/bin'
export DEMO_WARDEN="$WARDEN_BIN --config $CFG"
# TUI:  \$DEMO_ENV_PREFIX \$DEMO_WARDEN tui
# web:  http://$ADDR/   (no auth configured; \`unset WARDEN_TOKEN\` in the capturing shell; point the screenshot tool at this URL)
EOF
}

stop_all() {
  [ -d "$DEMO_ROOT" ] || return 0
  if [ -f "$DEMO_ROOT/daemon.pid" ]; then
    kill "$(cat "$DEMO_ROOT/daemon.pid")" 2>/dev/null || true
  fi
  if [ -f "$DEMO_ROOT/mock.pid" ]; then
    kill "$(cat "$DEMO_ROOT/mock.pid")" 2>/dev/null || true
  fi
  # kill ONLY the isolated tmux server (its own TMUX_TMPDIR), never the real one.
  [ -d "$TMUXD" ] && dtmux kill-server 2>/dev/null || true
  sleep 1
}

case "$CMD" in
  env)
    # Refuse unless THIS instance exists: with a missing demo HOME the TUI panes
    # would silently fall back to the default config and attach to the real daemon.
    [ -f "$DEMO_ROOT/$MARKER" ] && [ -f "$CFG" ] || {
      echo "no demo instance at DEMO_ROOT=$DEMO_ROOT (run 'up' first; pass the same DEMO_ROOT/DEMO_PORT)" >&2; exit 1; }
    print_env; exit 0 ;;
  down)
    stop_all
    rm -rf "$DEMO_ROOT"
    echo "demo instance $DEMO_ROOT stopped and removed"
    exit 0 ;;
  status)
    curl -fsS "$A/sessions" | jq -r '.sessions[]|[.id,.role,.status,.ai_cli,.name]|@tsv' | column -t
    exit 0 ;;
  verify) ;;   # handled below (needs helper fns)
  up) ;;
  *) echo "usage: $0 [up|down|status|verify|env]" >&2; exit 2 ;;
esac

[ -x "$WARDEN_BIN" ] || { echo "warden binary not found (set WARDEN_BIN)" >&2; exit 1; }
for t in tmux git python3 jq curl; do command -v "$t" >/dev/null || { echo "missing tool: $t" >&2; exit 1; }; done

verify() {
  local ok=1 sessions tree plan_id usage
  sessions="$(api GET /sessions)"
  tree="$(api GET /tree)"
  usage="$(api GET /usage)"
  plan_id="$(api GET /autopilot | jq -r '.runs[0].plan_id')"
  chk() { # chk <label> <jq-filter> <json> : filter must evaluate true
    if jq -e "$2" <<<"$3" >/dev/null 2>&1; then echo "  ok   $1"; else echo "  FAIL $1"; ok=0; fi
  }
  echo "==> verifying fixture on $ADDR"
  chk "3 projects"                   '.projects|length>=3' "$(api GET /projects)"
  chk "project group with 3 members" '[.groups[]|select(.name=="platform")|.project_ids|length]|max>=3' "$(api GET /project-groups)"
  chk "agent states working/waiting/idle/done" \
      '[.sessions[].status]|unique|(index("working")!=null and index("waiting_for_input")!=null and index("idle")!=null and index("done")!=null)' "$sessions"
  chk "roles autopilot/worker/orchestrator/planner + role-less general agent" \
      '([.sessions[].role]|unique|(index("autopilot")!=null and index("worker")!=null and index("orchestrator")!=null and index("planner")!=null)) and ([.sessions[]|select(.role==null and .kind==null)]|length>=1)' "$sessions"
  chk ">=3 terminals"                '[.sessions[]|select(.kind=="terminal")]|length>=3' "$sessions"
  chk "no session stuck spawning"    '[.sessions[]|select(.status=="spawning")]|length==0' "$sessions"
  chk "autopilot run on a plan with 5-task DAG" '.runs|map(select(.plan_tasks|length>=5))|length>=1' "$(api GET /autopilot)"
  chk "plan tasks done+in_progress+pending" '[.task_progress[]]|unique|(index("done")!=null and index("in_progress")!=null and index("pending")!=null)' "$(api GET "/plans/$plan_id")"
  chk "tree has plan + autopilot_run + workers" \
      '[..|objects|select(.type?)|.type]|unique|(index("plan")!=null and index("autopilot_run")!=null and index("worker")!=null)' "$tree"
  chk "usage: codex + cursor ok"     '[.backends[]|select(.status=="ok")]|length>=2' "$usage"
  chk "backends span 3 billing tiers" '[.backends[]|select(.installed)|.tier]|unique|length>=3' "$(api GET /backends)"
  if grep -rIl "/home/" "$PANES" >/dev/null 2>&1 || grep -q "/home/" <<<"$sessions$tree$usage"; then
    echo "  FAIL real path leaked into API output / panes"; ok=0
  else echo "  ok   no real path in API output / panes"; fi
  [ "$ok" = 1 ] && echo "fixture OK" || { echo "fixture INCOMPLETE" >&2; return 1; }
}
if [ "$CMD" = verify ]; then verify; exit $?; fi

# ---------------------------------------------------------------- reset ----
echo "==> resetting demo instance under $DEMO_ROOT (port $DEMO_PORT)"
stop_all
rm -rf "$DEMO_ROOT"
mkdir -p "$DEMO_ROOT" && : > "$DEMO_ROOT/$MARKER"
mkdir -p "$H/.warden" "$H/.config/cursor" "$DATA" "$TMUXD" "$WORK" "$BIN" "$PANES"
chmod 700 "$TMUXD"

# stub AI CLIs — names match the backend registry's binaries
for n in claude codex cursor-agent opencode aider; do ln -s "$STUBS/demo-cli" "$BIN/$n"; done
# guard: a real CLI in /usr/bin or /bin would shadow the stub's absence
for n in claude codex cursor-agent agy opencode aider crush goose; do
  [ ! -e "/usr/bin/$n" ] && [ ! -e "/bin/$n" ] || { echo "real $n on the demo PATH — refusing" >&2; exit 1; }
done
printf '{"accessToken":"demo-token"}\n' > "$H/.config/cursor/auth.json"

cat > "$CFG" <<EOF
# isolated demo config — generated by site/scripts/demo-seed.sh
addr: $ADDR
data_dir: $DATA
workspace_path: $WORK
tutorial: false
notify:
  enabled: false
worktree:
  spawn_gate: false   # the demo fleet is deliberately larger than the default 5-agent gate
EOF
# Expand to the full documented config once, then drop the two DEPRECATED
# autopilot.brain keys that expansion re-adds: they make every `warden` process
# (including each cockpit pane) print a WARN into the captured terminal.
wd config init >/dev/null 2>&1 || true
python3 - "$CFG" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
s = re.sub(r"\n    backends:\n(?:      .*\n)+?    allow_pay_per_use: (?:true|false)\n", "\n", s)
open(p, "w").write(s)
PY
# plain prompt in the demo shell: no real user/host name in terminal panes
printf "PS1='\\\\w \\\\$ '\nexport PS1\n" > "$H/.bashrc"
printf '[ -f ~/.bashrc ] && . ~/.bashrc\n' > "$H/.bash_profile"

# ------------------------------------------------------------- services ----
echo "==> starting Cursor-API mock (:$MOCK_PORT) and demo daemon ($ADDR)"
nohup env -i PATH=/usr/bin:/bin python3 "$STUBS/usage-mock.py" "$MOCK_PORT" >"$DEMO_ROOT/mock.log" 2>&1 &
echo $! > "$DEMO_ROOT/mock.pid"
nohup env -i HOME="$H" LANG=C.UTF-8 SHELL=/bin/bash TMUX_TMPDIR="$TMUXD" DEMO_PANES="$PANES" \
  XDG_CONFIG_HOME="$H/.config" CURSOR_API_ENDPOINT="http://127.0.0.1:$MOCK_PORT" \
  HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 \
  PATH="$BIN:/usr/bin:/bin" "$WARDEN_BIN" daemon --config "$CFG" >"$DEMO_ROOT/daemon.log" 2>&1 &
echo $! > "$DEMO_ROOT/daemon.pid"
for _ in $(seq 1 40); do curl -fsS "$A/capabilities" >/dev/null 2>&1 && break; sleep 0.5; done
curl -fsS "$A/capabilities" >/dev/null || { echo "daemon failed to start; see $DEMO_ROOT/daemon.log" >&2; exit 1; }
# every pane the daemon starts must see the pane dir (tmux server-global env)
dtmux new-session -d -s demo-anchor -x 200 -y 50 'exec sleep 2147483647'
dtmux set-environment -g DEMO_PANES "$PANES"
dtmux set-option -g status off

# ------------------------------------------------------------- backends ----
echo "==> backend registry: tiers + default"
api POST /backends/rescan '{}' >/dev/null || true
for p in opencode:free claude:subscription codex:subscription cursor:subscription aider:pay_per_use; do
  api PATCH "/backends/${p%%:*}" "{\"tier\":\"${p##*:}\"}" >/dev/null
done
api PUT /backends/default '{"id":"claude"}' >/dev/null

# ------------------------------------------------------------- projects ----
mkrepo() { # mkrepo <name> : a tiny committed repo (typed workers need a base commit)
  local d="$WORK/$1"
  mkdir -p "$d"
  git -C "$d" init -q -b main
  printf '# %s\n' "$1" > "$d/README.md"
  git -C "$d" add README.md
  git -C "$d" -c user.name=Demo -c user.email=demo@example.invalid commit -q -m "initial commit"
}
echo "==> projects + group"
for r in api-gateway billing-svc checkout-svc; do
  mkrepo "$r"
  api POST /projects/local "{\"path\":\"$WORK/$r\",\"name\":\"$r\"}" >/dev/null
done
# resolved ids (symlink-free under /tmp, but ask the daemon rather than assume)
PID_API="$(api GET /projects | jq -r '.projects[]|select(.name=="api-gateway").id')"
PID_BILL="$(api GET /projects | jq -r '.projects[]|select(.name=="billing-svc").id')"
PID_CHK="$(api GET /projects | jq -r '.projects[]|select(.name=="checkout-svc").id')"
api POST /project-groups "$(jq -nc --arg a "$PID_API" --arg b "$PID_BILL" --arg c "$PID_CHK" \
  '{name:"platform",project_ids:[$a,$b,$c]}')" >/dev/null

# ---------------------------------------------------------------- panes ----
setpane() { cat > "$PANES/$1.txt"; }
spawn() { # spawn <json> -> prints the agent/terminal id
  api POST /spawn "$1" | jq -r .id
}
mk() { # mk <jq-args...> <repo> <extra json object> : builds a spawn body
  jq -nc "$@"
}

# ---- plan + autopilot run (api-gateway) ----
echo "==> canonical plan + Autopilot run"
PLAN="$(api POST /plans "$(jq -nc --arg p "$PID_API" '{
  project_id:$p, name:"SSO rollout",
  goal:"Ship SSO login end to end across the gateway without breaking existing API clients",
  constraints:["No breaking API changes","Every task lands as its own reviewed PR"],
  done_when:["All SSO integration tests green","SSO flow documented"],
  tasks:[
   {id:"schema",     prompt:"Add SSO columns to the users table"},
   {id:"middleware", prompt:"Refactor auth middleware to share the token cache", after:["schema"]},
   {id:"login",      prompt:"Implement the SSO login + callback endpoints", after:["schema","middleware"]},
   {id:"tests",      prompt:"Write SSO integration tests", after:["login"]},
   {id:"docs",       prompt:"Document the SSO flow", after:["login"]}]}')" | jq -r .id)"
api POST "/plans/$PLAN/run" '{"execution_mode":"autopilot"}' >/dev/null
sleep 3
MGR="$(api GET /autopilot | jq -r '.runs[0].manager_slot_id')"
RUN="$(api GET /autopilot | jq -r '.runs[0].run_id')"
setpane "$MGR" <<'T'
● Autopilot run — plan "SSO rollout" (5 tasks, gate: auto)

  ✔ schema        landed   PR #31
  ✔ middleware    landed   PR #32
  ▶ login         worker sso-login in flight
  ◦ tests         waiting on login
  ◦ docs          waiting on login

  · Monitoring workers… (esc to interrupt)
T

ap_worker() { # ap_worker <name> <prompt> -> id (nested under the run's manager)
  spawn "$(jq -nc --arg n "$1" --arg p "$2" --arg r "$WORK/api-gateway" --arg m "$MGR" --arg plan "$PLAN" \
    '{role:"worker",repo:$r,name:$n,prompt:$p,parent_id:$m,plan_id:$plan,ai_cli:"claude"}')"
}
W_TOK="$(ap_worker token-cache "Refactor auth middleware to share the token cache")"
W_LOGIN="$(ap_worker sso-login "Implement the SSO login + callback endpoints")"
W_MIG="$(ap_worker sso-columns "Add SSO columns to the users table")"
setpane "$W_TOK" <<'T'
@@EXIT@@
● Shared the token cache across auth + session middleware.

● Bash(npm test -- auth)
  ⎿  42 passing (3.1s)

● Committed and pushed. PR #32 is green — handing back to Autopilot.
T
setpane "$W_LOGIN" <<'T'
● Wiring the SSO callback into the router.

● Update(src/routes/sso.ts)
  ⎿  Updated src/routes/sso.ts with 61 additions and 4 removals

● Bash(npm test -- sso)
  ⎿  Running the SSO suite…

  · Working… (esc to interrupt)
T
setpane "$W_MIG" <<'T'
● Applying the migration that adds the SSO columns to the users table.

● Bash(npm run db:migrate -- --name add_sso_columns)
  ⎿  Running a database migration against the dev database.

Do you want to run this command?

  npm run db:migrate -- --name add_sso_columns

────────────────────────────────────────────────────
 ❯ 1. Yes
   2. Yes, and don't ask again this session
   3. No, and tell Claude what to do differently
T
# Plan task state: schema+middleware done, login in progress. The plan form
# (human) marks done/in_progress; the run's own rollup is the brain form, which
# only the run's manager may call (X-Warden-Actor) — it marks `login` active.
curl -fsS -X POST "$A/autopilot/tasks/status" -H "X-Warden-Actor: $MGR" -H 'content-type: application/json' \
  -d "$(jq -nc --arg r "$RUN" '{run_id:$r,task_id:"login",status:"active"}')" >/dev/null \
  || echo "  (note: run rollup task status not set)" >&2
api POST "/plans/$PLAN/tasks/schema/status" '{"status":"done"}' >/dev/null
api POST "/plans/$PLAN/tasks/middleware/status" '{"status":"done"}' >/dev/null
api POST "/plans/$PLAN/tasks/login/status" '{"status":"in_progress"}' >/dev/null

# ---- billing-svc: worker on codex + a reviewer on cursor + terminal ----
echo "==> billing-svc / checkout-svc agents and terminals"
B_TEST="$(spawn "$(jq -nc --arg r "$WORK/billing-svc" '{role:"worker",repo:$r,name:"webhook-tests",ai_cli:"codex",prompt:"Write integration tests for the billing webhook signature and retry path"}')")"
B_REV="$(spawn "$(jq -nc --arg r "$WORK/billing-svc" '{role:"general",kind:"agent",cwd:$r,name:"pr-482-review",ai_cli:"cursor",prompt:"Review PR #482: rate-limit the public endpoints"}')")"
spawn "$(jq -nc --arg r "$WORK/billing-svc" '{kind:"terminal",cwd:$r,name:"billing-shell"}')" >/dev/null
setpane "$B_TEST" <<'T'
● Adding integration tests for the billing webhook signature + retry path.

● Write(test/webhook.billing.test.ts)
  ⎿  Wrote 96 lines

● Bash(npm run test:int -- billing)
  ⎿  Running…

  · Working… (esc to interrupt)
T
setpane "$B_REV" <<'T'
● Reviewing PR #482 — rate-limit the public endpoints.

● Read(src/routes/public.ts)
  ⎿  Read 210 lines

● The limiter is keyed on IP only; behind the LB that collapses to one bucket.
  Drafting a review comment recommending a per-API-key limiter.

  ⬢ Generating…  ctrl+c to stop
T

# ---- checkout-svc: orchestrator + planner + idle spike + terminal ----
C_ORCH="$(spawn "$(jq -nc --arg r "$WORK/checkout-svc" '{role:"orchestrator",cwd:$r,name:"checkout-orchestrator",ai_cli:"claude",prompt:"Coordinate the checkout reliability work"}')")"
C_PLAN="$(spawn "$(jq -nc --arg r "$WORK/checkout-svc" --arg o "$C_ORCH" '{role:"planner",cwd:$r,name:"retry-planner",ai_cli:"codex",parent_id:$o,prompt:"Plan the retry-budget rollout"}')")"
C_SPIKE="$(spawn "$(jq -nc --arg r "$WORK/checkout-svc" --arg o "$C_ORCH" '{role:"worker",repo:$r,name:"flaky-checkout-spike",ai_cli:"cursor",parent_id:$o,prompt:"Investigate the flaky checkout integration test"}')")"
spawn "$(jq -nc --arg r "$WORK/checkout-svc" '{kind:"terminal",cwd:$r,name:"checkout-shell"}')" >/dev/null
spawn "$(jq -nc --arg r "$WORK/api-gateway" '{kind:"terminal",cwd:$r,name:"gateway-shell"}')" >/dev/null
setpane "$C_ORCH" <<'T'
● Coordinating checkout reliability: planner drafting the retry budget,
  spike worker reproducing the flaky integration test.

  · Working… (esc to interrupt)
T
setpane "$C_PLAN" <<'T'
● Drafting the retry-budget rollout plan (3 tasks).

  · Planning… (esc to interrupt)
T
setpane "$C_SPIKE" <<'T'
● 50× green locally — the flake needs the CI network shim to reproduce.
  Findings written to notes/flaky-checkout.md.

  → Add a follow-up
T

# ----------------------------------------------------------- settle -------
echo "==> waiting for the poller to classify panes"
for _ in $(seq 1 40); do
  n="$(api GET /sessions | jq '[.sessions[]|select(.status=="spawning")]|length')"
  [ "$n" = 0 ] && break
  sleep 1.5
done
# a few Fast-Brain decisions/usage snapshots so the telemetry + usage pages are populated
curl -fsS "$A/usage" >/dev/null || true
sleep 3

echo
echo "==> demo instance seeded on $ADDR"
wd ls 2>/dev/null || true
verify || { echo "demo fixture incomplete — capture setup FAILED (instance left up for inspection; run 'down' to clean)" >&2; exit 1; }
cat <<EOF

Capture (each in its own shell; see 'env' for the exact prefix):
  eval "\$(bash $0 env)"
  # TUI (t2):   cd site/tape && PATH="\$PWD/_demobin:\$PATH" DEMO_ROOT=$DEMO_ROOT vhs cockpit.tape
  # web (t3):   unset WARDEN_TOKEN; screenshot http://$ADDR/ (shoot-web.mjs --url, added by t3)
  # cleanup:    DEMO_ROOT=$DEMO_ROOT DEMO_PORT=$DEMO_PORT bash $0 down
EOF

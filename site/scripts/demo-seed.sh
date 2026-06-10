#!/usr/bin/env bash
# Seed an ISOLATED, throwaway warden instance with a generic demo fleet so we can
# capture clean marketing screenshots (web-overview.png + cockpit.png) without
# exposing the recorder's real agents, home path, or live permission prompts.
#
# Everything lives under /tmp and a scratch $HOME; nothing touches ~/.warden or
# the user's real daemon. Run:   bash site/scripts/demo-seed.sh
# Then follow the printed next-steps to start the demo daemon and capture.
#
# Design: one isolated tmux server (TMUX_TMPDIR) hosts the stub agent sessions;
# the demo daemon (same TMUX_TMPDIR) polls them; the poller classifies status
# from pane content ("esc to interrupt" -> working, "❯"/"Do you want" ->
# waiting_for_input, neither -> the seeded status stands). Terminal statuses
# (done) need no tmux session. claude is kept OFF the daemon PATH so the
# best-effort summarizer no-ops and our seeded subjects stand.
set -euo pipefail

export DEMO_DATA="${DEMO_DATA:-/tmp/warden-demo}"
export DEMO_HOME="${DEMO_HOME:-/tmp/warden-demo-home}"
export TMUX_TMPDIR="${TMUX_TMPDIR:-/tmp/warden-demo-tmux}"
SESS="$DEMO_DATA/sessions"

# CRITICAL isolation: if we're running inside tmux, $TMUX points bare `tmux`
# commands at the REAL server regardless of TMUX_TMPDIR — which would (a) leak
# the recorder's real fleet and (b) scatter our stubs onto their server. Unset
# it so bare tmux uses the isolated default socket under TMUX_TMPDIR. The demo
# daemon and the VHS `warden tui` MUST be launched the same way (env -u TMUX).
unset TMUX

echo "==> resetting demo dirs"
rm -rf "$DEMO_DATA" "$DEMO_HOME" "$TMUX_TMPDIR"
mkdir -p "$SESS" "$DEMO_DATA/closed" "$DEMO_HOME" "$TMUX_TMPDIR"
chmod 700 "$TMUX_TMPDIR"

# Stagger UpdatedAt so the fleet sorts newest-first in a sensible order.
ts() { date -u -v-"$1"M +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "-$1 min" +%Y-%m-%dT%H:%M:%SZ; }

# write_session <file> <json>
sjson() { printf '%s\n' "$2" > "$SESS/$1.json"; }

# ev <minAgo> <type> <detail> -> one event JSON object (for the activity feed)
ev() { printf '{"ts":"%s","type":"%s","detail":"%s"}' "$(ts "$1")" "$2" "$3"; }

echo "==> writing demo session records"

sjson agent-7f3a91c2 "$(cat <<JSON
{ "id":"agent-7f3a91c2","type":"development","tmux_session":"agent-7f3a91c2",
  "workdir":"/work/api-gateway","subject":"Refactor auth middleware to share the token cache",
  "status":"working","supervised":true,"context_tokens":48000,"context_state":"ok",
  "created_at":"$(ts 26)","updated_at":"$(ts 1)","events":[$(ev 5 edit "src/middleware/auth.ts"),$(ev 2 tool "Bash: npm test -- auth")] }
JSON
)"

sjson agent-b8e4d6a0 "$(cat <<JSON
{ "id":"agent-b8e4d6a0","type":"development","tmux_session":"agent-b8e4d6a0",
  "workdir":"/work/api-gateway","worktree":"/work/api-gateway/.wt/sso","branch":"feat/sso-columns",
  "subject":"Add SSO columns to the users table",
  "status":"waiting_for_input","supervised":true,"context_tokens":72000,"context_state":"ok",
  "created_at":"$(ts 18)","updated_at":"$(ts 2)","events":[$(ev 7 edit "migrations/0042_add_sso.sql"),$(ev 2 waiting_for_input "approve: db:migrate")] }
JSON
)"

sjson agent-2c5f0e9d "$(cat <<JSON
{ "id":"agent-2c5f0e9d","type":"spike","tmux_session":"agent-2c5f0e9d",
  "workdir":"/work/checkout-svc","subject":"Investigate flaky checkout integration test",
  "status":"idle","context_tokens":134000,"context_state":"warning",
  "created_at":"$(ts 40)","updated_at":"$(ts 6)","events":[$(ev 18 spawned "spike: flaky checkout test"),$(ev 6 idle "50x green, awaiting review")] }
JSON
)"

sjson prreview-4a1b8c7e "$(cat <<JSON
{ "id":"prreview-4a1b8c7e","type":"pr-review","tmux_session":"prreview-4a1b8c7e",
  "workdir":"/work/api-gateway","pr":"https://github.com/acme/api-gateway/pull/482",
  "subject":"Review PR #482: rate-limit the public endpoints",
  "status":"working","context_tokens":33000,"context_state":"ok",
  "created_at":"$(ts 12)","updated_at":"$(ts 3)","events":[$(ev 9 spawned "review PR #482"),$(ev 3 tool "Read: src/routes/public.ts")] }
JSON
)"

sjson agent-5a6c3f81 "$(cat <<JSON
{ "id":"agent-5a6c3f81","type":"development","tmux_session":"agent-5a6c3f81",
  "workdir":"/work/billing-svc","subject":"Write integration tests for the billing webhook",
  "status":"working","context_tokens":61000,"context_state":"ok",
  "created_at":"$(ts 9)","updated_at":"$(ts 4)","events":[$(ev 8 edit "test/webhook.billing.test.ts"),$(ev 4 tool "Bash: npm run test:int -- billing")] }
JSON
)"

sjson agent-9d2e7b53 "$(cat <<JSON
{ "id":"agent-9d2e7b53","type":"development","tmux_session":"agent-9d2e7b53",
  "workdir":"/work/api-gateway","subject":"Add structured request logging to the gateway",
  "status":"done","exit_code":0,"context_tokens":54000,"context_state":"ok",
  "created_at":"$(ts 55)","updated_at":"$(ts 15)","events":[$(ev 50 spawned "add request logging"),$(ev 15 done "merged, exit 0")] }
JSON
)"

# ---- live tmux stub sessions (pane content drives the poller's classification) ----
echo "==> starting stub tmux sessions on isolated server ($TMUX_TMPDIR)"
TM=(tmux)

pane_working() { cat <<'TXT'
● I'll refactor the auth middleware so the three handlers share one token cache.

● Read(src/middleware/auth.ts)
  ⎿  Read 142 lines

● The token validation is duplicated across the handlers. Extracting a shared
  TokenCache and injecting it into each.

● Update(src/middleware/auth.ts)
  ⎿  Updated src/middleware/auth.ts with 23 additions and 8 removals

● Bash(npm test -- auth)
  ⎿  Running the auth suite…

  · Working… (esc to interrupt)
TXT
}

pane_tests() { cat <<'TXT'
● Adding integration tests for the billing webhook signature + retry path.

● Write(test/webhook.billing.test.ts)
  ⎿  Wrote 96 lines

● Bash(npm run test:int -- billing)
  ⎿  Running…

  · Working… (esc to interrupt)
TXT
}

pane_review() { cat <<'TXT'
● Reviewing PR #482 — rate-limit the public endpoints.

● Read(src/routes/public.ts)
  ⎿  Read 210 lines

● The limiter is keyed on IP only; behind the LB that collapses to one bucket.
  Drafting a review comment recommending a per-API-key key.

  · Working… (esc to interrupt)
TXT
}

pane_waiting() { cat <<'TXT'
● Applying the migration that adds the SSO columns to the users table.

● Bash(npm run db:migrate -- --name add_sso_columns)
  ⎿  Running a database migration against the dev database.

Do you want to run this command?

  npm run db:migrate -- --name add_sso_columns

────────────────────────────────────────────────────
 ❯ 1. Yes
   2. Yes, and don't ask again this session
   3. No, and tell Claude what to do differently
TXT
}

pane_idle() { cat <<'TXT'
● Reproduced the flaky checkout test: it fails ~1 in 8 runs from a race between
  the cart-clear and the order-confirm webhook.

  I added a synchronization point and re-ran the suite 50× with no failures:
    · checkout/order.ts        await the webhook ack before clearing the cart
    · checkout/order.test.ts   inject a deterministic clock

  Done — let me know if you'd like me to open a PR.
TXT
}

PANEDIR="$DEMO_DATA/panes"; mkdir -p "$PANEDIR"
start_stub() { # <session> <pane-fn>
  "${TM[@]}" kill-session -t "$1" 2>/dev/null || true
  "$2" > "$PANEDIR/$1.txt"
  "${TM[@]}" new-session -d -s "$1" -x 200 -y 44 \
    "clear; cat '$PANEDIR/$1.txt'; exec sleep 2147483647"
}

start_stub agent-7f3a91c2   pane_working
start_stub agent-5a6c3f81   pane_tests
start_stub prreview-4a1b8c7e pane_review
start_stub agent-b8e4d6a0   pane_waiting
start_stub agent-2c5f0e9d   pane_idle

# ---- cockpit capture prep (for cockpit.tape) ----
# The stub sessions above started the isolated tmux server. Panes that
# `warden tui` later spawns (list/master/detail) inherit THIS server's global
# environment — NOT the launch shell's — so WARDEN_ADDR must live here or the
# cockpit's list pane falls back to the user's REAL daemon (a fleet leak). Also
# hide the tmux status bar so the screenshot doesn't show the real hostname.
DEMOBIN="$(cd "$(dirname "$0")/../tape/_demobin" 2>/dev/null && pwd || true)"
"${TM[@]}" set-environment -g WARDEN_ADDR 127.0.0.1:8799
"${TM[@]}" set-environment -g WARDEN_DATA_DIR "$DEMO_DATA"
"${TM[@]}" set-environment -g WARDEN_APPROVALS 1
"${TM[@]}" set-environment -g HOME "$DEMO_HOME"
"${TM[@]}" set-environment -g TMUX_TMPDIR "$TMUX_TMPDIR"
[ -n "$DEMOBIN" ] && "${TM[@]}" set-environment -g PATH "$DEMOBIN:/usr/local/bin:/usr/bin:/bin"
"${TM[@]}" set-option -g status off

echo
echo "==> demo instance seeded."
"${TM[@]}" list-sessions 2>/dev/null || true
cat <<EOF

Next steps (each in its own shell, env shown):

  # 1. start the demo daemon (env -u TMUX so it polls the ISOLATED server;
  #    claude kept OFF PATH so the summarizer no-ops and subjects stay seeded):
  env -u TMUX HOME=$DEMO_HOME WARDEN_DATA_DIR=$DEMO_DATA TMUX_TMPDIR=$TMUX_TMPDIR \\
    PATH=/usr/local/bin:/usr/bin:/bin \\
    $(command -v warden) daemon --addr 127.0.0.1:8799

  # 2. verify the fleet:
  WARDEN_ADDR=127.0.0.1:8799 WARDEN_DATA_DIR=$DEMO_DATA $(command -v warden) ls

  # 3a. web shot: open http://127.0.0.1:8799/ and screenshot (Playwright MCP)
  # 3b. tui shot: cd site/tape && render cockpit.tape (drives warden tui)
EOF

# Rate-Limit Auto-Resume Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Harden the existing rate-limit auto-resume feature against three defects — false-positive detection on agent output, a blunt `continue` resume that pollutes the transcript, and `ParseRestoreTime` time-math that can immediate-loop — without changing the scheduler's timer/persistence model.

**Design spec:** `docs/superpowers/specs/2026-06-21-rate-limit-auto-resume-hardening-design.md`

**Architecture:** Three independent, mostly-orthogonal changes plus one config addition:
1. **Detection** (`internal/poller/detect.go`): anchor on Claude's real limit banner and match only the *trailing* lines of the pane; reorder `classify` so an actively-streaming agent (`esc to interrupt`) short-circuits before rate-limit detection.
2. **Resume** (`internal/daemon/ratelimit.go`): default to a bare un-pause keypress (no injected user turn), make a textual nudge opt-in via a new `rate_limit_resume_prompt` config key, and gate any action on the hardened detection still confirming the banner at resume time.
3. **Time parsing** (`internal/poller/detect.go`): roll past clock-times forward 24h instead of returning `now`, collapse the redundant am/pm logic into the regex group (deleting `detectAmPm`), and bias the zone-less fallback later.

**Tech Stack:** Go 1.26+, existing warden infrastructure (poller, daemon scheduler, lifecycle, config file). No new dependencies.

**Open question (blocks final regex/keypress values):** the exact Claude Code limit banner string and the un-pause keypress must be confirmed against a live limit hit. Until confirmed, implement behind the trailing-window + working-veto guards so a wrong guess **fails closed** (misses a real limit) rather than open (misclassifies a working agent). Banner-dependent literals are isolated into named constants so they can be corrected in one place — see Task 2.

---

## File Structure

### Modified Files
- `internal/config/config.go` — add `rate_limit_resume_prompt` field, default, schema entry
- `internal/config/config_test.go` — config load/default/drift tests
- `internal/poller/detect.go` — trailing-window + banner-anchored detection; time-parse rollover, am/pm collapse, later-bias; delete `detectAmPm`; export a banner-check helper
- `internal/poller/detect_test.go` — detection + time-parse tests
- `internal/poller/poller.go` — reorder `classify` so `esc to interrupt` wins first
- `internal/poller/poller_test.go` — classify ordering tests
- `internal/daemon/ratelimit.go` — `resumePrompt` field + constructor param; bare-keypress default; gated nudge
- `internal/daemon/ratelimit_test.go` — resume-path tests + updated constructor calls
- `internal/cli/daemon.go` — thread `cfg.RateLimitResumePrompt` into `NewRateLimitScheduler`

### No New Files
All changes land in existing files.

---

## Task 1: Add `rate_limit_resume_prompt` Config Key

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write test for the new key (load + default)**

Add to `internal/config/config_test.go`:

```go
func TestLoad_RateLimitResumePrompt_Default(t *testing.T) {
	path := tmpConfig(t, "") // empty file → all defaults
	c := Load(path)
	require.Equal(t, "", c.RateLimitResumePrompt, "default must be empty (keypress-only)")
}

func TestLoad_RateLimitResumePrompt_Set(t *testing.T) {
	path := tmpConfig(t, "rate_limit_resume_prompt: continue\n")
	c := Load(path)
	require.Equal(t, "continue", c.RateLimitResumePrompt)
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd internal/config && go test -run TestLoad_RateLimitResumePrompt -v
```

Expected: FAIL — field undefined.

- [ ] **Step 3: Add the struct field, schema entry, and default**

In `internal/config/config.go`, add the field next to the other rate-limit fields (`config.go:52-54`):

```go
	RateLimitResumePrompt  string `yaml:"rate_limit_resume_prompt"`
```

Add to the schema table next to the other rate-limit rows (`config.go:91-93`):

```go
	{"rate_limit_resume_prompt", "Text to send when resuming a rate-limited agent. Empty = bare keypress (no injected user turn). Values: any string"},
```

Add the default next to the other rate-limit defaults (`config.go:125-127`):

```go
		RateLimitResumePrompt:  "",
```

No validation needed (any string is valid; empty is the meaningful default). Do **not** add it to the `validDuration` block.

- [ ] **Step 4: Run to verify it passes + drift-guard stays green**

```bash
cd internal/config && go test -run "RateLimitResumePrompt|Drift|Schema" -v && go test ./...
```

Expected: PASS, including the reflection drift-guard test (YAML tags ↔ schema keys).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add rate_limit_resume_prompt (default empty)

Empty means resume with a bare keypress and no injected user turn.
A non-empty value opts into sending that text after the limit clears.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Anchor Detection on the Trailing Limit Banner

**Files:**
- Modify: `internal/poller/detect.go`
- Test: `internal/poller/detect_test.go`

**Defect addressed:** #1 (false positives). Loose keyword substring matching over the whole scrollback (`detect.go:14-39`) fires on any agent that merely *prints* "rate limit". Fix: require Claude's real banner shape (a limit phrase co-located with its `resets …` clause) AND match only the trailing ~6 lines.

- [ ] **Step 1: Write tests for the hardened detector**

Add to `internal/poller/detect_test.go`:

```go
func TestDetectRateLimit_TrailingBannerMatches(t *testing.T) {
	// Trailing lines are the real banner (limit phrase + reset clause).
	pane := "working...\n" + sampleLimitBanner // sampleLimitBanner = fixture, see Step 3
	got, _, _ := detectRateLimit(pane)
	require.True(t, got, "real trailing banner must be detected")
}

func TestDetectRateLimit_AgentOutputDoesNotMatch(t *testing.T) {
	// Agent is writing/reviewing rate-limit code; words appear but no banner shape.
	pane := `func detectRateLimit(pane string) {
  // matches "rate limit", "usage limit", "session limit"
}
❯ esc to interrupt`
	got, _, _ := detectRateLimit(pane)
	require.False(t, got, "agent output mentioning limits must not match")
}

func TestDetectRateLimit_BannerScrolledAwayDoesNotMatch(t *testing.T) {
	// Banner appeared earlier but newer output pushed it out of the trailing window.
	pane := sampleLimitBanner + strings.Repeat("\nnormal work line", 20)
	got, _, _ := detectRateLimit(pane)
	require.False(t, got, "banner outside the trailing window must not match")
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd internal/poller && go test -run TestDetectRateLimit -v
```

Expected: FAIL (current whole-pane keyword scan matches the agent-output and scrolled-away cases).

- [ ] **Step 3: Isolate banner literals and match the trailing window**

In `internal/poller/detect.go`, replace the keyword loop in `detectRateLimit` (`detect.go:14-39`) with a trailing-window, banner-anchored check. Keep all banner-dependent literals in named, clearly-flagged constants/vars so they can be corrected in one place once confirmed against a live hit:

```go
// claudeLimitBannerRe matches Claude Code's limit banner. It requires a limit
// phrase together with the reset clause ParseRestoreTime keys on, so an agent
// merely printing "rate limit" does not match.
//
// TODO(open-question): confirm the exact banner wording against a LIVE limit
// hit before relying on this in production. Until confirmed this errs toward
// failing CLOSED (a too-strict pattern misses a real limit) rather than open.
var claudeLimitBannerRe = regexp.MustCompile(
	`(?i)(rate limit|usage limit|session limit|quota exceeded)[\s\S]{0,80}?resets\s`,
)

// limitBannerTailLines is how many trailing pane lines we inspect. A real
// banner is the terminal state of the pane; anything that scrolled above it is
// stale output, not a live limit.
const limitBannerTailLines = 6

func detectRateLimit(pane string) (bool, time.Time, bool) {
	tail := lastLines(pane, limitBannerTailLines)
	if !claudeLimitBannerRe.MatchString(tail) {
		return false, time.Time{}, false
	}
	restoreTime, ok := ParseRestoreTime(tail)
	return true, restoreTime, ok
}
```

Notes:
- `lastLines` already exists in `internal/poller/poller.go:406` (same package) — reuse it; do not duplicate.
- Parse the restore time from `tail` (not the whole pane) so Task 4's am/pm-from-group logic only ever sees the banner region.
- The fixture `sampleLimitBanner` (and `strings` import) live in the test file; set it to the best-known banner text and add a comment that it must be replaced with the verbatim live string.

- [ ] **Step 4: Run to verify it passes**

```bash
cd internal/poller && go test -run TestDetectRateLimit -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/detect.go internal/poller/detect_test.go
git commit -m "fix(poller): anchor rate-limit detection on the trailing banner

Match Claude's limit banner (limit phrase + reset clause) in only the
last few pane lines instead of substring-scanning the whole scrollback,
so an agent that merely prints 'rate limit' is no longer misclassified.
Banner literals isolated into named constants pending live confirmation.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Let an Actively-Working Agent Win in `classify`

**Files:**
- Modify: `internal/poller/poller.go`
- Test: `internal/poller/poller_test.go`

**Defect addressed:** #1 (defense-in-depth). `classify` checks rate-limit *before* `esc to interrupt` (`poller.go:25-33`). A streaming agent and a real banner never legitimately coexist, so "working" should short-circuit first.

- [ ] **Step 1: Write the ordering test**

Add to `internal/poller/poller_test.go`:

```go
func TestClassify_WorkingVetoesStrayLimitKeyword(t *testing.T) {
	s := &store.Session{ID: "t", Status: store.StatusWorking}
	// Both a limit-ish line and the active-streaming marker present.
	pane := "discussing rate limit handling...\nesc to interrupt"
	got := classify(s, pane, true, 0, 0)
	require.Equal(t, store.StatusWorking, got, "esc to interrupt must win")
}

func TestClassify_RealLimitWhenNotStreaming(t *testing.T) {
	s := &store.Session{ID: "t", Status: store.StatusWorking}
	got := classify(s, sampleLimitBanner, true, 0, 0)
	require.Equal(t, store.StatusRateLimited, got)
}
```

- [ ] **Step 2: Run to verify the first test fails**

```bash
cd internal/poller && go test -run TestClassify_Working -v
```

Expected: FAIL — current order returns `rate_limited`.

- [ ] **Step 3: Reorder `classify`**

In `internal/poller/poller.go`, move the `esc to interrupt` check (`poller.go:31-33`) *above* the rate-limit check (`poller.go:25-29`):

```go
	if !sessionAlive {
		return store.StatusOrphaned
	}

	// An agent that is actively streaming (esc to interrupt) is working; a real
	// limit banner only appears once streaming has stopped, so working wins and
	// we never even evaluate rate-limit detection on a live agent.
	if strings.Contains(pane, "esc to interrupt") {
		return store.StatusWorking
	}

	// Rate limit is checked before the waiting/idle heuristics so a banner is
	// not misread as waiting_for_input.
	if isLimited, _, _ := detectRateLimit(pane); isLimited {
		return store.StatusRateLimited
	}
	// ... remaining checks unchanged (❯ / "Do you want", stuck→idle) ...
```

- [ ] **Step 4: Run to verify it passes + full poller suite**

```bash
cd internal/poller && go test ./... -v
```

Expected: PASS, existing waiting/idle/stuck cases unaffected.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go
git commit -m "fix(poller): check 'esc to interrupt' before rate-limit in classify

A streaming agent and a real limit banner never coexist; letting the
working marker short-circuit makes a stray limit keyword unable to
misclassify an active agent.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Fix `ParseRestoreTime` Time Math

**Files:**
- Modify: `internal/poller/detect.go`
- Test: `internal/poller/detect_test.go`

**Defect addressed:** #3. No date rollover (past clock-time → returns `now` → immediate retry loop, `detect.go:89-91`/`124-126`); redundant am/pm logic (`detect.go:65-72` + `detectAmPm` at `136-158`); zone-less generic fallback can land earlier than the true reset.

- [ ] **Step 1: Write tests for rollover, am/pm-from-group, and later-bias**

Add to `internal/poller/detect_test.go`:

```go
func TestParseRestoreTime_RollsPastTimeToTomorrow(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Madrid")
	now := time.Now().In(loc)
	// Pick a clock-time one hour BEFORE now → must roll to tomorrow, not return now.
	past := now.Add(-1 * time.Hour)
	pane := "resets " + past.Format("15:04") + " (Europe/Madrid)"
	got, ok := ParseRestoreTime(pane)
	require.True(t, ok)
	require.True(t, got.After(time.Now()), "past clock-time must roll forward, not return now")
	require.WithinDuration(t, now.Add(23*time.Hour), got, 90*time.Minute)
}

func TestParseRestoreTime_AmPmFromGroup(t *testing.T) {
	// An unrelated 'pm'/'am' elsewhere in the pane must not flip the parse.
	loc, _ := time.LoadLocation("Europe/Madrid")
	pane := "the pm reviewed it; resets 1:30am (Europe/Madrid)"
	got, ok := ParseRestoreTime(pane)
	require.True(t, ok)
	require.Equal(t, 1, got.In(loc).Hour())
}

func TestParseRestoreTime_24Hour(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Madrid")
	pane := "resets 13:30 (Europe/Madrid)"
	got, ok := ParseRestoreTime(pane)
	require.True(t, ok)
	require.Equal(t, 13, got.In(loc).Hour())
}

func TestParseRestoreTime_ZonelessNeverBeforeNow(t *testing.T) {
	got, ok := ParseRestoreTime("try again at 00:01")
	require.True(t, ok)
	require.False(t, got.Before(time.Now()), "zone-less fallback must never return a past time")
}
```

- [ ] **Step 2: Run to verify failures**

```bash
cd internal/poller && go test -run TestParseRestoreTime -v
```

Expected: FAIL — rollover returns `now`; am/pm from a whole-pane scan can misparse.

- [ ] **Step 3: Rewrite the two patterns and delete `detectAmPm`**

In `internal/poller/detect.go`:

Pattern 1 — capture am/pm in the regex group, pick the layout from that one capture (delete the `detect.go:65-72` whole-pane scan and the `detectAmPm` call at `detect.go:76`), and roll forward instead of returning `now`:

```go
	// (am|pm) is captured adjacent to the time; one source of truth.
	reClaudeCode := regexp.MustCompile(`(?i)resets\s+(\d{1,2}:\d{2})(am|pm)?\s*\(([^)]+)\)`)
	if m := reClaudeCode.FindStringSubmatch(pane); len(m) == 4 {
		timeStr, ampm, tzName := m[1], strings.ToLower(m[2]), m[3]
		loc, err := time.LoadLocation(tzName)
		if err != nil {
			return time.Time{}, false
		}
		layout := "15:04"
		if ampm != "" {
			layout, timeStr = "3:04pm", timeStr+ampm
		}
		resetTime, err := time.ParseInLocation(layout, timeStr, loc)
		if err != nil {
			return time.Time{}, false
		}
		now := time.Now().In(loc)
		result := time.Date(now.Year(), now.Month(), now.Day(),
			resetTime.Hour(), resetTime.Minute(), 0, 0, loc)
		// A clock-time earlier than now is the NEXT occurrence, not one already
		// past — roll to tomorrow so we never schedule an immediate retry loop.
		if result.Before(now) {
			result = result.Add(24 * time.Hour)
		}
		return result, true
	}
```

Pattern 2 (generic, zone-less) — same rollover, and never return a past time; let the scheduler's `+buffer` (`ratelimit.go:55`) absorb cross-zone skew:

```go
	reGeneric := regexp.MustCompile(`(?i)(?:at|again at)\s+(\d{1,2}:\d{2})\s*(am|pm)?`)
	if m := reGeneric.FindStringSubmatch(pane); len(m) >= 2 {
		timeStr, ampm := m[1], strings.ToLower(m[2])
		layout := "15:04"
		if ampm != "" {
			layout, timeStr = "3:04pm", timeStr+ampm
		}
		resetTime, err := time.Parse(layout, timeStr)
		if err != nil {
			return time.Time{}, false
		}
		now := time.Now()
		result := time.Date(now.Year(), now.Month(), now.Day(),
			resetTime.Hour(), resetTime.Minute(), 0, 0, now.Location())
		if result.Before(now) {
			result = result.Add(24 * time.Hour)
		}
		return result, true
	}
```

Then **delete** `detectAmPm` (`detect.go:136-158`) entirely — it has no remaining callers. Keep the `ParseRestoreTime` signature `(time.Time, bool)` unchanged so `ratelimit.go:50` and `ratelimit.go:184` are unaffected.

- [ ] **Step 4: Run to verify pass + no dangling references**

```bash
cd internal/poller && go vet ./... && go test -run TestParseRestoreTime -v
```

Expected: PASS; `go vet` confirms `detectAmPm` removal left no unused references.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/detect.go internal/poller/detect_test.go
git commit -m "fix(poller): roll past reset times forward; collapse am/pm parsing

Past clock-times now roll to tomorrow instead of returning now (no more
immediate retry loops). am/pm is captured in the regex group (one source
of truth) and detectAmPm is deleted. The zone-less fallback never returns
a past time, leaving the scheduler buffer to absorb cross-zone skew.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Bare-Keypress Resume, Configurable & Gated

**Files:**
- Modify: `internal/daemon/ratelimit.go`, `internal/cli/daemon.go`
- Test: `internal/daemon/ratelimit_test.go`

**Defect addressed:** #2 (blunt resume). Today `attemptResume` types `"continue"` via `life.Input` on `ErrAlreadyRunning` (`ratelimit.go:115-119`). Default to a bare un-pause keypress (`life.SendKeys`, no injected turn); send text only when `rate_limit_resume_prompt` is non-empty; gate either on the banner still being present.

- [ ] **Step 1: Export a banner-check helper from the poller**

The scheduler is in `internal/daemon` and cannot call the unexported `detectRateLimit`. Add a thin exported wrapper in `internal/poller/detect.go` so the gate reuses the exact Task-2 logic (no duplicated keyword list):

```go
// LimitBannerPresent reports whether pane's trailing lines show Claude's limit
// banner. Exported for the daemon resume gate; reuses detectRateLimit.
func LimitBannerPresent(pane string) bool {
	ok, _, _ := detectRateLimit(pane)
	return ok
}
```

Add a one-line test in `detect_test.go` asserting it tracks `detectRateLimit`.

- [ ] **Step 2: Write resume-path tests (keypress default, configured nudge, gating)**

Add to `internal/daemon/ratelimit_test.go` (extend the existing fake lifecycle to record `SendKeys`/`Input`/`Output` calls):

```go
func TestAttemptResume_DefaultUsesBareKeypressNotInput(t *testing.T) {
	// rate_limit_resume_prompt == "", tmux session exists (ErrAlreadyRunning),
	// banner still present in Output.
	life := &fakeLife{restoreErr: lifecycle.ErrAlreadyRunning, output: sampleLimitBanner}
	st := newFakeStore(rateLimitedSession("a"))
	sched := NewRateLimitScheduler(life, st, 30*time.Minute, time.Minute, true, "") // new "" arg
	sched.attemptResume("a")
	require.Equal(t, 1, life.sendKeysCalls, "default resume is a bare keypress")
	require.Equal(t, 0, life.inputCalls, "no injected user turn by default")
	require.Equal(t, store.StatusSpawning, st.get("a").Status)
}

func TestAttemptResume_ConfiguredPromptUsesInput(t *testing.T) {
	life := &fakeLife{restoreErr: lifecycle.ErrAlreadyRunning, output: sampleLimitBanner}
	st := newFakeStore(rateLimitedSession("a"))
	sched := NewRateLimitScheduler(life, st, 30*time.Minute, time.Minute, true, "continue")
	sched.attemptResume("a")
	require.Equal(t, 1, life.inputCalls)
	require.Equal(t, "continue", life.lastInput)
}

func TestAttemptResume_GateSkipsWhenBannerGone(t *testing.T) {
	// Agent already moved on: Output no longer shows the banner.
	life := &fakeLife{restoreErr: lifecycle.ErrAlreadyRunning, output: "normal work\nesc to interrupt"}
	st := newFakeStore(rateLimitedSession("a"))
	sched := NewRateLimitScheduler(life, st, 30*time.Minute, time.Minute, true, "continue")
	sched.attemptResume("a")
	require.Equal(t, 0, life.inputCalls, "no nudge when the banner is gone")
	require.Equal(t, 0, life.sendKeysCalls)
}
```

- [ ] **Step 3: Run to verify failures**

```bash
cd internal/daemon && go test -run TestAttemptResume -v
```

Expected: FAIL — constructor arity and new behavior absent.

- [ ] **Step 4: Add the field + constructor param**

In `internal/daemon/ratelimit.go`, add to the struct (after `enabled`):

```go
	resumePrompt string // text to inject on resume; "" = bare keypress only
```

Update the constructor (`ratelimit.go:30`):

```go
func NewRateLimitScheduler(life Lifecycle, st store.Store, retryInterval, buffer time.Duration, autoResume bool, resumePrompt string) *RateLimitScheduler {
	return &RateLimitScheduler{
		life:          life,
		store:         st,
		timers:        make(map[string]*time.Timer),
		retryInterval: retryInterval,
		buffer:        buffer,
		enabled:       autoResume,
		resumePrompt:  resumePrompt,
	}
}
```

- [ ] **Step 5: Replace the `"continue"` branch with gate + keypress/prompt**

In `attemptResume`, replace the `ErrAlreadyRunning` block (`ratelimit.go:115-148`). Capture the pane, gate on the banner, then either press the un-pause key or send the configured prompt:

```go
	if err == lifecycle.ErrAlreadyRunning {
		// Gate: only act if the limit banner is still the trailing pane state.
		// If the agent already moved on, do nothing destructive — clear and exit.
		pane, _ := r.life.Output(ctx, sess.TmuxSession, limitBannerTailLines)
		if !poller.LimitBannerPresent(pane) {
			_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusSpawning)
			_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
				TS: time.Now().UTC(), Type: "rate-limit-resumed",
				Detail: "banner cleared before resume; no nudge sent",
			})
			r.clearTimer(sess.ID)
			return
		}

		var sendErr error
		detail := "sent bare resume keypress (tmux session exists)"
		if r.resumePrompt == "" {
			sendErr = r.life.SendKeys(ctx, sess.TmuxSession, resumeKey) // see note
		} else {
			sendErr = r.life.Input(ctx, sess.TmuxSession, r.resumePrompt)
			detail = "sent resume prompt " + strconv.Quote(r.resumePrompt)
		}
		if sendErr != nil {
			_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusErrored)
			_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
				TS: time.Now().UTC(), Type: "rate-limit-resume-failed",
				Detail: "resume send failed: " + sendErr.Error(),
			})
			r.clearTimer(sess.ID)
			return
		}
		_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusSpawning)
		_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
			TS: time.Now().UTC(), Type: "rate-limit-resumed", Detail: detail,
		})
		r.clearTimer(sess.ID)
		return
	}
```

Notes:
- `resumeKey` is a banner-dependent named const (e.g. the key that un-pauses a limit-paused Claude pane) co-located with the Task-2 banner constants. **TODO(open-question):** confirm against a live limit hit. Mark it clearly; a wrong key is a no-op the next tick re-detects, not a transcript-corrupting action.
- Add `"github.com/srjn45/warden/internal/poller"` and `"strconv"` imports; `poller` is already imported for `ParseRestoreTime`.
- Extract the repeated `r.mu.Lock(); delete(r.timers, id); r.mu.Unlock()` into a small `clearTimer(id)` helper to keep this block readable (used in several existing branches too).

- [ ] **Step 6: Thread the config value at the call site**

In `internal/cli/daemon.go:107`, pass the new arg:

```go
rateLimitSched := daemon.NewRateLimitScheduler(life, st,
	cfg.RateLimitRetryIntervalDuration(), cfg.RateLimitBufferDuration(),
	cfg.RateLimitAutoResume, cfg.RateLimitResumePrompt)
```

Update the existing `NewRateLimitScheduler(...)` calls in `ratelimit_test.go` (the ~12 call sites) to pass a trailing `""` (or the prompt under test).

- [ ] **Step 7: Run to verify pass + build**

```bash
cd internal/daemon && go test ./... -v
cd /home/srjn45/dev/warden && go build ./...
```

Expected: PASS and clean build.

- [ ] **Step 8: Commit**

```bash
git add internal/poller/detect.go internal/poller/detect_test.go \
        internal/daemon/ratelimit.go internal/daemon/ratelimit_test.go \
        internal/cli/daemon.go
git commit -m "feat(daemon): resume rate-limited agents with a bare keypress

Default resume no longer types 'continue' as a user turn: it sends a bare
un-pause keypress. A non-empty rate_limit_resume_prompt opts into sending
text instead. Both are gated on the limit banner still being present, so a
stale schedule never nudges an agent that already moved on. resumeKey is a
named const pending live confirmation of the banner/keypress.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: Full Suite, Vet, and Manual Verification

**Files:** all modified files.

- [ ] **Step 1: Full test suite + vet**

```bash
cd /home/srjn45/dev/warden
go vet ./...
go test ./...
```

Expected: exit 0.

- [ ] **Step 2: Confirm the config file migrates the new key**

```bash
go run ./cmd/warden config init --config /tmp/warden-test.yaml
grep -n rate_limit_resume_prompt /tmp/warden-test.yaml   # present, default empty, with hint
```

- [ ] **Step 3: Resolve the open question against a live limit (when one occurs)**

When a real limit is hit, capture the verbatim pane (`warden attach`, or the stored `LastPaneExcerpt`) and:
- set `claudeLimitBannerRe` / `sampleLimitBanner` to the exact banner text,
- set `limitBannerTailLines` to the banner's true height,
- set `resumeKey` to the confirmed un-pause keystroke,
- re-run `internal/poller` and `internal/daemon` tests.

Until then the guards keep behavior fail-closed.

- [ ] **Step 4: Manual end-to-end smoke (best-effort)**

```bash
go build -o bin/warden ./cmd/warden
# Inject a fixture limit pane (or wait for a real one), then:
./bin/warden ls            # agent shows rate_limited only on a real trailing banner
./bin/warden status <id>   # rate-limit info + schedule
# After the scheduled time: agent un-pauses with no spurious 'continue' turn in transcript.
```

- [ ] **Step 5: Final commit (if any test fixtures/docs were touched)**

```bash
git add -A
git commit -m "test(rate-limit): finalize hardening fixtures and smoke checks

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Summary

Six tasks, each TDD (write test → fail → implement → pass → commit):

1. ✅ Add `rate_limit_resume_prompt` config key (default empty).
2. ✅ Anchor detection on the trailing limit banner (kills false positives).
3. ✅ Reorder `classify` so `esc to interrupt` short-circuits first.
4. ✅ Fix `ParseRestoreTime`: roll past times forward, collapse am/pm, bias later; delete `detectAmPm`.
5. ✅ Bare-keypress resume, configurable nudge, gated on the banner; export `LimitBannerPresent`; thread config.
6. ✅ Full suite + vet + manual verification; resolve the live-banner open question.

**Banner-dependent literals** (`claudeLimitBannerRe`, `limitBannerTailLines`, `resumeKey`, `sampleLimitBanner`) are isolated so the one open question — the exact Claude limit banner string and un-pause keypress — can be settled in a single place once a live limit is observed, with all guards failing closed until then.

package lifecycle

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

func enabledSettings() backendstore.HandoverSettings {
	return backendstore.HandoverSettings{
		Enabled:               true,
		ContextFillThreshold:  90,
		RollingQuotaThreshold: 90,
		CooldownPeriod:        15 * time.Minute,
	}
}

func TestDecideHotSwapDisabled(t *testing.T) {
	in := ThresholdInput{
		Settings:      backendstore.HandoverSettings{Enabled: false, ContextFillThreshold: 90},
		ContextTokens: 100, ContextLimit: 100, ContextKnown: true,
	}
	if sig := DecideHotSwap(in); sig.Trigger {
		t.Fatalf("disabled handover must never trigger, got %#v", sig)
	}
}

func TestDecideHotSwapContextFillTriggers(t *testing.T) {
	in := ThresholdInput{
		Settings:      enabledSettings(),
		ContextTokens: 185_000, ContextLimit: 200_000, ContextKnown: true, // 92.5%
	}
	sig := DecideHotSwap(in)
	if !sig.Trigger {
		t.Fatalf("92%% fill should trigger, got %#v", sig)
	}
	if sig.Reason != SwapReasonContextFill {
		t.Fatalf("Reason = %q, want context_fill", sig.Reason)
	}
	if sig.ContextFillPct != 92 {
		t.Fatalf("ContextFillPct = %d, want 92", sig.ContextFillPct)
	}
}

func TestDecideHotSwapBelowThresholdNoTrigger(t *testing.T) {
	in := ThresholdInput{
		Settings:      enabledSettings(),
		ContextTokens: 170_000, ContextLimit: 200_000, ContextKnown: true, // 85%
		QuotaUsed: 80, QuotaLimit: 100, QuotaKnown: true, // 80%
	}
	if sig := DecideHotSwap(in); sig.Trigger {
		t.Fatalf("both below threshold should not trigger, got %#v", sig)
	}
}

func TestDecideHotSwapQuotaDoesNotTrigger(t *testing.T) {
	in := ThresholdInput{
		Settings:  enabledSettings(),
		QuotaUsed: 95, QuotaLimit: 100, QuotaKnown: true, // 95%
	}
	if sig := DecideHotSwap(in); sig.Trigger {
		t.Fatalf("proactive quota readings must not trigger; hard-limit recovery is reactive, got %#v", sig)
	}
}

// TestDecideHotSwapContextWinsOverQuota: when both cross, context fill (the more
// urgent, unrecoverable failure) is reported.
func TestDecideHotSwapContextWinsOverQuota(t *testing.T) {
	in := ThresholdInput{
		Settings:      enabledSettings(),
		ContextTokens: 195_000, ContextLimit: 200_000, ContextKnown: true, // 97.5%
		QuotaUsed: 99, QuotaLimit: 100, QuotaKnown: true, // 99%
	}
	if sig := DecideHotSwap(in); sig.Reason != SwapReasonContextFill {
		t.Fatalf("Reason = %q, want context_fill to win, got %#v", sig.Reason, sig)
	}
}

// TestDecideHotSwapCooldownSuppresses: a swap within the cooldown window is
// suppressed even though the threshold is crossed.
func TestDecideHotSwapCooldownSuppresses(t *testing.T) {
	in := ThresholdInput{
		Settings:      enabledSettings(),
		ContextTokens: 195_000, ContextLimit: 200_000, ContextKnown: true,
		HasSwapped: true, SinceSwap: 5 * time.Minute, // < 15m cooldown
	}
	if sig := DecideHotSwap(in); sig.Trigger {
		t.Fatalf("swap within cooldown must be suppressed, got %#v", sig)
	}
	// Past the cooldown, the same fill triggers.
	in.SinceSwap = 20 * time.Minute
	if sig := DecideHotSwap(in); !sig.Trigger {
		t.Fatalf("swap after cooldown should trigger, got %#v", sig)
	}
}

// TestDecideHotSwapUnknownMeasurementsNoTrigger: a missing reading (unknown, or a
// zero limit) is never evaluated, so the policy cannot fire on data it lacks.
func TestDecideHotSwapUnknownMeasurementsNoTrigger(t *testing.T) {
	// Context not known, quota limit zero → nothing to evaluate.
	in := ThresholdInput{
		Settings:      enabledSettings(),
		ContextTokens: 999_999, ContextLimit: 0, ContextKnown: false,
		QuotaUsed: 999, QuotaLimit: 0, QuotaKnown: true,
	}
	if sig := DecideHotSwap(in); sig.Trigger {
		t.Fatalf("unknown measurements must not trigger, got %#v", sig)
	}
}

// TestDecideHotSwapThresholdDefaults: a zero/invalid threshold falls back to 90.
func TestDecideHotSwapThresholdDefaults(t *testing.T) {
	s := backendstore.HandoverSettings{Enabled: true} // thresholds unset (0)
	// 90% exactly should trigger against the 90 default.
	in := ThresholdInput{
		Settings:      s,
		ContextTokens: 90, ContextLimit: 100, ContextKnown: true,
	}
	if sig := DecideHotSwap(in); !sig.Trigger {
		t.Fatalf("90%% should meet the default 90 threshold, got %#v", sig)
	}
	// 89% should not.
	in.ContextTokens = 89
	if sig := DecideHotSwap(in); sig.Trigger {
		t.Fatalf("89%% should be below the default 90 threshold, got %#v", sig)
	}
}

func TestDecideHotSwapCustomThreshold(t *testing.T) {
	s := backendstore.HandoverSettings{Enabled: true, ContextFillThreshold: 75}
	in := ThresholdInput{
		Settings:      s,
		ContextTokens: 76, ContextLimit: 100, ContextKnown: true,
	}
	if sig := DecideHotSwap(in); !sig.Trigger {
		t.Fatalf("76%% should cross a custom 75%% threshold, got %#v", sig)
	}
}

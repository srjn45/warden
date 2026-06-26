package cli

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// statusOf finds a catalog model in a scored suggestion list.
func suggestionFor(t *testing.T, s []llmSuggestion, name string) llmSuggestion {
	t.Helper()
	for _, c := range s {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("model %q not in suggestions", name)
	return llmSuggestion{}
}

func recName(s []llmSuggestion) string {
	if r, ok := recommended(s); ok {
		return r.Name
	}
	return ""
}

func TestSuggestModels_RoomyMachinePicksBestConductor(t *testing.T) {
	// 48 GB total, 30 GB free: everything but the 65 GB server model fits now;
	// the recommendation is the best-scoring conductor with headroom — a leaner
	// agentic model, NOT the biggest (coding-tuned) one.
	s := suggestModels(48, 30)
	require.Equal(t, "gpt-oss:20b", recName(s))
	require.Equal(t, fitNow, suggestionFor(t, s, "qwen3:30b-a3b").Status)
	require.Equal(t, fitTooLarge, suggestionFor(t, s, "gpt-oss:120b").Status)
	// The heavy coding-tuned model fits but is never the pick over a better score.
	require.False(t, suggestionFor(t, s, "qwen2.5-coder:32b").Recommended)
}

func TestSuggestModels_BusyMachineScalesDown(t *testing.T) {
	// Same 48 GB box but only 16 GB free right now → a lighter model with comfort.
	s := suggestModels(48, 16)
	require.Equal(t, "qwen3:14b", recName(s))
	// A model that needs more than free is "free memory first", not "fits now".
	require.Equal(t, fitFreeFirst, suggestionFor(t, s, "qwen2.5-coder:32b").Status)
}

func TestSuggestModels_FreeMemoryBoundsTheFit(t *testing.T) {
	// Plenty of total RAM but almost none free: nothing runs now, but models that
	// fit the machine are flagged "free memory first", and the pick is the best
	// of those rather than nothing.
	s := suggestModels(32, 1)
	require.Equal(t, fitFreeFirst, suggestionFor(t, s, "qwen2.5:3b").Status)
	require.NotEmpty(t, recName(s))
	r, _ := recommended(s)
	require.Equal(t, fitFreeFirst, r.Status)
}

func TestSuggestModels_TinyMachineFallsBackToFloor(t *testing.T) {
	// 2 GB everything: even the smallest model is too large, but we still point at
	// the floor so the operator isn't left with no recommendation.
	s := suggestModels(2, 2)
	require.Equal(t, "qwen2.5:3b", recName(s))
	require.Equal(t, fitTooLarge, suggestionFor(t, s, "qwen2.5:3b").Status)
}

func TestBetter_PrefersScoreThenLessMemory(t *testing.T) {
	cat := []llmCandidate{
		{Name: "low", RAMGB: 8, Score: 5},
		{Name: "high-heavy", RAMGB: 20, Score: 9},
		{Name: "high-light", RAMGB: 16, Score: 9},
	}
	// higher score beats lower score
	require.Equal(t, 1, better(cat, 0, 1))
	// equal score → fewer GB wins
	require.Equal(t, 2, better(cat, 1, 2))
	// cur == -1 → take the candidate
	require.Equal(t, 0, better(cat, -1, 0))
}

func TestReserveGB_FloorAndFraction(t *testing.T) {
	require.Equal(t, 2.0, reserveGB(4))   // 20% = 0.8, floored to 2
	require.Equal(t, 2.0, reserveGB(10))  // 20% = 2, exactly the floor
	require.Equal(t, 12.0, reserveGB(60)) // 20% of a big box dominates the floor
}

func TestAvailMemGB_RoutesBySource(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		switch name {
		case "nvidia-smi":
			return []byte("2048\n8192\n"), nil // two GPUs; take the largest free
		case "vm_stat":
			return []byte("Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
				"Pages free: 65536.\nPages inactive: 65536.\n"), nil
		}
		return nil, errors.New("unexpected")
	}
	proc := func() (float64, bool) { return 12.5, true }

	// NVIDIA: largest GPU's free VRAM, MiB→GB (8192 MiB = 8 GB).
	gb, ok := availMemGB(run, "NVIDIA VRAM", proc)
	require.True(t, ok)
	require.InDelta(t, 8.0, gb, 0.001)

	// unified: vm_stat reclaimable pages (free+inactive = 131072 × 16384 B = 2 GB).
	gb, ok = availMemGB(run, "unified memory", proc)
	require.True(t, ok)
	require.InDelta(t, 2.0, gb, 0.001)

	// default (system RAM): the injected MemAvailable proxy.
	gb, ok = availMemGB(run, "system RAM (no GPU detected)", proc)
	require.True(t, ok)
	require.Equal(t, 12.5, gb)
}

func TestVMStatAvailGB_SumsReclaimablePages(t *testing.T) {
	raw := "Mach Virtual Memory Statistics: (page size of 4096 bytes)\n" +
		"Pages free:                  262144.\n" +
		"Pages active:                999999.\n" +
		"Pages inactive:              262144.\n" +
		"Pages speculative:                0.\n" +
		"Pages purgeable:                  0.\n"
	gb, ok := vmStatAvailGB(raw)
	require.True(t, ok)
	require.InDelta(t, 2.0, gb, 0.001) // (262144+262144)*4096 = 2 GiB
}

func TestVMStatAvailGB_NoBucketsIsNotOK(t *testing.T) {
	_, ok := vmStatAvailGB("garbage with no page lines")
	require.False(t, ok)
}

func TestAvgAvail_AveragesAndSkipsFailures(t *testing.T) {
	vals := []struct {
		v  float64
		ok bool
	}{{10, true}, {0, false}, {20, true}} // the failed sample is skipped
	i := 0
	sample := func() (float64, bool) {
		r := vals[i]
		i++
		return r.v, r.ok
	}
	got, ok := avgAvail(sample, 3, func() {})
	require.True(t, ok)
	require.Equal(t, 15.0, got) // mean of 10 and 20
}

func TestAvgAvail_AllFailIsNotOK(t *testing.T) {
	_, ok := avgAvail(func() (float64, bool) { return 0, false }, 3, func() {})
	require.False(t, ok)
}

func TestCatalogIsAscendingByRAM(t *testing.T) {
	for i := 1; i < len(llmCatalog); i++ {
		require.LessOrEqual(t, llmCatalog[i-1].RAMGB, llmCatalog[i].RAMGB,
			"catalog must stay ascending by RAM for the fit logic")
	}
}

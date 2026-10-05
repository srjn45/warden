package fastbrain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRouteTier(t *testing.T) {
	ok := func(raw string, conf float64) Response {
		r := prResp(StatusOK, raw)
		r.Confidence = conf
		return r
	}
	cases := []struct {
		name string
		r    Response
		tier string
		conf float64
		ok   bool
	}{
		{"tier-1", ok(`{"tier":"tier-1","confidence":0.9}`, 0.9), "tier-1", 0.9, true},
		{"tier-3 case/space", ok(`{"tier":" Tier-3 "}`, 0.85), "tier-3", 0.85, true},
		{"unknown tier", ok(`{"tier":"heavy"}`, 0.9), "", 0, false},
		{"missing tier", ok(`{}`, 0.9), "", 0, false},
		{"confidence out of range", ok(`{"tier":"tier-2"}`, 1.5), "", 0, false},
		{"timeout", prResp(StatusTimeout, ""), "", 0, false},
		{"invalid json", prResp(StatusInvalidJSON, ""), "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tier, conf, got := ParseRouteTier(c.r)
			require.Equal(t, c.ok, got)
			require.Equal(t, c.tier, tier)
			require.InDelta(t, c.conf, conf, 1e-9)
		})
	}
}

func TestRouteTierPromptClipsInput(t *testing.T) {
	p := RouteTierPrompt(strings.Repeat("x", maxPromptInputBytes*2))
	require.Contains(t, p, "tier-1")
	require.Less(t, len(p), maxPromptInputBytes+len(routeTierSystem)+50)
}

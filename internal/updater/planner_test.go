package updater

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultSupportWindowMinUpgradeFrom(t *testing.T) {
	s1, err := ParseSemver("9.28.0")
	require.NoError(t, err)
	min1 := DefaultSupportWindowMinUpgradeFrom(s1)
	require.Equal(t, "9.25.0", min1.String())

	s2, err := ParseSemver("9.2.0")
	require.NoError(t, err)
	min2 := DefaultSupportWindowMinUpgradeFrom(s2)
	require.Equal(t, "9.0.0", min2.String())
}

func TestComputePath_DirectWithinWindow(t *testing.T) {
	targetM := Manifest{
		Version:        "9.28.0",
		SchemaVersion:  14,
		MinSchema:      11,
		MinUpgradeFrom: "9.25.0",
		Notes:          []string{"integrity strict"},
		APICompat:      "Hub API v1",
	}

	plan, err := ComputePath("9.26.0", 12, targetM, nil)
	require.NoError(t, err)
	require.True(t, plan.Direct)
	require.Len(t, plan.Hops, 1)
	require.Equal(t, "9.28.0", plan.Hops[0].Release.Version)
	require.True(t, plan.RequiresConfirm) // schema 12 -> 14
	require.Contains(t, plan.APICompat[0], "Hub API v1")
}

func TestComputePath_MultiHopCrossingMajor(t *testing.T) {
	available := []Manifest{
		{
			Version:        "9.28.0",
			SchemaVersion:  14,
			Waypoint:       true,
			MinUpgradeFrom: "9.20.0",
		},
		{
			Version:        "10.0.0",
			SchemaVersion:  15,
			Waypoint:       true,
			MinUpgradeFrom: "9.28.0",
		},
		{
			Version:        "10.2.0",
			SchemaVersion:  15,
			MinUpgradeFrom: "10.0.0",
		},
	}
	target := available[2] // 10.2.0

	plan, err := ComputePath("9.25.0", 12, target, available)
	require.NoError(t, err)
	require.False(t, plan.Direct)
	require.Len(t, plan.Hops, 3)
	require.Equal(t, "9.28.0", plan.Hops[0].Release.Version)
	require.Equal(t, "10.0.0", plan.Hops[1].Release.Version)
	require.Equal(t, "10.2.0", plan.Hops[2].Release.Version)

	formatted := FormatPlanText(plan)
	require.Contains(t, formatted, "Multi-hop waypoint path (v9.28.0 → v10.0.0 → v10.2.0)")
}

func TestComputePath_RejectsDowngrade(t *testing.T) {
	targetM := Manifest{
		Version:       "9.20.0",
		SchemaVersion: 10,
	}
	_, err := ComputePath("9.28.0", 14, targetM, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "downgrade not supported; use wd rollback")
}

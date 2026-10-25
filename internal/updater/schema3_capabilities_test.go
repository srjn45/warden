package updater

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSchema3CapabilityEncodingAndVersionGate(t *testing.T) {
	// Synthetic versions are test inputs, never a claim about a shipping waypoint.
	valid := Manifest{Version: "99.1.0", SchemaVersion: 3, MinSchema: 2, MinUpgradeFrom: "99.0.0", Breaking: true, Notes: []string{"Ownership normalization; paired-data rollback"}, Requires: ManifestRequirements{Capabilities: Schema3Requirements()}}
	require.NoError(t, ValidateSchema3Release(valid))
	for _, name := range []string{"missing minimum", "schema1 shortcut", "nonbreaking", "missing capability", "future capability", "unknown capability"} {
		t.Run(name, func(t *testing.T) {
			m := valid
			m.Requires.Capabilities = Schema3Requirements()
			switch name {
			case "missing minimum":
				m.MinUpgradeFrom = ""
			case "schema1 shortcut":
				m.MinSchema = 1
			case "nonbreaking":
				m.Breaking = false
			case "missing capability":
				delete(m.Requires.Capabilities, CapabilityPairedBackup)
			case "future capability":
				m.Requires.Capabilities[CapabilitySchema3Handoff] = 2
			case "unknown capability":
				m.Requires.Capabilities["unimplemented"] = 1
			}
			require.Error(t, ValidateSchema3Release(m))
		})
	}
	_, err := ComputePath("99.0.0", 2, valid, nil)
	require.ErrorContains(t, err, "cutover")
	_, err = ComputePath("dev", 1, valid, nil)
	require.Error(t, err, "dev cannot bypass capabilities")
}

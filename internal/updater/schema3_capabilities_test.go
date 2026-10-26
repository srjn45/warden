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
	// The capability-gated lift: a valid format-3 envelope plans; the per-update
	// gate (txn.schema3TargetGate) binds the binary, version and data format.
	_, err := ComputePath("99.0.0", 2, valid, nil)
	require.NoError(t, err)
	invalid := valid
	invalid.Requires.Capabilities = nil
	_, err = ComputePath("99.0.0", 2, invalid, nil)
	require.Error(t, err, "a format-3 envelope without capabilities never plans")
}

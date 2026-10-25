package updater

import (
	"fmt"
	"sort"
)

// Capability names are versioned protocols, separate from release semver. A
// future version is incompatible unless implemented explicitly, never >=1.
const (
	CapabilityStoppedDaemon     = "stopped-daemon"
	CapabilitySchema3Handoff    = "schema3-transaction-handoff"
	CapabilityPairedBackup      = "verified-paired-backup"
	CapabilityGenerationRestore = "generation-restore"
)

// Schema3Requirements returns the exact machine requirements for format 3.
func Schema3Requirements() map[string]int {
	return map[string]int{CapabilityStoppedDaemon: 1, CapabilitySchema3Handoff: 1, CapabilityPairedBackup: 1, CapabilityGenerationRestore: 1}
}

// ValidateSchema3Release checks a proposed release envelope without inventing
// the first compatible updater version. Compatibility tests must supply that
// input. Runtime/CLI integration is intentionally still gated until cutover.
func ValidateSchema3Release(m Manifest) error {
	if m.SchemaVersion != 3 {
		return nil
	}
	if m.MinSchema != 2 || !m.Breaking {
		return fmt.Errorf("schema3: release requires min_schema=2 and breaking=true")
	}
	if m.MinUpgradeFrom == "" {
		return fmt.Errorf("schema3: min_upgrade_from must name the compatibility-tested schema-2 updater waypoint")
	}
	min, err := ParseSemver(m.MinUpgradeFrom)
	if err != nil {
		return err
	}
	target, err := ParseSemver(m.Version)
	if err != nil {
		return err
	}
	if min.Compare(target) >= 0 {
		return fmt.Errorf("schema3: updater waypoint must precede target release")
	}
	want := Schema3Requirements()
	names := make([]string, 0, len(m.Requires.Capabilities))
	for name := range m.Requires.Capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if want[name] != m.Requires.Capabilities[name] || want[name] == 0 {
			return fmt.Errorf("schema3: unsupported capability %s version %d", name, m.Requires.Capabilities[name])
		}
	}
	for name, version := range want {
		if m.Requires.Capabilities[name] != version {
			return fmt.Errorf("schema3: required capability %s version %d absent", name, version)
		}
	}
	if len(m.Notes) == 0 {
		return fmt.Errorf("schema3: ownership normalization and paired-data rollback release notes required")
	}
	return nil
}

// validateCapabilities protects current release planning from claiming that the
// intermediate migrate branch is a deployable schema-3 updater. Cutover must
// replace this refusal only after real old/new binary compatibility tests pass.
func validateCapabilities(m Manifest) error {
	if m.SchemaVersion == 3 {
		if err := ValidateSchema3Release(m); err != nil {
			return err
		}
		return fmt.Errorf("schema3: runtime cutover/readiness integration is not enabled in this build")
	}
	if len(m.Requires.Capabilities) > 0 {
		return fmt.Errorf("update: unsupported required capabilities")
	}
	return nil
}

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/updater"
)

func main() {
	var (
		versionFlag   string
		schemaFlag    int
		minSchemaFlag int
		minUpgrade    string
		waypointFlag  bool
		breakingFlag  bool
		notesFlag     string
		apiCompatFlag string
		manualFlag    string
		capabilities  string
		outFlag       string
	)

	flag.StringVar(&versionFlag, "version", "", "Release semver (e.g. 9.28.0 or v9.28.0)")
	flag.IntVar(&schemaFlag, "schema-version", schema.SchemaVersion, "Data format schema version")
	flag.IntVar(&minSchemaFlag, "min-schema", schema.MinSchema, "Minimum schema format supported")
	flag.StringVar(&minUpgrade, "min-upgrade-from", "", "Oldest version that can upgrade directly (defaults to support-window rule)")
	flag.BoolVar(&waypointFlag, "waypoint", false, "Whether this release is a waypoint release")
	flag.BoolVar(&breakingFlag, "breaking", false, "Whether this release introduces breaking changes")
	flag.StringVar(&notesFlag, "notes", "", "Comma-separated release notes")
	flag.StringVar(&apiCompatFlag, "api-compat", "Compatible with Hub API v1; Android app >= 1.4", "Connected client compatibility note")
	flag.StringVar(&manualFlag, "requires-manual", "", "Comma-separated required manual steps")
	flag.StringVar(&capabilities, "requires-capabilities", "", "Comma-separated capability=version requirements")
	flag.StringVar(&outFlag, "out", "dist/manifest.json", "Output file path")
	flag.Parse()

	ver := strings.TrimPrefix(strings.TrimSpace(versionFlag), "v")
	if ver == "" {
		ver = "0.0.0-dev"
	}

	sem, err := updater.ParseSemver(ver)
	if err == nil && minUpgrade == "" {
		minUpgrade = updater.DefaultSupportWindowMinUpgradeFrom(sem).String()
	}

	// A new major (X.0.0) is breaking by definition: it is where data-format
	// changes such as the schema-2 warden-db cutover land, and the planner must
	// ask for confirmation and route through the previous major's last release.
	if err == nil && isMajorRelease(sem) {
		breakingFlag = true
	}

	var notes []string
	if notesFlag != "" {
		for _, n := range strings.Split(notesFlag, ",") {
			if trimmed := strings.TrimSpace(n); trimmed != "" {
				notes = append(notes, trimmed)
			}
		}
	}

	var manual []string
	if manualFlag != "" {
		for _, m := range strings.Split(manualFlag, ",") {
			if trimmed := strings.TrimSpace(m); trimmed != "" {
				manual = append(manual, trimmed)
			}
		}
	}

	requires, err := parseCapabilities(capabilities)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genmanifest: parse required capabilities: %v\n", err)
		os.Exit(1)
	}

	m := updater.Manifest{
		Version:        ver,
		SchemaVersion:  schemaFlag,
		MinSchema:      minSchemaFlag,
		MinUpgradeFrom: strings.TrimPrefix(strings.TrimSpace(minUpgrade), "v"),
		Waypoint:       waypointFlag,
		Breaking:       breakingFlag,
		Notes:          notes,
		APICompat:      apiCompatFlag,
		Requires:       updater.ManifestRequirements{Manual: manual, Capabilities: requires},
	}

	if err := updater.ValidateSchema3Release(m); err != nil {
		fmt.Fprintf(os.Stderr, "genmanifest: validate release: %v\n", err)
		os.Exit(1)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "genmanifest: marshal JSON: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(outFlag, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "genmanifest: write %s: %v\n", outFlag, err)
		os.Exit(1)
	}
	fmt.Printf("genmanifest: wrote release manifest to %s\n", outFlag)
}

// isMajorRelease reports a release that opens a new major (X.0.0, X >= 1).
func isMajorRelease(s updater.Semver) bool { return s.Major >= 1 && s.Minor == 0 && s.Patch == 0 }

func parseCapabilities(raw string) (map[string]int, error) {
	if raw == "" {
		return nil, nil
	}
	requires := make(map[string]int)
	for _, item := range strings.Split(raw, ",") {
		name, version, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%q must be name=version", item)
		}
		value, err := strconv.Atoi(strings.TrimSpace(version))
		if err != nil || value < 1 {
			return nil, fmt.Errorf("%q has invalid version", item)
		}
		if _, duplicate := requires[name]; duplicate {
			return nil, fmt.Errorf("duplicate capability %q", name)
		}
		requires[name] = value
	}
	return requires, nil
}

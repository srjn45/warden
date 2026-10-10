package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
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

	m := updater.Manifest{
		Version:        ver,
		SchemaVersion:  schemaFlag,
		MinSchema:      minSchemaFlag,
		MinUpgradeFrom: strings.TrimPrefix(strings.TrimSpace(minUpgrade), "v"),
		Waypoint:       waypointFlag,
		Breaking:       breakingFlag,
		Notes:          notes,
		APICompat:      apiCompatFlag,
		Requires:       updater.ManifestRequirements{Manual: manual},
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

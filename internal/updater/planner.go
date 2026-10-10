package updater

import (
	"fmt"
	"sort"
	"strings"
)

// Hop represents one step on an upgrade path.
type Hop struct {
	Release  Release
	Manifest Manifest
}

// Plan represents an evaluated update trajectory from installed to target version.
type Plan struct {
	CurrentVersion  string   `json:"current_version"`
	CurrentSchema   int      `json:"current_schema"`
	TargetVersion   string   `json:"target_version"`
	TargetSchema    int      `json:"target_schema"`
	Direct          bool     `json:"direct"`
	Hops            []Hop    `json:"hops"`
	BreakingChanges []string `json:"breaking_changes,omitempty"`
	Notes           []string `json:"notes,omitempty"`
	APICompat       []string `json:"api_compat,omitempty"`
	ManualSteps     []string `json:"manual_steps,omitempty"`
	DowntimeDesc    string   `json:"downtime_desc"`
	DiskNeededBytes int64    `json:"disk_needed_bytes"`
	RequiresConfirm bool     `json:"requires_confirm"`
}

// DefaultSupportWindowMinUpgradeFrom calculates the oldest version that may directly
// upgrade to target (X.Y.Z) under the support-window rule:
// higher of 3 minors back or start of current major (X.max(Y-3, 0).0).
// Major crossings must route through the last release of the previous major.
func DefaultSupportWindowMinUpgradeFrom(target Semver) Semver {
	minMinor := target.Minor - 3
	if minMinor < 0 {
		minMinor = 0
	}
	return Semver{Major: target.Major, Minor: minMinor, Patch: 0}
}

// ComputePath computes the sequence of waypoint hops needed to transition from current to target
// given available release manifests.
//
// Rules:
// 1. Direct upgrade is allowed if current satisfies target.Manifest.MinUpgradeFrom (or DefaultSupportWindowMinUpgradeFrom).
// 2. Crossing a major boundary (current.Major < target.Major) MUST route via the last release of previous major(s).
// 3. Otherwise walk backwards from target, picking waypoints or intermediate releases.
func ComputePath(currentVer string, currentSchema int, targetManifest Manifest, availableManifests []Manifest) (Plan, error) {
	currClean := stripV(currentVer)
	targetClean := stripV(targetManifest.Version)

	plan := Plan{
		CurrentVersion: currClean,
		CurrentSchema:  currentSchema,
		TargetVersion:  targetClean,
		TargetSchema:   targetManifest.SchemaVersion,
		DowntimeDesc:   "Brief daemon restart during binary swap and schema verification (~2-5s)",
	}

	if currClean == "dev" {
		// Development binary upgrading to target: direct hop
		plan.Direct = true
		plan.Hops = []Hop{{
			Release:  Release{Tag: "v" + targetClean, Version: targetClean},
			Manifest: targetManifest,
		}}
		summarizePlan(&plan)
		return plan, nil
	}

	currSem, err := ParseSemver(currClean)
	if err != nil {
		return Plan{}, fmt.Errorf("parse current version %q: %w", currentVer, err)
	}
	targetSem, err := ParseSemver(targetClean)
	if err != nil {
		return Plan{}, fmt.Errorf("parse target version %q: %w", targetClean, err)
	}

	cmp := currSem.Compare(targetSem)
	if cmp > 0 {
		return Plan{}, fmt.Errorf("target version v%s is older than installed version v%s (downgrade not supported; use wd rollback)", targetClean, currClean)
	}
	if cmp == 0 {
		plan.Direct = true
		plan.Hops = []Hop{{
			Release:  Release{Tag: "v" + targetClean, Version: targetClean},
			Manifest: targetManifest,
		}}
		summarizePlan(&plan)
		return plan, nil
	}

	// Index available manifests by version
	manifestMap := make(map[string]Manifest)
	for _, m := range availableManifests {
		manifestMap[stripV(m.Version)] = m
	}
	manifestMap[targetClean] = targetManifest

	// Check if direct upgrade is permitted
	minDirect := DefaultSupportWindowMinUpgradeFrom(targetSem)
	if targetManifest.MinUpgradeFrom != "" {
		if parsedMin, err := ParseSemver(targetManifest.MinUpgradeFrom); err == nil {
			minDirect = parsedMin
		}
	}

	canDirect := false
	if currSem.Major == targetSem.Major && currSem.Compare(minDirect) >= 0 {
		canDirect = true
	}

	if canDirect {
		plan.Direct = true
		plan.Hops = []Hop{{
			Release:  Release{Tag: "v" + targetClean, Version: targetClean},
			Manifest: targetManifest,
		}}
		summarizePlan(&plan)
		return plan, nil
	}

	// Multi-hop path planning needed
	// Sort all available manifests in ascending order
	var all []Manifest
	for _, m := range manifestMap {
		all = append(all, m)
	}
	sort.Slice(all, func(i, j int) bool {
		vi, erri := ParseSemver(all[i].Version)
		vj, errj := ParseSemver(all[j].Version)
		if erri != nil || errj != nil {
			return all[i].Version < all[j].Version
		}
		return vi.Compare(vj) < 0
	})

	// Walk backwards from target down to current to find hops
	var path []Manifest
	currTarget := targetManifest

	for {
		path = append([]Manifest{currTarget}, path...)
		targetS, _ := ParseSemver(currTarget.Version)

		minDirect := DefaultSupportWindowMinUpgradeFrom(targetS)
		if currTarget.MinUpgradeFrom != "" {
			if pm, err := ParseSemver(currTarget.MinUpgradeFrom); err == nil {
				minDirect = pm
			}
		}

		if currSem.Major == targetS.Major && currSem.Compare(minDirect) >= 0 {
			// Current version can reach currTarget directly!
			break
		}

		// Find the best waypoint to reach currTarget
		// Candidate must be < currTarget and reachable from below, preferably a waypoint or last of previous major
		var best *Manifest
		if currSem.Major < targetS.Major {
			// If targetS is not the start of its major, route to the start of targetS.Major (or nearest waypoint in this major)
			startOfMajor := Semver{Major: targetS.Major, Minor: 0, Patch: 0}
			if targetS.Compare(startOfMajor) > 0 {
				for i := len(all) - 1; i >= 0; i-- {
					candSem, cerr := ParseSemver(all[i].Version)
					if cerr != nil {
						continue
					}
					if candSem.Major == targetS.Major && candSem.Compare(targetS) < 0 {
						if all[i].Waypoint || candSem.Compare(minDirect) >= 0 {
							cand := all[i]
							best = &cand
							break
						}
					}
				}
			}

			if best == nil {
				// We are at the start of major targetS.Major; entry point is (targetS.Major-1).last
				for i := len(all) - 1; i >= 0; i-- {
					candSem, cerr := ParseSemver(all[i].Version)
					if cerr != nil {
						continue
					}
					if candSem.Major < targetS.Major && candSem.Compare(currSem) >= 0 {
						// Found latest release in lower major
						cand := all[i]
						best = &cand
						break
					}
				}
			}
		} else {
			// Same major: find the earliest waypoint reachable that can reach targetS directly (>= minDirect)
			for i := 0; i < len(all); i++ {
				candSem, cerr := ParseSemver(all[i].Version)
				if cerr != nil {
					continue
				}
				if candSem.Compare(targetS) < 0 && candSem.Compare(currSem) >= 0 && candSem.Compare(minDirect) >= 0 {
					if all[i].Waypoint {
						cand := all[i]
						best = &cand
						break
					}
				}
			}
			if best == nil {
				// Fallback: earliest release in window that can reach targetS directly
				for i := 0; i < len(all); i++ {
					candSem, cerr := ParseSemver(all[i].Version)
					if cerr != nil {
						continue
					}
					if candSem.Compare(targetS) < 0 && candSem.Compare(currSem) >= 0 && candSem.Compare(minDirect) >= 0 {
						cand := all[i]
						best = &cand
						break
					}
				}
			}
			if best == nil {
				// Otherwise walk backwards from targetS looking for waypoint
				for i := len(all) - 1; i >= 0; i-- {
					candSem, cerr := ParseSemver(all[i].Version)
					if cerr != nil {
						continue
					}
					if candSem.Compare(targetS) < 0 && candSem.Compare(currSem) >= 0 {
						if all[i].Waypoint {
							cand := all[i]
							best = &cand
							break
						}
					}
				}
			}
		}

		if best == nil {
			return Plan{}, fmt.Errorf("no upgrade path found from v%s to v%s within support window", currClean, targetClean)
		}

		if best.Version == currTarget.Version {
			return Plan{}, fmt.Errorf("cycle detected in path planning at v%s", best.Version)
		}
		currTarget = *best
	}

	plan.Direct = (len(path) == 1)
	for _, m := range path {
		plan.Hops = append(plan.Hops, Hop{
			Release:  Release{Tag: "v" + stripV(m.Version), Version: stripV(m.Version)},
			Manifest: m,
		})
	}
	summarizePlan(&plan)
	return plan, nil
}

func summarizePlan(plan *Plan) {
	// Baseline disk needed per hop: binary (~50MB) + archive (~25MB) + store backup (~50MB)
	plan.DiskNeededBytes = int64(len(plan.Hops)) * 120 * 1024 * 1024

	requiresConfirm := false
	for _, h := range plan.Hops {
		m := h.Manifest
		if m.Breaking {
			requiresConfirm = true
			plan.BreakingChanges = append(plan.BreakingChanges, fmt.Sprintf("v%s: breaking changes", m.Version))
		}
		if len(m.Notes) > 0 {
			for _, n := range m.Notes {
				plan.Notes = append(plan.Notes, fmt.Sprintf("v%s: %s", m.Version, n))
			}
		}
		if m.APICompat != "" {
			plan.APICompat = append(plan.APICompat, fmt.Sprintf("v%s: %s", m.Version, m.APICompat))
		}
		if len(m.Requires.Manual) > 0 {
			requiresConfirm = true
			for _, step := range h.Manifest.Requires.Manual {
				plan.ManualSteps = append(plan.ManualSteps, fmt.Sprintf("v%s: %s", m.Version, step))
			}
		}
	}

	if plan.TargetSchema > plan.CurrentSchema {
		requiresConfirm = true
	}
	plan.RequiresConfirm = requiresConfirm
}

// FormatPlanText formats the update plan for human inspection in `wd update --plan`.
func FormatPlanText(p Plan) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Update Plan: v%s → v%s\n", p.CurrentVersion, p.TargetVersion))
	sb.WriteString(fmt.Sprintf("Data Schema: %d → %d\n", p.CurrentSchema, p.TargetSchema))
	if p.Direct {
		sb.WriteString("Trajectory: Direct upgrade\n")
	} else {
		var hopVers []string
		for _, h := range p.Hops {
			hopVers = append(hopVers, "v"+h.Release.Version)
		}
		sb.WriteString(fmt.Sprintf("Trajectory: Multi-hop waypoint path (%s)\n", strings.Join(hopVers, " → ")))
	}

	sb.WriteString(fmt.Sprintf("Expected Downtime: %s\n", p.DowntimeDesc))
	sb.WriteString(fmt.Sprintf("Estimated Free Disk Needed: ~%d MB\n", p.DiskNeededBytes/(1024*1024)))

	if len(p.BreakingChanges) > 0 {
		sb.WriteString("\nBreaking Changes:\n")
		for _, b := range p.BreakingChanges {
			sb.WriteString(fmt.Sprintf("  • %s\n", b))
		}
	}

	if len(p.APICompat) > 0 {
		sb.WriteString("\nConnected Client Compatibility Notes:\n")
		for _, a := range p.APICompat {
			sb.WriteString(fmt.Sprintf("  • %s\n", a))
		}
	}

	if len(p.Notes) > 0 {
		sb.WriteString("\nRelease Notes & Migrations:\n")
		for _, n := range p.Notes {
			sb.WriteString(fmt.Sprintf("  • %s\n", n))
		}
	}

	if len(p.ManualSteps) > 0 {
		sb.WriteString("\nRequired Manual Steps:\n")
		for _, m := range p.ManualSteps {
			sb.WriteString(fmt.Sprintf("  • %s\n", m))
		}
	}

	if p.RequiresConfirm {
		sb.WriteString("\nNote: This update involves data schema changes or breaking changes and requires confirmation.\n")
	}

	return sb.String()
}

// BuildPlan resolves the target release, retrieves manifests, and computes the upgrade plan.
func BuildPlan(opts Options) (Plan, error) {
	if err := normalizeOptions(&opts); err != nil {
		return Plan{}, err
	}
	targetRel, err := resolveRelease(opts)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve target release: %w", err)
	}

	targetManifest, err := FetchManifest(opts, targetRel)
	if err != nil {
		// If release has no manifest (e.g. pre-manifest legacy release), synthesize baseline manifest
		targetManifest = Manifest{
			Version:        targetRel.Version,
			SchemaVersion:  opts.CurrentSchema,
			MinSchema:      opts.CurrentSchema,
			MinUpgradeFrom: targetRel.Version,
		}
	}

	// Fetch all available releases to find waypoint hops if needed
	var availableManifests []Manifest
	releases, relErr := fetchReleases(opts)
	if relErr == nil && len(releases) > 0 {
		for _, r := range releases {
			if m, err := FetchManifest(opts, r); err == nil {
				availableManifests = append(availableManifests, m)
			}
		}
	}

	return ComputePath(opts.CurrentVersion, opts.CurrentSchema, targetManifest, availableManifests)
}

package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ManifestFileName is the standard release manifest asset name published per release.
const ManifestFileName = "manifest.json"

// ManifestRequirements specifies preconditions or manual actions needed for an update.
type ManifestRequirements struct {
	Manual []string `json:"manual,omitempty"`
}

// Manifest represents the metadata published with every release archive.
type Manifest struct {
	Version        string               `json:"version"`
	SchemaVersion  int                  `json:"schema_version"`
	MinSchema      int                  `json:"min_schema"`
	MinUpgradeFrom string               `json:"min_upgrade_from"`
	Waypoint       bool                 `json:"waypoint"`
	Breaking       bool                 `json:"breaking"`
	Notes          []string             `json:"notes,omitempty"`
	APICompat      string               `json:"api_compat,omitempty"`
	Requires       ManifestRequirements `json:"requires,omitempty"`
}

// Semver represents parsed major.minor.patch version components.
type Semver struct {
	Major int
	Minor int
	Patch int
}

// ParseSemver parses a semver string like "9.28.0" or "v9.28.0".
func ParseSemver(v string) (Semver, error) {
	clean := stripV(v)
	// Remove any pre-release or build metadata for comparison
	if idx := strings.IndexAny(clean, "-+"); idx != -1 {
		clean = clean[:idx]
	}
	parts := strings.Split(clean, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("invalid semver %q: must be X.Y.Z", v)
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return Semver{}, fmt.Errorf("invalid major in %q: %w", v, err)
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return Semver{}, fmt.Errorf("invalid minor in %q: %w", v, err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return Semver{}, fmt.Errorf("invalid patch in %q: %w", v, err)
	}
	return Semver{Major: maj, Minor: min, Patch: patch}, nil
}

// Compare compares two semvers. Returns -1 if a < b, 0 if a == b, 1 if a > b.
func (s Semver) Compare(o Semver) int {
	if s.Major != o.Major {
		if s.Major < o.Major {
			return -1
		}
		return 1
	}
	if s.Minor != o.Minor {
		if s.Minor < o.Minor {
			return -1
		}
		return 1
	}
	if s.Patch != o.Patch {
		if s.Patch < o.Patch {
			return -1
		}
		return 1
	}
	return 0
}

// String formats semver back to "X.Y.Z".
func (s Semver) String() string {
	return fmt.Sprintf("%d.%d.%d", s.Major, s.Minor, s.Patch)
}

// FetchManifest retrieves and decodes a Manifest asset from GitHub release assets or AssetBase.
func FetchManifest(opts Options, rel Release) (Manifest, error) {
	url := assetURL(opts, rel, ManifestFileName)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return Manifest{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "warden-updater")

	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch %s for %s: %w", ManifestFileName, rel.Tag, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return Manifest{}, fmt.Errorf("%s not found for release %s (404)", ManifestFileName, rel.Tag)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Manifest{}, fmt.Errorf("fetch %s for %s returned HTTP %d: %s", ManifestFileName, rel.Tag, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s for %s: %w", ManifestFileName, rel.Tag, err)
	}

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse %s for %s: %w", ManifestFileName, rel.Tag, err)
	}
	m.Version = stripV(m.Version)
	m.MinUpgradeFrom = stripV(m.MinUpgradeFrom)
	return m, nil
}

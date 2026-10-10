package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Release is the subset of GitHub release metadata the updater needs.
type Release struct {
	Tag     string // e.g. v1.2.3
	Version string // e.g. 1.2.3
}

type githubRelease struct {
	TagName string `json:"tag_name"`
}

func resolveRelease(opts Options) (Release, error) {
	if pin := strings.TrimSpace(opts.TargetVersion); pin != "" {
		ver := stripV(pin)
		if ver == "" {
			return Release{}, fmt.Errorf("empty --version")
		}
		return Release{Tag: "v" + ver, Version: ver}, nil
	}
	return fetchLatest(opts)
}

func fetchLatest(opts Options) (Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", opts.Repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "warden-updater")

	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("query GitHub releases: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Release{}, fmt.Errorf("read GitHub releases response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub releases API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var gr githubRelease
	if err := json.Unmarshal(body, &gr); err != nil {
		return Release{}, fmt.Errorf("parse GitHub releases response: %w", err)
	}
	tag := strings.TrimSpace(gr.TagName)
	if tag == "" {
		return Release{}, fmt.Errorf("GitHub releases API returned empty tag_name")
	}
	ver := stripV(tag)
	return Release{Tag: "v" + ver, Version: ver}, nil
}

func assetURL(opts Options, rel Release, name string) string {
	if opts.AssetBase != "" {
		return strings.TrimRight(opts.AssetBase, "/") + "/" + name
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", opts.Repo, rel.Tag, name)
}

func archiveName(rel Release, goos, goarch string) string {
	return fmt.Sprintf("warden_%s_%s_%s.tar.gz", rel.Version, goos, goarch)
}

func fetchReleases(opts Options) ([]Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100", opts.Repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "warden-updater")

	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query GitHub releases: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, fmt.Errorf("read GitHub releases response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub releases API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var grs []githubRelease
	if err := json.Unmarshal(body, &grs); err != nil {
		return nil, fmt.Errorf("parse GitHub releases response: %w", err)
	}
	var res []Release
	for _, gr := range grs {
		tag := strings.TrimSpace(gr.TagName)
		if tag != "" {
			ver := stripV(tag)
			res = append(res, Release{Tag: "v" + ver, Version: ver})
		}
	}
	return res, nil
}

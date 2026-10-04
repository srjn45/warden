package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// DiscoveredModel is one model id surfaced by an installed AI CLI's native
// model-menu command (or Codex's config.toml default), before it is registered
// in warden's catalog.
type DiscoveredModel struct {
	BackendID   string `json:"backend_id"`
	ModelID     string `json:"model_id"`
	DisplayName string `json:"display_name,omitempty"`
}

// discoverCmd runners are package vars so unit tests can stub tool output
// without requiring the real binaries.
var (
	discoverCursorCmd = func() ([]byte, error) {
		return exec.Command("cursor-agent", "--list-models").Output()
	}
	discoverAgyCmd = func() ([]byte, error) {
		return exec.Command("agy", "models").Output()
	}
	discoverOpencodeCmd = func() ([]byte, error) {
		return exec.Command("opencode", "models").Output()
	}
	discoverCrushCmd = func() ([]byte, error) {
		return exec.Command("crush", "models").Output()
	}
	discoverCodexConfigPath = func() string {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, ".codex", "config.toml")
		}
		return ""
	}
	discoverReadFile = os.ReadFile
)

// discoverInstalledModels probes each known AI CLI for its live model menu and
// returns the union, sorted by (backend, model). backendFilter, when non-empty,
// restricts the probe to that single backend id. Missing binaries / unreadable
// config degrade silently (that backend contributes nothing).
func discoverInstalledModels(backendFilter string) ([]DiscoveredModel, error) {
	filter := strings.TrimSpace(backendFilter)
	var out []DiscoveredModel

	type probe struct {
		backend string
		run     func() ([]DiscoveredModel, error)
	}
	probes := []probe{
		{"cursor", discoverCursorModels},
		{"antigravity", discoverAgyModels},
		{"opencode", discoverOpencodeModels},
		{"crush", discoverCrushModels},
		{"codex", discoverCodexModels},
	}
	for _, p := range probes {
		if filter != "" && p.backend != filter {
			continue
		}
		models, err := p.run()
		if err != nil {
			// Soft-fail: a missing/broken tool should not abort the whole discover.
			continue
		}
		out = append(out, models...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BackendID == out[j].BackendID {
			return out[i].ModelID < out[j].ModelID
		}
		return out[i].BackendID < out[j].BackendID
	})
	return out, nil
}

func discoverCursorModels() ([]DiscoveredModel, error) {
	raw, err := discoverCursorCmd()
	if err != nil {
		return nil, err
	}
	return parseCursorDiscover(raw, "cursor"), nil
}

func discoverAgyModels() ([]DiscoveredModel, error) {
	raw, err := discoverAgyCmd()
	if err != nil {
		return nil, err
	}
	return parseAgyDiscover(raw, "antigravity"), nil
}

func discoverOpencodeModels() ([]DiscoveredModel, error) {
	raw, err := discoverOpencodeCmd()
	if err != nil {
		return nil, err
	}
	return parseOneIDPerLine(raw, "opencode"), nil
}

func discoverCrushModels() ([]DiscoveredModel, error) {
	raw, err := discoverCrushCmd()
	if err != nil {
		return nil, err
	}
	return parseOneIDPerLine(raw, "crush"), nil
}

func discoverCodexModels() ([]DiscoveredModel, error) {
	path := discoverCodexConfigPath()
	if path == "" {
		return nil, fmt.Errorf("no home directory")
	}
	raw, err := discoverReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCodexConfigModels(raw, "codex"), nil
}

// parseCursorDiscover normalizes `cursor-agent --list-models` stdout into
// DiscoveredModel rows. Lines look like "<id> - <Display Name>" (optional
// "(current, default)" suffix on the display side).
func parseCursorDiscover(out []byte, backend string) []DiscoveredModel {
	models := []DiscoveredModel{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		idx := strings.Index(line, " - ")
		if idx < 0 {
			continue
		}
		id := strings.TrimSpace(line[:idx])
		if id == "" {
			continue
		}
		display := strings.TrimSpace(line[idx+3:])
		// Strip trailing status markers like "(current, default)".
		if paren := strings.Index(display, " ("); paren >= 0 {
			display = strings.TrimSpace(display[:paren])
		}
		if display == "" {
			display = id
		}
		models = append(models, DiscoveredModel{BackendID: backend, ModelID: id, DisplayName: display})
	}
	return models
}

// parseAgyDiscover normalizes `agy models` stdout. Prefer tab-separated
// "<id>\t<Display Name>"; fall back to a bare id (or a legacy display-only
// label, which is then used as both id and display).
func parseAgyDiscover(out []byte, backend string) []DiscoveredModel {
	models := []DiscoveredModel{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(strings.ToLower(line), "fetching ") {
			continue
		}
		id, display := line, line
		if tab := strings.IndexByte(line, '\t'); tab >= 0 {
			id = strings.TrimSpace(line[:tab])
			display = strings.TrimSpace(line[tab+1:])
		}
		if id == "" {
			continue
		}
		if display == "" {
			display = id
		}
		models = append(models, DiscoveredModel{BackendID: backend, ModelID: id, DisplayName: display})
	}
	return models
}

// parseOneIDPerLine handles `opencode models` / `crush models` (one model id
// per line, no decoration).
func parseOneIDPerLine(out []byte, backend string) []DiscoveredModel {
	models := []DiscoveredModel{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		id := strings.TrimSpace(sc.Text())
		if id == "" || strings.HasPrefix(id, "#") {
			continue
		}
		// Crush may annotate unconfigured providers; drop those markers.
		if strings.Contains(id, "(not configured)") {
			continue
		}
		models = append(models, DiscoveredModel{BackendID: backend, ModelID: id, DisplayName: id})
	}
	return models
}

// parseCodexConfigModels extracts the top-level `model = "..."` (or `'...'`)
// from ~/.codex/config.toml. Nested tables are ignored — only the bare model
// key before the first `[section]` is considered the active default.
func parseCodexConfigModels(out []byte, backend string) []DiscoveredModel {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			// Entered a table — stop looking for the top-level model key.
			break
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) != "model" {
			continue
		}
		id := unquoteTOMLString(strings.TrimSpace(val))
		if id == "" {
			continue
		}
		return []DiscoveredModel{{BackendID: backend, ModelID: id, DisplayName: id}}
	}
	return nil
}

func unquoteTOMLString(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

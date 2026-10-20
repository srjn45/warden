package backends

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
)

var modelCatalogReadFile = os.ReadFile
var modelCatalogUserConfigDir = os.UserConfigDir

func catalogFailed(source agentbackend.SourceKind, detail string, err error) agentbackend.CatalogObservation {
	class := "command_failed"
	if ce := catalogCommandError(err); ce != nil {
		class = ce.class
	}
	return agentbackend.CatalogObservation{Source: source, SourceDetail: detail, Completeness: agentbackend.CatalogFailed, Err: &agentbackend.CatalogError{Class: class}, Models: []agentbackend.ObservedModel{}}
}

func catalogVersion(ctx context.Context, binary string) string {
	b, err := modelCatalogRun(ctx, binary, "--version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

// Cursor --------------------------------------------------------------------

func (Cursor) ResolveScope(ctx context.Context) (agentbackend.CatalogScope, error) {
	return agentbackend.CatalogScope{AiCli: "cursor", CLIVersion: catalogVersion(ctx, "cursor-agent")}, nil
}

func (Cursor) DiscoverModels(ctx context.Context, scope agentbackend.CatalogScope) agentbackend.CatalogObservation {
	b, err := modelCatalogRun(ctx, "cursor-agent", "--list-models")
	if errors.Is(err, errCatalogOutputLimit) {
		models, _ := parseCursorCatalog(b)
		return agentbackend.CatalogObservation{Source: agentbackend.SourceLiveCommand, SourceDetail: "cursor-agent --list-models", Completeness: agentbackend.CatalogPartial, Err: &agentbackend.CatalogError{Class: "partial_result"}, Models: models}
	}
	if err != nil {
		return catalogFailed(agentbackend.SourceLiveCommand, "cursor-agent --list-models", err)
	}
	models, header := parseCursorCatalog(b)
	if !header || len(models) == 0 {
		return agentbackend.CatalogObservation{Source: agentbackend.SourceLiveCommand, SourceDetail: "cursor-agent --list-models", Completeness: agentbackend.CatalogEmpty, Err: &agentbackend.CatalogError{Class: "malformed_or_empty"}, Models: models}
	}
	return agentbackend.CatalogObservation{Source: agentbackend.SourceLiveCommand, SourceDetail: "cursor-agent --list-models", Completeness: agentbackend.CatalogComplete, Models: models}
}

func parseCursorCatalog(b []byte) ([]agentbackend.ObservedModel, bool) {
	var out []agentbackend.ObservedModel
	header := false
	s := bufio.NewScanner(bytes.NewReader(b))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.EqualFold(line, "available models") {
			header = true
			continue
		}
		id, display, ok := strings.Cut(line, " - ")
		if !ok || strings.TrimSpace(id) == "" {
			continue
		}
		display = strings.TrimSpace(display)
		if i := strings.Index(display, " ("); i >= 0 {
			display = strings.TrimSpace(display[:i])
		}
		out = append(out, agentbackend.ObservedModel{ModelID: strings.TrimSpace(id), DisplayName: display, Visibility: "listed"})
	}
	return out, header
}

// Antigravity ---------------------------------------------------------------

func (Antigravity) ResolveScope(ctx context.Context) (agentbackend.CatalogScope, error) {
	return agentbackend.CatalogScope{AiCli: "antigravity", CLIVersion: catalogVersion(ctx, "agy")}, nil
}

func (Antigravity) DiscoverModels(ctx context.Context, scope agentbackend.CatalogScope) agentbackend.CatalogObservation {
	b, err := modelCatalogRun(ctx, "agy", "models")
	if errors.Is(err, errCatalogOutputLimit) {
		models, _ := parseAgyCatalog(b)
		return agentbackend.CatalogObservation{Source: agentbackend.SourceLiveCommand, SourceDetail: "agy models", Completeness: agentbackend.CatalogPartial, Err: &agentbackend.CatalogError{Class: "partial_result"}, Models: models}
	}
	if err != nil {
		return catalogFailed(agentbackend.SourceLiveCommand, "agy models", err)
	}
	models, complete := parseAgyCatalog(b)
	if len(models) == 0 {
		return agentbackend.CatalogObservation{Source: agentbackend.SourceLiveCommand, SourceDetail: "agy models", Completeness: agentbackend.CatalogEmpty, Err: &agentbackend.CatalogError{Class: "malformed_or_empty"}, Models: models}
	}
	c := agentbackend.CatalogPartial
	if complete {
		c = agentbackend.CatalogComplete
	}
	return agentbackend.CatalogObservation{Source: agentbackend.SourceLiveCommand, SourceDetail: "agy models", Completeness: c, Models: models}
}

func parseAgyCatalog(b []byte) ([]agentbackend.ObservedModel, bool) {
	var out []agentbackend.ObservedModel
	complete := true
	s := bufio.NewScanner(bytes.NewReader(b))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(strings.ToLower(line), "fetching ") {
			continue
		}
		id, display, ok := strings.Cut(line, "\t")
		if !ok {
			complete = false
			id, display = line, line
		}
		id, display = strings.TrimSpace(id), strings.TrimSpace(display)
		if id != "" {
			out = append(out, agentbackend.ObservedModel{ModelID: id, DisplayName: display, Visibility: "listed"})
		}
	}
	return out, complete
}

// OpenCode ------------------------------------------------------------------

func (OpenCode) ResolveScope(ctx context.Context) (agentbackend.CatalogScope, error) {
	return agentbackend.CatalogScope{AiCli: "opencode", CLIVersion: catalogVersion(ctx, "opencode")}, nil
}

func (OpenCode) DiscoverModels(ctx context.Context, scope agentbackend.CatalogScope) agentbackend.CatalogObservation {
	b, err := modelCatalogRun(ctx, "opencode", "models")
	if errors.Is(err, errCatalogOutputLimit) {
		return agentbackend.CatalogObservation{Source: agentbackend.SourceLiveCommand, SourceDetail: "opencode models", Completeness: agentbackend.CatalogPartial, Err: &agentbackend.CatalogError{Class: "partial_result"}, Models: parseIDsCatalog(b)}
	}
	if err != nil {
		return catalogFailed(agentbackend.SourceLiveCommand, "opencode models", err)
	}
	models := parseIDsCatalog(b)
	c := agentbackend.CatalogPartial
	if len(models) == 0 {
		c = agentbackend.CatalogEmpty
	}
	return agentbackend.CatalogObservation{Source: agentbackend.SourceLiveCommand, SourceDetail: "opencode models", Completeness: c, Models: models}
}

func parseIDsCatalog(b []byte) []agentbackend.ObservedModel {
	out := []agentbackend.ObservedModel{}
	s := bufio.NewScanner(bytes.NewReader(b))
	for s.Scan() {
		id := strings.TrimSpace(s.Text())
		if id == "" || strings.HasPrefix(id, "#") {
			continue
		}
		out = append(out, agentbackend.ObservedModel{ModelID: id, DisplayName: id, Visibility: "listed"})
	}
	return out
}

// Crush ---------------------------------------------------------------------

func (Crush) ResolveScope(ctx context.Context) (agentbackend.CatalogScope, error) {
	profile := ""
	if dir, err := modelCatalogUserConfigDir(); err == nil {
		profile = filepath.Join(dir, "crush")
	}
	return agentbackend.CatalogScope{AiCli: "crush", Profile: profile, CLIVersion: catalogVersion(ctx, "crush")}, nil
}

func (Crush) DiscoverModels(ctx context.Context, scope agentbackend.CatalogScope) agentbackend.CatalogObservation {
	providers := crushProviders(scope.Profile)
	if len(providers) == 0 {
		return agentbackend.CatalogObservation{Source: agentbackend.SourceProviderCatalog, SourceDetail: "crush models (configured providers only)", Completeness: agentbackend.CatalogPartial, Err: &agentbackend.CatalogError{Class: "scope_unresolved"}, Models: []agentbackend.ObservedModel{}}
	}
	b, err := modelCatalogRun(ctx, "crush", "models")
	if errors.Is(err, errCatalogOutputLimit) {
		err = nil // filter the bounded prefix below; it remains explicitly partial.
		all := parseIDsCatalog(b)
		out := make([]agentbackend.ObservedModel, 0, len(all))
		for _, m := range all {
			provider, _, ok := strings.Cut(m.ModelID, "/")
			if ok && providers[provider] {
				out = append(out, m)
			}
		}
		return agentbackend.CatalogObservation{Source: agentbackend.SourceProviderCatalog, SourceDetail: "crush models (configured providers only)", Completeness: agentbackend.CatalogPartial, Err: &agentbackend.CatalogError{Class: "partial_result"}, Models: out}
	}
	if err != nil {
		return catalogFailed(agentbackend.SourceProviderCatalog, "crush models", err)
	}
	all := parseIDsCatalog(b)
	out := make([]agentbackend.ObservedModel, 0, len(all))
	for _, m := range all {
		provider, _, ok := strings.Cut(m.ModelID, "/")
		if ok && providers[provider] {
			out = append(out, m)
		}
	}
	return agentbackend.CatalogObservation{Source: agentbackend.SourceProviderCatalog, SourceDetail: "crush models (configured providers only)", Completeness: agentbackend.CatalogPartial, Models: out}
}

func crushProviders(profile string) map[string]bool {
	if profile == "" {
		return nil
	}
	b, err := modelCatalogReadFile(filepath.Join(profile, "crush.json"))
	if err != nil {
		return nil
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	v, ok := raw["providers"]
	if !ok {
		v = raw["provider"]
	}
	var names map[string]json.RawMessage
	if json.Unmarshal(v, &names) != nil {
		return nil
	}
	out := make(map[string]bool, len(names))
	for name := range names {
		out[name] = true
	}
	return out
}

// Codex ---------------------------------------------------------------------

type codexCache struct {
	FetchedAt     time.Time         `json:"fetched_at"`
	ETag          string            `json:"etag"`
	ClientVersion string            `json:"client_version"`
	Identity      string            `json:"identity"`
	Models        []codexCacheModel `json:"models"`
}
type codexCacheModel struct {
	Slug                     string `json:"slug"`
	DisplayName              string `json:"display_name"`
	Visibility               string `json:"visibility"`
	SupportedInAPI           *bool  `json:"supported_in_api"`
	Priority                 any    `json:"priority"`
	SupportedReasoningLevels []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
	DefaultReasoningLevel string   `json:"default_reasoning_level"`
	ContextWindow         int      `json:"context_window"`
	ServiceTiers          []string `json:"service_tiers"`
	Upgrade               string   `json:"upgrade"`
}

func (Codex) ResolveScope(ctx context.Context) (agentbackend.CatalogScope, error) {
	home := codexHome()
	if home == "" {
		return agentbackend.CatalogScope{AiCli: "codex"}, errors.New("CODEX_HOME is unavailable")
	}
	s := agentbackend.CatalogScope{AiCli: "codex", Profile: home, CLIVersion: catalogVersion(ctx, "codex")}
	if b, err := modelCatalogReadFile(filepath.Join(home, "models_cache.json")); err == nil {
		var cache codexCache
		if json.Unmarshal(b, &cache) == nil {
			s.AccountRef = cache.Identity
		}
	}
	return s, nil
}

func (Codex) DiscoverModels(ctx context.Context, scope agentbackend.CatalogScope) agentbackend.CatalogObservation {
	if scope.Profile == "" {
		scope.Profile = codexHome()
	}
	if scope.Profile == "" {
		return agentbackend.CatalogObservation{Source: agentbackend.SourceCLICache, SourceDetail: "$CODEX_HOME/models_cache.json", Completeness: agentbackend.CatalogFailed, Err: &agentbackend.CatalogError{Class: "scope_unresolved"}, Models: []agentbackend.ObservedModel{}}
	}
	path := filepath.Join(scope.Profile, "models_cache.json")
	b, err := modelCatalogReadFile(path)
	if err != nil {
		return catalogFailed(agentbackend.SourceCLICache, "$CODEX_HOME/models_cache.json", err)
	}
	var cache codexCache
	if err := json.Unmarshal(b, &cache); err != nil {
		return agentbackend.CatalogObservation{Source: agentbackend.SourceCLICache, SourceDetail: "$CODEX_HOME/models_cache.json", Completeness: agentbackend.CatalogFailed, Err: &agentbackend.CatalogError{Class: "malformed"}, Models: []agentbackend.ObservedModel{}}
	}
	if scope.AccountRef != "" && cache.Identity != "" && scope.AccountRef != cache.Identity {
		return agentbackend.CatalogObservation{Source: agentbackend.SourceCLICache, SourceDetail: "$CODEX_HOME/models_cache.json", SourceFetchedAt: cache.FetchedAt, Completeness: agentbackend.CatalogPartial, Err: &agentbackend.CatalogError{Class: "identity_mismatch"}, Models: []agentbackend.ObservedModel{}}
	}
	models := codexObserved(cache.Models)
	c := agentbackend.CatalogComplete
	var ce *agentbackend.CatalogError
	if len(models) == 0 {
		c, ce = agentbackend.CatalogEmpty, &agentbackend.CatalogError{Class: "empty"}
	}
	if cache.FetchedAt.IsZero() || time.Since(cache.FetchedAt) > 24*time.Hour {
		c, ce = agentbackend.CatalogPartial, &agentbackend.CatalogError{Class: "stale_cache"}
	}
	if scope.CLIVersion != "" && cache.ClientVersion != "" && !strings.Contains(scope.CLIVersion, cache.ClientVersion) {
		c, ce = agentbackend.CatalogPartial, &agentbackend.CatalogError{Class: "version_mismatch"}
	}
	return agentbackend.CatalogObservation{Source: agentbackend.SourceCLICache, SourceDetail: "$CODEX_HOME/models_cache.json", SourceFetchedAt: cache.FetchedAt, Completeness: c, Err: ce, Models: models}
}

func codexObserved(models []codexCacheModel) []agentbackend.ObservedModel {
	out := make([]agentbackend.ObservedModel, 0, len(models))
	for _, m := range models {
		if strings.TrimSpace(m.Slug) == "" {
			continue
		}
		raw := map[string]string{}
		if m.SupportedInAPI != nil {
			raw["supported_in_api"] = strconv.FormatBool(*m.SupportedInAPI)
		}
		if len(m.ServiceTiers) > 0 {
			raw["service_tiers"] = strings.Join(m.ServiceTiers, ",")
		}
		if m.Priority != nil {
			raw["priority"] = "declared"
		}
		reasoning := make([]string, 0, len(m.SupportedReasoningLevels))
		for _, r := range m.SupportedReasoningLevels {
			if r.Effort != "" {
				reasoning = append(reasoning, r.Effort)
			}
		}
		visibility := "listed"
		if m.Visibility == "hide" {
			visibility = "hidden"
		}
		out = append(out, agentbackend.ObservedModel{ModelID: m.Slug, DisplayName: m.DisplayName, Visibility: visibility, ReasoningLevels: reasoning, DefaultReasoning: m.DefaultReasoningLevel, ContextWindow: m.ContextWindow, UpgradeTo: m.Upgrade, Raw: raw})
	}
	return out
}

// ModelLister remains a compatibility projection during the migration window.
func catalogList(ctx context.Context, src agentbackend.ModelCatalogSource) ([]string, bool) {
	scope, err := src.ResolveScope(ctx)
	if err != nil {
		return nil, false
	}
	o := src.DiscoverModels(ctx, scope)
	if o.Completeness == agentbackend.CatalogFailed || o.Completeness == agentbackend.CatalogEmpty {
		return nil, false
	}
	ids := make([]string, 0, len(o.Models))
	for _, m := range o.Models {
		if m.Visibility != "hidden" {
			ids = append(ids, m.ModelID)
		}
	}
	return ids, true
}

package plansync

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// EnvToken is the environment variable that overrides plan_sync.token when set.
const EnvToken = "WARDEN_PLAN_SYNC_TOKEN"

// ClientConfig selects and configures a PlanSyncProvider. Provider defaults to
// local. Hub requires BaseURL; Token may come from the field or EnvToken.
type ClientConfig struct {
	// Provider is "local" or "hub" (default local). "fake" is allowed for tests.
	Provider string
	// BaseURL is the Hub origin when Provider=hub.
	BaseURL string
	// Token is the bearer credential when Provider=hub. EnvToken overrides when set.
	Token string
	// HTTP optional client for Hub; nil ⇒ default timeout client.
	HTTP *http.Client
	// Now optional clock for Hub stamping; nil ⇒ time.Now.
	Now func() time.Time
}

// New returns a PlanSyncProvider from cfg. Default / empty provider is Local().
// Unknown providers error. Hub construction requires a non-empty BaseURL.
func New(cfg ClientConfig) (PlanSyncProvider, error) {
	switch NormalizeProvider(cfg.Provider) {
	case ProviderLocal:
		return Local(), nil
	case ProviderFake:
		return NewFake(), nil
	case ProviderHub:
		token := strings.TrimSpace(cfg.Token)
		if env := strings.TrimSpace(os.Getenv(EnvToken)); env != "" {
			token = env
		}
		return NewHub(HubOptions{
			BaseURL: cfg.BaseURL,
			Token:   token,
			HTTP:    cfg.HTTP,
			Now:     cfg.Now,
		})
	default:
		return nil, fmt.Errorf("plansync: unknown provider %q (want local|hub)", cfg.Provider)
	}
}

// NormalizeProvider lowercases and trims provider; empty becomes local.
func NormalizeProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "", ProviderLocal:
		return ProviderLocal
	case ProviderHub:
		return ProviderHub
	case ProviderFake:
		return ProviderFake
	default:
		return strings.ToLower(strings.TrimSpace(p))
	}
}

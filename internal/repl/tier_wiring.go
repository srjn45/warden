package repl

import (
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/llm"
)

// NewRouterFromConfig is retained for call-site compatibility. Planning is owned
// by the injected Chatter (Fast-Brain), so the legacy local_llm_tier /
// local_llm_escalate / local_llm_classifier settings no longer influence routing.
func NewRouterFromConfig(_ config.Config, _ llm.Completer) *Router { return NewRouter() }

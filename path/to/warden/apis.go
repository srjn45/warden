package warden

import (
	"net/http"
)

// ListAPIs returns a list of available API endpoints.
func ListAPIs() []string {
	return []string{
		"/sessions",
		"/status",
		"/digest",
		"/prune",
		"/schedule",
		"/snapshot",
		"/plugin",
		"/pipeline",
		"/context",
		"/confirmer",
		"/daemon",
	}
}

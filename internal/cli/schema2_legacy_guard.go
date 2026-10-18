package cli

import (
	"fmt"

	"github.com/srjn45/warden/internal/schema"
)

// onSchema2 reports whether dataDir's ledger says schema 2: warden-db is then
// the single authority and the schema-1 store directories left beside it are
// inert, migrated-from copies (the cutover never deletes them).
func onSchema2(dataDir string) bool {
	l, err := schema.Load(dataDir)
	return err == nil && l.SchemaVersion >= schema.Schema2
}

// errLegacyStoreInert is returned by CLI paths that open a schema-1 store
// directory directly. On schema 2 they would read a frozen copy or — worse —
// write one the daemon never looks at (a silent second writer), so they refuse.
func errLegacyStoreInert(what string) error {
	return fmt.Errorf("%s: this data directory is on schema 2 (warden-db); the schema-1 store directories are inert copies kept from the migration "+
		"and are no longer read or written. Use the daemon-backed interfaces (REST/MCP/TUI), or for offline verification/repair `warden repair warden-db`", what)
}

// requireSchema1Stores returns errLegacyStoreInert when dataDir is on schema 2.
func requireSchema1Stores(dataDir, what string) error {
	if onSchema2(dataDir) {
		return errLegacyStoreInert(what)
	}
	return nil
}

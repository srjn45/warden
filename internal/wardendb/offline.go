package wardendb

import (
	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
)

// OpenOfflineLegacyDir opens a schema-1 collection directory for offline
// migration inspection. Callers must not use it against a running daemon handle.
func OpenOfflineLegacyDir(dir string) (*scriva.DB, error) {
	return scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
}

// OpenOfflineStagingDir opens a review-only staging directory for migration
// output. It is deliberately separate from Dir(dataDir), the cutover database.
func OpenOfflineStagingDir(dir string) (*scriva.DB, error) {
	return scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeAlways))
}

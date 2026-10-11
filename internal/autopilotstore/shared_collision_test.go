package autopilotstore_test

import (
	"testing"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/wardendb"
)

func TestSharedStoreCoexistsWithRunStore(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := autopilotstore.NewStoreFromDB(db); err != nil {
		t.Fatal(err)
	}
	if _, err := autopilot.NewRunStoreFromDB(db); err != nil {
		t.Fatal(err)
	}
	if autopilotstore.CollectionSpecs()[0].Name == autopilot.RunCollectionSpecs()[0].Name {
		t.Fatal("identity and run stores share a collection")
	}
}

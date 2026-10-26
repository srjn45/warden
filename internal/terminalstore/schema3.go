package terminalstore

import "github.com/srjn45/warden/internal/wardendb"

func NewSchema3StoreFromDB(db *wardendb.DB) (*Store, error) {
	col, err := wardendb.RegisterOwned[Terminal](db, terminalsCollection, wardendb.OwnerContract{
		Active: func(m map[string]any) bool { return m["status"] == "running" },
	}, "project_id", "status")
	if err != nil {
		return nil, err
	}
	return &Store{col: col}, nil
}

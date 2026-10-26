package planexport

import (
	"fmt"

	"github.com/srjn45/warden/internal/wardendb"
)

func NewSchema3StoreFromDB(db *wardendb.DB) (*Store, error) {
	col, err := wardendb.RegisterOwned[Record](db, exportsCollection, wardendb.OwnerContract{
		AllowClosedCreates: true,
		Immutable:          []string{"plan_id", "repository", "target_ref", "output_path"},
		References:         map[string]string{"plan_id": "plans"},
		Encode: func(body map[string]any) (map[string]any, error) {
			m, err := wardendb.LosslessDocument(body)
			if err != nil {
				return nil, err
			}
			planID, _ := m["plan_id"].(string)
			if planID == "" {
				return nil, fmt.Errorf("planexport: direct Plan ownership required")
			}
			handle, err := db.Handle()
			if err != nil {
				return nil, err
			}
			plans, err := handle.Engine().Collection("plans")
			if err != nil {
				return nil, err
			}
			plan, err := plans.GetByKey(planID)
			if err != nil {
				return nil, err
			}
			if owner, _ := m["project_id"].(string); owner != "" && owner != plan.Data["project_id"] {
				return nil, fmt.Errorf("planexport: cross-project export")
			}
			m["project_id"] = plan.Data["project_id"]
			return m, nil
		},
	}, "plan_id", "project_id")
	if err != nil {
		return nil, err
	}
	return &Store{col: col}, nil
}

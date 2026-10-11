package savings

// Shared-handle construction (warden-db porting pattern, see
// internal/projectstore/shared.go). NewStoreFromDB takes the shared
// *wardendb.DB: no path, no scriva.Open, no Close of the handle. The ledger is
// the "savings_events" collection (one auto-id record per event, schema-1
// shaped) and the calibration sidecar is the single "current" record of
// "savings_calibration". The JSONL ledger stays the production default;
// production wiring is untouched.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	eventsCollection      = "savings_events"
	calibrationCollection = "savings_calibration"
	calibrationKey        = "current"
	// recordSchemaVersion is the registry version of the schema-1 shaped
	// records this port persists.
	recordSchemaVersion = 1
)

// docs is the document surface the shared ledger needs from a collection.
type docs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	GetByKey(string) (engine.Record, error)
	Insert(map[string]any) (uint64, time.Time, error)
	Upsert(string, map[string]any) (engine.Record, error)
}

// sharedDocs resolves the engine collection on every call (Engine() fails with
// ErrClosed after handle retirement, so it must not be retained).
type sharedDocs struct {
	get func() (*engine.Collection, error)
}

func (d sharedDocs) Config() engine.CollectionConfig {
	c, err := d.get()
	if err != nil {
		return engine.CollectionConfig{}
	}
	return c.Config()
}

func (d sharedDocs) Scan(f query.Filter) ([]engine.ScanResult, error) {
	c, err := d.get()
	if err != nil {
		return nil, err
	}
	return c.Scan(f)
}

func (d sharedDocs) GetByKey(k string) (engine.Record, error) {
	c, err := d.get()
	if err != nil {
		return engine.Record{}, err
	}
	return c.GetByKey(k)
}

func (d sharedDocs) Insert(m map[string]any) (uint64, time.Time, error) {
	c, err := d.get()
	if err != nil {
		return 0, time.Time{}, err
	}
	return c.Insert(m)
}

func (d sharedDocs) Upsert(k string, m map[string]any) (engine.Record, error) {
	c, err := d.get()
	if err != nil {
		return engine.Record{}, err
	}
	return c.Upsert(k, m)
}

// sharedLedger is the warden-db backing of Store.
type sharedLedger struct {
	events docs
	calib  docs
}

// CollectionSpecs declares the collections this store owns on the shared handle.
func CollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{
		{Name: eventsCollection, Version: recordSchemaVersion},
		{Name: calibrationCollection, Version: recordSchemaVersion},
	}
}

// NewStoreFromDB builds a ledger over collections registered on the shared
// warden-db handle. The caller keeps ownership of db and closes it; the
// collections must not already be registered on it.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	ev, err := wardendb.Register[Event](db, CollectionSpecs()[0])
	if err != nil {
		return nil, err
	}
	cal, err := wardendb.Register[Calibration](db, CollectionSpecs()[1])
	if err != nil {
		return nil, err
	}
	return &Store{shared: &sharedLedger{
		events: sharedDocs{get: ev.Engine},
		calib:  sharedDocs{get: cal.Engine},
	}}, nil
}

func toRecord(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func fromRecord(m map[string]any, out any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (l *sharedLedger) record(ev Event) error {
	m, err := toRecord(ev)
	if err != nil {
		return err
	}
	_, _, err = l.events.Insert(m)
	return err
}

// read returns events with TS >= since in insertion order; an undecodable
// record is skipped like a corrupt JSONL line.
func (l *sharedLedger) read(since time.Time) ([]Event, error) {
	res, err := l.events.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	sort.Slice(res, func(i, j int) bool { return res[i].ID < res[j].ID })
	var out []Event
	for _, r := range res {
		var ev Event
		if fromRecord(r.Data, &ev) != nil {
			continue
		}
		if !since.IsZero() && ev.TS.Before(since) {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

func (l *sharedLedger) calibration() (Calibration, bool, error) {
	rec, err := l.calib.GetByKey(calibrationKey)
	if err != nil {
		if errors.Is(err, engine.ErrKeyNotFound) {
			return Calibration{}, false, nil
		}
		return Calibration{}, false, err
	}
	var c Calibration
	if err := fromRecord(rec.Data, &c); err != nil {
		return Calibration{}, false, err
	}
	if c.BytesPerToken <= 0 {
		return Calibration{}, false, nil
	}
	return c, true, nil
}

func (l *sharedLedger) saveCalibration(c Calibration) error {
	m, err := toRecord(c)
	if err != nil {
		return err
	}
	if _, err := l.calib.Upsert(calibrationKey, m); err != nil {
		return fmt.Errorf("savings: save calibration: %w", err)
	}
	return nil
}

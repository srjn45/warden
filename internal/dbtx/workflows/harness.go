// Package workflows models seven representative multi-collection Warden
// workflows as transaction-backed FIXTURES over internal/dbtx, each paired with
// an invariant checker that holds for ANY database state.
//
// These are fixtures, not a production migration: they use minimal typed
// records shaped like the schema-2 collections (see
// docs/specs/2026-10-10-warden-db-collection-contract.md) and never touch the
// existing production stores. External effects (tmux, git, files) are not
// performed; they appear only as operation-journal rows written by the
// transaction (intent first, outcome in a second transaction).
//
// The package is built for reuse by crash/fault matrices: everything is
// deterministic (fixed ids, no clocks), parameterised by an open *scriva.DB and
// a *dbtx.Boundary, and exposed through the All() table.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/dbtx"
)

// Collection names (schema-2 shaped; agents_archive is the schema-1 two-
// collection archive shape, used here to exercise a delete+insert move).
const (
	CollProjects      = "projects"
	CollAgents        = "agents"
	CollAgentsArchive = "agents_archive"
	CollPlans         = "plans"
	CollPipelines     = "pipelines"
	CollPipelineJobs  = "pipeline_jobs"
	CollRuns          = "autopilot_runs"
	CollViews         = "project_views"
	CollEvents        = "events"
	CollOperations    = "operations"
)

type collSpec struct {
	name    string
	unique  []string
	indexed []string
}

// collections lists every participant collection and its indexes. Every record
// carries a logical string key in field "id" (XTx ops address numeric record
// ids only), enforced unique.
var collections = []collSpec{
	{CollProjects, []string{"id"}, nil},
	{CollAgents, []string{"id"}, []string{"project_id"}},
	{CollAgentsArchive, []string{"id"}, []string{"project_id"}},
	{CollPlans, []string{"id"}, []string{"project_id"}},
	{CollPipelines, []string{"id"}, []string{"plan_id"}},
	{CollPipelineJobs, []string{"id"}, []string{"pipeline_id"}},
	// live_plan is set only while a run is non-terminal; a unique index on it
	// enforces "one live run per plan" (contract §3.9, R9).
	{CollRuns, []string{"id", "live_plan"}, []string{"plan_id"}},
	{CollViews, []string{"id"}, nil},
	{CollEvents, []string{"id"}, []string{"subject"}},
	{CollOperations, []string{"id"}, []string{"subject_id"}},
}

// CollectionNames returns every collection Setup creates, in a stable order.
func CollectionNames() []string {
	out := make([]string, len(collections))
	for i, c := range collections {
		out[i] = c.name
	}
	return out
}

// Setup creates every collection and index up front. Collection creation is
// not transactional (audit G8), so it must run before any workflow; it is
// idempotent and is called again after a reopen.
func Setup(db *scriva.DB) error {
	for _, c := range collections {
		var opts []scriva.CollectionOption
		if len(c.unique) > 0 {
			opts = append(opts, scriva.WithUniqueIndex(c.unique...))
		}
		col, err := db.Collection(c.name, opts...)
		if err != nil {
			return fmt.Errorf("workflows: setup %s: %w", c.name, err)
		}
		for _, f := range c.indexed {
			if err := col.EnsureIndex(f); err != nil {
				return fmt.Errorf("workflows: setup %s.%s: %w", c.name, f, err)
			}
		}
	}
	return nil
}

// jsonCodec stores a record as its JSON object form.
type jsonCodec[T any] struct{}

func (jsonCodec[T]) Encode(v T) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(raw, &m)
	return m, err
}

func (jsonCodec[T]) Decode(m map[string]any) (T, error) {
	var v T
	raw, err := json.Marshal(m)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(raw, &v)
	return v, err
}

// reader is the read-only, non-transactional view of one collection.
type reader[T any] struct {
	name  string
	col   *engine.Collection
	codec jsonCodec[T]
}

func newReader[T any](db *scriva.DB, name string) (*reader[T], error) {
	col, err := db.Engine().Collection(name)
	if err != nil {
		return nil, fmt.Errorf("workflows: collection %q: %w", name, err)
	}
	return &reader[T]{name: name, col: col}, nil
}

// All returns every live record, ordered by physical id.
func (r *reader[T]) All() ([]dbtx.Versioned[T], error) {
	rows, err := r.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	out := make([]dbtx.Versioned[T], 0, len(rows))
	for _, row := range rows {
		v, err := r.codec.Decode(row.Data)
		if err != nil {
			return nil, fmt.Errorf("%s/%d: %w", r.name, row.ID, err)
		}
		out = append(out, dbtx.Versioned[T]{ID: row.ID, Rev: row.Rev, Value: v})
	}
	return out, nil
}

// ByKey returns the record whose logical id equals key. More than one match is
// an invariant breach (the unique index should make it impossible).
func (r *reader[T]) ByKey(key string) (dbtx.Versioned[T], bool, error) {
	ids, ok := r.col.IndexLookup("id", key)
	if !ok {
		all, err := r.All()
		if err != nil {
			return dbtx.Versioned[T]{}, false, err
		}
		for _, v := range all {
			if keyOf(v.Value) == key {
				return v, true, nil
			}
		}
		return dbtx.Versioned[T]{}, false, nil
	}
	if len(ids) > 1 {
		return dbtx.Versioned[T]{}, false, fmt.Errorf("%s: %d records share id %q", r.name, len(ids), key)
	}
	if len(ids) == 0 {
		return dbtx.Versioned[T]{}, false, nil
	}
	rec, err := r.col.Get(ids[0])
	if err != nil {
		return dbtx.Versioned[T]{}, false, err
	}
	v, err := r.codec.Decode(rec.Data)
	if err != nil {
		return dbtx.Versioned[T]{}, false, err
	}
	return dbtx.Versioned[T]{ID: rec.ID, Rev: rec.Rev, Value: v}, true, nil
}

func keyOf(v any) string {
	raw, _ := json.Marshal(v)
	var m struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.ID
}

// Repo is a typed handle: transactional writes via the dbtx boundary plus
// plain reads.
type Repo[T any] struct {
	*dbtx.Coll[T]
	*reader[T]
}

func bindRepo[T any](db *scriva.DB, b *dbtx.Boundary, name string) (*Repo[T], error) {
	c, err := dbtx.Bind[T](b, name, jsonCodec[T]{})
	if err != nil {
		return nil, err
	}
	r, err := newReader[T](db, name)
	if err != nil {
		return nil, err
	}
	return &Repo[T]{Coll: c, reader: r}, nil
}

// Env bundles an open DB, its boundary and the bound collections. Build one per
// DB open (after Setup).
type Env struct {
	DB *scriva.DB
	B  *dbtx.Boundary

	Projects  *Repo[Project]
	Agents    *Repo[Agent]
	Archive   *Repo[Agent]
	Plans     *Repo[Plan]
	Pipelines *Repo[Pipeline]
	Jobs      *Repo[PipelineJob]
	Runs      *Repo[Run]
	Views     *Repo[View]
	Events    *Repo[Event]
	Ops       *Repo[Operation]

	// BeforeCommit is a test/fault seam: called once per build attempt after
	// every write is staged and before commit. Returning an error aborts the
	// attempt (rollback); a competing write made here forces a conflict.
	BeforeCommit func(attempt int) error
}

// NewEnv binds every collection. Setup must have run on db.
func NewEnv(db *scriva.DB, b *dbtx.Boundary) (*Env, error) {
	e := &Env{DB: db, B: b}
	var err error
	bind := func(dst any, name string) {
		if err != nil {
			return
		}
		switch d := dst.(type) {
		case **Repo[Project]:
			*d, err = bindRepo[Project](db, b, name)
		case **Repo[Agent]:
			*d, err = bindRepo[Agent](db, b, name)
		case **Repo[Plan]:
			*d, err = bindRepo[Plan](db, b, name)
		case **Repo[Pipeline]:
			*d, err = bindRepo[Pipeline](db, b, name)
		case **Repo[PipelineJob]:
			*d, err = bindRepo[PipelineJob](db, b, name)
		case **Repo[Run]:
			*d, err = bindRepo[Run](db, b, name)
		case **Repo[View]:
			*d, err = bindRepo[View](db, b, name)
		case **Repo[Event]:
			*d, err = bindRepo[Event](db, b, name)
		case **Repo[Operation]:
			*d, err = bindRepo[Operation](db, b, name)
		}
	}
	bind(&e.Projects, CollProjects)
	bind(&e.Agents, CollAgents)
	bind(&e.Archive, CollAgentsArchive)
	bind(&e.Plans, CollPlans)
	bind(&e.Pipelines, CollPipelines)
	bind(&e.Jobs, CollPipelineJobs)
	bind(&e.Runs, CollRuns)
	bind(&e.Views, CollViews)
	bind(&e.Events, CollEvents)
	bind(&e.Ops, CollOperations)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// run executes one workflow as exactly one dbtx transaction. build re-reads and
// restages from scratch on every attempt; returning dbtx.ErrNoChange means the
// target state already holds.
func (e *Env) run(ctx context.Context, intent string, build func(tx *dbtx.Tx) error) (*dbtx.Result, error) {
	attempt := 0
	return e.B.Run(ctx, intent, func(_ context.Context, tx *dbtx.Tx) error {
		attempt++
		if err := build(tx); err != nil {
			return err
		}
		if e.BeforeCommit != nil {
			return e.BeforeCommit(attempt)
		}
		return nil
	})
}

// Domain errors returned from rebuild callbacks (never retried).
var (
	ErrProjectMissing = errors.New("workflows: project not found")
	ErrProjectClosed  = errors.New("workflows: project is closed")
	ErrProjectBusy    = errors.New("workflows: project has a live autopilot run")
	ErrPlanMissing    = errors.New("workflows: plan not found")
	ErrPlanNotReady   = errors.New("workflows: plan is not ready to start")
	ErrPlanLinked     = errors.New("workflows: plan is already linked to a different pipeline")
	ErrAgentMissing   = errors.New("workflows: agent not found")
	ErrExists         = errors.New("workflows: record already exists")
)

// Workflow is one row of the table crash/fault suites iterate.
type Workflow struct {
	Name string
	// Seed establishes preconditions (idempotent; may use other workflows).
	Seed func(ctx context.Context, e *Env) error
	// Run performs the whole multi-collection change in ONE dbtx transaction.
	// It is re-runnable: when the target state already holds it is a no-op.
	Run func(ctx context.Context, e *Env) error
	// Check reports whether the workflow's cross-collection invariant holds in
	// db, whatever state it is in. It reads only.
	Check func(db *scriva.DB) error
	// Interfere makes a benign competing write to a record Run fences with
	// ExpectedRev, forcing one conflict when called from BeforeCommit.
	Interfere func(e *Env) error
	// Collections names the participants a successful Run writes.
	Collections []string
}

// violations accumulates invariant breaches into one error.
type violations struct {
	checker string
	list    []string
}

func (v *violations) addf(format string, a ...any) {
	v.list = append(v.list, fmt.Sprintf(format, a...))
}

func (v *violations) err() error {
	if len(v.list) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %d violation(s): %s", v.checker, len(v.list), strings.Join(v.list, "; "))
}

// Snapshot renders every workflow collection as canonical text (record data
// only: no physical ids or revisions), for before/after equality assertions.
func Snapshot(db *scriva.DB) (string, error) {
	var sb strings.Builder
	for _, c := range collections {
		col, err := db.Engine().Collection(c.name)
		if err != nil {
			return "", err
		}
		rows, err := col.Scan(query.MatchAll)
		if err != nil {
			return "", err
		}
		lines := make([]string, 0, len(rows))
		for _, r := range rows {
			raw, _ := json.Marshal(r.Data) // map keys marshal sorted
			lines = append(lines, string(raw))
		}
		sort.Strings(lines)
		fmt.Fprintf(&sb, "## %s (%d)\n", c.name, len(lines))
		for _, l := range lines {
			sb.WriteString(l + "\n")
		}
	}
	return sb.String(), nil
}

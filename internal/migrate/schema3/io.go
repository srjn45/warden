package schema3

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

type IndexSpec struct {
	Field  string `json:"field"`
	Unique bool   `json:"unique"`
}
type Indexes map[string][]IndexSpec

// Read reads a private copy, after full checksum/transaction verification. No
// best-effort store List decoder participates. Numeric payloads use UseNumber.
func Read(ctx context.Context, dir string) (Generation, Indexes, error) {
	if err := SafeTree(dir); err != nil {
		return nil, nil, err
	}
	before, err := fingerprint(dir)
	if err != nil {
		return nil, nil, err
	}
	rep, err := engine.VerifyDir(ctx, dir, engine.VerifyOptions{Mode: engine.VerifyFull})
	if err != nil {
		return nil, nil, err
	}
	check := func(findings []engine.Finding) error {
		for _, f := range findings {
			if f.Severity != engine.SeverityInfo && f.Severity != engine.SeverityRepairableIndex {
				return fmt.Errorf("schema3: source integrity: %v", f)
			}
		}
		return nil
	}
	if err = check(rep.Findings); err != nil {
		return nil, nil, err
	}
	for _, c := range rep.Collections {
		if err = check(c.Findings); err != nil {
			return nil, nil, err
		}
	}
	tmp, err := os.MkdirTemp("", "warden-schema2-reader-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	if err = CopyTree(dir, filepath.Join(tmp, "db")); err != nil {
		return nil, nil, err
	}
	g, indexes, err := readCopy(ctx, filepath.Join(tmp, "db"))
	if err != nil {
		return nil, nil, err
	}
	after, err := fingerprint(dir)
	if err != nil {
		return nil, nil, err
	}
	if before != after {
		return nil, nil, fmt.Errorf("schema3: source changed during read-only inspection")
	}
	return g, indexes, nil
}
func readCopy(ctx context.Context, dir string) (Generation, Indexes, error) {
	db, err := wardendb.OpenOfflineLegacyDir(dir)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	g := Generation{}
	idx := Indexes{}
	for _, de := range entries {
		if err = ctx.Err(); err != nil {
			return nil, nil, err
		}
		if !de.IsDir() {
			continue
		}
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			return nil, nil, fmt.Errorf("schema3: unknown database directory %s", name)
		}
		// Runtime catalogues are deliberately not consulted: the source is frozen.
		if !sourceCollections[name] && name != "plan_tasks" {
			return nil, nil, fmt.Errorf("schema3: unknown collection %s", name)
		}
		raw, err := rawRecords(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		col, err := db.Collection(name)
		if err != nil {
			return nil, nil, err
		}
		rows, err := col.Scan(query.MatchAll)
		if err != nil {
			return nil, nil, err
		}
		g[name] = map[string]Object{}
		for _, row := range rows {
			rec, err := col.Get(row.ID)
			if err != nil {
				return nil, nil, err
			}
			candidates := raw[fmt.Sprintf("%d/%d", rec.ID, rec.Rev)]
			var body Object
			for _, candidate := range candidates {
				if body != nil && !equal(body, candidate) {
					return nil, nil, fmt.Errorf("schema3: ambiguous raw revision %s/%d/%d", name, rec.ID, rec.Rev)
				}
				body = candidate
			}
			if body == nil {
				return nil, nil, fmt.Errorf("schema3: raw record missing %s/%d", name, rec.ID)
			}
			key, err := sourceRecordKey(name, rec.ID, body)
			if err != nil {
				return nil, nil, err
			}
			body = clone(body)
			delete(body, "_key")
			if g[name][key] != nil {
				return nil, nil, fmt.Errorf("schema3: duplicate key %s/%s", name, key)
			}
			g[name][key] = body
		}
		files, err := os.ReadDir(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		for _, f := range files {
			if strings.HasPrefix(f.Name(), "sidx_") && strings.HasSuffix(f.Name(), ".json") {
				raw, err := os.ReadFile(filepath.Join(dir, name, f.Name()))
				if err != nil {
					return nil, nil, err
				}
				var spec IndexSpec
				if err = json.Unmarshal(raw, &spec); err != nil {
					return nil, nil, err
				}
				if spec.Field != "_key" {
					idx[name] = append(idx[name], spec)
				}
			}
		}

	}
	return g, idx, nil
}

// sourceRecordKey returns the stable logical key used in the schema-3
// generation. Schema 2 normally persisted it in the document body. The
// savings ledger was the one deliberate exception: it is append-only and used
// engine-assigned numeric IDs. Those IDs are durable and unique, so preserve
// every historical event under a namespaced deterministic key. Do not relax
// this for other collections: a missing key there is ambiguous ownership data.
func sourceRecordKey(collection string, id uint64, body Object) (string, error) {
	if key := text(body["_key"]); key != "" {
		return key, nil
	}
	if collection == "savings_events" {
		return fmt.Sprintf("savings_events/%d", id), nil
	}
	return "", fmt.Errorf("schema3: keyless record %s/%d", collection, id)
}

func rawRecords(dir string) (map[string][]Object, error) {
	out := map[string][]Object{}
	files, err := filepath.Glob(filepath.Join(dir, "seg_*.ndjson"))
	if err != nil {
		return nil, err
	}
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 65536), 64<<20)
		for sc.Scan() {
			var e struct {
				ID   json.Number `json:"id"`
				Rev  json.Number `json:"rev"`
				Op   string      `json:"op"`
				Data Object      `json:"data"`
			}
			d := json.NewDecoder(bytes.NewReader(sc.Bytes()))
			d.UseNumber()
			if err = d.Decode(&e); err != nil {
				f.Close()
				return nil, fmt.Errorf("schema3: %s: %w", p, err)
			}
			if e.Rev == "" {
				e.Rev = "0"
			}
			if e.Data != nil {
				out[string(e.ID)+"/"+string(e.Rev)] = append(out[string(e.ID)+"/"+string(e.Rev)], e.Data)
			}
		}
		err = sc.Err()
		ce := f.Close()
		if err != nil {
			return nil, err
		}
		if ce != nil {
			return nil, ce
		}
	}
	return out, nil
}

var RequiredIndexes = lookupIndexes(map[string][]string{
	"projects": {"status", "group_id", "path"}, "project_groups": {"name"},
	"agents":    {"project_id", "parent_agent_id", "plan_id", "pipeline_id", "job_id", "autopilot_run_id", "autopilot_task_id", "status", "lifecycle"},
	"terminals": {"project_id", "status"}, "plans": {"project_id", "status", "execution_mode"},
	"plan_tasks":     {"plan_id", "project_id", "status", "seq", "task_id"},
	"pipelines":      {"project_id", "parent_agent_id", "plan_id", "status"},
	"pipeline_jobs":  {"pipeline_id", "project_id", "status", "seq", "job_id"},
	"autopilot_runs": {"project_id", "plan_id", "parent_agent_id", "state"},
	"plan_events":    {"plan_id", "project_id"}, "plan_notes": {"plan_id", "project_id"}, "plan_exports": {"plan_id", "project_id"},
})

func lookupIndexes(in map[string][]string) Indexes {
	out := Indexes{}
	for c, fields := range in {
		for _, f := range fields {
			out[c] = append(out[c], IndexSpec{Field: f})
		}
	}
	return out
}

// WriteStaging accepts only an absent, transaction-named staging directory. It
// creates a fresh physical generation: no old tombstones/retired tables survive.
func WriteStaging(ctx context.Context, dir string, g Generation, sourceIndexes Indexes) error {
	if err := ValidateTarget(g); err != nil {
		return err
	}
	if !strings.HasPrefix(filepath.Base(dir), ".schema3-staging-") {
		return fmt.Errorf("schema3: staging name required")
	}
	// Scriva v1.6 decodes numeric payloads through float64 when verifying CRCs.
	// Refuse values that cannot make a stable engine round trip before writing.
	var engineView Generation
	if err := json.Unmarshal(encode(g), &engineView); err != nil {
		return err
	}
	if !equal(g, engineView) {
		return fmt.Errorf("schema3: numeric payload is not losslessly representable by the storage engine")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	db, err := wardendb.OpenOfflineStagingDir(dir)
	if err != nil {
		return err
	}
	writeErr := func() error {
		for _, c := range keys(g) {
			if c == "autopilots" || c == "archived_agents" || (!sourceCollections[c] && c != "plan_tasks") {
				return fmt.Errorf("schema3: invalid target collection %s", c)
			}
			var opts []scriva.CollectionOption
			field := ""
			if _, ok := frozenFields[c]; ok {
				field = "id"
			}
			if c == "plan_tasks" || c == "pipeline_jobs" {
				field = "id"
			}
			if c == "autopilot_runs" {
				field = "run_id"
			}
			if field != "" {
				opts = append(opts, scriva.WithUniqueIndex(field))
			}
			col, e := db.Collection(c, opts...)
			if e != nil {
				return e
			}
			indexes := append(append([]IndexSpec{}, sourceIndexes[c]...), RequiredIndexes[c]...)
			for _, f := range indexes {
				if retiredIndex(c, f.Field) {
					continue
				}
				if f.Unique && !(c == "projects" && f.Field == "path") {
					e = col.EnsureUniqueIndex(f.Field)
				} else {
					e = col.EnsureIndex(f.Field)
				}
				if e != nil {
					return e
				}
			}
			for _, k := range keys(g[c]) {
				if e = ctx.Err(); e != nil {
					return e
				}
				if _, _, e = col.InsertWithKey(k, clone(g[c][k])); e != nil {
					return fmt.Errorf("schema3: write %s/%s: %w", c, k, e)
				}
			}
		}
		return nil
	}()
	closeErr := db.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return SyncTree(dir)
}
func retiredIndex(c, f string) bool {
	fields := map[string]string{"projects": "agents pipelines terminals plans autopilots", "project_groups": "project_ids", "agents": "parent_id child_agents child_pipelines child_autopilots", "plans": "tasks task_progress task_outcomes pipeline_id autopilot_run_id orchestrator_id", "pipelines": "jobs", "pipeline_jobs": "agent_id session_id", "autopilot_runs": "brain_id"}
	for _, x := range strings.Fields(fields[c]) {
		if x == f || strings.HasPrefix(f, x+".") {
			return true
		}
	}
	return false
}
func SafeTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("schema3: unsafe path %s", p)
		}
		return nil
	})
}
func SyncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		e = f.Sync()
		ce := f.Close()
		if e != nil {
			return e
		}
		return ce
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		f, e := os.Open(dirs[i])
		if e != nil {
			return e
		}
		e = f.Sync()
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	}
	return nil
}

// CopyTree never follows symlinks, never overlays an existing destination, and
// preserves source file permissions. Callers must hold exclusive ownership.
func CopyTree(src, dst string) error {
	if err := SafeTree(src); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		to := filepath.Join(dst, rel)
		info, e := d.Info()
		if e != nil {
			return e
		}
		if d.IsDir() {
			return os.Mkdir(to, info.Mode().Perm()|0700)
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		defer in.Close()
		out, e := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if e != nil {
			return e
		}
		_, e = io.Copy(out, in)
		if e == nil {
			e = out.Sync()
		}
		ce := out.Close()
		if e != nil {
			return e
		}
		return ce
	})
}

// VerifyIndexes checks both required schema-3 indexes and retained source index
// definitions. Compound uniqueness is validated by Map; constituent indexes are
// never misrepresented as compound unique indexes.
func VerifyIndexes(actual, source Indexes, g Generation) error {
	expected := Indexes{}
	for c, fields := range source {
		if g[c] == nil {
			continue
		}
		for _, spec := range fields {
			if retiredIndex(c, spec.Field) {
				continue
			}
			if c == "projects" && spec.Field == "path" {
				spec.Unique = false
			}
			expected[c] = append(expected[c], spec)
		}
	}
	for c, fields := range RequiredIndexes {
		if g[c] != nil {
			expected[c] = append(expected[c], fields...)
		}
	}
	for _, c := range keys(g) {
		field := ""
		if _, ok := frozenFields[c]; ok {
			field = "id"
		}
		if c == "plan_tasks" || c == "pipeline_jobs" {
			field = "id"
		}
		if c == "autopilot_runs" {
			field = "run_id"
		}
		if field != "" {
			expected[c] = append(expected[c], IndexSpec{Field: field, Unique: true})
		}
	}
	for c, fields := range expected {
		for _, want := range fields {
			found := false
			for _, got := range actual[c] {
				if want.Field == got.Field && (!want.Unique || got.Unique) {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("schema3: missing index %s/%s unique=%v", c, want.Field, want.Unique)
			}
		}
	}
	return nil
}

func fingerprint(dir string) (string, error) {
	files := map[string]any{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		fi, e := d.Info()
		if e != nil {
			return e
		}
		if d.IsDir() {
			files[rel] = fi.Mode().String()
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("schema3: unsafe path %s", p)
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		files[rel] = []any{fi.Mode().String(), fmt.Sprintf("%x", h.Sum(nil))}
		return nil
	})
	if err != nil {
		return "", err
	}
	return Digest(files), nil
}

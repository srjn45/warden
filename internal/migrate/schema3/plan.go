package schema3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/srjn45/warden/internal/schema"
)

// Plan is read-only, including on refusal. Update recomputes it under exclusive
// ownership; a CLI preview never authorizes applying the preview to later data.
func Plan(ctx context.Context, dataDir string) (*Report, error) {
	l, err := schema.Load(dataDir)
	if err != nil {
		return nil, err
	}
	if l.SchemaVersion != 2 || l.InProgress != nil {
		return nil, fmt.Errorf("schema3: planning requires exact committed schema 2")
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, schema.FileName))
	if err != nil {
		return nil, err
	}
	g, _, err := Read(ctx, filepath.Join(dataDir, "warden-db"))
	if err != nil {
		return &Report{Policy: Policy, Findings: []Finding{{Collection: "warden-db", Field: "raw_reader", Evidence: err.Error(), Disposition: "blocked"}}}, err
	}
	after, readErr := os.ReadFile(filepath.Join(dataDir, schema.FileName))
	if readErr != nil {
		return nil, readErr
	}
	if !bytes.Equal(raw, after) {
		return nil, fmt.Errorf("schema3: ledger changed during planning")
	}
	_, r, err := Map(g)
	r.SourceLedgerHash = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	return r, err
}

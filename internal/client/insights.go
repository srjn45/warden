package client

import (
	"context"
	"time"

	"github.com/srjn45/warden/internal/insights"
	"github.com/srjn45/warden/internal/store"
)

// InsightsParams scopes the data the engine mines (#48). A zero Since lets the
// daemon default its archive look-back; HistoryLimit caps archived records (0 ⇒
// daemon default). MaxFileScans bounds the best-effort per-session digest fetches
// used to recover touched-file sets for the co-edit / parallelization heuristics
// (0 ⇒ defaultMaxFileScans); set it small on big fleets to keep `wd insights`
// snappy.
type InsightsParams struct {
	Since        time.Time
	HistoryLimit int
	MaxFileScans int
}

// defaultMaxFileScans caps how many sessions get a digest round-trip for their
// file set. File-set recovery only feeds two of the report's sections, so a
// bounded best-effort sweep is the right trade against an unbounded fan-out.
const defaultMaxFileScans = 50

// Insights gathers warden's own history through the existing read endpoints —
// live sessions (List), the archive (History), and per-agent metric summaries
// (GetAgentHistory) — recovers a best-effort touched-file set per live session
// (Digest), and runs the deterministic insights engine over the lot. It is the
// single shared aggregator behind both `wd insights` and the MCP insights tool;
// narration (the optional LLM layer) is applied by the CLI on top of this Report.
//
// File-set recovery leans on live sessions: the digest endpoint resolves only
// active records, so archived sessions contribute to the duration / error-rate /
// busy-period analysis but not (today) to co-edit / parallelization. Every digest
// fetch is best-effort — an error or a missing session is skipped, never fatal.
func (c *Client) Insights(ctx context.Context, p InsightsParams) (*insights.Report, error) {
	active, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	archived, err := c.History(ctx, HistoryParams{Since: p.Since, Limit: p.HistoryLimit})
	if err != nil {
		return nil, err
	}
	sinceStr := ""
	if !p.Since.IsZero() {
		sinceStr = p.Since.UTC().Format(time.RFC3339)
	}
	agents, err := c.GetAgentHistory(ctx, sinceStr, "")
	if err != nil {
		return nil, err
	}

	maxScans := p.MaxFileScans
	if maxScans <= 0 {
		maxScans = defaultMaxFileScans
	}

	records := make([]insights.SessionRecord, 0, len(active)+len(archived))
	scans := 0
	for _, s := range active {
		if s.IsTerminal() {
			continue // terminals have no transcript/co-edit/error signal to analyze
		}
		var files []string
		if scans < maxScans {
			files = c.bestEffortFiles(ctx, s.ID)
			scans++
		}
		records = append(records, insights.FromSession(s, files))
	}
	for _, s := range archived {
		if s.IsTerminal() {
			continue
		}
		records = append(records, insights.FromSession(s, nil))
	}

	rep := insights.Analyze(insights.Input{
		Sessions: records,
		Agents:   agents,
		Now:      time.Now(),
	})
	return &rep, nil
}

// bestEffortFiles returns the touched-file paths for one session via its digest,
// or nil on any error. The file set only enriches two report sections, so a
// failure (gone worktree, archived record, transient daemon error) degrades
// silently rather than failing the whole report.
func (c *Client) bestEffortFiles(ctx context.Context, id string) []string {
	d, err := c.Digest(ctx, id)
	if err != nil || d == nil {
		return nil
	}
	files := make([]string, 0, len(d.Files))
	for _, f := range d.Files {
		files = append(files, f.Path)
	}
	return files
}

// compile-time guard: the methods Insights leans on stay on *Client.
var _ insightsSource = (*Client)(nil)

// insightsSource is the read surface Insights composes. It is unexported and used
// only for the compile-time assertion above (mirrors the rotate pattern): if any
// of these signatures drift, the build breaks here rather than at a call site.
type insightsSource interface {
	List(ctx context.Context) ([]*store.Session, error)
	History(ctx context.Context, p HistoryParams) ([]*store.Session, error)
}

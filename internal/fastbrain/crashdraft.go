package fastbrain

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// IssueDraft is a sanitized, local-only GitHub issue draft. Nothing in this
// package submits it anywhere: submission requires explicit user approval and
// is the caller's (CLI/TUI) job.
type IssueDraft struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	Title       string    `json:"title"`
	Environment string    `json:"environment"`
	Stack       string    `json:"stack"` // sanitized excerpt
	AgentID     string    `json:"agent_id,omitempty"`
	ExitCode    int       `json:"exit_code"`
	Signal      string    `json:"signal,omitempty"`
	Command     string    `json:"command,omitempty"`
	Status      string    `json:"status"` // "staged" until the user acts
}

// Body renders the markdown issue body from the sanitized fields.
func (d *IssueDraft) Body() string {
	return fmt.Sprintf("**Environment:** %s\n\n**Exit code:** %d  **Signal:** %s\n\n```\n%s\n```\n",
		d.Environment, d.ExitCode, d.Signal, d.Stack)
}

var (
	rePanicMsg = regexp.MustCompile(`(?m)panic:\s*(?:runtime error:\s*)?(.+)$`)
	reFrame    = regexp.MustCompile(`srjn45/warden/internal/([A-Za-z0-9_]+)[\w/]*\.(?:\([^)]*\)\.)?(\w+)\(`)
	rePkgPath  = regexp.MustCompile(`(?:^|[\s(])internal/([A-Za-z0-9_]+)/`)
)

func draftTitle(excerpt string) string {
	pkg, fn := "warden", ""
	if m := reFrame.FindStringSubmatch(excerpt); m != nil {
		pkg, fn = m[1], m[2]
	} else if m := rePkgPath.FindStringSubmatch(excerpt); m != nil {
		pkg = m[1]
	}
	msg := "unexpected crash"
	if m := rePanicMsg.FindStringSubmatch(excerpt); m != nil {
		msg = strings.TrimSpace(m[1])
		if strings.Contains(msg, "nil pointer dereference") {
			msg = "nil pointer dereference"
		}
		if len(msg) > 80 {
			msg = msg[:80]
		}
	}
	t := fmt.Sprintf("crash(%s): %s", pkg, msg)
	if fn != "" {
		t += " in " + fn
	}
	return t
}

func buildDraft(in CrashInput, excerpt string, now time.Time) *IssueDraft {
	ver := strings.TrimPrefix(in.Version, "v")
	if ver == "" {
		ver = "unknown"
	}
	goos, goarch := in.GOOS, in.GOARCH
	if goos == "" {
		goos = "unknown"
	}
	if goarch == "" {
		goarch = "unknown"
	}
	id := in.AgentID
	if id == "" {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		id = "crash-" + now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
	}
	return &IssueDraft{
		ID: id, CreatedAt: now.UTC(), Title: draftTitle(excerpt),
		Environment: fmt.Sprintf("Warden v%s (%s/%s)", ver, goos, goarch),
		Stack:       excerpt, AgentID: in.AgentID, ExitCode: in.ExitCode,
		Signal: Sanitize(in.Signal), Command: Sanitize(in.Command), Status: "staged",
	}
}

// DefaultCrashDir is ~/.warden/crashes.
func DefaultCrashDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".warden", "crashes"), nil
}

var reSafeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func draftPath(dir, id string) (string, error) {
	if !reSafeID.MatchString(id) || strings.Contains(id, "..") {
		return "", fmt.Errorf("fastbrain: invalid crash id %q", id)
	}
	return filepath.Join(dir, id+".json"), nil
}

// StageCrashDraft writes d to <dir>/<id>.json (dir 0700, file 0600) and returns
// the path. Text fields are re-sanitized defensively. Local only.
func StageCrashDraft(dir string, d *IssueDraft) (string, error) {
	if d == nil {
		return "", fmt.Errorf("%w: nil draft", ErrInvalidRequest)
	}
	p, err := draftPath(dir, d.ID)
	if err != nil {
		return "", err
	}
	c := *d
	c.Title, c.Stack, c.Command, c.Signal = Sanitize(c.Title), Sanitize(c.Stack), Sanitize(c.Command), Sanitize(c.Signal)
	data, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	tmp, err := os.CreateTemp(dir, ".crash-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return p, os.Rename(tmp.Name(), p)
}

// ErrCrashNotFound is returned by LoadCrashDraft for an unknown id.
var ErrCrashNotFound = errors.New("fastbrain: crash draft not found")

// LoadCrashDraft reads a staged draft by id.
func LoadCrashDraft(dir, id string) (*IssueDraft, error) {
	p, err := draftPath(dir, id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrCrashNotFound
	}
	if err != nil {
		return nil, err
	}
	var d IssueDraft
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListCrashDrafts returns staged drafts, newest first. A missing dir is empty.
func ListCrashDrafts(dir string) ([]*IssueDraft, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*IssueDraft
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if d, err := LoadCrashDraft(dir, strings.TrimSuffix(e.Name(), ".json")); err == nil {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

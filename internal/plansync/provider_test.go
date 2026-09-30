package plansync

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planexport"
	"github.com/srjn45/warden/internal/planstore"
)

func samplePlan() *planstore.Plan {
	return &planstore.Plan{
		ID:          "plan-cafebabe",
		ProjectID:   "proj-1",
		Name:        "ship-feature",
		Goal:        "land the feature",
		Status:      planstore.PlanStatusInProgress,
		Revision:    3,
		ContentHash: "sha256:abc",
		Branches:    []string{"feat/ship"},
		BranchSummaries: []planstore.BranchSummary{
			{
				Name: "feat/ship",
				PR:   &planstore.PullRequestSummary{URL: "https://example.com/pr/9", Number: 9},
			},
		},
	}
}

func sampleEnvelope(t *testing.T) Envelope {
	t.Helper()
	env, err := EnvelopeFromPlan(samplePlan(), EnvelopeOptions{
		Scope: Scope{
			OrganizationID: "org-1",
			TeamID:         "team-1",
			ProjectID:      "proj-1",
		},
		Visibility: VisibilityTeam,
		OwnerID:    "user-1",
		Origin: ChangeOrigin{
			Kind:    OriginLocalAgent,
			ActorID: "agent-1",
			NodeID:  "node-1",
		},
	})
	require.NoError(t, err)
	return env
}

func TestEnvelopeFromPlan_carriesRequiredHubFields(t *testing.T) {
	env := sampleEnvelope(t)
	require.Equal(t, SchemaVersion, env.SchemaVersion)
	require.Equal(t, "org-1", env.Scope.OrganizationID)
	require.Equal(t, "team-1", env.Scope.TeamID)
	require.Equal(t, "proj-1", env.Scope.ProjectID)
	require.Equal(t, "proj-1", env.ProjectID)
	require.Equal(t, "plan-cafebabe", env.PlanID)
	require.Equal(t, int64(3), env.Revision)
	require.Equal(t, "sha256:abc", env.ContentHash)
	require.Equal(t, VisibilityTeam, env.Visibility)
	require.Equal(t, "user-1", env.OwnerID)
	require.Equal(t, planstore.PlanStatusInProgress, env.Lifecycle)
	require.Equal(t, ConflictToken(3, "sha256:abc"), env.ConflictToken)
	require.Equal(t, OriginLocalAgent, env.Origin.Kind)
	require.Equal(t, "agent-1", env.Origin.ActorID)

	kinds := map[string]int{}
	for _, a := range env.Artifacts {
		kinds[a.Kind]++
	}
	require.GreaterOrEqual(t, kinds["branch"], 1)
	require.GreaterOrEqual(t, kinds["pull_request"], 1)
}

func TestEnvelopeFromPlan_nilPlan(t *testing.T) {
	_, err := EnvelopeFromPlan(nil, EnvelopeOptions{})
	require.Error(t, err)
}

func TestLocalProvider_contract(t *testing.T) {
	runProviderContract(t, Local())
}

func TestFakeProvider_contract(t *testing.T) {
	runProviderContract(t, NewFake())
}

// runProviderContract exercises the shared PlanSyncProvider surface. Local
// returns empty pulls; Fake retains pushes — both must accept a valid envelope
// and reject a malformed one without panicking.
func runProviderContract(t *testing.T, p PlanSyncProvider) {
	t.Helper()
	ctx := context.Background()
	require.NotEmpty(t, p.Name())

	env := sampleEnvelope(t)
	require.NoError(t, p.Push(ctx, env))

	bad := env
	bad.PlanID = ""
	require.Error(t, p.Push(ctx, bad))

	got, err := p.Pull(ctx, PullQuery{Scope: env.Scope, PlanID: env.PlanID})
	require.NoError(t, err)
	if p.Enabled() {
		require.Len(t, got, 1)
		require.Equal(t, env.PlanID, got[0].PlanID)
		require.Equal(t, env.ConflictToken, got[0].ConflictToken)
	} else {
		require.Empty(t, got)
	}

	disc, err := p.Discover(ctx, env.Scope, nil)
	require.NoError(t, err)
	if p.Enabled() {
		require.NotEmpty(t, disc)
		for _, d := range disc {
			require.True(t, d.Lifecycle == planstore.PlanStatusPending || d.Lifecycle == planstore.PlanStatusInProgress)
		}
	} else {
		require.Empty(t, disc)
	}
}

func TestFakeProvider_conflictAndDiscover(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	env := sampleEnvelope(t)
	require.NoError(t, f.Push(ctx, env))
	require.NoError(t, f.Push(ctx, env), "idempotent same token")

	newer := env
	newer.Revision = 4
	newer.ContentHash = "sha256:def"
	newer.ConflictToken = ConflictToken(newer.Revision, newer.ContentHash)
	err := f.Push(ctx, newer)
	require.Error(t, err)
	var cerr *ConflictError
	require.True(t, errors.As(err, &cerr))
	require.Equal(t, env.ConflictToken, cerr.Expected)
	require.ErrorIs(t, err, ErrConflict)

	require.NoError(t, f.Replace(ctx, newer))
	require.Equal(t, 1, f.Len())

	// Completed plans are excluded from default Discover.
	done := newer
	done.Lifecycle = planstore.PlanStatusCompleted
	done.PlanID = "plan-done0001"
	done.ConflictToken = ConflictToken(1, "sha256:done")
	done.Revision = 1
	done.ContentHash = "sha256:done"
	require.NoError(t, f.Push(ctx, done))

	disc, err := f.Discover(ctx, Scope{ProjectID: "proj-1"}, nil)
	require.NoError(t, err)
	require.Len(t, disc, 1)
	require.Equal(t, newer.PlanID, disc[0].PlanID)

	// Scoped org filter excludes mismatched org.
	other, err := f.Discover(ctx, Scope{OrganizationID: "org-other", ProjectID: "proj-1"}, nil)
	require.NoError(t, err)
	require.Empty(t, other)
}

func TestDefault_isLocalAndDisabled(t *testing.T) {
	p := Default()
	require.Equal(t, ProviderLocal, p.Name())
	require.False(t, p.Enabled())
	_, ok := p.(*LocalProvider)
	require.True(t, ok)
}

// TestDefaultInstallationMakesNoNetworkCalls proves the default provider never
// dials: (1) Local/noop sources import no net packages, (2) exercising every
// PlanSyncProvider method with http.DefaultTransport replaced by a tripwire
// that fails the test on any RoundTrip.
func TestDefaultInstallationMakesNoNetworkCalls(t *testing.T) {
	assertNoNetImports(t, "noop.go", "provider.go", "envelope.go", "errors.go")

	var dials atomic.Int64
	oldTransport := http.DefaultTransport
	oldClient := http.DefaultClient
	trip := &tripwireTransport{dials: &dials}
	http.DefaultTransport = trip
	http.DefaultClient = &http.Client{Transport: trip}
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
		http.DefaultClient = oldClient
	})

	// Also fail if anything dials via the default resolver path used by net.Dial.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			_ = c.Close()
			t.Error("unexpected Accept on tripwire listener — default provider dialed the network")
		}
	}()

	ctx := context.Background()
	p := Default()
	env := sampleEnvelope(t)
	require.NoError(t, p.Push(ctx, env))
	_, err = p.Pull(ctx, PullQuery{Scope: env.Scope})
	require.NoError(t, err)
	_, err = p.Discover(ctx, env.Scope, []planstore.PlanStatus{planstore.PlanStatusPending})
	require.NoError(t, err)

	// Give a racing dial a moment; Local is synchronous so this is belt-and-suspenders.
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int64(0), dials.Load(), "default PlanSyncProvider must not dial")
}

type tripwireTransport struct {
	dials *atomic.Int64
}

func (t *tripwireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.dials.Add(1)
	return nil, errors.New("plansync test: unexpected HTTP RoundTrip to " + req.URL.String())
}

func assertNoNetImports(t *testing.T, files ...string) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(thisFile)
	forbidden := map[string]bool{
		"net": true, "net/http": true, "net/url": true, "crypto/tls": true,
	}
	fset := token.NewFileSet()
	for _, name := range files {
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		require.NoError(t, err, path)
		for _, imp := range f.Imports {
			impPath := imp.Path.Value
			// strip quotes
			impPath = impPath[1 : len(impPath)-1]
			require.False(t, forbidden[impPath], "%s must not import %s (default install is offline)", name, impPath)
		}
	}
}

// TestRepoExportIsSeparateFromHubSync ensures planexport.Syncer does not
// satisfy PlanSyncProvider — repo export and Hub sync stay distinct providers.
func TestRepoExportIsSeparateFromHubSync(t *testing.T) {
	var s any = &planexport.Syncer{}
	_, ok := s.(PlanSyncProvider)
	require.False(t, ok, "planexport.Syncer must not implement PlanSyncProvider")

	var _ PlanSyncProvider = Local()
	var _ PlanSyncProvider = NewFake()
	var _ PlanSyncProvider = Default()
}

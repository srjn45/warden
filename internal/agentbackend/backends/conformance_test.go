// Backend conformance harness.
//
// Each backend adapter detects an agent's coarse run state (idle / working /
// needs-input) by scanning the captured tmux pane for marker substrings its CLI
// prints — the "esc to interrupt" streaming hint, an approval box header, a
// trust prompt, and so on. Those markers are the ONE thing that silently drifts
// when the underlying CLI ships a new TUI: the unit tests exercise the parsing
// logic against hand-written strings, but nothing pins the parser to a REAL,
// versioned capture of what the tool actually prints. When a marker moves,
// warden mis-reads the agent (a working agent looks idle, an approval never
// surfaces) and nothing fails until someone notices in a live session.
//
// This harness closes that gap with a single data-driven regression net: every
// entry in conformanceCases names a real pane capture on disk
// (testdata/<backend>/<scenario>.txt), the CLI version it was captured against,
// and the neutral State warden must infer from it. TestBackendConformance loads
// each fixture THROUGH THE REGISTRY (agentbackend.Get, the exact path core uses)
// and asserts DetectState / ParseApproval still classify it correctly. If a
// backend's marker constants change without a matching fixture refresh, this
// fails loudly instead of drifting silently in production.
//
// # Fixture directory convention
//
// Raw pane captures live at:
//
//	testdata/<backend-id>/<scenario>.txt
//
// where <scenario> is one of the canonical names below (add more as needed):
//
//	state-idle.txt              agent at rest, composer ready
//	state-idle-after-turn.txt   at rest immediately after a completed turn
//	state-working.txt           mid-turn, streaming a response
//	approval*.txt               an open approval / permission prompt
//	trust-prompt.txt            a first-run "trust this directory" prompt
//
// A fixture is a verbatim `tmux capture-pane -p` dump — copy the pane exactly,
// including box-drawing and trailing footer lines, so the capture matches what
// DetectState sees at runtime.
//
// # Versioning (retaining an old capture across a CLI update)
//
// The CapturedWith field records the CLI version a fixture was taken against —
// metadata for the human refreshing it, not asserted. When a CLI update MOVES a
// marker, refresh the scenario file in place and bump CapturedWith. If you want
// to keep the OLD version's capture as an additional regression fixture (proving
// the parser still handles both the old and new TUI), copy it aside with a
// version suffix and add a second manifest row pointing at it:
//
//	testdata/codex/state-working.txt          → CapturedWith "0.145.0" (current)
//	testdata/codex/state-working@0.142.3.txt  → CapturedWith "0.142.3" (retained)
//
// The harness loads each fixture by its explicit relative path, so flat
// scenario names and version-suffixed names both work with no code change.
//
// # Adding a backend / scenario
//
// Capture the pane, drop it under testdata/<backend>/, and append one
// conformanceCase row. That is the whole workflow — see testdata/README.md.
package backends

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// conformanceCase pins one real pane capture to the neutral state warden must
// infer from it. It is loaded through the registry so it exercises the same
// Backend contract core does.
type conformanceCase struct {
	backend      string             // registered backend id (agentbackend.Get)
	fixture      string             // path under testdata/<backend>/, relative to that dir
	capturedWith string             // CLI version the capture was taken against (metadata)
	wantState    agentbackend.State // state DetectState must return for this pane
	wantApproval bool               // ParseApproval must find a prompt (true for approval/trust panes)
}

// conformanceCases is the manifest: the versioned pane fixtures every backend's
// state detector is regression-locked against. Backends whose DetectState is
// always Unknown (crush / goose / opencode infer state from their transcript,
// not the pane) carry no pane fixtures here.
//
// Coverage today: codex, cursor, antigravity, aider. The remaining pane-driven
// backend (claude — its DetectState tests still use inline literals) is a
// follow-up: capture testdata/claude/{state-working,approval,state-idle}.txt and
// add rows below.
var conformanceCases = []conformanceCase{
	// --- codex (has no positive idle marker; idle is inferred from staleness) ---
	{backend: "codex", fixture: "state-working.txt", capturedWith: "0.142.3", wantState: agentbackend.StateWorking},
	{backend: "codex", fixture: "approval-command.txt", capturedWith: "0.142.3", wantState: agentbackend.StateNeedsInput, wantApproval: true},
	{backend: "codex", fixture: "state-idle.txt", capturedWith: "0.142.3", wantState: agentbackend.StateUnknown},

	// --- cursor (cursor-agent; positive idle marker on the composer) ---
	{backend: "cursor", fixture: "state-working.txt", capturedWith: "cursor-agent 2026.06", wantState: agentbackend.StateWorking},
	{backend: "cursor", fixture: "approval.txt", capturedWith: "cursor-agent 2026.06", wantState: agentbackend.StateNeedsInput, wantApproval: true},
	{backend: "cursor", fixture: "trust-prompt.txt", capturedWith: "cursor-agent 2026.06", wantState: agentbackend.StateNeedsInput, wantApproval: true},
	{backend: "cursor", fixture: "state-idle.txt", capturedWith: "cursor-agent 2026.06", wantState: agentbackend.StateIdle},
	{backend: "cursor", fixture: "state-idle-after-turn.txt", capturedWith: "cursor-agent 2026.06", wantState: agentbackend.StateIdle},

	// --- antigravity (agy; positive idle + working markers in the footer) ---
	{backend: "antigravity", fixture: "state-idle.txt", capturedWith: "agy 0.1", wantState: agentbackend.StateIdle},
	{backend: "antigravity", fixture: "state-working.txt", capturedWith: "agy 0.1", wantState: agentbackend.StateWorking},
	{backend: "antigravity", fixture: "approval.txt", capturedWith: "agy 0.1", wantState: agentbackend.StateNeedsInput, wantApproval: true},

	// --- aider (needs-input on an open y/n prompt; idle inferred from staleness) ---
	{backend: "aider", fixture: "approval-gitignore.txt", capturedWith: "aider 0.86", wantState: agentbackend.StateNeedsInput, wantApproval: true},
	{backend: "aider", fixture: "approval-addfile.txt", capturedWith: "aider 0.86", wantState: agentbackend.StateNeedsInput, wantApproval: true},
	{backend: "aider", fixture: "pane-idle.txt", capturedWith: "aider 0.86", wantState: agentbackend.StateUnknown},
}

// TestBackendConformance is the regression net: for every manifest row it loads
// the real captured pane and asserts the backend (resolved through the registry,
// exactly as core resolves it) still infers the expected neutral state, and that
// approval panes still parse into a prompt. A drifted marker constant fails here.
func TestBackendConformance(t *testing.T) {
	for _, tc := range conformanceCases {
		t.Run(tc.backend+"/"+tc.fixture, func(t *testing.T) {
			b, err := agentbackend.Get(tc.backend)
			require.NoErrorf(t, err, "backend %q is not registered", tc.backend)

			pane := loadPaneFixture(t, tc.backend, tc.fixture)

			got := b.DetectState(pane)
			require.Equalf(t, tc.wantState, got,
				"%s DetectState drifted on %s (captured with %s): the CLI's %s marker likely moved — recapture the pane and update the fixture (see testdata/README.md)",
				tc.backend, tc.fixture, tc.capturedWith, tc.wantState)

			if tc.wantApproval {
				_, ok := b.ParseApproval(pane)
				require.Truef(t, ok,
					"%s ParseApproval no longer recognizes %s (captured with %s): the approval-prompt markers likely moved — recapture and update the fixture",
					tc.backend, tc.fixture, tc.capturedWith)
			}
		})
	}
}

// TestBackendConformanceManifestSane guards the harness itself: every referenced
// fixture must exist on disk (a typo'd path would otherwise fail as a confusing
// "state drifted" rather than a missing file), and a needs-input row must assert
// its approval so the two signals can never silently disagree.
func TestBackendConformanceManifestSane(t *testing.T) {
	require.NotEmpty(t, conformanceCases, "the conformance manifest must not be empty")

	for _, tc := range conformanceCases {
		path := filepath.Join("testdata", tc.backend, tc.fixture)
		_, err := os.Stat(path)
		require.NoErrorf(t, err, "fixture %s referenced by the manifest is missing", path)

		if tc.wantApproval {
			require.Equalf(t, agentbackend.StateNeedsInput, tc.wantState,
				"an approval fixture (%s/%s) must also expect StateNeedsInput", tc.backend, tc.fixture)
		}
	}
}

// loadPaneFixture reads a captured pane under testdata/<backend>/<fixture>.
func loadPaneFixture(t *testing.T, backend, fixture string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", backend, fixture))
	require.NoError(t, err)
	return string(b)
}

package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/role"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// resolveRole fills unset spawn fields from the role's defaults, with precedence
// explicit request value > role default > global default.
func TestResolveRolePrecedence(t *testing.T) {
	// worker: default type=development fills an unset type; the canonical role
	// name is persisted.
	req := SpawnRequest{Role: "worker"}
	r, err := resolveRole(&req)
	require.NoError(t, err)
	require.Equal(t, "worker", r.Name)
	require.Equal(t, "worker", req.Role)
	require.Equal(t, store.TypeDevelopment, req.Type)

	// An explicit type wins over the role default.
	req = SpawnRequest{Role: "worker", Type: store.TypeSpike}
	_, err = resolveRole(&req)
	require.NoError(t, err)
	require.Equal(t, store.TypeSpike, req.Type)

	// orchestrator: default permission_mode=auto fills unset…
	req = SpawnRequest{Role: "orchestrator"}
	_, err = resolveRole(&req)
	require.NoError(t, err)
	require.Equal(t, "auto", req.PermissionMode)
	// …but an explicit permission mode wins.
	req = SpawnRequest{Role: "orchestrator", PermissionMode: "acceptEdits"}
	_, err = resolveRole(&req)
	require.NoError(t, err)
	require.Equal(t, "acceptEdits", req.PermissionMode)

	// brain: both permission_mode=auto and auto_approve=true are applied.
	req = SpawnRequest{Role: "brain"}
	_, err = resolveRole(&req)
	require.NoError(t, err)
	require.Equal(t, "auto", req.PermissionMode)
	require.True(t, req.AutoApprove)

	// auto_approve is OR-ed: an explicit true survives a role with no auto_approve
	// default.
	req = SpawnRequest{Role: "worker", AutoApprove: true}
	_, err = resolveRole(&req)
	require.NoError(t, err)
	require.True(t, req.AutoApprove)
}

// A role's default tags are UNIONED onto the request's tags, never replacing
// them. None of the five built-in roles ship default tags, so for them the union
// is a passthrough: the request's own tags survive resolution untouched (Spawn
// normalizes them afterward via store.NormalizeTags).
func TestResolveRoleTagsPreserved(t *testing.T) {
	req := SpawnRequest{Role: "worker", Tags: []string{"frontend", "urgent"}}
	_, err := resolveRole(&req)
	require.NoError(t, err)
	require.Equal(t, []string{"frontend", "urgent"}, req.Tags)
	require.Equal(t, store.TypeDevelopment, req.Type)
}

// unionTags is the exact tag-union step resolveRole applies when a role DOES ship
// default tags: role tags are appended to the request's and de-duplicated
// (case-insensitively, first-seen order) via the store's normalizer. This locks
// the union semantics in even though no built-in role currently ships tags.
func TestRoleTagsUnionSemantics(t *testing.T) {
	reqTags := []string{"Backend", "urgent"}
	roleTags := []string{"review", "backend"} // "backend" collides case-insensitively
	got := store.NormalizeTags(append(append([]string{}, reqTags...), roleTags...))
	require.Equal(t, []string{"backend", "urgent", "review"}, got)
}

// The empty role resolves to general: it is persisted as "" (byte-identical to a
// pre-roles agent), applies no default flags, and injects no persona.
func TestResolveRoleEmptyIsGeneral(t *testing.T) {
	req := SpawnRequest{}
	r, err := resolveRole(&req)
	require.NoError(t, err)
	require.Equal(t, role.Default, r.Name)
	require.Equal(t, "", req.Role, "general is persisted as empty so the field stays JSON-omitted")
	require.Equal(t, store.Type(""), req.Type, "general applies no type default → stays free-form")
	require.Equal(t, "", req.PermissionMode)
	require.False(t, req.AutoApprove)
	require.Equal(t, "", personaGuidance(req.Role))

	// An explicit "general" is equivalent to empty.
	req = SpawnRequest{Role: "general"}
	_, err = resolveRole(&req)
	require.NoError(t, err)
	require.Equal(t, "", req.Role)
}

func TestResolveRoleUnknownErrors(t *testing.T) {
	req := SpawnRequest{Role: "does-not-exist"}
	_, err := resolveRole(&req)
	require.Error(t, err)
}

// End-to-end: a typed spawn under a role persists the role name and file-backs the
// persona (ahead of the collab/git/pipeline hints) via one --append-system-prompt,
// keeping it off the tmux launch line.
func TestSpawnRolePersistsAndInjectsPersona(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr, &FakeConfig{})
	lc.HintsDir = "/state/hints"
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-360", Repo: "/repo", Role: "worker",
	})
	require.NoError(t, err)
	require.Equal(t, "worker", s.Role, "the resolved role name is persisted on the session")

	persona := personaGuidance("worker")
	require.NotEmpty(t, persona)

	// The persona is written into the per-agent hints file FIRST (before the
	// collab/git/pipeline guidance).
	hintFile := "/state/hints/" + s.ID
	wantText := persona + "\n\n" + pipelineHintGuidance + "\n\n" + collabHintGuidance + "\n\n" + gitConventionsGuidance
	require.Contains(t, fr.calledArgs(),
		[]string{"sh", "-c", `umask 077; printf '%s' "$1" > "$2"`, "sh", wantText, hintFile})

	// The launch line references the file, not the inline persona text.
	launch := claudeLaunch(s.ClaudeSessionID, s.ID, "", "auto") +
		` --append-system-prompt "$(cat ` + shellQuoteArg(hintFile) + `)"`
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, launch, "Enter"})
	// The persona never rides the tmux launch line itself.
	require.NotContains(t, launch, "You are a worker")
}

// The general (empty) role injects no persona: the launch line stays byte-identical
// to a pre-roles spawn (only the collab/git/pipeline hints ride it).
func TestSpawnGeneralRoleInjectsNoPersona(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr, &FakeConfig{})
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-361", Repo: "/repo",
	})
	require.NoError(t, err)
	require.Equal(t, "", s.Role)

	want := claudeLaunch(s.ClaudeSessionID, s.ID, "", "auto") + pipelineHint() + collabHint() + gitConventionsHint()
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, want, "Enter"})
	for _, a := range fr.calledArgs() {
		if len(a) >= 2 && a[0] == "tmux" && a[1] == "send-keys" {
			require.False(t, strings.Contains(a[4], "You are"), "no persona on a general-role launch line")
		}
	}
}

// A role default type flips a would-be free-form spawn (no explicit type) into a
// typed, worktree-backed one.
func TestSpawnRoleDefaultTypeFlipsFreeForm(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr, &FakeConfig{})
	lc.PromptsDir = "/state/prompts"
	// No Type given, but worker defaults type=development, so this becomes a
	// managed worktree spawn rather than a free-form one.
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Repo: "/repo", Cwd: "/repo", Role: "worker", Prompt: "do the thing",
	})
	require.NoError(t, err)
	require.Equal(t, store.TypeDevelopment, s.Type)
	require.NotEmpty(t, s.Worktree, "role default type=development creates a worktree")
	require.Equal(t, "worker", s.Role)
}

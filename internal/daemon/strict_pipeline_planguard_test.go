package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
)

// planOwnedPipeline runs a plan in pipeline mode (no agents are spawned: the
// fake lifecycle is used) and returns the plan id (== pipeline id).
func planOwnedPipeline(t *testing.T) (srv *Server, plans *planstore.Store, pips *pipeline.Store, id string) {
	t.Helper()
	_, srv, plans, pips, _, id = newPipelineRestartPlan(t)
	return
}

func guardCode(err error) (int, string) {
	var ae apiError
	if errors.As(err, &ae) {
		return ae.code, ae.msg
	}
	return 0, ""
}

func directVerb(srv *Server, verb, pid string) error {
	ctx := context.Background()
	var err error
	switch verb {
	case "pause":
		_, err = srv.PausePipeline(ctx, oapi.PausePipelineRequestObject{Pid: pid})
	case "resume":
		_, err = srv.ResumePipeline(ctx, oapi.ResumePipelineRequestObject{Pid: pid})
	case "cancel":
		_, err = srv.CancelPipeline(ctx, oapi.CancelPipelineRequestObject{Pid: pid})
	case "delete":
		_, err = srv.DeletePipeline(ctx, oapi.DeletePipelineRequestObject{Pid: pid})
	}
	return err
}

func TestPlanOwnedPipelineRefusesDirectControl(t *testing.T) {
	want := map[string]string{
		"pause":  "wd plan pause ",
		"resume": "wd plan resume ",
		"cancel": "wd plan stop ",
		"delete": "wd plan stop ",
	}
	for verb, hint := range want {
		t.Run(verb, func(t *testing.T) {
			srv, _, pips, id := planOwnedPipeline(t)
			err := directVerb(srv, verb, id)
			code, msg := guardCode(err)
			require.Equal(t, http.StatusConflict, code, "err=%v", err)
			require.Contains(t, msg, "is run by plan "+id)
			require.Contains(t, msg, hint+id)
			if verb == "delete" {
				require.Contains(t, msg, "wd plan archive "+id)
			}
			// nothing changed
			p, gerr := pips.Get(id)
			require.NoError(t, gerr)
			require.NotEqual(t, pipeline.StatusCanceled, p.Status)
		})
	}
}

// Documents today's (guarded) behaviour: the plan stays in_progress after the
// refused verbs, and the plan's own control path still works end to end.
func TestPlanControlStillWorksOnOwnedPipeline(t *testing.T) {
	srv, plans, pips, id := planOwnedPipeline(t)
	ctx := context.Background()
	ctl := func(action string) {
		t.Helper()
		resp, err := srv.ControlPlan(ctx, oapi.ControlPlanRequestObject{PlanId: id, Action: action})
		require.NoError(t, err)
		_, ok := resp.(oapi.ControlPlan200JSONResponse)
		require.True(t, ok, "%s: %#v", action, resp)
	}
	ctl("pause")
	p, _ := pips.Get(id)
	require.Equal(t, pipeline.StatusPaused, p.Status)
	ctl("resume")
	p, _ = pips.Get(id)
	require.NotEqual(t, pipeline.StatusPaused, p.Status)
	ctl("stop")
	p, _ = pips.Get(id)
	require.Equal(t, pipeline.StatusCanceled, p.Status)
	pl, err := plans.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, pl.Status) // stop leaves plan to archive
}

func TestBackReferencedPipelineStaysControllable(t *testing.T) {
	for _, verb := range []string{"pause", "resume", "cancel", "delete"} {
		t.Run(verb, func(t *testing.T) {
			srv, _, pips, id := planOwnedPipeline(t)
			ref := &pipeline.Pipeline{
				ID: "user-pl", Name: "user-pl", Repo: t.TempDir(), PlanID: id, Status: pipeline.StatusPending,
				Jobs: []pipeline.Job{{ID: "x", Prompt: "p", Worktree: "fresh", Type: "development", Status: pipeline.JobPending}},
			}
			require.NoError(t, pips.Create(ref))
			switch verb {
			case "pause":
				require.NoError(t, pips.Update("user-pl", func(p *pipeline.Pipeline) { p.Status = pipeline.StatusRunning }))
			case "resume":
				require.NoError(t, pips.Update("user-pl", func(p *pipeline.Pipeline) { p.Status = pipeline.StatusPaused }))
			}
			err := directVerb(srv, verb, "user-pl")
			code, msg := guardCode(err)
			require.NotEqual(t, http.StatusConflict, code, msg)
			require.NoError(t, err)
		})
	}
}

func TestUnlinkedPipelineStaysControllable(t *testing.T) {
	srv, _, pips, _ := planOwnedPipeline(t)
	require.NoError(t, pips.Create(&pipeline.Pipeline{
		ID: "solo", Name: "solo", Repo: t.TempDir(), Status: pipeline.StatusPending,
		Jobs: []pipeline.Job{{ID: "x", Prompt: "p", Worktree: "fresh", Type: "development", Status: pipeline.JobPending}},
	}))
	require.NoError(t, directVerb(srv, "cancel", "solo"))
	require.NoError(t, directVerb(srv, "delete", "solo"))
}

func TestOrphanedPlanPipelineCanBeCleanedUp(t *testing.T) {
	for _, how := range []string{"deleted", "archived"} {
		t.Run(how, func(t *testing.T) {
			srv, plans, pips, id := planOwnedPipeline(t)
			ctx := context.Background()
			if how == "deleted" {
				require.NoError(t, plans.Delete(ctx, id))
			} else {
				require.NoError(t, plans.Update(ctx, id, func(p *planstore.Plan) error {
					p.Status = planstore.PlanStatusArchived
					return nil
				}))
			}
			require.NoError(t, directVerb(srv, "cancel", id))
			require.NoError(t, directVerb(srv, "delete", id))
			_, err := pips.Get(id)
			require.ErrorIs(t, err, pipeline.ErrNotFound)
		})
	}
}

func TestPlanOwnedGuardOverHTTPKeepsJobVerbs(t *testing.T) {
	ts, _, _, _, _, id := newPipelineRestartPlan(t)
	resp := postJSON(t, ts+"/api/v1/pipelines/"+id+"/pause", map[string]any{})
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.True(t, strings.Contains(string(b), "wd plan pause "+id), string(b))

	resp2, err := http.Get(ts + "/api/v1/pipelines/" + id)
	require.NoError(t, err)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
}

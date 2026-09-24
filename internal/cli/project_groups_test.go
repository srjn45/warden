package cli

import (
	"strings"
	"testing"
)

func TestProjectGroupsListCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/project-groups": `{"groups":[{"id":"grp-1","name":"Core","project_ids":["/repos/a","/repos/b"]}]}`,
	}, nil, nil))

	out, err := runCLI(t, addr, "project-groups", "list")
	if err != nil {
		t.Fatalf("project-groups list: %v", err)
	}
	for _, want := range []string{"ID", "NAME", "MEMBERS", "grp-1", "Core", "/repos/a, /repos/b"} {
		if !strings.Contains(out, want) {
			t.Fatalf("project-groups list missing %q: %q", want, out)
		}
	}

	jsonOut, err := runCLI(t, addr, "project-groups", "list", "--json")
	if err != nil {
		t.Fatalf("project-groups list --json: %v", err)
	}
	if !strings.Contains(jsonOut, `"grp-1"`) || !strings.Contains(jsonOut, `"Core"`) {
		t.Fatalf("project-groups list --json unexpected: %q", jsonOut)
	}
}

func TestProjectGroupsShowCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/project-groups/grp-1": `{"id":"grp-1","name":"Core","project_ids":["/repos/a","/repos/b"]}`,
	}, nil, nil))

	out, err := runCLI(t, addr, "project-groups", "show", "grp-1")
	if err != nil {
		t.Fatalf("project-groups show: %v", err)
	}
	for _, want := range []string{"ID:      grp-1", "Name:    Core", "Members (2):", "  - /repos/a", "  - /repos/b"} {
		if !strings.Contains(out, want) {
			t.Fatalf("project-groups show missing %q: %q", want, out)
		}
	}

	jsonOut, err := runCLI(t, addr, "project-groups", "show", "grp-1", "--json")
	if err != nil {
		t.Fatalf("project-groups show --json: %v", err)
	}
	if !strings.Contains(jsonOut, `"grp-1"`) || !strings.Contains(jsonOut, `"Core"`) {
		t.Fatalf("project-groups show --json unexpected: %q", jsonOut)
	}
}

func TestProjectGroupsCreateCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/project-groups": `{"id":"grp-new","name":"Infra","project_ids":["/repos/ops"]}`,
	}, nil, body))

	out, err := runCLI(t, addr, "project-groups", "create", "Infra", "--project", "/repos/ops")
	if err != nil {
		t.Fatalf("project-groups create: %v", err)
	}
	if !strings.Contains(out, "created project group grp-new (Infra)") {
		t.Fatalf("project-groups create output unexpected: %q", out)
	}
	if !strings.Contains(body["/api/v1/project-groups"], `"Infra"`) || !strings.Contains(body["/api/v1/project-groups"], `"/repos/ops"`) {
		t.Fatalf("body missing fields: %q", body["/api/v1/project-groups"])
	}
}

func TestProjectGroupsUpdateCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PUT /api/v1/project-groups/grp-1": `{"id":"grp-1","name":"Core v2","project_ids":["/repos/x"]}`,
	}, nil, body))

	out, err := runCLI(t, addr, "project-groups", "update", "grp-1", "--name", "Core v2", "--project", "/repos/x")
	if err != nil {
		t.Fatalf("project-groups update: %v", err)
	}
	if !strings.Contains(out, "updated project group grp-1 (Core v2)") {
		t.Fatalf("project-groups update output unexpected: %q", out)
	}
	if !strings.Contains(body["/api/v1/project-groups/grp-1"], `"Core v2"`) || !strings.Contains(body["/api/v1/project-groups/grp-1"], `"/repos/x"`) {
		t.Fatalf("body missing fields: %q", body["/api/v1/project-groups/grp-1"])
	}
}

func TestProjectGroupsDeleteCmd(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"DELETE /api/v1/project-groups/grp-1": `{"status":"deleted"}`,
	}, method, nil))

	out, err := runCLI(t, addr, "project-groups", "delete", "grp-1")
	if err != nil {
		t.Fatalf("project-groups delete: %v", err)
	}
	if !strings.Contains(out, "deleted project group grp-1") {
		t.Fatalf("project-groups delete output unexpected: %q", out)
	}
	if method["/api/v1/project-groups/grp-1"] != "DELETE" {
		t.Fatalf("method expected DELETE, got %q", method["/api/v1/project-groups/grp-1"])
	}
}

func TestProjectGroupsMembersAddCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/project-groups/grp-1/members": `{"id":"grp-1","name":"Core","project_ids":["/repos/a","/repos/b"]}`,
	}, nil, body))

	out, err := runCLI(t, addr, "project-groups", "members", "add", "grp-1", "/repos/b")
	if err != nil {
		t.Fatalf("project-groups members add: %v", err)
	}
	if !strings.Contains(out, "added /repos/b to group Core (members: 2)") {
		t.Fatalf("project-groups members add output unexpected: %q", out)
	}
	if !strings.Contains(body["/api/v1/project-groups/grp-1/members"], `"/repos/b"`) {
		t.Fatalf("body missing project_id: %q", body["/api/v1/project-groups/grp-1/members"])
	}
}

func TestProjectGroupsMembersRemoveCmd(t *testing.T) {
	method := map[string]string{}
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"DELETE /api/v1/project-groups/grp-1/members": `{"id":"grp-1","name":"Core","project_ids":["/repos/a"]}`,
	}, method, body))

	out, err := runCLI(t, addr, "project-groups", "members", "remove", "grp-1", "/repos/b")
	if err != nil {
		t.Fatalf("project-groups members remove: %v", err)
	}
	if !strings.Contains(out, "removed /repos/b from group Core (members: 1)") {
		t.Fatalf("project-groups members remove output unexpected: %q", out)
	}
	if method["/api/v1/project-groups/grp-1/members"] != "DELETE" {
		t.Fatalf("method expected DELETE, got %q", method["/api/v1/project-groups/grp-1/members"])
	}
	if !strings.Contains(body["/api/v1/project-groups/grp-1/members"], `"/repos/b"`) {
		t.Fatalf("body missing project_id: %q", body["/api/v1/project-groups/grp-1/members"])
	}
}

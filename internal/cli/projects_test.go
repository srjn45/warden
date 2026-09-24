package cli

import (
	"strings"
	"testing"
)

func TestProjectsListCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/projects": `{"projects":[{"id":"/repos/alpha","name":"Alpha","path":"/repos/alpha","status":"open"}]}`,
	}, nil, nil))

	out, err := runCLI(t, addr, "projects", "list")
	if err != nil {
		t.Fatalf("projects list: %v", err)
	}
	for _, want := range []string{"ID", "NAME", "STATUS", "PATH", "/repos/alpha", "Alpha", "open"} {
		if !strings.Contains(out, want) {
			t.Fatalf("projects list missing %q: %q", want, out)
		}
	}

	jsonOut, err := runCLI(t, addr, "projects", "list", "--json")
	if err != nil {
		t.Fatalf("projects list --json: %v", err)
	}
	if !strings.Contains(jsonOut, `"/repos/alpha"`) || !strings.Contains(jsonOut, `"Alpha"`) {
		t.Fatalf("projects list --json output unexpected: %q", jsonOut)
	}
}

func TestProjectsOpenCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/open": `{"id":"/repos/alpha","name":"Alpha","path":"/repos/alpha","status":"open"}`,
	}, nil, body))

	out, err := runCLI(t, addr, "projects", "open", "/repos/alpha", "--name", "Alpha")
	if err != nil {
		t.Fatalf("projects open: %v", err)
	}
	if !strings.Contains(out, "opened project /repos/alpha (open)") {
		t.Fatalf("projects open output unexpected: %q", out)
	}
	if !strings.Contains(body["/api/v1/projects/open"], `"/repos/alpha"`) {
		t.Fatalf("body missing id: %q", body["/api/v1/projects/open"])
	}
}

func TestProjectsOpenLocalCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/local": `{"id":"/repos/local","name":"local","path":"/repos/local","status":"open"}`,
	}, nil, body))

	out, err := runCLI(t, addr, "projects", "open-local", "/repos/local")
	if err != nil {
		t.Fatalf("projects open-local: %v", err)
	}
	if !strings.Contains(out, "opened local project /repos/local (open)") {
		t.Fatalf("projects open-local output unexpected: %q", out)
	}
	if !strings.Contains(body["/api/v1/projects/local"], `"/repos/local"`) {
		t.Fatalf("body missing path: %q", body["/api/v1/projects/local"])
	}
}

func TestProjectsOpenRemoteCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/remote": `{"id":"https://github.com/org/repo","name":"repo","path":"/work/repo","status":"open"}`,
	}, nil, body))

	out, err := runCLI(t, addr, "projects", "open-remote", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("projects open-remote: %v", err)
	}
	if !strings.Contains(out, "opened remote project https://github.com/org/repo (open)") {
		t.Fatalf("projects open-remote output unexpected: %q", out)
	}
	if !strings.Contains(body["/api/v1/projects/remote"], `"https://github.com/org/repo"`) {
		t.Fatalf("body missing url: %q", body["/api/v1/projects/remote"])
	}
}

func TestProjectsNewCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/new": `{"id":"/work/brand-new","name":"brand-new","path":"/work/brand-new","status":"open"}`,
	}, nil, body))

	out, err := runCLI(t, addr, "projects", "new", "brand-new")
	if err != nil {
		t.Fatalf("projects new: %v", err)
	}
	if !strings.Contains(out, "created project /work/brand-new (open)") {
		t.Fatalf("projects new output unexpected: %q", out)
	}
	if !strings.Contains(body["/api/v1/projects/new"], `"brand-new"`) {
		t.Fatalf("body missing name: %q", body["/api/v1/projects/new"])
	}
}

func TestProjectsCloseCmd(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/alpha/close": `{"id":"alpha","name":"Alpha","path":"/repos/alpha","status":"closed"}`,
	}, method, nil))

	out, err := runCLI(t, addr, "projects", "close", "alpha")
	if err != nil {
		t.Fatalf("projects close: %v", err)
	}
	if !strings.Contains(out, "closed project alpha (closed)") {
		t.Fatalf("projects close output unexpected: %q", out)
	}
}

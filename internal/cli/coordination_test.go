package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCoordinationNamespaceCanonicalAndCompatibilityPaths(t *testing.T) {
	root := newRootCmd()
	pairs := map[string]string{
		"context set": "ctx set", "context cas": "ctx cas", "context append": "ctx append",
		"context get": "ctx get", "context list": "ctx list", "context delete": "ctx del",
		"message send": "msg send", "message inbox": "msg inbox", "message wait": "msg wait",
		"approval list": "approvals", "approval answer": "approve",
		"approval auto set": "auto-approve", "approval auto rules": "auto-approve rules",
		"approval auto allow": "auto-approve allow", "approval auto deny": "auto-approve deny",
		"approval auto clear": "auto-approve clear", "approval auto enable": "auto-approve enable",
		"approval auto disable": "auto-approve disable",
	}
	for canonical, legacy := range pairs {
		canonicalCmd := findExactCommand(t, root, canonical)
		legacyCmd := findExactCommand(t, root, legacy)
		if !legacyCmd.Hidden {
			t.Errorf("legacy %q hidden=false, want compatibility wrapper", legacy)
		}
		if got := legacyCmd.Annotations[AnnotationAliasKind]; got != AliasCompatibility {
			t.Errorf("legacy %q alias kind=%q, want %q", legacy, got, AliasCompatibility)
		}
		if got, want := legacyCmd.Annotations[AnnotationCanonicalPath], "warden "+canonical; got != want {
			t.Errorf("legacy %q canonical=%q, want %q", legacy, got, want)
		}
		if got, want := commandFlagSignature(canonicalCmd), commandFlagSignature(legacyCmd); !reflect.DeepEqual(got, want) {
			t.Errorf("%s flags differ from %s: %v != %v", canonical, legacy, got, want)
		}
	}
}

func TestCoordinationCanonicalAliasDispatch(t *testing.T) {
	ctxRoutes := map[string]string{
		"PUT /api/v1/context/global.k": `{"key":"global.k","value":"v"}`,
		"GET /api/v1/context/global.k": `{"key":"global.k","value":"v"}`,
	}
	for _, args := range [][]string{
		{"context", "set", "global.k", "v"}, {"ctx", "set", "global.k", "v"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			method := map[string]string{}
			addr := stubDaemon(t, routedDaemon(t, ctxRoutes, method, nil))
			out, err := runCLI(t, addr, args...)
			if err != nil {
				t.Fatal(err)
			}
			if method["/api/v1/context/global.k"] != http.MethodPut || !strings.Contains(out, "set global.k") {
				t.Fatalf("dispatch/output mismatch: method=%v out=%q", method, out)
			}
		})
	}

	msgRoutes := map[string]string{
		"POST /api/v1/sessions/B-2/messages": `{"message":{"id":"9","from":"human","to":"B-2","body":"hi"},"woke":false}`,
	}
	for _, args := range [][]string{
		{"message", "send", "B-2", "hi"}, {"msg", "send", "B-2", "hi"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			method := map[string]string{}
			addr := stubDaemon(t, routedDaemon(t, msgRoutes, method, nil))
			out, err := runCLI(t, addr, args...)
			if err != nil {
				t.Fatal(err)
			}
			if method["/api/v1/sessions/B-2/messages"] != http.MethodPost || !strings.Contains(out, "sent to B-2 (id 9)") {
				t.Fatalf("dispatch/output mismatch: method=%v out=%q", method, out)
			}
		})
	}

	approvalRoutes := map[string]string{
		"GET /api/v1/approvals": `{"enabled":true,"approvals":[{"id":"A-1","question":"Run rm?","options":["Yes","No"],"fingerprint":"ff","recognized":true}]}`,
	}
	for _, args := range [][]string{{"approval", "list"}, {"approvals"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			addr := stubDaemon(t, routedDaemon(t, approvalRoutes, map[string]string{}, nil))
			out, err := runCLI(t, addr, args...)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "warden approve A-1") {
				t.Fatalf("approval list output changed: %q", out)
			}
		})
	}
}

func TestCoordinationApprovalAnswerPreservesOptionValidation(t *testing.T) {
	routes := map[string]string{
		"GET /api/v1/approvals": `{"enabled":true,"approvals":[{"id":"A-1","options":["Yes","No"],"fingerprint":"ff","recognized":true}]}`,
	}
	for _, args := range [][]string{{"approval", "answer", "A-1", "zero"}, {"approve", "A-1", "zero"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			addr := stubDaemon(t, routedDaemon(t, routes, map[string]string{}, nil))
			_, err := runCLI(t, addr, args...)
			if err == nil || !strings.Contains(err.Error(), "positive integer") {
				t.Fatalf("option validation changed: %v", err)
			}
		})
	}
}

func TestCoordinationAutoApproveCanonicalAndAlias(t *testing.T) {
	body := map[string]string{}
	routes := map[string]string{
		"PATCH /api/v1/sessions/A-1/auto-approve": `{"enabled":true}`,
	}
	for _, args := range [][]string{
		{"approval", "auto", "set", "A-1", "on"}, {"auto-approve", "A-1", "on"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			method := map[string]string{}
			addr := stubDaemon(t, routedDaemon(t, routes, method, body))
			out, err := runCLI(t, addr, args...)
			if err != nil {
				t.Fatal(err)
			}
			if method["/api/v1/sessions/A-1/auto-approve"] != http.MethodPatch || !strings.Contains(out, "auto-approve enabled for A-1") {
				t.Fatalf("dispatch/output mismatch: method=%v out=%q", method, out)
			}
		})
	}
}

func TestCoordinationProgressiveHelp(t *testing.T) {
	namespace, err := executeHelp(t, "help", "context")
	if err != nil {
		t.Fatal(err)
	}
	deleteLeaf, err := executeHelp(t, "help", "context", "delete")
	if err != nil {
		t.Fatal(err)
	}
	message, err := executeHelp(t, "help", "message")
	if err != nil {
		t.Fatal(err)
	}
	approval, err := executeHelp(t, "help", "approval")
	if err != nil {
		t.Fatal(err)
	}
	auto, err := executeHelp(t, "help", "approval", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(namespace, "namespaced key/value store") || strings.Contains(namespace, "context delete") {
		t.Fatalf("context namespace help is not focused: %s", namespace)
	}
	if !strings.Contains(deleteLeaf, "Delete a context key") || strings.Contains(deleteLeaf, "context set") {
		t.Fatalf("context delete help is not focused: %s", deleteLeaf)
	}
	if !strings.Contains(message, "directed messages") || strings.Contains(message, "message send") {
		t.Fatalf("message namespace help is not focused: %s", message)
	}
	if !strings.Contains(approval, "option number") || strings.Contains(approval, "approval auto allow") {
		t.Fatalf("approval namespace help is not focused: %s", approval)
	}
	if !strings.Contains(auto, "allow/deny rule engine") || strings.Contains(auto, "approval auto set") {
		t.Fatalf("approval auto help is not focused: %s", auto)
	}
	for name, got := range map[string]string{
		"context": namespace, "context_delete": deleteLeaf, "message": message,
		"approval": approval, "approval_auto": auto,
	} {
		want, err := os.ReadFile(filepath.Join("testdata", "help_"+name+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Fatalf("%s golden mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
		}
	}
}

func TestCoordinationFactoriesAreFreshAndErrorsMatch(t *testing.T) {
	a, b := newContextCmd(), newContextCmd()
	if a == b || a.Commands()[0] == b.Commands()[0] {
		t.Fatal("context factory reused Cobra command pointers")
	}
	setA := findExactCommand(t, wrapRoot(a), "context set")
	setB := findExactCommand(t, wrapRoot(b), "context set")
	if err := setA.Flags().Set("as", "changed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := setB.Flags().GetString("as"); got != "" {
		t.Fatalf("context factories share flag storage: %q", got)
	}
	_, canonicalErr := runCLI(t, "", "approval", "answer", "A-1", "zero")
	_, aliasErr := runCLI(t, "", "approve", "A-1", "zero")
	if canonicalErr == nil || aliasErr == nil || canonicalErr.Error() != aliasErr.Error() {
		t.Fatalf("canonical/alias errors differ: %v != %v", canonicalErr, aliasErr)
	}
}

func TestMessageSendDistinctFromAgentSend(t *testing.T) {
	root := newRootCmd()
	agentSend := findExactCommand(t, root, "agent send")
	msgSend := findExactCommand(t, root, "message send")
	if got, want := agentSend.Annotations[AnnotationCanonicalPath], "warden agent send"; got != want {
		t.Fatalf("agent send canonical=%q, want %q", got, want)
	}
	if got, want := msgSend.Annotations[AnnotationCanonicalPath], "warden message send"; got != want {
		t.Fatalf("message send canonical=%q, want %q", got, want)
	}
}

func TestCoordinationContextDeleteAlias(t *testing.T) {
	method := map[string]string{}
	routes := map[string]string{"DELETE /api/v1/context/k": `{}`}
	addr := stubDaemon(t, routedDaemon(t, routes, method, nil))
	for _, args := range [][]string{{"context", "delete", "k"}, {"ctx", "del", "k"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			out, err := runCLI(t, addr, args...)
			if err != nil {
				t.Fatal(err)
			}
			if method["/api/v1/context/k"] != http.MethodDelete || !strings.Contains(out, "deleted k") {
				t.Fatalf("delete dispatch/output mismatch: method=%v out=%q", method, out)
			}
		})
	}
}

func TestCoordinationHelpRootShowsCanonicalNamespaces(t *testing.T) {
	out, err := executeHelp(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	outline := rootHelpOutline(out)
	for _, want := range []string{"context", "message", "approval"} {
		if !strings.Contains(outline, want) {
			t.Fatalf("root help missing %q:\n%s", want, outline)
		}
	}
	for _, hidden := range []string{"ctx", "msg", "approvals", "approve", "auto-approve"} {
		if strings.Contains(outline, hidden+"\n") || strings.HasSuffix(outline, hidden) {
			t.Fatalf("root help leaked legacy command %q:\n%s", hidden, outline)
		}
	}
	want, err := os.ReadFile(filepath.Join("testdata", "help_root.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if outline != string(want) {
		t.Fatalf("help_root golden mismatch\n--- want ---\n%s\n--- got ---\n%s", want, outline)
	}
}

func TestCoordinationMsgInboxRequiresIdentity(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	for _, args := range [][]string{{"message", "inbox"}, {"msg", "inbox"}} {
		if _, err := runCLI(t, "", args...); err == nil {
			t.Fatalf("%v: expected identity error", args)
		}
	}
}

func TestWriteCoordinationGoldens(t *testing.T) {
	if os.Getenv("WRITE_GOLDENS") != "1" {
		t.Skip("set WRITE_GOLDENS=1 to regenerate goldens")
	}
	cases := map[string][]string{
		"context":        {"help", "context"},
		"context_delete": {"help", "context", "delete"},
		"message":        {"help", "message"},
		"approval":       {"help", "approval"},
		"approval_auto":  {"help", "approval", "auto"},
	}
	for name, args := range cases {
		got, err := executeHelp(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "help_"+name+".golden")
		if err := os.WriteFile(path, []byte(got), 0644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := executeHelp(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "help_root.golden"), []byte(rootHelpOutline(root)), 0644); err != nil {
		t.Fatal(err)
	}
}

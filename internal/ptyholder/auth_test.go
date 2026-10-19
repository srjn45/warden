package ptyholder

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

func launched(t *testing.T, name string) (root string, h *Handle, shellPID int) {
	t.Helper()
	root, _ = tmpDirs(t)
	l := testLauncher(root)
	ad, err := l.Launch(context.Background(), name, []string{"/bin/sh"}, t.TempDir(), os.Environ(), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	pid := ad.PID
	_ = ad.Master.Close()
	ad.Abort()
	h, err = Open(root, name)
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(metaPath(root, name))
	t.Cleanup(func() {
		_ = os.WriteFile(metaPath(root, name), orig, 0o600) // tests tamper with the record; teardown needs the real identity

		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = h.Release(context.Background())
	})
	return root, h, pid
}

// F-AUTH-01: the credential is required; a refusal is journaled (audit file)
// without the credential or any terminal bytes.
func TestF_AUTH_01_CredentialRequiredAndRefusalAudited(t *testing.T) {
	root, h, _ := launched(t, "a-cred")
	bad := &Handle{Root: root, ID: "a-cred", cred: strings.Repeat("0", 64)}
	if _, err := bad.Adopt(context.Background(), true); !errors.Is(err, ErrAuth) {
		t.Fatalf("adopt with a wrong credential: %v, want ErrAuth", err)
	}
	empty := &Handle{Root: root, ID: "a-cred"}
	if _, err := empty.Adopt(context.Background(), true); !errors.Is(err, ErrAuth) {
		t.Fatalf("adopt with no credential: %v, want ErrAuth", err)
	}
	log := Audit(root, "a-cred")
	if strings.Count(log, "refused bad credential") != 2 {
		t.Fatalf("audit does not record both refusals:\n%s", log)
	}
	if strings.Contains(log, h.cred) || strings.Contains(log, strings.Repeat("0", 16)) {
		t.Fatalf("audit leaks a credential:\n%s", log)
	}
	ad, err := h.Adopt(context.Background(), true)
	if err != nil {
		t.Fatalf("the right credential must work: %v", err)
	}
	_ = ad.Master.Close()
	ad.Abort()
}

// F-AUTH-02: a recycled pid is never adopted — neither a holder nor a child
// whose (pid, start time) differs from what was recorded.
func TestF_AUTH_02_RecycledPidNotAdopted(t *testing.T) {
	for _, field := range []string{"holder_start", "child_start", "holder_pid", "child_pid"} {
		t.Run(field, func(t *testing.T) {
			root, h, _ := launched(t, "a-pid-"+field)
			p := metaPath(root, "a-pid-"+field)
			b, _ := os.ReadFile(p)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			m[field] = m[field].(float64) + 1 // as if the pid now belonged to a different process
			nb, _ := json.Marshal(m)
			if err := os.WriteFile(p, nb, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Adopt(context.Background(), true); !errors.Is(err, ErrIdentity) {
				t.Fatalf("adopt with tampered %s: %v, want ErrIdentity", field, err)
			}
		})
	}
}

// F-AUTH-03: the socket and state are private to this uid; widened modes or a
// foreign owner refuse adoption.
func TestF_AUTH_03_SocketPrivateAndOwnerChecked(t *testing.T) {
	root, h, _ := launched(t, "a-sock")
	m, _ := h.Meta()
	fi, err := os.Stat(m.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600", fi.Mode().Perm())
	}
	if di, _ := os.Stat(root); di.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode %v, want 0700", di.Mode().Perm())
	}
	if ci, _ := os.Stat(credPath(root, "a-sock")); ci.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode %v, want 0600", ci.Mode().Perm())
	}
	if err := os.Chmod(m.Socket, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Adopt(context.Background(), true); !errors.Is(err, ErrInsecure) {
		t.Fatalf("adopt through a world-writable socket: %v, want ErrInsecure", err)
	}
	_ = os.Chmod(m.Socket, 0o600)
	if err := os.Chmod(credPath(root, "a-sock"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, "a-sock"); !errors.Is(err, ErrInsecure) {
		t.Fatalf("open with a readable credential file: %v, want ErrInsecure", err)
	}
	if os.Getuid() != 0 {
		if err := checkOwned("/etc/hostname", 0o644); !errors.Is(err, ErrInsecure) {
			t.Fatalf("a root-owned file must not count as ours: %v", err)
		}
	}
}

// A foreign process squatting on the socket path (right path, wrong process)
// is refused even though it would happily send a descriptor.
func TestForeignListenerOnSocketPathRefused(t *testing.T) {
	root, h, _ := launched(t, "a-squat")
	m, _ := h.Meta()
	// Replace the holder's meta so the recorded holder pid is not the listener.
	m.HolderPID = os.Getpid()
	b, _ := json.Marshal(m)
	_ = os.WriteFile(metaPath(root, "a-squat"), b, 0o600)
	if _, err := h.Adopt(context.Background(), true); !errors.Is(err, ErrIdentity) {
		t.Fatalf("listener that is not the recorded holder: %v, want ErrIdentity", err)
	}
}

// F-ROLL-02 groundwork: a protocol mismatch is refused with a version error on
// both sides and the process is untouched.
func TestProtocolMismatchRefused(t *testing.T) {
	root, h, pid := launched(t, "a-proto")
	p := metaPath(root, "a-proto")
	b, _ := os.ReadFile(p)
	var m Meta
	_ = json.Unmarshal(b, &m)
	m.Proto = ProtoVersion + 1
	nb, _ := json.Marshal(m)
	_ = os.WriteFile(p, nb, 0o600)
	if _, err := h.Adopt(context.Background(), true); !errors.Is(err, ErrProto) {
		t.Fatalf("future holder protocol: %v, want ErrProto", err)
	}
	if !alive(pid) {
		t.Fatal("a refused adoption must not touch the process")
	}
}

// F-ROLL-02: old-binary/new-broker and new-binary/old-broker protocol handshake:
// refuse with version error (ErrProto), never misparse, and leave the process untouched.
func TestF_ROLL_02_ProtocolHandshakeRefusalOnVersionMismatch(t *testing.T) {
	// Case 1: New binary client adopting an old-broker session (meta Proto older than ProtoVersion).
	root, h, pid := launched(t, "roll02-old-broker")
	p := metaPath(root, "roll02-old-broker")
	b, _ := os.ReadFile(p)
	var m Meta
	_ = json.Unmarshal(b, &m)
	m.Proto = ProtoVersion - 1
	nb, _ := json.Marshal(m)
	_ = os.WriteFile(p, nb, 0o600)
	if _, err := h.Adopt(context.Background(), true); !errors.Is(err, ErrProto) {
		t.Fatalf("old broker meta protocol %d: got %v, want ErrProto", m.Proto, err)
	}
	if !alive(pid) {
		t.Fatal("process must stay alive after old-broker adoption refusal")
	}

	// Case 2: New binary client adopting a future-broker session (meta Proto newer than ProtoVersion).
	m.Proto = ProtoVersion + 1
	nb, _ = json.Marshal(m)
	_ = os.WriteFile(p, nb, 0o600)
	if _, err := h.Adopt(context.Background(), true); !errors.Is(err, ErrProto) {
		t.Fatalf("future broker meta protocol %d: got %v, want ErrProto", m.Proto, err)
	}
	if !alive(pid) {
		t.Fatal("process must stay alive after future-broker adoption refusal")
	}

	// Restore metadata so teardown works
	_ = os.WriteFile(p, b, 0o600)

	// Case 3: Old binary client (speaks ProtoVersion - 1) dials new broker directly.
	// Broker checks req.Proto and refuses with version error; audits refusal; child stays alive.
	root2, h2, pid2 := launched(t, "roll02-old-client")
	m2, err := h2.meta()
	if err != nil {
		t.Fatal(err)
	}
	c, err := h2.dial(context.Background(), m2)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Send an adopt request stamped with older ProtoVersion - 1
	oldReq, _ := json.Marshal(request{Op: "adopt", Proto: ProtoVersion - 1, ID: h2.ID, Cred: h2.cred})
	oldReq = append(oldReq, '\n')
	if _, err := c.Write(oldReq); err != nil {
		t.Fatal(err)
	}
	r, files, err := readReply(c)
	for _, f := range files {
		_ = f.Close()
	}
	if err != nil {
		t.Fatalf("readReply: %v", err)
	}
	if err := classify(r); !errors.Is(err, ErrProto) {
		t.Fatalf("old binary client request: got %v, want ErrProto", err)
	}
	if !alive(pid2) {
		t.Fatal("process must stay alive after old-client rejection")
	}

	// Audit file should record the protocol refusal
	audit, err := os.ReadFile(auditPath(root2, "roll02-old-client"))
	if err != nil || !strings.Contains(string(audit), "protocol") {
		t.Fatalf("audit file must record protocol mismatch refusal: %q", string(audit))
	}
}

// Durable data boundary: a secret passed to the shell through its environment
// is never written beside the holder state (the spec file is removed once
// read; the credential file holds only the credential), and the audit log
// holds no credential or terminal byte.
func TestHolderStateNeverHoldsEnvironmentSecrets(t *testing.T) {
	root, _ := tmpDirs(t)
	l := testLauncher(root)
	const secret = "ENV-SECRET-VALUE-9f3a1c"
	ad, err := l.Launch(context.Background(), "p1", []string{"/bin/sh"}, t.TempDir(), append(os.Environ(), "WARDEN_TEST_SECRET="+secret), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	pid := ad.PID
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); l.Release(context.Background(), "p1") })
	_ = ad.Master.Close()
	ad.Abort()
	bad := &Handle{Root: root, ID: "p1", cred: "wrong"}
	_, _ = bad.Adopt(context.Background(), true) // leaves an audit line
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Type()&os.ModeSocket != 0 {
			continue
		}
		b, _ := os.ReadFile(root + "/" + e.Name())
		if strings.Contains(string(b), secret) {
			t.Fatalf("%s holds the environment secret", e.Name())
		}
		if strings.HasSuffix(e.Name(), ".spec") {
			t.Fatalf("spec file %s was left on disk", e.Name())
		}
		if strings.HasSuffix(e.Name(), ".audit") && strings.Contains(string(b), "wrong") && !strings.Contains(string(b), "refused bad credential") {
			t.Fatalf("audit line is not the fixed refusal text: %s", b)
		}
	}
}

// The holder ignores SIGHUP/SIGINT/SIGPIPE for itself but must not pass that on:
// an ignored signal stays ignored across exec, which would make the hosted
// program immune to Ctrl-C.
func TestHostedProgramKeepsDefaultSignalDispositions(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("needs /proc")
	}
	root, _ := tmpDirs(t)
	l := testLauncher(root)
	ad, err := l.Launch(context.Background(), "sig1", []string{"/bin/sh", "-c", "grep SigIgn /proc/self/status"}, t.TempDir(), os.Environ(), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Abort()
	var out strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(out.String(), "SigIgn") {
		n, err := ad.Master.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}
	var mask uint64
	for _, f := range strings.Fields(out.String()) {
		if len(f) == 16 {
			for _, c := range f {
				mask = mask<<4 | uint64(strings.IndexRune("0123456789abcdef", c))
			}
			break
		}
	}
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGPIPE} {
		if mask&(1<<(sig-1)) != 0 {
			t.Fatalf("%v is ignored in the hosted program (SigIgn=%016x)", sig, mask)
		}
	}
}

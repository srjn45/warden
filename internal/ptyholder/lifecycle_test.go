package ptyholder

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestCleanupDoesNotSignalChildFromTamperedMetadata(t *testing.T) {
	root, _ := tmpDirs(t)
	l := testLauncher(root)
	ad, err := l.Launch(context.Background(), "cleanup-identity", []string{"/bin/sh"}, "", nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_ = ad.Master.Close()
	ad.Abort()
	h, err := Open(root, "cleanup-identity")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := h.Meta()
	if !ok {
		t.Fatal("missing metadata")
	}
	// This process is unrelated to the holder's child. The old PID sweep would
	// trust it; authenticated release instead asks the actual holder to clean up.
	foreign := exec.Command("sleep", "30")
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Process.Kill(); _ = foreign.Wait() })
	m.ChildPID = foreign.Process.Pid
	m.ChildStart, _ = StartTime(m.ChildPID)
	b, _ := json.Marshal(m)
	if err := os.WriteFile(metaPath(root, h.ID), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := KillAll(root); err != nil {
		t.Fatal(err)
	}
	if st, err := StartTime(m.ChildPID); err != nil || st != m.ChildStart {
		t.Fatal("cleanup signalled an unrelated process")
	}
}

func TestOwnerExitReapsFixtureAndRetainsProductionDefault(t *testing.T) {
	root, _ := tmpDirs(t)
	owner := exec.Command("sleep", "30")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	start, err := StartTime(owner.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	l := testLauncher(root)
	l.TestOwner = &Owner{PID: owner.Process.Pid, Start: start}
	ad, err := l.Launch(context.Background(), "owned", []string{"/bin/sh"}, "", nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Master.Close()
	defer ad.Abort()
	h, err := Open(root, "owned")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := h.Meta()
	if !ok || m.TestOwner == nil || *m.TestOwner != *l.TestOwner {
		t.Fatalf("missing owner marker: %+v", m)
	}
	_ = owner.Process.Kill()
	_ = owner.Wait()
	eventually(t, "test owner cleanup", func() bool {
		_, err := os.Stat(metaPath(root, "owned"))
		_, childErr := StartTime(ad.PID)
		return errors.Is(err, os.ErrNotExist) && childErr != nil
	})
	// The production zero value must never acquire a test-owner lifetime.
	if (&Launcher{}).TestOwner != nil {
		t.Fatal("production launcher acquired an owner")
	}
}

func TestRecycledOwnerRejectedBeforeStartingHolder(t *testing.T) {
	root, _ := tmpDirs(t)
	l := testLauncher(root)
	l.TestOwner.Start++
	before, _ := Resources()
	if _, err := l.Launch(context.Background(), "reused", []string{"/bin/sh"}, "", nil, 80, 24); err == nil {
		t.Fatal("accepted recycled owner identity")
	}
	after, _ := Resources()
	if after != before || Exists(root, "reused") {
		t.Fatal("rejected owner started a holder")
	}
}

func TestReadinessCancellationReapsChildAndClosesPipe(t *testing.T) {
	root, _ := tmpDirs(t)
	exe, _ := os.Executable()
	eventually(t, "prior holders to be reaped", func() bool { n, _ := Resources(); return n == 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Launch(ctx, LaunchSpec{Root: root, ID: "blocked", Command: []string{exe, "__blocked_ready"}, Argv: []string{"/bin/sh"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("launch: %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("startup cancellation was not bounded")
	}
	eventually(t, "holder reap", func() bool { n, _ := Resources(); return n == 0 })
	if Exists(root, "blocked") {
		t.Fatal("failed launch left adoptable state")
	}
}

func TestReadinessDescriptorIsNotInheritedByCommand(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("requires Linux descriptor inspection")
	}
	root, _ := tmpDirs(t)
	ad, err := testLauncher(root).Launch(context.Background(), "ready-fd", []string{"/bin/sh", "-c", "test ! -e /proc/self/fd/3"}, "", nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Master.Close()
	defer ad.Abort()
	if status := ad.Wait(); status.Lost || status.Code != 0 || status.Signal != "" {
		t.Fatalf("ready descriptor reached child: %+v", status)
	}
}

func TestCancellationAfterCommandStartReapsOwnedLeader(t *testing.T) {
	root, _ := tmpDirs(t)
	exe, _ := os.Executable()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Launch(ctx, LaunchSpec{Root: root, ID: "silent", Command: []string{exe, "__silent_holder"}, Argv: []string{"/bin/sh"}})
		done <- err
	}()
	var m Meta
	eventually(t, "command started before readiness acknowledgement", func() bool {
		b, err := os.ReadFile(metaPath(root, "silent"))
		return err == nil && json.Unmarshal(b, &m) == nil && m.ChildStart != 0
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("launch: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("readiness cancellation did not finish")
	}
	eventually(t, "cancelled startup child to be reaped", func() bool { _, err := StartTime(m.ChildPID); return err != nil })
}

func TestUnresolvedHolderReleasePreventsReplacement(t *testing.T) {
	root, _ := tmpDirs(t)
	l := testLauncher(root)
	ad, err := l.Launch(context.Background(), "unresolved", []string{"/bin/sh"}, "", nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Master.Close()
	defer ad.Abort()
	credential, err := os.ReadFile(credPath(root, "unresolved"))
	if err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(credPath(root, "unresolved"), credential, 0o600)
	if err := os.WriteFile(credPath(root, "unresolved"), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = l.Launch(context.Background(), "unresolved", []string{"/bin/sh"}, "", nil, 80, 24)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("replacement launch: %v", err)
	}
	if st, err := StartTime(ad.PID); err != nil || st == 0 {
		t.Fatal("failed release killed the existing child")
	}
	current, err := os.ReadFile(credPath(root, "unresolved"))
	if err != nil || string(current) != "wrong" {
		t.Fatal("failed replacement overwrote unresolved ownership evidence")
	}
}

// A holder that died (e.g. SIGKILLed with its child) leaves an unreachable
// record. Once both recorded incarnations are verified gone it must not block
// an explicit restore/replacement launch under the same name.
func TestDeadHolderRecordDoesNotBlockReplacement(t *testing.T) {
	root, _ := tmpDirs(t)
	l := testLauncher(root)
	ad, err := l.Launch(context.Background(), "dead-holder", []string{"/bin/sh"}, "", nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_ = ad.Master.Close()
	ad.Abort()
	h, err := Open(root, "dead-holder")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := h.Meta()
	if !ok {
		t.Fatal("missing metadata")
	}
	_ = syscall.Kill(-m.ChildPID, syscall.SIGKILL)
	_ = syscall.Kill(m.HolderPID, syscall.SIGKILL)
	eventually(t, "holder and child to exit", func() bool {
		_, he := StartTime(m.HolderPID)
		_, ce := StartTime(m.ChildPID)
		return he != nil && ce != nil
	})
	ad2, err := l.Launch(context.Background(), "dead-holder", []string{"/bin/sh"}, "", nil, 80, 24)
	if err != nil {
		t.Fatalf("replacement launch after verified holder death: %v", err)
	}
	defer ad2.Master.Close()
	defer ad2.Abort()
	if ad2.PID == m.ChildPID {
		t.Fatal("replacement reused the dead child identity")
	}
}

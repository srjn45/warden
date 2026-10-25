package ptyruntime

import (
	"os"
	"testing"
	"time"
)

func TestNormalExitReleasesMasterAndWorkersWithoutClose(t *testing.T) {
	before := Resources()
	r := startFake(t, Options{})
	r.waitOutput("ready")
	r.human("exit 0")
	r.waitDone()
	for end := time.Now().Add(time.Second); ; {
		after := Resources()
		if after.Masters == before.Masters && after.Goroutines == before.Goroutines && after.Children == before.Children {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("normal exit leaked resources: before=%+v after=%+v", before, after)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCloseBoundsLostReaperAndAccountsResources(t *testing.T) {
	j, err := OpenJournal(t.TempDir(), "lost-wait", Retention{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()
	abort := make(chan struct{})
	before := Resources()
	s, err := Start(Config{Journal: j, Options: Options{KillGrace: 20 * time.Millisecond}, Adopt: &Adoption{
		Master: rd, PID: os.Getpid(), Alive: func() bool { return false },
		Wait:  func() ExitStatus { <-abort; return ExitStatus{Lost: true} },
		Abort: func() { close(abort) },
	}})
	if err != nil {
		rd.Close()
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Close(); err == nil {
		t.Fatal("lost reaper cleanup succeeded without reporting timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Close exceeded deadline")
	}
	if s.Close() == nil {
		t.Fatal("idempotent Close lost cleanup error")
	}
	for end := time.Now().Add(time.Second); ; {
		after := Resources()
		if after.Masters == before.Masters && after.Goroutines == before.Goroutines && after.Children == before.Children {
			if after.CleanupTimeouts != before.CleanupTimeouts+1 {
				t.Fatal("timeout was not counted exactly once")
			}
			break
		}
		if time.Now().After(end) {
			t.Fatalf("cleanup leaked resources: before=%+v after=%+v", before, after)
		}
		time.Sleep(time.Millisecond)
	}
}

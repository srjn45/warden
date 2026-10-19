package ptyhost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/terminalcontract"
)

func countOpenFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

func readProcessRSSKiB() int {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	rssPages, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	pageSizeKiB := os.Getpagesize() / 1024
	return rssPages * pageSizeKiB
}

// F-RES-01: SoakSessions idle sessions: RSS/FD/goroutine per session within budget;
// no leak after teardown (-race).
func TestF_RES_01_SoakSessionsResourceLimitsAndNoLeaks(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeOn)
	defer m.Close()

	ctx := context.Background()
	th := m.TerminalHost()

	// Capture baseline resources
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	baseFDs := countOpenFDs()
	baseRSS := readProcessRSSKiB()

	numSessions := terminalcontract.Default.SoakSessions
	require.Equal(t, 50, numSessions)

	// Launch 50 idle direct terminal sessions
	for i := 0; i < numSessions; i++ {
		name := fmt.Sprintf("soak-term-%02d", i)
		require.NoError(t, th.NewSession(ctx, name, t.TempDir()))
	}

	// Verify all 50 sessions exist and are active
	for i := 0; i < numSessions; i++ {
		name := fmt.Sprintf("soak-term-%02d", i)
		require.True(t, th.HasSession(ctx, name))
		require.NotNil(t, m.Direct(name))
	}

	// Measure steady state per session
	currentGoroutines := runtime.NumGoroutine()
	addedGoroutines := currentGoroutines - baseGoroutines
	perSessionGoroutines := 0
	if addedGoroutines > 0 {
		perSessionGoroutines = addedGoroutines / numSessions
	}
	require.LessOrEqual(t, perSessionGoroutines, terminalcontract.Default.PerSessionGoroutines,
		"goroutines per session must be within budget (<= %d, got %d)",
		terminalcontract.Default.PerSessionGoroutines, perSessionGoroutines)

	if baseFDs > 0 {
		currentFDs := countOpenFDs()
		addedFDs := currentFDs - baseFDs
		perSessionFDs := 0
		if addedFDs > 0 {
			perSessionFDs = addedFDs / numSessions
		}
		require.LessOrEqual(t, perSessionFDs, terminalcontract.Default.PerSessionFDs,
			"FDs per session must be within budget (<= %d, got %d)",
			terminalcontract.Default.PerSessionFDs, perSessionFDs)
	}

	if baseRSS > 0 {
		currentRSS := readProcessRSSKiB()
		addedRSS := currentRSS - baseRSS
		perSessionRSS := 0
		if addedRSS > 0 {
			perSessionRSS = addedRSS / numSessions
		}
		require.LessOrEqual(t, perSessionRSS, terminalcontract.Default.PerSessionRSSKiB,
			"RSS KiB per session must be within budget (<= %d KiB, got %d KiB)",
			terminalcontract.Default.PerSessionRSSKiB, perSessionRSS)
	}

	// Teardown all 50 sessions
	for i := 0; i < numSessions; i++ {
		name := fmt.Sprintf("soak-term-%02d", i)
		require.NoError(t, th.KillSession(ctx, name))
		require.False(t, th.HasSession(ctx, name))
	}
}

// F-RES-02: journal retention holds at JournalMaxBytes under 10x output flood.
func TestF_RES_02_JournalRetentionHoldsUnderOutputFlood(t *testing.T) {
	// 1. Verify contract default retention aligns with JournalMaxBytes budget
	require.Equal(t, terminalcontract.Default.JournalMaxBytes, ptyruntime.DefaultRetention.MaxBytes,
		"default retention must equal contract budget JournalMaxBytes (64 MiB)")

	// 2. Test bounded disk retention under a 10x flood
	dir := filepath.Join(t.TempDir(), "journal-flood")
	ret := ptyruntime.Retention{
		MaxBytes:      16 * 1024, // 16 KiB bound
		KeepSnapshots: 2,
		MaxAge:        24 * time.Hour,
	}

	j, err := ptyruntime.OpenJournal(dir, "flood-test", ret)
	require.NoError(t, err)
	defer j.Close()

	// Initial lifecycle spawned event required by stream contract
	spawnEv := ptyevent.Event{
		V:         ptyevent.SchemaVersion,
		SessionID: "flood-test",
		Epoch:     1,
		Seq:       1,
		MonoNS:    100,
		Wall:      time.Now(),
		Kind:      ptyevent.KindLifecycle,
		Lifecycle: &ptyevent.Lifecycle{Phase: ptyevent.PhaseSpawned, PID: 100},
		Redacted:  true,
	}
	_, err = j.Append(spawnEv)
	require.NoError(t, err)

	// Flood 10x the MaxBytes limit: 160 KiB of payload
	totalFloodBytes := 10 * ret.MaxBytes
	chunk := []byte(strings.Repeat("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789\r\n", 20)) // ~760 bytes
	written := int64(0)
	i := 0

	for written < totalFloodBytes {
		seq := j.NextSeq()
		ev := ptyevent.Event{
			V:         ptyevent.SchemaVersion,
			SessionID: "flood-test",
			Epoch:     1,
			Seq:       seq,
			MonoNS:    int64(seq * 1000),
			Wall:      time.Now(),
			Kind:      ptyevent.KindOutput,
			Output:    &ptyevent.Output{Data: chunk},
			Redacted:  true,
		}
		_, err = j.Append(ev)
		require.NoError(t, err)
		written += int64(len(chunk))
		i++

		// Periodically write snapshots every ~8 KiB to allow segment boundaries
		if i%10 == 0 {
			snapSeq := j.NextSeq()
			snapEv := ptyevent.Event{
				V:         ptyevent.SchemaVersion,
				SessionID: "flood-test",
				Epoch:     1,
				Seq:       snapSeq,
				MonoNS:    int64(snapSeq * 1000),
				Wall:      time.Now(),
				Kind:      ptyevent.KindMarker,
				Marker:    &ptyevent.Marker{Type: ptyevent.MarkerSnapshot, ToSeq: snapSeq - 1, Snapshot: "screen", Cols: 80, Rows: 24},
				Redacted:  true,
			}
			_, err = j.Append(snapEv)
			require.NoError(t, err)
		}
	}

	// Calculate total disk usage of journal segments
	var totalDiskBytes int64
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			totalDiskBytes += info.Size()
		}
		return nil
	})
	require.NoError(t, err)

	// Total disk bytes must hold within the retention bounds (well below 64 MiB JournalMaxBytes)
	require.Less(t, totalDiskBytes, terminalcontract.Default.JournalMaxBytes)
	maxExpectedDisk := ret.MaxBytes * int64(ret.KeepSnapshots+2)
	require.LessOrEqual(t, totalDiskBytes, maxExpectedDisk,
		"total journal disk usage (%d) must remain bounded by retention (<= %d)",
		totalDiskBytes, maxExpectedDisk)

	// Verify retained stream validity
	w := j.Window()
	var retained []ptyevent.Event
	err = j.Read(w.OldestSeq, func(ev ptyevent.Event) error {
		retained = append(retained, ev)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, retained)
	violations := ptyevent.ValidateStream(retained)
	require.Empty(t, violations, "retained stream after flood must have zero violations")
}

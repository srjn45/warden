package ptyruntime

import "sync/atomic"

var masters atomic.Int64
var workers atomic.Int64
var children atomic.Int64
var cleanupTimeouts atomic.Int64

// ResourceCounts are process-local gauges, plus a cumulative cleanup counter.
// A detached production process belongs to its holder, not this runtime.
type ResourceCounts struct {
	Masters, Goroutines, Children, CleanupTimeouts int64
}

func Resources() ResourceCounts {
	return ResourceCounts{masters.Load(), workers.Load(), children.Load(), cleanupTimeouts.Load()}
}

func worker(fn func()) {
	workers.Add(1)
	go func() { defer workers.Add(-1); fn() }()
}

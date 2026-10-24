package ptyholder

import "sync/atomic"

var holderChildren atomic.Int64
var cleanupFailures atomic.Int64

// Resources counts local holder children not yet reaped and failed cleanups.
// Detached production holders remain counted until they exit; a daemon restart
// resets these process-local counters, without terminating surviving holders.
func Resources() (children, failures int64) {
	return holderChildren.Load(), cleanupFailures.Load()
}

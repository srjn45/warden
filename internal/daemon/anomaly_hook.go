package daemon

import (
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/poller"
)

// AnomalyMessage builds the notification for a poller-raised health anomaly
// (OOM-suspected crash, infinite loop, pre-crash context). The poller already
// records a durable event for every anomaly; this is the user-facing alert.
func AnomalyMessage(sess *agentstore.Agent, a poller.Anomaly) (title, body string) {
	subj := sess.Subject
	if subj == "" {
		subj = sess.Activity
	}
	if subj == "" {
		subj = sess.ID
	}
	switch a.Kind {
	case "oom":
		title = "warden — possible OOM"
	case "loop":
		title = "warden — possible loop"
	case "context_precrash":
		title = "warden — compact to avoid crash"
	case "approval_loop":
		title = "warden — auto-approve halted"
	default:
		title = "warden — anomaly"
	}
	return title, sess.ID + " (" + subj + "): " + a.Detail
}

// NotifyOnAnomaly returns a poller OnAnomaly hook that fires the notifier
// (best-effort, async) for each raised health anomaly.
func NotifyOnAnomaly(n notify.Notifier) func(*agentstore.Agent, poller.Anomaly) {
	return func(sess *agentstore.Agent, a poller.Anomaly) {
		title, body := AnomalyMessage(sess, a)
		go n.Notify(title, body)
	}
}

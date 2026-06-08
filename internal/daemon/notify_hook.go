package daemon

import (
	"github.com/srajanpathak/warden/internal/notify"
	"github.com/srajanpathak/warden/internal/store"
)

// notifyMessage builds the notification for a transition into status `to`. It
// returns actionable=false for states that don't need the user's attention.
func notifyMessage(sess *store.Session, to store.Status) (title, body string, actionable bool) {
	subj := sess.Subject
	if subj == "" {
		subj = sess.ID
	}
	switch to {
	case store.StatusWaitingForInput:
		return "warden — needs input", sess.ID + ": " + subj, true
	case store.StatusIdle:
		return "warden — stuck", sess.ID + " went idle: " + subj, true
	case store.StatusOrphaned:
		return "warden — agent lost", sess.ID + " tmux gone: " + subj, true
	case store.StatusErrored:
		return "warden — errored", sess.ID + ": " + subj, true
	}
	return "", "", false
}

// NotifyOnTransition returns a poller OnTransition hook that fires the notifier
// (best-effort, async) when an agent enters a state that needs the user.
func NotifyOnTransition(n notify.Notifier) func(*store.Session, store.Status, store.Status) {
	return func(sess *store.Session, _ store.Status, to store.Status) {
		title, body, ok := notifyMessage(sess, to)
		if !ok {
			return
		}
		go n.Notify(title, body)
	}
}

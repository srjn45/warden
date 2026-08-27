package daemon

import (
	"fmt"

	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/store"
)

// ContextAlertMessage builds the notification for an agent crossing a
// context-size threshold. Warning nudges the user to compact; critical notes
// that warden will auto-/compact once the agent is idle.
func ContextAlertMessage(sess *store.Session, state ctxtokens.State, tokens int) (title, body string) {
	subj := sess.Subject
	if subj == "" {
		subj = sess.ID
	}
	size := fmt.Sprintf("%dk", tokens/1000)
	switch state {
	case ctxtokens.StateCritical:
		return "warden — context critical",
			fmt.Sprintf("%s at %s (%s) — auto-/compact when idle", sess.ID, size, subj)
	default: // warning
		return "warden — context high",
			fmt.Sprintf("%s at %s (%s) — consider /compact", sess.ID, size, subj)
	}
}

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
		return "warden — possibly-stuck", sess.ID + " went idle: " + subj, true
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

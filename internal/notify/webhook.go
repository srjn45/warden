package notify

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// webhookTimeout bounds a single webhook POST so a slow or unresponsive endpoint
// can't stall the (goroutine-wrapped) caller. Kept short: a notification is
// best-effort, not worth waiting on.
const webhookTimeout = 5 * time.Second

// webhookPayload is the JSON body POSTed on each notification. Text is the
// Slack-compatible field (Slack incoming webhooks render the "text" key and
// ignore the rest), so a Slack webhook URL works out of the box; Title and Body
// give generic consumers the parts separately.
type webhookPayload struct {
	Text  string `json:"text"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// webhookNotifier POSTs a JSON payload to a configured URL on each Notify.
// Best-effort: a transport error or non-2xx response is logged, never
// propagated, so it can't disrupt the poll loop. A short client timeout bounds
// the call.
type webhookNotifier struct {
	url    string
	client *http.Client
}

// NewWebhook returns a Notifier that POSTs notifications to url. Compose it with
// the platform notifier via Multi to run both channels off the same seam.
func NewWebhook(url string) Notifier {
	return webhookNotifier{url: url, client: &http.Client{Timeout: webhookTimeout}}
}

func (w webhookNotifier) Notify(title, body string) {
	buf, err := json.Marshal(webhookPayload{Text: title + "\n" + body, Title: title, Body: body})
	if err != nil {
		slog.Warn("notify: webhook marshal failed", "err", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, w.url, bytes.NewReader(buf))
	if err != nil {
		slog.Warn("notify: webhook request build failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		slog.Warn("notify: webhook post failed", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("notify: webhook non-2xx response", "status", resp.StatusCode)
	}
}

// multiNotifier fans a notification out to several notifiers in turn. nil
// members are skipped so callers can compose optional channels without guards.
type multiNotifier []Notifier

// Multi composes several notifiers into one that delivers to each in turn,
// skipping any nil entries. Returns the single notifier unchanged when only one
// non-nil notifier is given.
func Multi(notifiers ...Notifier) Notifier {
	out := make(multiNotifier, 0, len(notifiers))
	for _, n := range notifiers {
		if n != nil {
			out = append(out, n)
		}
	}
	if len(out) == 1 {
		return out[0]
	}
	return out
}

func (m multiNotifier) Notify(title, body string) {
	for _, n := range m {
		n.Notify(title, body)
	}
}

package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// webhookTimeout bounds a single webhook POST so a slow or unresponsive endpoint
// can't stall the (goroutine-wrapped) caller. Kept short: a notification is
// best-effort, not worth waiting on.
const webhookTimeout = 5 * time.Second

// errRedirect is returned by the client's CheckRedirect to refuse following any
// 3xx. A webhook receiver has no legitimate need to redirect us, and following a
// redirect is the classic SSRF pivot (a benign-looking URL 302s to an internal
// address). We treat a redirect as a delivery failure and log it.
var errRedirect = errors.New("webhook: refusing to follow redirect")

// errLinkLocalTarget is returned by the dial guard when the resolved address is
// link-local — i.e. the cloud metadata endpoint (169.254.169.254) or an IPv6
// fe80::/10 address. No legitimate webhook target lives there, and it is the
// highest-value SSRF destination, so we refuse to connect regardless of the
// configured URL. Loopback and RFC-1918/LAN targets stay allowed: a local or
// on-LAN notification relay is a supported setup.
var errLinkLocalTarget = errors.New("webhook: refusing to connect to a link-local address")

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
//
// The URL is operator-configured trusted egress, but the notifier is hardened
// as defense-in-depth: only http/https schemes are accepted (anything else
// yields a no-op notifier), redirects are never followed, and connections to
// link-local (cloud-metadata) addresses are refused. A misconfigured or
// non-http URL degrades to a logged no-op rather than failing the daemon.
func NewWebhook(url string) Notifier {
	if !validWebhookScheme(url) {
		slog.Warn("notify: webhook url is not http/https; webhook disabled", "url", url)
		return logNotifier{}
	}
	return webhookNotifier{url: url, client: newWebhookClient()}
}

// validWebhookScheme reports whether raw parses as an absolute http/https URL.
func validWebhookScheme(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// newWebhookClient builds the hardened HTTP client: short timeout, no redirect
// following, and a dial guard that rejects link-local destinations.
func newWebhookClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: webhookTimeout,
		// Control runs after name resolution with the concrete ip:port about to
		// be dialed, so it catches a hostname that resolves to a link-local
		// address too — not just a literal-IP URL.
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				host = address
			}
			if ip := net.ParseIP(host); ip != nil &&
				(ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
				return errLinkLocalTarget
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   webhookTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:   webhookTimeout,
		Transport: tr,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errRedirect
		},
	}
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
		// A blocked redirect or link-local dial surfaces here; both are wrapped
		// in *url.Error, so unwrap to log the specific cause.
		switch {
		case errors.Is(err, errRedirect):
			slog.Warn("notify: webhook redirect refused", "err", err)
		case errors.Is(err, errLinkLocalTarget):
			slog.Warn("notify: webhook link-local target refused", "err", err)
		default:
			slog.Warn("notify: webhook post failed", "err", err)
		}
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

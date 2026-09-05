package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/srjn45/warden/relay/wire"
)

// DefaultHubURL is the warden-hub base URL `warden login` targets when --hub is
// not given. The hub's agreed dev default binds :9876, chosen so it runs beside a
// daemon on :8765 with no configuration (hub-node-contract § 3.11).
const DefaultHubURL = "http://localhost:9876"

// Device-flow poll errors (hub-node-contract § 3.9). The hub returns these as
// 400 responses with a {"error": "<code>"} body.
const (
	devErrAuthorizationPending = "authorization_pending" // keep polling: awaiting browser approval
	devErrSlowDown             = "slow_down"             // polling too fast: back off, keep polling
	devErrExpiredToken         = "expired_token"         // user code TTL lapsed: give up
	devErrAccessDenied         = "access_denied"         // rejected in browser: give up
)

// slowDownStep is added to the poll interval when the hub answers slow_down,
// matching the RFC 8628 device-flow convention.
const slowDownStep = 5 * time.Second

// requestTimeout bounds a single start/token HTTP call so a wedged hub cannot
// hang the whole login (the poll loop's own pacing is separate).
const requestTimeout = 30 * time.Second

// LoginOptions configures a `warden login` device-authorization run.
type LoginOptions struct {
	// HubURL is the warden-hub base URL; empty ⇒ DefaultHubURL.
	HubURL string
	// Hostname is the node display label sent to the hub; empty ⇒ os.Hostname.
	Hostname string
	// Caps advertises node capabilities on the token request.
	Caps []string
	// Store persists the issued credentials; nil ⇒ DefaultCredentialStore.
	Store *CredentialStore
	// Out receives the human-facing verification prompt (URI + user code).
	Out io.Writer
	// HTTP is the client used for both calls; nil ⇒ http.DefaultClient.
	HTTP *http.Client
	// Sleep waits d or returns early on ctx cancellation. nil ⇒ a real timer.
	// Overridable so tests drive the poll loop without wall-clock delays.
	Sleep func(ctx context.Context, d time.Duration) error
}

// LoginResult reports the outcome of a successful login.
type LoginResult struct {
	DaemonID string
	Store    *CredentialStore
}

// Login runs the device-authorization flow: it starts an authorization, prints
// the verification URI and user code for the human to approve in a browser, then
// polls — presenting a LOCALLY GENERATED CSR — until the hub issues a signed cert.
// The private key never leaves this process and no response is allowed to carry
// key material (see assertNoKeyMaterial), upholding the daemon-holds-key
// invariant. On approval the issued cert, CA chain, and the local key are saved
// to the credential store, ready to dial the relay.
func Login(ctx context.Context, opts LoginOptions) (*LoginResult, error) {
	hubURL := strings.TrimRight(opts.HubURL, "/")
	if hubURL == "" {
		hubURL = DefaultHubURL
	}
	store := opts.Store
	if store == nil {
		store = DefaultCredentialStore()
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	client := opts.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	// 1. Start the authorization.
	var start wire.DeviceStartResponse
	if err := postJSON(ctx, client, hubURL+"/api/v1/login/device/start",
		wire.DeviceStartRequest{Hostname: opts.Hostname}, &start); err != nil {
		return nil, fmt.Errorf("start device authorization: %w", err)
	}
	fmt.Fprintf(out, "To authorize this node, open:\n\n    %s\n\nand enter the code:  %s\n\n",
		start.VerificationURI, start.UserCode)
	if start.ExpiresIn > 0 {
		fmt.Fprintf(out, "Waiting for approval (the code expires in %s)…\n", time.Duration(start.ExpiresIn)*time.Second)
	} else {
		fmt.Fprintln(out, "Waiting for approval…")
	}

	// 2. Generate the keypair + CSR locally. Only the CSR is ever sent.
	key, csrPEM, err := GenerateKeyAndCSR(opts.Hostname)
	if err != nil {
		return nil, err
	}

	// 3. Poll.
	interval := time.Duration(start.Interval) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	var deadline time.Time
	if start.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	}
	tokReq := wire.DeviceTokenRequest{
		DeviceCode: start.DeviceCode,
		CSRPEM:     csrPEM,
		Hostname:   opts.Hostname,
		Caps:       opts.Caps,
	}
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, fmt.Errorf("device authorization timed out before approval")
		}
		var tok wire.DeviceTokenResponse
		errCode, err := postDeviceToken(ctx, client, hubURL+"/api/v1/login/device/token", tokReq, &tok)
		if err != nil {
			return nil, err
		}
		switch errCode {
		case "":
			// Approved. Persist the issued cert + CA alongside the local key.
			if tok.CertPEM == "" || tok.CACertPEM == "" {
				return nil, fmt.Errorf("hub approved but returned no certificate")
			}
			creds := Credentials{
				DaemonID:   tok.DaemonID,
				HubURL:     hubURL,
				PrivateKey: key,
				CertPEM:    tok.CertPEM,
				CACertPEM:  tok.CACertPEM,
			}
			if err := store.Save(creds); err != nil {
				return nil, fmt.Errorf("save credentials: %w", err)
			}
			fmt.Fprintf(out, "\nAuthorized. Node identity %s saved to %s\n", tok.DaemonID, store.Dir)
			return &LoginResult{DaemonID: tok.DaemonID, Store: store}, nil
		case devErrAuthorizationPending:
			// Keep waiting at the current cadence.
		case devErrSlowDown:
			interval += slowDownStep
		case devErrExpiredToken:
			return nil, fmt.Errorf("device code expired before approval; run `warden login` again")
		case devErrAccessDenied:
			return nil, fmt.Errorf("authorization was denied in the browser")
		default:
			return nil, fmt.Errorf("device authorization failed: %s", errCode)
		}
		if err := sleep(ctx, interval); err != nil {
			return nil, err
		}
	}
}

// postDeviceToken performs one poll. On HTTP 200 it decodes into out and returns
// ("", nil). On a 400 device-flow error it returns the error code (e.g.
// "authorization_pending") and nil. Any other status is a hard error.
func postDeviceToken(ctx context.Context, client *http.Client, url string, body any, out *wire.DeviceTokenResponse) (string, error) {
	status, raw, err := doJSON(ctx, client, url, body)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK:
		if err := assertNoKeyMaterial(raw); err != nil {
			return "", err
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return "", fmt.Errorf("decode token response: %w", err)
		}
		return "", nil
	case http.StatusBadRequest:
		var e struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &e); err != nil || e.Error == "" {
			return "", fmt.Errorf("hub returned 400 with unrecognized body: %s", strings.TrimSpace(string(raw)))
		}
		return e.Error, nil
	default:
		return "", fmt.Errorf("hub returned unexpected status %d: %s", status, strings.TrimSpace(string(raw)))
	}
}

// assertNoKeyMaterial fails if a provisioning response carries private-key
// material. The daemon holds its own key and must never accept one from the hub;
// this makes the invariant an active check rather than a silently-dropped field.
func assertNoKeyMaterial(raw []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil // not an object; the typed decode will surface any real problem
	}
	forbidden := map[string]bool{"key_pem": true, "private_key": true, "priv_key": true, "key": true, "private_key_pem": true}
	for k := range obj {
		if forbidden[strings.ToLower(k)] {
			return fmt.Errorf("hub response carried forbidden key material field %q; refusing (daemon holds its own key)", k)
		}
	}
	return nil
}

// postJSON POSTs body as JSON and decodes a 2xx response into out.
func postJSON(ctx context.Context, client *http.Client, url string, body, out any) error {
	status, raw, err := doJSON(ctx, client, url, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("status %d: %s", status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// doJSON POSTs body as JSON and returns the response status and body bytes.
func doJSON(ctx context.Context, client *http.Client, url string, body any) (int, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("encode request: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// sleepCtx waits d or returns early with ctx.Err() on cancellation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

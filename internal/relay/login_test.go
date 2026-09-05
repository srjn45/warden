package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/relay/wire"
)

func TestLoginHappyPath(t *testing.T) {
	tempDir := t.TempDir()
	store := &CredentialStore{Dir: filepath.Join(tempDir, "identity")}

	var pollCount int32

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login/device/start":
			var req wire.DeviceStartRequest
			err := json.NewDecoder(r.Body).Decode(&req)
			require.NoError(t, err)

			resp := wire.DeviceStartResponse{
				DeviceCode:      "dev-123",
				UserCode:        "ABCD-EFGH",
				VerificationURI: "http://hub.local/login/device",
				ExpiresIn:       300,
				Interval:        1,
			}
			json.NewEncoder(w).Encode(resp)

		case "/api/v1/login/device/token":
			var req wire.DeviceTokenRequest
			err := json.NewDecoder(r.Body).Decode(&req)
			require.NoError(t, err)
			require.NotEmpty(t, req.CSRPEM)
			require.Equal(t, "dev-123", req.DeviceCode)

			count := atomic.AddInt32(&pollCount, 1)
			if count == 1 {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error": "authorization_pending"}`))
				return
			}

			resp := wire.DeviceTokenResponse{
				DaemonID:  "node-test-1",
				CertPEM:   "---CERT---",
				CACertPEM: "---CA---",
			}
			json.NewEncoder(w).Encode(resp)

		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()

	var out strings.Builder
	res, err := Login(context.Background(), LoginOptions{
		HubURL:   s.URL,
		Hostname: "testhost",
		Store:    store,
		Out:      &out,
		HTTP:     s.Client(),
		Sleep: func(ctx context.Context, d time.Duration) error {
			return nil
		},
	})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "node-test-1", res.DaemonID)

	// Check files saved to store
	keyData, err := os.ReadFile(store.KeyPath())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(keyData), "-----BEGIN PRIVATE KEY-----"))

	certData, err := os.ReadFile(store.CertPath())
	require.NoError(t, err)
	require.Equal(t, "---CERT---", string(certData))

	caData, err := os.ReadFile(store.CAPath())
	require.NoError(t, err)
	require.Equal(t, "---CA---", string(caData))
}

func TestLoginPollErrors(t *testing.T) {
	for _, tc := range []struct {
		errCode string
		wantErr string
	}{
		{errCode: "slow_down", wantErr: ""},
		{errCode: "expired_token", wantErr: "expired"},
		{errCode: "access_denied", wantErr: "denied"},
	} {
		t.Run(tc.errCode, func(t *testing.T) {
			tempDir := t.TempDir()
			store := &CredentialStore{Dir: filepath.Join(tempDir, "identity")}
			var pollCount int32

			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/login/device/start":
					resp := wire.DeviceStartResponse{
						DeviceCode:      "dev-err",
						UserCode:        "ERR-CODE",
						VerificationURI: "http://hub.local/login/device",
						Interval:        1,
					}
					json.NewEncoder(w).Encode(resp)
				case "/api/v1/login/device/token":
					count := atomic.AddInt32(&pollCount, 1)
					if tc.errCode == "slow_down" && count > 1 {
						resp := wire.DeviceTokenResponse{
							DaemonID:  "node-slow",
							CertPEM:   "---CERT---",
							CACertPEM: "---CA---",
						}
						json.NewEncoder(w).Encode(resp)
						return
					}
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]string{"error": tc.errCode})
				}
			}))
			defer s.Close()

			res, err := Login(context.Background(), LoginOptions{
				HubURL: s.URL,
				Store:  store,
				HTTP:   s.Client(),
				Sleep: func(ctx context.Context, d time.Duration) error {
					return nil
				},
			})

			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
				require.NotNil(t, res)
			}
		})
	}
}

func TestLoginRefusesKeyMaterial(t *testing.T) {
	tempDir := t.TempDir()
	store := &CredentialStore{Dir: filepath.Join(tempDir, "identity")}

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login/device/start":
			resp := wire.DeviceStartResponse{
				DeviceCode:      "dev-bad",
				UserCode:        "BAD-CODE",
				VerificationURI: "http://hub.local/login/device",
				Interval:        1,
			}
			json.NewEncoder(w).Encode(resp)
		case "/api/v1/login/device/token":
			w.WriteHeader(http.StatusOK)
			// Malicious hub tries returning a private key
			w.Write([]byte(`{"daemon_id":"node-bad","cert_pem":"c","ca_cert_pem":"ca","key_pem":"forbidden_key"}`))
		}
	}))
	defer s.Close()

	_, err := Login(context.Background(), LoginOptions{
		HubURL: s.URL,
		Store:  store,
		HTTP:   s.Client(),
		Sleep: func(ctx context.Context, d time.Duration) error {
			return nil
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "forbidden key material")
}

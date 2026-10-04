package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckLatestAvailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/repos/srjn45/warden/releases/latest", r.URL.Path)
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.0"}`))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	res, err := Check(Options{
		CurrentVersion: "9.8.0",
		HTTPClient:     rewriteClient(srv.URL),
		Stdout:         &buf,
		// Prevent real side effects if Check somehow applied.
		Restart:     func() error { t.Fatal("restart"); return nil },
		Codesign:    func(string) error { t.Fatal("codesign"); return nil },
		Migrate:     func() error { t.Fatal("migrate"); return nil },
		HealthProbe: func(string) error { t.Fatal("health"); return nil },
	})
	require.NoError(t, err)
	require.False(t, res.UpToDate)
	require.Equal(t, "9.9.0", res.TargetVersion)
	require.Contains(t, buf.String(), "update available: v9.8.0 → v9.9.0")
}

func TestCheckAlreadyUpToDate(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	res, err := Check(Options{
		CurrentVersion: "v1.2.3",
		TargetVersion:  "1.2.3",
		Stdout:         &buf,
	})
	require.NoError(t, err)
	require.True(t, res.UpToDate)
	require.Contains(t, buf.String(), "already up to date")
}

func TestApplySuccess(t *testing.T) {
	t.Parallel()
	payload := []byte("#!/bin/sh\necho new-warden\n")
	archiveBytes := mustTarGz(t, "warden", payload)
	sum := sha256.Sum256(archiveBytes)
	sumsBody := hex.EncodeToString(sum[:]) + "  warden_2.0.0_linux_amd64.tar.gz\n"

	var healthOK atomic.Bool
	healthOK.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/srjn45/warden/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0"}`))
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		switch name {
		case "warden_2.0.0_linux_amd64.tar.gz":
			_, _ = w.Write(archiveBytes)
		case "checksums.txt":
			_, _ = w.Write([]byte(sumsBody))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	installBin := filepath.Join(home, ".local", "bin", "warden")
	require.NoError(t, os.MkdirAll(filepath.Dir(installBin), 0o755))
	require.NoError(t, os.WriteFile(installBin, []byte("old"), 0o755))

	var restarted, migrated atomic.Bool
	var buf bytes.Buffer
	res, err := Apply(Options{
		CurrentVersion: "1.0.0",
		GOOS:           "linux",
		GOARCH:         "amd64",
		InstallBin:     installBin,
		StagingDir:     filepath.Join(home, ".warden", "tmp"),
		AssetBase:      srv.URL + "/download",
		HTTPClient:     rewriteClient(srv.URL),
		Stdout:         &buf,
		Restart:        func() error { restarted.Store(true); return nil },
		Migrate:        func() error { migrated.Store(true); return nil },
		HealthProbe: func(string) error {
			if healthOK.Load() {
				return nil
			}
			return fmt.Errorf("down")
		},
		Sleep: func(time.Duration) {},
	})
	require.NoError(t, err)
	require.True(t, res.Updated)
	require.True(t, restarted.Load())
	require.True(t, migrated.Load())

	got, err := os.ReadFile(installBin)
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.Contains(t, buf.String(), "updated to v2.0.0")

	// wd symlink should exist
	alias, err := os.Readlink(filepath.Join(filepath.Dir(installBin), "wd"))
	require.NoError(t, err)
	require.Equal(t, "warden", alias)
}

func TestApplyRollbackOnHealthFailure(t *testing.T) {
	t.Parallel()
	payload := []byte("new-broken-binary")
	archiveBytes := mustTarGz(t, "warden", payload)
	sum := sha256.Sum256(archiveBytes)
	sumsBody := hex.EncodeToString(sum[:]) + "  warden_3.0.0_linux_amd64.tar.gz\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		switch name {
		case "warden_3.0.0_linux_amd64.tar.gz":
			_, _ = w.Write(archiveBytes)
		case "checksums.txt":
			_, _ = w.Write([]byte(sumsBody))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	installBin := filepath.Join(home, ".local", "bin", "warden")
	require.NoError(t, os.MkdirAll(filepath.Dir(installBin), 0o755))
	old := []byte("good-old-binary")
	require.NoError(t, os.WriteFile(installBin, old, 0o755))

	var buf bytes.Buffer
	res, err := Apply(Options{
		CurrentVersion: "2.0.0",
		TargetVersion:  "3.0.0",
		GOOS:           "linux",
		GOARCH:         "amd64",
		InstallBin:     installBin,
		StagingDir:     filepath.Join(home, ".warden", "tmp"),
		AssetBase:      srv.URL + "/download",
		HTTPClient:     srv.Client(),
		Stdout:         &buf,
		Restart:        func() error { return nil },
		Migrate:        func() error { return nil },
		HealthProbe:    func(string) error { return fmt.Errorf("unhealthy") },
		Sleep:          func(time.Duration) {},
	})
	require.Error(t, err)
	require.True(t, res.RolledBack)
	require.False(t, res.Updated)

	got, readErr := os.ReadFile(installBin)
	require.NoError(t, readErr)
	require.Equal(t, old, got, "binary should be restored from backup")
	require.Contains(t, buf.String(), "rolled back")
}

func TestApplyForceReinstallSameVersion(t *testing.T) {
	t.Parallel()
	payload := []byte("reinstalled")
	archiveBytes := mustTarGz(t, "warden", payload)
	sum := sha256.Sum256(archiveBytes)
	sumsBody := hex.EncodeToString(sum[:]) + "  warden_1.0.0_linux_amd64.tar.gz\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		switch name {
		case "warden_1.0.0_linux_amd64.tar.gz":
			_, _ = w.Write(archiveBytes)
		case "checksums.txt":
			_, _ = w.Write([]byte(sumsBody))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	installBin := filepath.Join(home, "warden")
	require.NoError(t, os.WriteFile(installBin, []byte("old"), 0o755))

	res, err := Apply(Options{
		CurrentVersion: "1.0.0",
		TargetVersion:  "v1.0.0",
		Force:          true,
		GOOS:           "linux",
		GOARCH:         "amd64",
		InstallBin:     installBin,
		StagingDir:     filepath.Join(home, "tmp"),
		AssetBase:      srv.URL + "/download",
		HTTPClient:     srv.Client(),
		Stdout:         io.Discard,
		Restart:        func() error { return nil },
		Migrate:        func() error { return nil },
		HealthProbe:    func(string) error { return nil },
		Sleep:          func(time.Duration) {},
	})
	require.NoError(t, err)
	require.True(t, res.Updated)
	got, err := os.ReadFile(installBin)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestVerifyChecksumRejectsBadDownload(t *testing.T) {
	t.Parallel()
	// Integration-style: download path fails checksum before swap.
	good := mustTarGz(t, "warden", []byte("good"))
	bad := mustTarGz(t, "warden", []byte("tampered"))
	sum := sha256.Sum256(good)
	sumsBody := hex.EncodeToString(sum[:]) + "  warden_4.0.0_linux_amd64.tar.gz\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		switch name {
		case "warden_4.0.0_linux_amd64.tar.gz":
			_, _ = w.Write(bad)
		case "checksums.txt":
			_, _ = w.Write([]byte(sumsBody))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	installBin := filepath.Join(home, "warden")
	require.NoError(t, os.WriteFile(installBin, []byte("old"), 0o755))

	_, err := Apply(Options{
		CurrentVersion: "3.0.0",
		TargetVersion:  "4.0.0",
		GOOS:           "linux",
		GOARCH:         "amd64",
		InstallBin:     installBin,
		StagingDir:     filepath.Join(home, "tmp"),
		AssetBase:      srv.URL + "/download",
		HTTPClient:     srv.Client(),
		Stdout:         io.Discard,
		Restart:        func() error { return nil },
		Migrate:        func() error { return nil },
		HealthProbe:    func(string) error { return nil },
		Sleep:          func(time.Duration) {},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "SHA256 mismatch")
	got, _ := os.ReadFile(installBin)
	require.Equal(t, []byte("old"), got)
}

// rewriteClient returns an HTTP client that redirects api.github.com host
// requests to the test server base URL while preserving the path.
func rewriteClient(base string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			u := *req.URL
			if strings.Contains(u.Host, "github.com") || u.Host == "" {
				bu, _ := http.NewRequest(req.Method, base+u.Path, req.Body)
				bu.Header = req.Header.Clone()
				return http.DefaultTransport.RoundTrip(bu)
			}
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mustTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(content)),
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

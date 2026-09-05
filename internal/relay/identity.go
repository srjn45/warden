package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// Identity file names under the credential directory. The private key is written
// 0600 and never leaves the node — it is not part of any request or response.
const (
	keyFileName  = "key.pem"       // PKCS#8 EC private key (0600) — never transmitted
	certFileName = "cert.pem"      // hub-signed leaf client cert (CN == daemon_id)
	caFileName   = "ca.pem"        // per-user CA chain to trust
	metaFileName = "identity.json" // daemon_id + hub base URL (bookkeeping)
)

// Credentials is the identity provisioned by `warden login` (or Leg-0 enrollment):
// the locally generated private key plus the hub-issued cert and CA chain. The
// daemon presents Cert/Key as its client certificate when it dials the relay and
// proves possession of the key via wire.SignChallenge.
type Credentials struct {
	DaemonID   string
	HubURL     string
	PrivateKey *ecdsa.PrivateKey
	CertPEM    string
	CACertPEM  string
}

// GenerateKeyAndCSR generates a fresh ECDSA P-256 keypair locally and returns it
// with a PEM-encoded PKCS#10 certificate-signing request over the public key. The
// key matches the daemon's relay-auth signing scheme (ECDSA-P256/SHA-256, see
// relay/wire.SignChallenge). The private key never leaves this process; only the
// CSR is sent to the hub, which signs it and returns a cert with CN == daemon_id.
// commonName is a cosmetic subject hint (the hostname); the hub sets the
// authoritative CN when it mints the daemon_id.
func GenerateKeyAndCSR(commonName string) (*ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate key: %w", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: commonName},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, "", fmt.Errorf("create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	return key, string(csrPEM), nil
}

// marshalKeyPEM encodes an ECDSA private key as a PKCS#8 PEM block.
func marshalKeyPEM(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("marshal key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// CredentialStore persists a daemon's relay Credentials under Dir. It is the
// on-disk home for `warden login` output; the daemon reads the same files to dial
// the relay.
type CredentialStore struct {
	Dir string
}

// DefaultCredentialDir is the identity directory under the warden home
// (~/.warden/identity). It sits beside token.env; a nonexistent home falls back
// to a relative path so the store still works in a bare environment.
func DefaultCredentialDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".warden", "identity")
	}
	return filepath.Join(home, ".warden", "identity")
}

// DefaultCredentialStore returns the store rooted at DefaultCredentialDir.
func DefaultCredentialStore() *CredentialStore {
	return &CredentialStore{Dir: DefaultCredentialDir()}
}

func (s *CredentialStore) KeyPath() string  { return filepath.Join(s.Dir, keyFileName) }
func (s *CredentialStore) CertPath() string { return filepath.Join(s.Dir, certFileName) }
func (s *CredentialStore) CAPath() string   { return filepath.Join(s.Dir, caFileName) }
func (s *CredentialStore) MetaPath() string { return filepath.Join(s.Dir, metaFileName) }

// Save writes the credentials to disk: the private key 0600 (it is a secret), the
// cert/CA/metadata 0644. The directory is created 0700. It never writes a key it
// received from the hub — Credentials.PrivateKey is always the locally generated
// one — upholding the daemon-holds-key invariant.
func (s *CredentialStore) Save(c Credentials) error {
	if c.PrivateKey == nil {
		return fmt.Errorf("relay: refusing to save credentials without a locally generated private key")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create identity dir: %w", err)
	}
	keyPEM, err := marshalKeyPEM(c.PrivateKey)
	if err != nil {
		return err
	}
	// 0600 the key BEFORE anything else; a partial write must never leave a
	// world-readable key behind.
	if err := os.WriteFile(s.KeyPath(), []byte(keyPEM), 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	if err := os.WriteFile(s.CertPath(), []byte(c.CertPEM), 0o644); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(s.CAPath(), []byte(c.CACertPEM), 0o644); err != nil {
		return fmt.Errorf("write ca: %w", err)
	}
	meta := fmt.Sprintf("{\n  \"daemon_id\": %q,\n  \"hub_url\": %q\n}\n", c.DaemonID, c.HubURL)
	if err := os.WriteFile(s.MetaPath(), []byte(meta), 0o644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	return nil
}

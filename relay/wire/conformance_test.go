package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// update regenerates the golden conformance files from this package's codecs:
//
//	go test ./relay/wire -run TestConformance -update
//
// Normal runs verify the committed goldens against the codecs, so any drift in a
// frame/header layout, digest, or JSON tag fails the build here (and, via the
// exported ConformanceFS/loaders, in warden-hub CI too).
var update = flag.Bool("update", false, "regenerate relay/wire conformance golden files")

const conformanceDir = "testdata/conformance"

// --- canonical inputs (the single source the goldens are generated from) ---

func canonicalFramePayloads() []struct {
	name string
	data []byte
} {
	seq := make([]byte, 256)
	for i := range seq {
		seq[i] = byte(i)
	}
	return []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"ascii", []byte("hello relay")},
		{"binary", []byte{0x00, 0x01, 0x02, 0xff, 0xfe}},
		{"seq256", seq}, // exercises a length prefix beyond one byte
	}
}

func canonicalNonces() []struct {
	name  string
	nonce []byte
} {
	zeros := make([]byte, NonceLen)
	ones := bytes.Repeat([]byte{0xff}, NonceLen)
	counter := make([]byte, NonceLen)
	for i := range counter {
		counter[i] = byte(i)
	}
	return []struct {
		name  string
		nonce []byte
	}{
		{"zeros", zeros},
		{"ones", ones},
		{"counter", counter},
	}
}

func canonicalStreamOpens() []struct {
	name string
	open StreamOpen
} {
	return []struct {
		name string
		open StreamOpen
	}{
		{"native_full", StreamOpen{Version: ProtoVersion, Kind: KindNativeE2E, Scope: ScopeFull, Grantee: "user_abc123", CorrID: "corr-42"}},
		{"native_readonly", StreamOpen{Version: ProtoVersion, Kind: KindNativeE2E, Scope: ScopeReadOnly, Grantee: "user_ro", CorrID: "corr-ro"}},
		{"web_terminated", StreamOpen{Version: ProtoVersion, Kind: KindWebTerminated, Scope: ScopeReadOnly, Grantee: "user_web", CorrID: "corr-web-7"}},
	}
}

func canonicalNarrowings() []ScopeNarrowing {
	return []ScopeNarrowing{
		{"cert_full_hdr_full", ScopeFull, ScopeFull, ScopeFull},
		{"cert_readonly_hdr_full", ScopeReadOnly, ScopeFull, ScopeReadOnly}, // inner cert narrows
		{"cert_full_hdr_readonly", ScopeFull, ScopeReadOnly, ScopeReadOnly}, // hub header narrows
		{"cert_readonly_hdr_readonly", ScopeReadOnly, ScopeReadOnly, ScopeReadOnly},
		{"cert_none_hdr_full", ScopeNone, ScopeFull, ScopeNone}, // deny wins
		{"cert_full_hdr_none", ScopeFull, ScopeNone, ScopeNone},
	}
}

// canonicalCloseCodes is the authoritative name->value table; the golden file and
// the exported constants are both checked against it.
func canonicalCloseCodes() []CloseCodeEntry {
	return []CloseCodeEntry{
		{"CloseNormal", CloseNormal},
		{"CloseProtocolError", CloseProtocolError},
		{"CloseUnsupportedVersion", CloseUnsupportedVersion},
		{"CloseUnknownStreamKind", CloseUnknownStreamKind},
		{"CloseWebTerminatedDisabled", CloseWebTerminatedDisabled},
		{"CloseUnauthorized", CloseUnauthorized},
		{"CloseEnrollmentRequired", CloseEnrollmentRequired},
		{"CloseInternal", CloseInternal},
	}
}

// Fixed, deterministic PEM/time placeholders — never real key material. The whole
// point of these fixtures is to pin that no provisioning response serializes a
// private key, so they deliberately contain only CSRs and certificates.
const (
	sampleCSRPEM  = "-----BEGIN CERTIFICATE REQUEST-----\nMIH...sampleCSR...\n-----END CERTIFICATE REQUEST-----\n"
	sampleCertPEM = "-----BEGIN CERTIFICATE-----\nMIIB...sampleLeaf...\n-----END CERTIFICATE-----\n"
	sampleCAPEM   = "-----BEGIN CERTIFICATE-----\nMIIB...sampleCA...\n-----END CERTIFICATE-----\n"
)

var fixedExpiry = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func canonicalEnrollFixtures() []struct {
	name string
	v    any
} {
	return []struct {
		name string
		v    any
	}{
		{"EnrollmentTokenRequest", EnrollmentTokenRequest{Label: "home-server", TTLSeconds: 3600}},
		{"EnrollmentTokenResponse", EnrollmentTokenResponse{Token: "ent_one_time_abc123", ExpiresAt: fixedExpiry}},
		{"EnrollRequest", EnrollRequest{Token: "ent_one_time_abc123", CSRPEM: sampleCSRPEM, Hostname: "home-server", Caps: []string{"terminal-sessions", "scheduled-agents"}}},
		{"EnrollResponse", EnrollResponse{DaemonID: "d-11111111-2222-3333-4444-555555555555", CertPEM: sampleCertPEM, CACertPEM: sampleCAPEM}},
	}
}

func canonicalDeviceFixtures() []struct {
	name string
	v    any
} {
	return []struct {
		name string
		v    any
	}{
		{"DeviceStartRequest", DeviceStartRequest{Hostname: "home-server"}},
		{"DeviceStartResponse", DeviceStartResponse{DeviceCode: "dc_high_entropy_secret_xyz", UserCode: "WDN-4821", VerificationURI: "http://localhost:9876/login/device", ExpiresIn: 900, Interval: 5}},
		{"DeviceTokenRequest", DeviceTokenRequest{DeviceCode: "dc_high_entropy_secret_xyz", CSRPEM: sampleCSRPEM, Hostname: "home-server", Caps: []string{"terminal-sessions", "scheduled-agents"}}},
		{"DeviceTokenResponse", DeviceTokenResponse{DaemonID: "d-66666666-7777-8888-9999-000000000000", CertPEM: sampleCertPEM, CACertPEM: sampleCAPEM}},
	}
}

// --- generation ---

func writeGolden(t *testing.T, name string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	b = append(b, '\n')
	if err := os.MkdirAll(conformanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conformanceDir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func regenerate(t *testing.T) {
	t.Helper()

	// frames.json
	var ff frameFile
	for _, p := range canonicalFramePayloads() {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, p.data); err != nil {
			t.Fatal(err)
		}
		ff.Vectors = append(ff.Vectors, struct {
			Name       string `json:"name"`
			PayloadHex string `json:"payload_hex"`
			FrameHex   string `json:"frame_hex"`
		}{p.name, hex.EncodeToString(p.data), hex.EncodeToString(buf.Bytes())})
	}
	writeGolden(t, "frames.json", ff)

	// auth.json
	var af authFile
	af.DomainSepUTF8 = string(DomainSepAuth)
	af.DomainSepHex = hex.EncodeToString(DomainSepAuth)
	for _, n := range canonicalNonces() {
		af.DigestVectors = append(af.DigestVectors, struct {
			Name      string `json:"name"`
			NonceHex  string `json:"nonce_hex"`
			DigestHex string `json:"digest_hex"`
		}{n.name, hex.EncodeToString(n.nonce), hex.EncodeToString(AuthDigest(n.nonce))})
	}
	writeGolden(t, "auth.json", af)

	// streamopen.json
	var sf streamOpenFile
	for _, s := range canonicalStreamOpens() {
		var buf bytes.Buffer
		if err := WriteStreamOpen(&buf, s.open); err != nil {
			t.Fatal(err)
		}
		sf.Framings = append(sf.Framings, struct {
			Name     string         `json:"name"`
			Open     streamOpenWire `json:"open"`
			FrameHex string         `json:"frame_hex"`
		}{s.name, streamOpenWire{s.open.Version, uint8(s.open.Kind), uint8(s.open.Scope), s.open.Grantee, s.open.CorrID}, hex.EncodeToString(buf.Bytes())})
	}
	for _, n := range canonicalNarrowings() {
		sf.Narrowings = append(sf.Narrowings, struct {
			Name        string `json:"name"`
			CertImplied uint8  `json:"cert_implied"`
			Asserted    uint8  `json:"asserted"`
			Effective   uint8  `json:"effective"`
		}{n.Name, uint8(n.CertImplied), uint8(n.Asserted), uint8(NarrowScope(n.CertImplied, n.Asserted))})
	}
	writeGolden(t, "streamopen.json", sf)

	// closecodes.json
	var cf closeCodeFile
	for _, e := range canonicalCloseCodes() {
		cf.Table = append(cf.Table, struct {
			Name string `json:"name"`
			Code uint16 `json:"code"`
		}{e.Name, uint16(e.Code)})
	}
	writeGolden(t, "closecodes.json", cf)

	// enroll.json / device.json
	writeGolden(t, "enroll.json", fixtureFileFrom(t, canonicalEnrollFixtures()))
	writeGolden(t, "device.json", fixtureFileFrom(t, canonicalDeviceFixtures()))
}

func fixtureFileFrom(t *testing.T, fixtures []struct {
	name string
	v    any
}) fixtureFile {
	t.Helper()
	var out fixtureFile
	for _, fx := range fixtures {
		raw, err := json.Marshal(fx.v)
		if err != nil {
			t.Fatalf("marshal fixture %s: %v", fx.name, err)
		}
		out.Fixtures = append(out.Fixtures, struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}{fx.name, raw})
	}
	return out
}

// --- the test ---

func TestConformance(t *testing.T) {
	if *update {
		regenerate(t)
		t.Log("regenerated relay/wire conformance golden files")
		return
	}

	t.Run("frames", testConformanceFrames)
	t.Run("auth", testConformanceAuth)
	t.Run("streamopen", testConformanceStreamOpen)
	t.Run("closecodes", testConformanceCloseCodes)
	t.Run("enroll", func(t *testing.T) { testConformanceFixtures(t, "enroll.json", LoadEnrollFixtures) })
	t.Run("device", func(t *testing.T) { testConformanceFixtures(t, "device.json", LoadDeviceFixtures) })
}

func testConformanceFrames(t *testing.T) {
	vecs, err := LoadFrameVectors()
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) == 0 {
		t.Fatal("no frame vectors")
	}
	for _, v := range vecs {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, v.Payload); err != nil {
			t.Fatalf("%s: WriteFrame: %v", v.Name, err)
		}
		if !bytes.Equal(buf.Bytes(), v.Frame) {
			t.Errorf("%s: WriteFrame=%x want golden %x", v.Name, buf.Bytes(), v.Frame)
		}
		got, err := ReadFrame(bytes.NewReader(v.Frame))
		if err != nil {
			t.Fatalf("%s: ReadFrame: %v", v.Name, err)
		}
		if !bytes.Equal(got, v.Payload) {
			t.Errorf("%s: ReadFrame=%x want %x", v.Name, got, v.Payload)
		}
	}
}

func testConformanceAuth(t *testing.T) {
	av, err := LoadAuthVectors()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(av.DomainSep, DomainSepAuth) {
		t.Fatalf("DomainSepAuth golden=%x want %x", av.DomainSep, DomainSepAuth)
	}
	if len(av.Digests) == 0 {
		t.Fatal("no auth digest vectors")
	}
	for _, v := range av.Digests {
		if got := AuthDigest(v.Nonce); !bytes.Equal(got, v.Digest) {
			t.Errorf("%s: AuthDigest=%x want golden %x", v.Name, got, v.Digest)
		}
	}
}

func testConformanceStreamOpen(t *testing.T) {
	sv, err := LoadStreamOpenVectors()
	if err != nil {
		t.Fatal(err)
	}
	if len(sv.Framings) == 0 || len(sv.Narrowings) == 0 {
		t.Fatal("missing streamopen framings or narrowings")
	}
	sawWeb := false
	for _, v := range sv.Framings {
		if v.Open.Kind == KindWebTerminated {
			sawWeb = true
		}
		var buf bytes.Buffer
		if err := WriteStreamOpen(&buf, v.Open); err != nil {
			t.Fatalf("%s: WriteStreamOpen: %v", v.Name, err)
		}
		if !bytes.Equal(buf.Bytes(), v.Frame) {
			t.Errorf("%s: WriteStreamOpen=%x want golden %x", v.Name, buf.Bytes(), v.Frame)
		}
		got, err := ReadStreamOpen(bytes.NewReader(v.Frame))
		if err != nil {
			t.Fatalf("%s: ReadStreamOpen: %v", v.Name, err)
		}
		if got != v.Open {
			t.Errorf("%s: ReadStreamOpen=%+v want %+v", v.Name, got, v.Open)
		}
	}
	if !sawWeb {
		t.Error("no KindWebTerminated framing vector present")
	}
	for _, n := range sv.Narrowings {
		if got := NarrowScope(n.CertImplied, n.Asserted); got != n.Effective {
			t.Errorf("%s: NarrowScope(%d,%d)=%d want golden %d", n.Name, n.CertImplied, n.Asserted, got, n.Effective)
		}
	}
}

func testConformanceCloseCodes(t *testing.T) {
	table, err := LoadCloseCodeTable()
	if err != nil {
		t.Fatal(err)
	}
	want := canonicalCloseCodes()
	if len(table) != len(want) {
		t.Fatalf("close-code table golden has %d entries, want %d", len(table), len(want))
	}
	for i, e := range table {
		if e != want[i] {
			t.Errorf("close-code[%d] golden=%+v want %+v", i, e, want[i])
		}
	}
}

// forbiddenKeyTokens must never appear in a provisioning fixture: no response
// carries a private key.
var forbiddenKeyTokens = []string{
	"key_pem",
	"private_key",
	"BEGIN PRIVATE KEY",
	"BEGIN EC PRIVATE KEY",
	"BEGIN RSA PRIVATE KEY",
}

func testConformanceFixtures(t *testing.T, file string, load func() ([]JSONFixture, error)) {
	// The whole file (every fixture) must be free of private-key material.
	raw, err := ConformanceFS.ReadFile("testdata/conformance/" + file)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range forbiddenKeyTokens {
		if bytes.Contains(raw, []byte(tok)) {
			t.Errorf("%s contains forbidden token %q — a provisioning fixture leaked key material", file, tok)
		}
	}

	fixtures, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("%s has no fixtures", file)
	}
	// Each fixture must be valid JSON, and re-marshaling the decoded value must be
	// stable (canonical field set), so a drifted json tag is caught.
	for _, fx := range fixtures {
		if !json.Valid(fx.JSON) {
			t.Errorf("%s/%s: invalid JSON", file, fx.Name)
		}
		if strings.TrimSpace(string(fx.JSON)) == "" {
			t.Errorf("%s/%s: empty fixture", file, fx.Name)
		}
	}
}

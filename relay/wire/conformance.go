package wire

import (
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ConformanceFS holds language-agnostic golden vectors for the relay wire
// contract. They are the frozen expected outputs of this package's codecs so an
// external implementation — notably warden-hub, a separate Go module that imports
// this package — can run byte-for-byte identical conformance checks and catch any
// divergence from the shared contract. warden's own conformance_test.go verifies
// every codec against these same files.
//
// Files under testdata/conformance:
//
//	frames.json      WriteFrame / ReadFrame golden vectors
//	auth.json        DomainSepAuth bytes + AuthDigest(nonce) golden digests
//	streamopen.json  StreamOpen framings (incl. KindWebTerminated) + NarrowScope table
//	closecodes.json  the CloseCode table (name -> value)
//	enroll.json      enrollment request/response JSON fixtures (never any key material)
//	device.json      device-flow request/response JSON fixtures (never any key material)
//
//go:embed testdata/conformance
var ConformanceFS embed.FS

// --- public, typed vectors (decoded from ConformanceFS) ---

// FrameVector is a golden WriteFrame/ReadFrame case: WriteFrame(Payload) must
// emit exactly Frame, and ReadFrame(Frame) must return Payload.
type FrameVector struct {
	Name    string
	Payload []byte
	Frame   []byte
}

// AuthDigestVector pins AuthDigest(Nonce) == Digest (SHA-256(DomainSepAuth||Nonce)).
type AuthDigestVector struct {
	Name   string
	Nonce  []byte
	Digest []byte
}

// AuthVectors carries the exact DomainSepAuth bytes plus the digest vectors.
type AuthVectors struct {
	DomainSep []byte
	Digests   []AuthDigestVector
}

// StreamOpenVector is a golden WriteStreamOpen framing: WriteStreamOpen(Open)
// must emit exactly Frame and ReadStreamOpen(Frame) must return Open.
type StreamOpenVector struct {
	Name  string
	Open  StreamOpen
	Frame []byte
}

// ScopeNarrowing pins NarrowScope(CertImplied, Asserted) == Effective — the
// min(cert-implied, Scope) ceiling a KindNativeE2E stream is held to.
type ScopeNarrowing struct {
	Name        string
	CertImplied Scope
	Asserted    Scope
	Effective   Scope
}

// StreamOpenVectors bundles the framing vectors and the scope-narrowing table.
type StreamOpenVectors struct {
	Framings   []StreamOpenVector
	Narrowings []ScopeNarrowing
}

// CloseCodeEntry maps a CloseCode constant's name to its numeric value.
type CloseCodeEntry struct {
	Name string
	Code CloseCode
}

// JSONFixture is a named golden JSON encoding of a wire struct. The enrollment
// and device fixtures exist partly to pin the invariant that no provisioning
// response ever serializes a private key.
type JSONFixture struct {
	Name string
	JSON json.RawMessage
}

// --- on-disk file schemas (hex-encoded for language-agnostic goldens) ---

type frameFile struct {
	Vectors []struct {
		Name       string `json:"name"`
		PayloadHex string `json:"payload_hex"`
		FrameHex   string `json:"frame_hex"`
	} `json:"vectors"`
}

type authFile struct {
	DomainSepUTF8 string `json:"domain_sep_utf8"`
	DomainSepHex  string `json:"domain_sep_hex"`
	DigestVectors []struct {
		Name      string `json:"name"`
		NonceHex  string `json:"nonce_hex"`
		DigestHex string `json:"digest_hex"`
	} `json:"digest_vectors"`
}

type streamOpenWire struct {
	Version uint8  `json:"version"`
	Kind    uint8  `json:"kind"`
	Scope   uint8  `json:"scope"`
	Grantee string `json:"grantee"`
	CorrID  string `json:"corr_id"`
}

type streamOpenFile struct {
	Framings []struct {
		Name     string         `json:"name"`
		Open     streamOpenWire `json:"open"`
		FrameHex string         `json:"frame_hex"`
	} `json:"framings"`
	Narrowings []struct {
		Name        string `json:"name"`
		CertImplied uint8  `json:"cert_implied"`
		Asserted    uint8  `json:"asserted"`
		Effective   uint8  `json:"effective"`
	} `json:"narrowings"`
}

type closeCodeFile struct {
	Table []struct {
		Name string `json:"name"`
		Code uint16 `json:"code"`
	} `json:"table"`
}

type fixtureFile struct {
	Fixtures []struct {
		Name string          `json:"name"`
		JSON json.RawMessage `json:"json"`
	} `json:"fixtures"`
}

// --- loaders (usable by warden's tests and by hub CI) ---

func readConformance(name string, v any) error {
	b, err := ConformanceFS.ReadFile("testdata/conformance/" + name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("wire: decode %s: %w", name, err)
	}
	return nil
}

// LoadFrameVectors returns the golden WriteFrame/ReadFrame vectors.
func LoadFrameVectors() ([]FrameVector, error) {
	var f frameFile
	if err := readConformance("frames.json", &f); err != nil {
		return nil, err
	}
	out := make([]FrameVector, 0, len(f.Vectors))
	for _, v := range f.Vectors {
		payload, err := hex.DecodeString(v.PayloadHex)
		if err != nil {
			return nil, fmt.Errorf("wire: frame %q payload: %w", v.Name, err)
		}
		frame, err := hex.DecodeString(v.FrameHex)
		if err != nil {
			return nil, fmt.Errorf("wire: frame %q frame: %w", v.Name, err)
		}
		out = append(out, FrameVector{Name: v.Name, Payload: payload, Frame: frame})
	}
	return out, nil
}

// LoadAuthVectors returns the DomainSepAuth bytes and AuthDigest golden vectors.
func LoadAuthVectors() (AuthVectors, error) {
	var f authFile
	if err := readConformance("auth.json", &f); err != nil {
		return AuthVectors{}, err
	}
	domainSep, err := hex.DecodeString(f.DomainSepHex)
	if err != nil {
		return AuthVectors{}, fmt.Errorf("wire: auth domain_sep: %w", err)
	}
	av := AuthVectors{DomainSep: domainSep}
	for _, v := range f.DigestVectors {
		nonce, err := hex.DecodeString(v.NonceHex)
		if err != nil {
			return AuthVectors{}, fmt.Errorf("wire: auth %q nonce: %w", v.Name, err)
		}
		digest, err := hex.DecodeString(v.DigestHex)
		if err != nil {
			return AuthVectors{}, fmt.Errorf("wire: auth %q digest: %w", v.Name, err)
		}
		av.Digests = append(av.Digests, AuthDigestVector{Name: v.Name, Nonce: nonce, Digest: digest})
	}
	return av, nil
}

// LoadStreamOpenVectors returns the StreamOpen framings and the NarrowScope table.
func LoadStreamOpenVectors() (StreamOpenVectors, error) {
	var f streamOpenFile
	if err := readConformance("streamopen.json", &f); err != nil {
		return StreamOpenVectors{}, err
	}
	var out StreamOpenVectors
	for _, v := range f.Framings {
		frame, err := hex.DecodeString(v.FrameHex)
		if err != nil {
			return StreamOpenVectors{}, fmt.Errorf("wire: streamopen %q frame: %w", v.Name, err)
		}
		out.Framings = append(out.Framings, StreamOpenVector{
			Name: v.Name,
			Open: StreamOpen{
				Version: v.Open.Version,
				Kind:    StreamKind(v.Open.Kind),
				Scope:   Scope(v.Open.Scope),
				Grantee: v.Open.Grantee,
				CorrID:  v.Open.CorrID,
			},
			Frame: frame,
		})
	}
	for _, n := range f.Narrowings {
		out.Narrowings = append(out.Narrowings, ScopeNarrowing{
			Name:        n.Name,
			CertImplied: Scope(n.CertImplied),
			Asserted:    Scope(n.Asserted),
			Effective:   Scope(n.Effective),
		})
	}
	return out, nil
}

// LoadCloseCodeTable returns the golden CloseCode name/value table.
func LoadCloseCodeTable() ([]CloseCodeEntry, error) {
	var f closeCodeFile
	if err := readConformance("closecodes.json", &f); err != nil {
		return nil, err
	}
	out := make([]CloseCodeEntry, 0, len(f.Table))
	for _, e := range f.Table {
		out = append(out, CloseCodeEntry{Name: e.Name, Code: CloseCode(e.Code)})
	}
	return out, nil
}

// LoadEnrollFixtures returns the enrollment JSON fixtures.
func LoadEnrollFixtures() ([]JSONFixture, error) { return loadFixtures("enroll.json") }

// LoadDeviceFixtures returns the device-flow JSON fixtures.
func LoadDeviceFixtures() ([]JSONFixture, error) { return loadFixtures("device.json") }

func loadFixtures(name string) ([]JSONFixture, error) {
	var f fixtureFile
	if err := readConformance(name, &f); err != nil {
		return nil, err
	}
	out := make([]JSONFixture, 0, len(f.Fixtures))
	for _, fx := range f.Fixtures {
		out = append(out, JSONFixture{Name: fx.Name, JSON: fx.JSON})
	}
	return out, nil
}

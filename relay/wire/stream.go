package wire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// StreamKind tags what a hub->daemon yamux stream carries. The hub writes it in
// the StreamOpen header that precedes any payload bytes, so the daemon can route
// the stream before reading further.
type StreamKind uint8

const (
	// KindInvalid is the zero value and never appears on the wire.
	KindInvalid StreamKind = 0
	// KindNativeE2E: after the StreamOpen frame the stream is raw and the client
	// and daemon run mTLS *inside* it end-to-end; the hub relays ciphertext and
	// cannot read or inject. The daemon verifies the client cert against the
	// per-user CA and takes the effective scope as min(cert-implied, Scope).
	KindNativeE2E StreamKind = 1
	// KindWebTerminated: the hub terminated TLS (a browser that cannot present a
	// custom-CA client cert). After the frame the daemon speaks plain HTTP over
	// the stream and trusts the hub-asserted {Grantee, Scope}. A daemon with
	// relay.allow_web_terminated=false rejects these with CloseWebTerminatedDisabled.
	KindWebTerminated StreamKind = 2
	// KindControl: the daemon-opened control stream carrying Hello/Heartbeat/Bye/
	// ConfigPush (framed JSON). Opened by the daemon immediately after the yamux
	// handshake; there is no reserved stream id, the Kind identifies it.
	KindControl StreamKind = 3
)

// Scope is the authorization ceiling the hub asserts for a stream. It maps 1:1
// onto the daemon's internal auth scope. Values are explicit and leave a
// reserved gap (4..9) so future scopes slot in without renumbering.
type Scope uint8

const (
	// ScopeNone is the zero value and never rides an opened stream.
	ScopeNone Scope = 0
	// ScopeReadOnly permits read/observe requests only.
	ScopeReadOnly Scope = 1
	// ScopeFull permits writes/mutations.
	ScopeFull Scope = 2
)

// StreamOpen is the header the hub writes (as a single WriteFrame payload)
// before any client bytes when it opens a stream to the daemon. It feeds the
// daemon's relay-identity branch in authorize():
//
//   - NativeE2E: relayIdentity = {peerCN from the inner client cert, effScope};
//     effScope = min(cert-implied, Scope). The header can only NARROW, never widen.
//   - WebTerminated: relayIdentity = {Grantee, Scope} — trusted because the hub
//     already TLS-terminated and authenticated the user.
type StreamOpen struct {
	Version uint8      // wire version for this stream; reject unknown with CloseUnsupportedVersion
	Kind    StreamKind // NativeE2E | WebTerminated | Control
	Scope   Scope      // hub-asserted ceiling
	Grantee string     // hub's stable opaque user id; audit + relayIdentity CN for WebTerminated
	CorrID  string     // correlation id; the daemon echoes it into its audit log
}

// maxStringLen bounds the u16-length-prefixed Grantee/CorrID fields.
const maxStringLen = 1 << 16

func (s StreamOpen) encode() ([]byte, error) {
	if len(s.Grantee) >= maxStringLen {
		return nil, fmt.Errorf("wire: grantee too long (%d bytes)", len(s.Grantee))
	}
	if len(s.CorrID) >= maxStringLen {
		return nil, fmt.Errorf("wire: corrID too long (%d bytes)", len(s.CorrID))
	}
	var b bytes.Buffer
	b.WriteByte(s.Version)
	b.WriteByte(byte(s.Kind))
	b.WriteByte(byte(s.Scope))
	writeU16String(&b, s.Grantee)
	writeU16String(&b, s.CorrID)
	return b.Bytes(), nil
}

func writeU16String(b *bytes.Buffer, s string) {
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(s)))
	b.Write(l[:])
	b.WriteString(s)
}

func readU16String(r io.Reader) (string, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return "", err
	}
	buf := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// WriteStreamOpen frames and writes the header. Everything the caller writes to
// w after this is raw stream payload.
func WriteStreamOpen(w io.Writer, s StreamOpen) error {
	body, err := s.encode()
	if err != nil {
		return err
	}
	return WriteFrame(w, body)
}

// ReadStreamOpen reads the header frame from r. On return the next bytes on r
// are the raw stream payload (inner mTLS for NativeE2E, plain HTTP for
// WebTerminated), so r must be the same reader the caller continues to use.
func ReadStreamOpen(r io.Reader) (StreamOpen, error) {
	body, err := ReadFrame(r)
	if err != nil {
		return StreamOpen{}, err
	}
	br := bytes.NewReader(body)
	var head [3]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return StreamOpen{}, err
	}
	s := StreamOpen{Version: head[0], Kind: StreamKind(head[1]), Scope: Scope(head[2])}
	if s.Grantee, err = readU16String(br); err != nil {
		return StreamOpen{}, err
	}
	if s.CorrID, err = readU16String(br); err != nil {
		return StreamOpen{}, err
	}
	return s, nil
}

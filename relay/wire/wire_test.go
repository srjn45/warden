package wire

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello relay")
	if err := WriteFrame(&buf, payload); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("frame got %q want %q", got, payload)
	}
}

func TestReadFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF}) // advertise ~4 GiB
	if _, err := ReadFrame(&buf); err == nil {
		t.Fatal("expected ErrFrameTooLarge, got nil")
	}
}

func TestStreamOpenRoundTripLeavesTrailer(t *testing.T) {
	want := StreamOpen{
		Version: ProtoVersion,
		Kind:    KindNativeE2E,
		Scope:   ScopeFull,
		Grantee: "user_abc123",
		CorrID:  "corr-42",
	}
	var buf bytes.Buffer
	if err := WriteStreamOpen(&buf, want); err != nil {
		t.Fatal(err)
	}
	// Raw client bytes that follow the header on the same stream must survive.
	trailer := []byte("RAW-CLIENT-BYTES")
	buf.Write(trailer)

	got, err := ReadStreamOpen(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("streamopen got %+v want %+v", got, want)
	}
	if rest := buf.Bytes(); !bytes.Equal(rest, trailer) {
		t.Fatalf("trailer got %q want %q — ReadStreamOpen over-read the stream", rest, trailer)
	}
}

func TestControlRoundTrip(t *testing.T) {
	want := ControlMessage{
		Type: ControlHello,
		Hello: &Hello{
			ProtoVersion: ProtoVersion,
			DaemonID:     "d-1",
			Caps:         []string{"terminal-sessions", "scheduled-agents"},
		},
	}
	var buf bytes.Buffer
	if err := WriteControl(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadControl(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != ControlHello || got.Hello == nil {
		t.Fatalf("control got %+v", got)
	}
	if got.Hello.DaemonID != "d-1" || len(got.Hello.Caps) != 2 {
		t.Fatalf("hello payload got %+v", got.Hello)
	}
}

func TestAuthChallengeSignVerify(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != NonceLen {
		t.Fatalf("nonce len %d want %d", len(nonce), NonceLen)
	}
	sig, err := SignChallenge(priv, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyChallenge(&priv.PublicKey, nonce, sig) {
		t.Fatal("valid signature rejected")
	}
	// A signature must not verify against a different nonce (domain-sep digest).
	other, _ := NewNonce()
	if VerifyChallenge(&priv.PublicKey, other, sig) {
		t.Fatal("signature verified against the wrong nonce")
	}
}

func TestNegotiateVersion(t *testing.T) {
	cases := []struct{ a, b, want uint8 }{
		{1, 2, 1},
		{3, 1, 1},
		{2, 2, 2},
		{0, 2, 0},
		{2, 0, 0},
	}
	for _, c := range cases {
		if got := NegotiateVersion(c.a, c.b); got != c.want {
			t.Errorf("NegotiateVersion(%d,%d)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

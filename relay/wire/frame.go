package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrameLen bounds a single length-prefixed frame's payload so ReadFrame can
// never be coerced into an unbounded allocation by a hostile or corrupt peer.
// Relay frames (StreamOpen headers, control messages) are small; 1 MiB is
// generous headroom.
const MaxFrameLen = 1 << 20 // 1 MiB

// ErrFrameTooLarge is returned when a frame's advertised length exceeds
// MaxFrameLen.
var ErrFrameTooLarge = errors.New("wire: frame exceeds MaxFrameLen")

// WriteFrame writes payload as a single length-prefixed frame: a uint32
// big-endian length followed by exactly that many bytes. It is the sole framing
// primitive on the relay control path — both the binary StreamOpen header and
// the JSON control messages ride it, so the two sides share one encoder.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameLen {
		return fmt.Errorf("wire: payload %d bytes: %w", len(payload), ErrFrameTooLarge)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one length-prefixed frame written by WriteFrame and returns
// its payload. It consumes exactly the 4-byte header plus the advertised
// payload and no more, so on a StreamOpen stream the bytes that follow (raw
// client<->daemon traffic) are left untouched — pass ReadFrame the same
// unbuffered reader the caller continues to read from.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameLen {
		return nil, fmt.Errorf("wire: advertised %d bytes: %w", n, ErrFrameTooLarge)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

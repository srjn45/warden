package wire

// ProtoVersion is the relay wire-protocol version this package implements. Both
// peers exchange it in the control-stream Hello and negotiate the minimum
// (NegotiateVersion). Bump it on any breaking change to a frame or header
// layout; additive, backward-compatible fields do not require a bump.
const ProtoVersion uint8 = 1

// NegotiateVersion returns the version two peers agree to speak: the lower of
// the two advertised versions, so a newer peer transparently downgrades to an
// older one. A zero result means no common version (a side advertised 0, which
// is never valid on the wire).
func NegotiateVersion(a, b uint8) uint8 {
	if a == 0 || b == 0 {
		return 0
	}
	if a < b {
		return a
	}
	return b
}

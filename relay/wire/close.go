package wire

// CloseCode is a relay-level, application close reason for the daemon<->hub
// protocol, distinct from the underlying yamux/websocket transport close codes.
// It is carried in a Bye message or a websocket close frame. Values start at
// 4000 to stay clear of the RFC 6455 reserved/registered ranges.
type CloseCode uint16

const (
	CloseNormal                CloseCode = 4000 // graceful, no error
	CloseProtocolError         CloseCode = 4001 // malformed frame or header
	CloseUnsupportedVersion    CloseCode = 4002 // no common ProtoVersion
	CloseUnknownStreamKind     CloseCode = 4003 // StreamOpen.Kind not recognized
	CloseWebTerminatedDisabled CloseCode = 4004 // daemon has relay.allow_web_terminated=false
	CloseUnauthorized          CloseCode = 4005 // nonce signature failed or scope denied
	CloseEnrollmentRequired    CloseCode = 4006 // daemon not enrolled / cert unknown
	CloseInternal              CloseCode = 4007 // unexpected server-side error
)

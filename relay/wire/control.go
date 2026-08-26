package wire

import (
	"encoding/json"
	"io"
	"time"
)

// ControlType tags a control-stream message.
type ControlType string

const (
	ControlHello      ControlType = "hello"
	ControlHeartbeat  ControlType = "heartbeat"
	ControlBye        ControlType = "bye"
	ControlConfigPush ControlType = "config_push"
)

// Hello is the first control message, sent by the daemon right after it opens
// the control stream. It advertises the daemon's proto version, its hub-minted
// id, and its capability list (mirrors the daemon's GET /capabilities response).
type Hello struct {
	ProtoVersion uint8    `json:"proto_version"`
	DaemonID     string   `json:"daemon_id"`
	Caps         []string `json:"caps,omitempty"`
}

// Heartbeat is sent by the daemon on a timer to keep presence fresh. LastSeen is
// the daemon's own clock at send time; the hub records arrival independently and
// need not trust it for ordering.
type Heartbeat struct {
	LastSeen time.Time `json:"last_seen"`
}

// Bye is a graceful shutdown/close notice from either side, optionally carrying
// a CloseCode-derived reason.
type Bye struct {
	Code   CloseCode `json:"code,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// ConfigPush is the hub->daemon config channel on the control stream (e.g.
// toggling relay policy). Settings are opaque string key/values the daemon
// interprets; unknown keys are ignored so the hub can add settings without a
// proto bump.
type ConfigPush struct {
	Settings map[string]string `json:"settings,omitempty"`
}

// ControlMessage is the JSON envelope carried in a control frame. Exactly one of
// the payload pointers is set, matching Type.
type ControlMessage struct {
	Type       ControlType `json:"type"`
	Hello      *Hello      `json:"hello,omitempty"`
	Heartbeat  *Heartbeat  `json:"heartbeat,omitempty"`
	Bye        *Bye        `json:"bye,omitempty"`
	ConfigPush *ConfigPush `json:"config_push,omitempty"`
}

// WriteControl marshals m to JSON and writes it as one frame.
func WriteControl(w io.Writer, m ControlMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return WriteFrame(w, b)
}

// ReadControl reads one control frame and unmarshals it.
func ReadControl(r io.Reader) (ControlMessage, error) {
	b, err := ReadFrame(r)
	if err != nil {
		return ControlMessage{}, err
	}
	var m ControlMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return ControlMessage{}, err
	}
	return m, nil
}

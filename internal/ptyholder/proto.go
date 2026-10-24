package ptyholder

import (
	"encoding/json"
	"errors"
	"time"
)

// ProtoVersion is the holder wire protocol version. A mismatch is refused with
// ErrProto on either side; the downgrade boundary is documented in the runbook.
const ProtoVersion = 1

// Errors.
var (
	// ErrGone: no holder answers (it exited or was killed). The process may
	// still exist but nothing warden-owned can adopt it.
	ErrGone = errors.New("ptyholder: holder is not running")
	// ErrAuth: the holder refused the credential or the peer uid.
	ErrAuth = errors.New("ptyholder: authentication refused")
	// ErrIdentity: the holder or the child is not the process that was recorded
	// (pid reuse, a foreign listener on the socket).
	ErrIdentity = errors.New("ptyholder: process identity mismatch; refusing to adopt")
	// ErrProto: protocol version mismatch.
	ErrProto = errors.New("ptyholder: protocol version mismatch")
	// ErrInsecure: the socket or state directory permissions are wrong.
	ErrInsecure = errors.New("ptyholder: insecure socket or state directory")
)

// Owner identifies an opt-in test scope. Production holders have no Owner and
// deliberately survive their launcher. PID plus start time prevents PID reuse
// from keeping abandoned fixtures alive.
type Owner struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

func (o Owner) alive() bool {
	st, err := StartTime(o.PID)
	return o.PID > 0 && o.Start != 0 && err == nil && st == o.Start
}

// Spec is what a holder is started with (a 0600 file removed once read, because
// Env may carry secrets).
type Spec struct {
	TestOwner  *Owner   `json:"test_owner,omitempty"`
	ID         string   `json:"id"`
	Root       string   `json:"root"`
	Argv       []string `json:"argv"`
	Cwd        string   `json:"cwd,omitempty"`
	Env        []string `json:"env,omitempty"`
	Cols       uint16   `json:"cols,omitempty"`
	Rows       uint16   `json:"rows,omitempty"`
	Credential string   `json:"credential"`
	Socket     string   `json:"socket"`
	// ExitedLinger is how long a holder outlives its child waiting for a daemon
	// to collect the exit status (default 24h).
	ExitedLinger time.Duration `json:"exited_linger,omitempty"`
}

// Meta is the holder's public identity, written 0600 after it is listening.
// The daemon compares what it adopts against it.
type Meta struct {
	CleanupError string `json:"cleanup_error,omitempty"`
	TestOwner    *Owner `json:"test_owner,omitempty"`
	Proto        int    `json:"proto"`
	ID           string `json:"id"`
	HolderPID    int    `json:"holder_pid"`
	HolderStart  uint64 `json:"holder_start"`
	ChildPID     int    `json:"child_pid"`
	ChildStart   uint64 `json:"child_start"`
	Socket       string `json:"socket"`
}

type request struct {
	Op    string `json:"op"` // adopt | wait | release
	Proto int    `json:"proto"`
	ID    string `json:"id"`
	Cred  string `json:"cred"`
}

type reply struct {
	OK     bool   `json:"ok"`
	Err    string `json:"err,omitempty"`
	Proto  int    `json:"proto,omitempty"`
	PID    int    `json:"pid,omitempty"`
	Start  uint64 `json:"start,omitempty"`
	Exited bool   `json:"exited,omitempty"`
	Code   int    `json:"code,omitempty"`
	Signal string `json:"signal,omitempty"`
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

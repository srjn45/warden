package wardendb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/srjn45/warden/internal/audit"
)

// Lease bounds. A zero ttl selects the default.
const (
	DefaultLeaseTTL = 5 * time.Minute
	MaxLeaseTTL     = 24 * time.Hour
)

// Lease errors. Each is wrapped by a *LeaseError.
var (
	// ErrLeaseHeld: another holder has a live lease.
	ErrLeaseHeld = errors.New("wardendb: operation lease is held")
	// ErrLeaseFenced: the lease was superseded (taken over or released); its
	// holder is stale and may not write.
	ErrLeaseFenced = errors.New("wardendb: operation lease is stale")
	// ErrLeaseExpired: the lease ran out before the write. The holder must
	// re-acquire, which sends a running operation to reconciling.
	ErrLeaseExpired = errors.New("wardendb: operation lease has expired")
	// ErrLeaseRequired: the operation is in a leased state and the write
	// carried no lease.
	ErrLeaseRequired = errors.New("wardendb: operation lease required")
	// ErrNotClaimable: the operation is in a state that takes no lease.
	ErrNotClaimable = errors.New("wardendb: operation is not claimable")
)

// Lease is the proof of a claim: the owner and the fencing epoch it was
// granted at. A write is accepted only while the row still carries both and
// the lease has not expired.
type Lease struct {
	OperationID string
	Owner       string
	Epoch       uint64
	ExpiresAt   time.Time
}

// LeaseError reports a refused lease request or a fenced write.
type LeaseError struct {
	OperationID string
	// Owner and Epoch are those of the refused request; Epoch is zero for an
	// Acquire.
	Owner string
	Epoch uint64
	// Holder, HolderEpoch and ExpiresAt describe the lease on the row.
	Holder      string
	HolderEpoch uint64
	ExpiresAt   time.Time
	// State is set when the state takes no lease (ErrNotClaimable).
	State OperationState
	cause error
}

func (e *LeaseError) Error() string {
	msg := fmt.Sprintf("%v: operation %s", e.cause, e.OperationID)
	if e.State != "" {
		msg += " is " + string(e.State)
	}
	if e.Holder != "" {
		msg += fmt.Sprintf(" (holder %s epoch %d until %s)", e.Holder, e.HolderEpoch, e.ExpiresAt.Format(time.RFC3339))
	}
	return msg
}

// Unwrap exposes the Err* lease sentinel.
func (e *LeaseError) Unwrap() error { return e.cause }

// NewLeaseOwner returns a random lease owner identity.
func NewLeaseOwner() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "lease-" + hex.EncodeToString(raw[:]), nil
}

// checkLease is the fence: l must be the lease the row carries, and live.
func checkLease(op *Operation, l Lease, now time.Time) error {
	e := &LeaseError{
		OperationID: op.ID, Owner: l.Owner, Epoch: l.Epoch,
		Holder: op.LeaseOwner, HolderEpoch: op.LeaseEpoch, ExpiresAt: op.LeaseExpiresAt,
	}
	switch {
	case l.OperationID != op.ID || l.Owner == "" || op.LeaseOwner != l.Owner || op.LeaseEpoch != l.Epoch:
		e.cause = ErrLeaseFenced
	case !now.Before(op.LeaseExpiresAt):
		e.cause = ErrLeaseExpired
	default:
		return nil
	}
	return e
}

func leaseTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		return DefaultLeaseTTL, nil
	}
	if ttl < 0 || ttl > MaxLeaseTTL {
		return 0, invalid("lease ttl %s outside (0, %s]", ttl, MaxLeaseTTL)
	}
	return ttl, nil
}

// fenced reports a refused stale or expired write to the handle's sinks.
func (j *Journal) fenced(e *LeaseError) {
	m := j.db.mon
	if m.metrics != nil {
		m.metrics.Inc(ActionOperationFenced, OperationsCollection)
	}
	if m.auditor == nil {
		return
	}
	reason := "stale"
	if errors.Is(e, ErrLeaseExpired) {
		reason = "expired"
	}
	m.auditor.Log(audit.Event{Time: j.now(), Action: ActionOperationFenced, Actor: "wardendb", Target: e.OperationID, Detail: map[string]string{
		"reason":       reason,
		"lease_owner":  e.Owner,
		"lease_epoch":  strconv.FormatUint(e.Epoch, 10),
		"holder":       e.Holder,
		"holder_epoch": strconv.FormatUint(e.HolderEpoch, 10),
	}})
}

// Acquire grants owner the lease on the operation for ttl (DefaultLeaseTTL
// when zero) and returns it with the updated operation. An empty owner gets a
// random identity. Every grant raises the fencing epoch, so an earlier holder
// can no longer write.
//
//   - ready: the operation is claimed — it moves to running and Attempt grows;
//   - running, reconciling, compensating with a live lease: refused with
//     ErrLeaseHeld, also for the same owner (use Renew);
//   - the same states without a live lease (expired, or released): taken over.
//     A running operation moves to reconciling, because an expired lease does
//     not prove that no effect was issued and it must never be blindly rerun;
//   - any other state: refused with ErrNotClaimable.
func (j *Journal) Acquire(ctx context.Context, id, owner string, ttl time.Duration, stages ...TxStage) (Lease, Operation, error) {
	ttl, err := leaseTTL(ttl)
	if err != nil {
		return Lease{}, Operation{}, err
	}
	if owner == "" {
		if owner, err = NewLeaseOwner(); err != nil {
			return Lease{}, Operation{}, fmt.Errorf("wardendb: mint lease owner: %w", err)
		}
	}
	if err := cleanToken("lease owner", owner, true); err != nil {
		return Lease{}, Operation{}, err
	}
	op, err := j.mutate(ctx, id, "acquire", stages, func(op *Operation, now time.Time) (*change, error) {
		from, event, detail := op.State, OperationEventAcquire, ""
		switch {
		case from == OperationReady:
			op.State = OperationRunning
			op.Attempt++
			op.Step, op.Checkpoint = "", nil
			event = OperationEventClaim
		case from.Leased():
			if op.LeaseLive(now) {
				return nil, &LeaseError{
					OperationID: op.ID, Owner: owner, Holder: op.LeaseOwner,
					HolderEpoch: op.LeaseEpoch, ExpiresAt: op.LeaseExpiresAt, cause: ErrLeaseHeld,
				}
			}
			if op.LeaseEpoch > 0 {
				event = OperationEventTakeover
				detail = "lease released"
				if op.LeaseOwner != "" {
					detail = "lease of " + op.LeaseOwner + " expired at " + op.LeaseExpiresAt.Format(time.RFC3339Nano)
				}
			}
			if from == OperationRunning {
				op.State = OperationReconciling
			}
		default:
			return nil, &LeaseError{OperationID: op.ID, Owner: owner, State: from, cause: ErrNotClaimable}
		}
		op.LeaseEpoch++
		op.LeaseOwner = owner
		op.LeaseExpiresAt = now.Add(ttl)
		return &change{ActionOperationLease, record(op, now, event, from, owner, detail)}, nil
	})
	if err != nil {
		return Lease{}, Operation{}, err
	}
	return Lease{OperationID: op.ID, Owner: op.LeaseOwner, Epoch: op.LeaseEpoch, ExpiresAt: op.LeaseExpiresAt}, op, nil
}

// Renew extends a live lease to now+ttl and returns it. A stale or expired
// lease is refused: it cannot be revived, only re-acquired.
func (j *Journal) Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	ttl, err := leaseTTL(ttl)
	if err != nil {
		return Lease{}, err
	}
	op, err := j.mutate(ctx, lease.OperationID, "renew", nil, func(op *Operation, now time.Time) (*change, error) {
		if err := checkLease(op, lease, now); err != nil {
			return nil, err
		}
		op.LeaseExpiresAt = now.Add(ttl)
		return nil, nil
	})
	if err != nil {
		return Lease{}, err
	}
	lease.ExpiresAt = op.LeaseExpiresAt
	return lease, nil
}

// Release gives the lease up without changing state, so another worker can
// take the operation over at once. It is accepted from the current holder
// even after expiry and refused with ErrLeaseFenced from anyone else.
func (j *Journal) Release(ctx context.Context, lease Lease) error {
	_, err := j.mutate(ctx, lease.OperationID, "release", nil, func(op *Operation, now time.Time) (*change, error) {
		if err := checkLease(op, lease, now); err != nil && !errors.Is(err, ErrLeaseExpired) {
			return nil, err
		}
		op.LeaseOwner, op.LeaseExpiresAt = "", time.Time{}
		return &change{ActionOperationLease, record(op, now, OperationEventRelease, op.State, lease.Owner, "")}, nil
	})
	return err
}

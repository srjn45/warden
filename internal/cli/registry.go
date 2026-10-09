package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/ownerlock"
)

// registryAPI is the slice of the backend registry the `models` / `role`
// commands use. It is satisfied by *backendstore.Store (offline: direct open
// under the CLI ownership lock) and by daemonRegistry (a daemon owns the data
// dir, so the operation goes through its API — the daemon is the single writer).
type registryAPI interface {
	ListModels(tier backendstore.ModelTier) ([]backendstore.ModelEntry, error)
	SetModelTier(backendID, modelID string, tier backendstore.ModelTier) error
	AddModel(backendID, modelID, displayName string, tier backendstore.ModelTier, autoAssign bool, quotaScope string) error
	ListRoleTiers() ([]backendstore.RoleTierMapping, error)
	SetRoleTier(role string, tier backendstore.ModelTier) error
	Close() error
}

// openRegistry picks the mode for a CLI command: daemon-owned data dir → daemon
// API; otherwise the direct open (openBackendStore). Output is identical in
// both modes because both return the same types.
// probeDataDirOwner is a seam so tests never probe a real data dir.
var probeDataDirOwner = ownerlock.Probe

func openRegistry(cmd *cobra.Command) (registryAPI, error) {
	cfg := config.Load(configPathFor(cmd))
	oe, err := probeDataDirOwner(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if oe != nil && (oe.Owner == nil || oe.Owner.Kind == ownerlock.KindDaemon) {
		return &daemonRegistry{c: clientFor(cmd), owner: oe}, nil
	}
	return openBackendStore(cmd)
}

// daemonRegistry routes registry operations through the daemon's REST API.
type daemonRegistry struct {
	c     *client.Client
	owner *ownerlock.OwnedError
}

func (d *daemonRegistry) Close() error { return nil }

// wrap maps daemon status errors back to the store's sentinel errors (so CLI
// error handling is mode-independent) and turns a transport failure into the
// actionable ownership guidance: the daemon owns the dir but is unreachable.
func (d *daemonRegistry) wrap(err error) error {
	if err == nil {
		return nil
	}
	var se *client.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case 404:
			return backendstore.ErrModelNotFound
		case 409:
			return backendstore.ErrExists
		case 400:
			if se.Msg != "" {
				return errors.New(se.Msg)
			}
		}
		return err
	}
	return fmt.Errorf("%w\nthe daemon owns the data directory but is not reachable (%v); check --addr / config addr, or stop the daemon to use the CLI directly", d.owner, err)
}

func (d *daemonRegistry) ListModels(t backendstore.ModelTier) ([]backendstore.ModelEntry, error) {
	out, err := d.c.ListModels(context.Background(), string(t))
	return out, d.wrap(err)
}

func (d *daemonRegistry) SetModelTier(b, m string, t backendstore.ModelTier) error {
	_, err := d.c.SetModelTier(context.Background(), b, m, string(t))
	return d.wrap(err)
}

func (d *daemonRegistry) AddModel(b, m, display string, t backendstore.ModelTier, auto bool, scope string) error {
	_, err := d.c.AddModel(context.Background(), b, m, display, string(t), auto, scope)
	return d.wrap(err)
}

func (d *daemonRegistry) ListRoleTiers() ([]backendstore.RoleTierMapping, error) {
	out, err := d.c.ListRoleTiers(context.Background())
	return out, d.wrap(err)
}

func (d *daemonRegistry) SetRoleTier(role string, t backendstore.ModelTier) error {
	_, err := d.c.SetRoleTier(context.Background(), role, string(t))
	return d.wrap(err)
}

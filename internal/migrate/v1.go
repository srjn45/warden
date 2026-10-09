package migrate

import (
	"context"
	"io"
	"os"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/legacyimport"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
)

const Migration0001ID = "0001-legacy-import"

func init() {
	_ = Register(Migration0001)
}

// Migration0001 ports all pre-ledger boot-time importers into an explicit,
// idempotent, journaled sequence, then retires their sentinel files.
var Migration0001 = Migration{
	ID:   Migration0001ID,
	From: 0,
	To:   1,
	Kind: KindData,
	Check: func(env Env) []Finding {
		var findings []Finding
		checkStore := func(name string, imp legacyimport.Importer) {
			if ok, err := imp.Present(env.DataDir); err == nil && ok {
				findings = append(findings, Finding{
					Severity: SeverityAuto,
					Store:    name,
					Message:  "legacy " + name + " data found; will import into ScrivaDB",
				})
			}
		}
		checkStore("sessions", store.LegacyImport)
		checkStore("agents", agentstore.LegacyImport)
		checkStore("terminals", terminalstore.LegacyImport)
		checkStore("pipelines", pipeline.LegacyImport)
		checkStore("schedules", schedule.LegacyImport)
		checkStore("snapshots", snapshot.LegacyImport)
		checkStore("autopilot_runs", autopilotstore.LegacyImport)
		return findings
	},
	Steps: []Step{
		{
			Name: "sessions",
			Run: func(env Env) error {
				return store.LegacyImport.Import(env.DataDir)
			},
		},
		{
			Name: "agents",
			Run: func(env Env) error {
				return agentstore.LegacyImport.Import(env.DataDir)
			},
		},
		{
			Name: "terminals",
			Run: func(env Env) error {
				return terminalstore.LegacyImport.Import(env.DataDir)
			},
		},
		{
			Name: "pipelines",
			Run: func(env Env) error {
				return pipeline.LegacyImport.Import(env.DataDir)
			},
		},
		{
			Name: "schedules",
			Run: func(env Env) error {
				return schedule.LegacyImport.Import(env.DataDir)
			},
		},
		{
			Name: "snapshots",
			Run: func(env Env) error {
				return snapshot.LegacyImport.Import(env.DataDir)
			},
		},
		{
			Name: "autopilot-runs",
			Run: func(env Env) error {
				return autopilotstore.LegacyImport.Import(env.DataDir)
			},
		},
		{
			Name: "backend-ladder",
			Run: func(env Env) error {
				var free, sub, ppu []string
				allowPaid := false
				if env.Config != nil {
					ladder := env.Config.AutopilotBrainBackends()
					free, sub, ppu = ladder.Free, ladder.Subscription, ladder.PayPerUse
					allowPaid = env.Config.AutopilotAllowPayPerUse()
				}
				return backendstore.LegacyImport(env.DataDir, free, sub, ppu, allowPaid)
			},
		},
		{
			Name: "autopilot-plans",
			Run: func(env Env) error {
				if env.Config != nil {
					ctx := env.Context
					if ctx == nil {
						ctx = context.Background()
					}
					baseDir, _ := os.Getwd()
					_, _ = autopilot.MigrateLegacyPlans(ctx, autopilot.NewExecEnv(), nil, env.Config.AutopilotPlanFiles(), baseDir, io.Discard)
				}
				return nil
			},
		},
		{
			Name: "retire-sentinels",
			Run: func(env Env) error {
				return RetireSentinels(env.DataDir, schema.LegacySentinels)
			},
		},
	},
	Verify: func(env Env) error {
		if err := store.LegacyImport.Verify(env.DataDir); err != nil {
			return err
		}
		if err := agentstore.LegacyImport.Verify(env.DataDir); err != nil {
			return err
		}
		if err := terminalstore.LegacyImport.Verify(env.DataDir); err != nil {
			return err
		}
		if err := pipeline.LegacyImport.Verify(env.DataDir); err != nil {
			return err
		}
		if err := schedule.LegacyImport.Verify(env.DataDir); err != nil {
			return err
		}
		if err := snapshot.LegacyImport.Verify(env.DataDir); err != nil {
			return err
		}
		if err := autopilotstore.LegacyImport.Verify(env.DataDir); err != nil {
			return err
		}
		return nil
	},
}

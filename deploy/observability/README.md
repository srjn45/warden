# PTY runtime observability assets

- `alerts.yaml` - Prometheus alert rules over the `warden_pty_*` series.
- `dashboard.json` - Grafana dashboard.

Scrape `GET /api/v1/pty/metrics.prom` (bearer token). Labels are closed vocabularies; none
carries prompt text, paths or secrets. See [docs/PTY_OBSERVABILITY.md](../../docs/PTY_OBSERVABILITY.md).

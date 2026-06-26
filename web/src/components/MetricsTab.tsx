// MetricsTab is a placeholder this phase (navigation-shell only). Later phases
// flesh it out with per-agent CPU / memory / context charts, fleet-size trend,
// and tokens-saved (spec §4.4). The route, tab, and mount point exist now so the
// shell is complete and the charts can drop in without further wiring.
export default function MetricsTab() {
  return (
    <div className="detail empty">
      Metrics — per-agent CPU, memory &amp; context, fleet size, and tokens saved
      land here in a later phase.
    </div>
  );
}

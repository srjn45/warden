import type { Status } from '../lib/types';
import { busyIdle } from '../lib/status';

export default function BusyIdleBadge({ status, exitCode }: { status: Status; exitCode?: number | null }) {
  const b = busyIdle(status, exitCode);
  return <span className={`badge ${b.kind}`} title={status}>{b.label}</span>;
}

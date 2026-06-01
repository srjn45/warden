import type { Status } from '../lib/types';
import { busyIdle } from '../lib/status';

export default function BusyIdleBadge({ status }: { status: Status }) {
  const b = busyIdle(status);
  return <span className={`badge ${b.kind}`} title={status}>{b.label}</span>;
}

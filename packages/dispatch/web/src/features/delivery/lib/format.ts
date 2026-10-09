// The number formats the measures panel and the Pipeline page share, ported from the prototype:
// `formatMinutes` from web/src/lib/dora.ts:34-40, `formatDuration` and `formatRate` from
// web/src/lib/pipeline.ts:60-83, and `pct` from web/src/components/DoraPanel.tsx:13-15. Pure.

import { formatMinutes as formatLeadTime } from "./prList";

/** Minutes as the measures panel shows durations: the PR list's buckets ("45m", "3.5h", "2.1d"),
 *  and "n/a" when there is nothing to measure. */
export function formatMinutes(v: number | null): string {
  return v === null ? "n/a" : formatLeadTime(v);
}

/** Minutes as the Pipeline page shows them: "4.6m" under ten minutes, "93m" under three hours,
 *  then hours ("3.2h"); "n/a" when there is nothing to measure. */
export function formatDuration(minutes: number | null): string {
  if (minutes === null) return "n/a";
  if (minutes < 10) return `${minutes.toFixed(1)}m`;
  if (minutes < 180) return `${Math.round(minutes)}m`;
  return `${(minutes / 60).toFixed(1)}h`;
}

/** A success rate as a whole percentage; "n/a" when nothing ended in success or failure. */
export function formatRate(rate: number | null): string {
  return rate === null ? "n/a" : `${Math.round(rate * 100)}%`;
}

/** A share as a percentage to `digits` decimals: "4.96%". */
export function pct(v: number, digits = 1): string {
  return `${(v * 100).toFixed(digits)}%`;
}

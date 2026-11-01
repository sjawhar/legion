// A step line of merged PRs not yet in production, so blockages show. Pure: takes the population
// of tracked merges (already facet-filtered) and a window, and returns cumulative-count points
// clipped to that window.
import type { DeliveryPR, DeliveryWaitingPR } from "../../../api/types";

export interface WaitingPoint {
  at: string; // ISO
  count: number;
}

/** A merge the line counts: when it merged and when it shipped, null while it waits. */
export type WaitingMerge = Pick<DeliveryPR, "merged_at" | "deployed_at"> | DeliveryWaitingPR;

interface WaitingEvent {
  at: number; // epoch ms, for sorting/clipping
  atIso: string;
  delta: 1 | -1;
}

/** The line over `window`. `merges` must hold every merge the line could count, not only those
 *  inside `window`: the server's `waiting` (merged before the read window, not shipped by its
 *  start) and every tracked merge of the read window, whatever the brush. A merge before the
 *  window's start that is still waiting there raises the line's first point, so the brush narrows
 *  the line's time range and never its population. */
export function waitingSeries(
  merges: readonly WaitingMerge[],
  window: { start: string; end: string }
): WaitingPoint[] {
  const startMs = new Date(window.start).getTime();
  const endMs = new Date(window.end).getTime();

  const events: WaitingEvent[] = [];
  for (const merge of merges) {
    if (merge.merged_at === null) continue;
    const mergedMs = new Date(merge.merged_at).getTime();
    if (mergedMs <= endMs) events.push({ at: mergedMs, atIso: merge.merged_at, delta: 1 });
    if (merge.deployed_at !== null) {
      const deployedMs = new Date(merge.deployed_at).getTime();
      if (deployedMs <= endMs) events.push({ at: deployedMs, atIso: merge.deployed_at, delta: -1 });
    }
  }
  events.sort((a, b) => a.at - b.at);

  const points: WaitingPoint[] = [{ at: window.start, count: 0 }];
  let lastMs = startMs;
  let count = 0;
  for (const event of events) {
    count += event.delta;
    if (event.at < startMs) {
      // Before the window: what is still waiting at its start is where the line begins.
      points[0] = { at: window.start, count: Math.max(count, 0) };
      continue;
    }
    points.push({ at: event.atIso, count });
    lastMs = event.at;
  }

  // Compared as instants: the server's timestamps and a brush's toISOString() spell one instant
  // differently (fractional seconds, offset), so their strings never match.
  if (points.length === 1 || lastMs !== endMs) points.push({ at: window.end, count });
  return points;
}

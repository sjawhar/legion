// A step line of merged PRs not yet in production, so blockages show. Pure: takes a PR population
// (already facet-filtered) and a window, returns cumulative-count points clipped to that window.
import type { DeliveryPR } from "../../../api/types";

export interface WaitingPoint {
  at: string; // ISO
  count: number;
}

interface WaitingEvent {
  at: number; // epoch ms, for sorting/clipping
  atIso: string;
  delta: 1 | -1;
}

export function waitingSeries(
  prs: readonly DeliveryPR[],
  window: { start: string; end: string }
): WaitingPoint[] {
  const startMs = new Date(window.start).getTime();
  const endMs = new Date(window.end).getTime();

  const events: WaitingEvent[] = [];
  for (const pr of prs) {
    if (pr.merged_at === null) continue;
    const mergedMs = new Date(pr.merged_at).getTime();
    if (mergedMs <= endMs) events.push({ at: mergedMs, atIso: pr.merged_at, delta: 1 });
    if (pr.deployed_at !== null) {
      const deployedMs = new Date(pr.deployed_at).getTime();
      if (deployedMs <= endMs) events.push({ at: deployedMs, atIso: pr.deployed_at, delta: -1 });
    }
  }
  events.sort((a, b) => a.at - b.at);

  const points: WaitingPoint[] = [{ at: window.start, count: 0 }];
  let count = 0;
  for (const event of events) {
    count += event.delta;
    if (event.at < startMs) {
      // A PR merged before a brush window's start still counts while it waits.
      points[0] = { at: window.start, count: Math.max(count, 0) };
      continue;
    }
    points.push({ at: event.atIso, count });
  }

  const lastPoint = points[points.length - 1];
  if (lastPoint === undefined || lastPoint.at !== window.end) {
    points.push({ at: window.end, count });
  }
  return points;
}

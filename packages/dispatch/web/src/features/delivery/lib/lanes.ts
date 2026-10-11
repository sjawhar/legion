// The timeline's columns with swimlanes on, and the brush window a drag sets. Pure: no React, no
// ECharts.
import type { DeliveryComponent, DeliveryPR } from "../../../api/types";
import { type ColorFacet, colorKeyFor } from "./colorScale";
import { type Filters, PLACEHOLDER_LABELS, shortRepoLabel } from "./facets";

// Layout constants shared between the echarts grid and the plain-HTML lane header row above it,
// so lane columns line up exactly between the two. Wide enough for hour-level axis labels
// ("Sep 26 18:00") once zoomed in.
export const LEFT_AXIS_WIDTH = 80;
export const RIGHT_SLIDER_WIDTH = 44;

// Real-value lanes are sized to the chart's current width at MIN_LANE_WIDTH_PX per lane; past
// that budget the smallest ones fold into one "Other" lane rather than squeezing every lane below
// a readable width. Placeholder lanes ("No issue" etc., see lib/facets.ts) and "Other" always sort
// after every real value, so the biggest groups shown are never a placeholder.
export const MIN_LANE_WIDTH_PX = 76;

/** The key of the lane the folded values share. */
export const OTHER_LANE = "__other__";
/** The key of the one merges column when swimlanes are off. */
export const MERGES_LANE = "__merges__";

export interface Lane {
  key: string; // colorKeyFor() value, OTHER_LANE for the folded lane, or MERGES_LANE when lanes are off
  label: string;
  count: number;
}

export interface LaneOptions {
  colorBy: ColorFacet;
  lanes: boolean;
  filters: Filters;
  /** The chart's width in pixels, which sets how many lanes fit. */
  width: number;
  components: Readonly<Record<string, DeliveryComponent>>;
}

/** The merges' columns: one "Merges" column with swimlanes off; with them on, one per value of
 *  `colorBy` (priority in P0-P3 order, every other facet busiest first), as many as fit `width`
 *  beside the global Deploys/fails column, the rest folded into "Other", then one per placeholder
 *  value ("No issue" and the like), always shown and always last. A facet with values selected in
 *  its own filter shows exactly those, never folded. */
export function foldLanes(prs: readonly DeliveryPR[], options: LaneOptions): Lane[] {
  const { colorBy, lanes, filters, width, components } = options;
  if (!lanes) return [{ key: MERGES_LANE, label: "Merges", count: prs.length }];
  const counts = new Map<string, number>();
  for (const pr of prs) {
    const key = colorKeyFor(pr, colorBy);
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  const allKeys = Array.from(counts.keys());
  const laneLabel = (key: string): string => {
    const placeholder = PLACEHOLDER_LABELS[key];
    if (placeholder !== undefined) return placeholder;
    if (colorBy === "component") return components[key]?.title ?? key;
    if (colorBy === "repo") return shortRepoLabel(allKeys, key);
    return key;
  };
  const byPriorityThenCount = (a: [string, number], b: [string, number]) =>
    colorBy === "priority" ? a[0].localeCompare(b[0]) : b[1] - a[1];
  const realEntries = Array.from(counts.entries())
    .filter(([key]) => !(key in PLACEHOLDER_LABELS))
    .sort(byPriorityThenCount);
  const placeholderEntries = Array.from(counts.entries())
    .filter(([key]) => key in PLACEHOLDER_LABELS)
    .sort((a, b) => b[1] - a[1]);

  // The slicing by a facet is the point of selecting in it: show the slice, never a summary.
  const skipFold = filters[colorBy].length > 0;

  // One slot goes to the Deploys/fails column and one to each placeholder lane.
  const available = Math.max(0, width - LEFT_AXIS_WIDTH - RIGHT_SLIDER_WIDTH - MIN_LANE_WIDTH_PX);
  const totalBudget = Math.max(1, Math.floor(available / MIN_LANE_WIDTH_PX));
  const budgetForReal = Math.max(1, totalBudget - placeholderEntries.length);

  let shownReal = realEntries;
  let otherCount: number | null = null;
  if (!skipFold && realEntries.length > budgetForReal) {
    // "Other" takes one of the budget's lanes, so one fewer real value shows beside it: none at
    // all when the budget is a single lane.
    shownReal = realEntries.slice(0, budgetForReal - 1);
    otherCount = realEntries.slice(shownReal.length).reduce((sum, [, count]) => sum + count, 0);
  }

  const result: Lane[] = shownReal.map(([key, count]) => ({ key, label: laneLabel(key), count }));
  if (otherCount !== null) result.push({ key: OTHER_LANE, label: "Other", count: otherCount });
  for (const [key, count] of placeholderEntries) {
    result.push({ key, label: laneLabel(key), count });
  }
  return result;
}

/** The brush window (the page's `ws`/`we`) between two time-axis values a drag spans, earlier
 *  first, or null when the chart could not place either end on the axis. */
export function brushWindow(t0: unknown, t1: unknown): { start: string; end: string } | null {
  if (typeof t0 !== "number" || typeof t1 !== "number") return null;
  if (Number.isNaN(t0) || Number.isNaN(t1)) return null;
  return {
    start: new Date(Math.min(t0, t1)).toISOString(),
    end: new Date(Math.max(t0, t1)).toISOString(),
  };
}

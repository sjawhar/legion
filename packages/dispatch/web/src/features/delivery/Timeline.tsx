import type { ECharts } from "echarts";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";

import type { DeliveryComponent, DeliveryPR, DeliveryRun } from "../../api/types";
import { borderDefault, brushBand, surfaceBg, textMutedOnCanvas } from "../../theme/classes";
import { useMediaQuery } from "../shell/useDialog";
import { type ColorFacet, colorKeyFor, PLACEHOLDER_COLOR } from "./lib/colorScale";
import { type Filters, PLACEHOLDER_LABELS, shortRepoLabel } from "./lib/facets";
import { waitingSeries } from "./lib/waiting";

// echarts is loaded only here, dynamically, so it ships with the /delivery route's own chunk and
// never the app shell — no other module in this feature imports it.

// Layout constants shared between the echarts grid and the plain-HTML lane header row above it,
// so lane columns line up exactly between the two. Wide enough for hour-level axis labels
// ("Sep 26 18:00") once zoomed in.
const LEFT_AXIS_WIDTH = 80;
const RIGHT_SLIDER_WIDTH = 44;
const GLOBAL_DEPLOY_X = 0.15;
const GLOBAL_FAILURE_X = 0.85;
const GLOBAL_WAITING_X0 = 0.15;
const GLOBAL_WAITING_X1 = 0.85;
const INITIAL_VISIBLE_MS = 5 * 86_400_000; // ~5 days on first paint

// Real-value lanes are sized to the chart's current width at MIN_LANE_WIDTH_PX per lane; past
// that budget the smallest ones fold into one "Other" lane rather than squeezing every lane below
// a readable width. Placeholder lanes ("No issue" etc., see lib/facets.ts) and "Other" always sort
// after every real value, so the biggest groups shown are never a placeholder.
const MIN_LANE_WIDTH_PX = 76;

// The chart draws on a canvas, so its colours are values, not Tailwind classes: the prototype's
// own dark palette, and its light counterpart for a light colour scheme.
const CHART_COLORS = {
  dark: { axis: "#334155", label: "#94a3b8", split: "#1e293b", outline: "#0f172a" },
  light: { axis: "#cbd5e1", label: "#64748b", split: "#e2e8f0", outline: "#ffffff" },
};
const DEPLOY_COLOR = "#22c55e";
const FAILURE_COLOR = "#ef4444";
const WAITING_COLOR = "#f59e0b";

export type TimelineSelection =
  | { kind: "pr"; id: string }
  | { kind: "deploy"; id: number }
  | { kind: "failure"; id: number };

interface Props {
  prs: readonly DeliveryPR[];
  runs: readonly DeliveryRun[];
  window: { start: string; end: string };
  colorBy: ColorFacet;
  colorScale: Map<string, string>;
  lanes: boolean;
  filters: Filters;
  components: Readonly<Record<string, DeliveryComponent>>;
  onSelect: (selection: TimelineSelection) => void;
  onBrush: (range: { start: string; end: string }) => void;
}

interface Lane {
  key: string; // colorKeyFor() value, "__other__" for the folded lane, or "__merges__" when lanes are off
  label: string;
  count: number;
}

/** A point the chart was handed, as a click or the tooltip reads it back: what it is, its id, and
 *  its tooltip text. */
type ChartPoint =
  | { kind: "pr"; id: string; tip: string }
  | { kind: "deploy" | "failure"; id: number; tip: string };

/** The `ChartPoint` echarts hands back for a clicked or hovered item, or null for anything else
 *  (the waiting line's points, which carry no kind). */
function chartPoint(data: unknown): ChartPoint | null {
  if (typeof data !== "object" || data === null) return null;
  if (!("kind" in data) || !("id" in data) || !("tip" in data)) return null;
  const { kind, id, tip } = data;
  if (typeof tip !== "string") return null;
  if (kind === "pr" && typeof id === "string") return { kind, id, tip };
  if ((kind === "deploy" || kind === "failure") && typeof id === "number") return { kind, id, tip };
  return null;
}

// Deterministic per-PR jitter so merges sharing a timestamp/lane don't stack exactly on top of
// one another — not cryptographic, just spread.
function jitterFor(id: string): number {
  let hash = 0;
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) >>> 0;
  return (hash % 100) / 100; // 0..1
}

/** The delivery timeline: time runs down the left axis, newest at the top. One column holds the
 *  successful production deploys (sized by the PRs they shipped), the pipeline failures and the
 *  step line of merged PRs waiting for production; the merges, coloured by `colorBy` and rework
 *  drawn as diamonds, fill one more column, or one per value with swimlanes on. A plain wheel pans
 *  through time and Ctrl+wheel zooms; dragging up or down sets a brush window. */
export function Timeline({
  prs,
  runs,
  window: timeWindow,
  colorBy,
  colorScale,
  lanes,
  filters,
  components,
  onSelect,
  onBrush,
}: Props): ReactNode {
  const containerRef = useRef<HTMLDivElement | null>(null);
  // The chart instance is state, not a ref: the data effect below depends on it, so the first
  // render after `echarts.init` resolves (asynchronously, in the mount effect) always runs the
  // data effect at least once with a ready chart.
  const [chart, setChart] = useState<ECharts | undefined>(undefined);
  const encodeHTMLRef = useRef<(text: string) => string>((text) => text);
  const lastWindowKeyRef = useRef<string | null>(null);
  const justBrushedRef = useRef(false);
  const [containerWidth, setContainerWidth] = useState(900);
  // The band drawn while a brush drag is in progress, in chart pixels.
  const [dragBand, setDragBand] = useState<{ y0: number; y1: number } | null>(null);
  const dark = useMediaQuery("(prefers-color-scheme: dark)");
  const palette = dark ? CHART_COLORS.dark : CHART_COLORS.light;

  const windowStartMs = new Date(timeWindow.start).getTime();
  const windowEndMs = new Date(timeWindow.end).getTime();

  // Read inside the mount effect's handlers, which otherwise close over the first render's
  // values forever.
  const boundsRef = useRef({ start: windowStartMs, end: windowEndMs });
  boundsRef.current = { start: windowStartMs, end: windowEndMs };
  const onSelectRef = useRef(onSelect);
  onSelectRef.current = onSelect;
  const onBrushRef = useRef(onBrush);
  onBrushRef.current = onBrush;

  const laneList: Lane[] = useMemo(() => {
    if (!lanes) return [{ key: "__merges__", label: "Merges", count: prs.length }];
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
    // Priority lanes read in priority order (P0 first); every other facet's lanes go busiest first.
    const byPriorityThenCount = (a: [string, number], b: [string, number]) =>
      colorBy === "priority" ? a[0].localeCompare(b[0]) : b[1] - a[1];
    const realEntries = Array.from(counts.entries())
      .filter(([key]) => !(key in PLACEHOLDER_LABELS))
      .sort(byPriorityThenCount);
    const placeholderEntries = Array.from(counts.entries())
      .filter(([key]) => key in PLACEHOLDER_LABELS)
      .sort((a, b) => b[1] - a[1]);

    // A facet with values selected in its own filter shows exactly those values -- no folding
    // into "Other" (the point of slicing by a facet is to see the slice, not a summary of it).
    const skipFold = filters[colorBy].length > 0;

    // Real-value lane count sizes to the chart's current width instead of a fixed cap, reserving
    // one slot for the "Deploys/fails" global column and one per placeholder lane (always shown,
    // always last).
    const available = Math.max(
      0,
      containerWidth - LEFT_AXIS_WIDTH - RIGHT_SLIDER_WIDTH - MIN_LANE_WIDTH_PX
    );
    const totalBudget = Math.max(1, Math.floor(available / MIN_LANE_WIDTH_PX));
    const budgetForReal = Math.max(1, totalBudget - placeholderEntries.length);

    let shownReal = realEntries;
    let otherEntry: [string, number] | null = null;
    if (!skipFold && realEntries.length > budgetForReal) {
      shownReal = realEntries.slice(0, Math.max(1, budgetForReal - 1));
      const folded = realEntries.slice(shownReal.length);
      otherEntry = ["__other__", folded.reduce((sum, [, c]) => sum + c, 0)];
    }

    const result: Lane[] = shownReal.map(([key, count]) => ({
      key,
      label: laneLabel(key),
      count,
    }));
    if (otherEntry) result.push({ key: otherEntry[0], label: "Other", count: otherEntry[1] });
    for (const [key, count] of placeholderEntries) {
      result.push({ key, label: laneLabel(key), count });
    }
    return result;
  }, [prs, colorBy, lanes, filters, containerWidth, components]);

  // Maps every original colorKeyFor() value (including ones folded into "Other") to the lane it's
  // actually drawn in — merges keep their own color regardless, only their x-position groups by
  // this.
  const keyToLaneKey = useMemo(() => {
    const map = new Map<string, string>();
    const shownKeys = new Set(laneList.map((lane) => lane.key));
    for (const pr of prs) {
      const key = colorKeyFor(pr, colorBy);
      map.set(key, shownKeys.has(key) ? key : "__other__");
    }
    return map;
  }, [prs, colorBy, laneList]);

  // +1: lane 0 is the global column.
  const laneIndex = useMemo(
    () => new Map(laneList.map((lane, i) => [lane.key, i + 1] as const)),
    [laneList]
  );

  const totalLanes = laneList.length + 1;

  const deploys = useMemo(
    () => runs.filter((run) => run.production?.conclusion === "success"),
    [runs]
  );
  const failures = useMemo(
    () =>
      runs.filter((run) => run.failed_jobs.length > 0 || run.production?.conclusion === "failure"),
    [runs]
  );
  const maxDeployPRs = Math.max(1, ...deploys.map((run) => run.prs.length));

  const trackedPrs = useMemo(() => prs.filter((pr) => pr.deployed_status !== "not_tracked"), [prs]);
  const waitingPoints = useMemo(
    () => waitingSeries(trackedPrs, timeWindow),
    [trackedPrs, timeWindow]
  );
  const maxWaiting = Math.max(1, ...waitingPoints.map((point) => point.count));

  // ECharts instance lifecycle: init once, resize on container changes, and dispose on unmount.
  // Options (series/axes) are pushed by a separate effect below, keyed on the actual data.
  useEffect(() => {
    let disposed = false;
    let cleanup: (() => void) | undefined;

    void import("echarts").then((echarts) => {
      const el = containerRef.current;
      if (disposed || el === null) return;
      const instance = echarts.init(el, undefined, { renderer: "canvas" });
      encodeHTMLRef.current = echarts.format.encodeHTML;
      setChart(instance);
      setContainerWidth(el.clientWidth);
      const observer = new ResizeObserver(() => {
        instance.resize();
        setContainerWidth(el.clientWidth);
      });
      observer.observe(el);

      instance.on("click", (params) => {
        const point = chartPoint(params.data);
        if (point === null) return;
        onSelectRef.current(
          point.kind === "pr" ? { kind: "pr", id: point.id } : { kind: point.kind, id: point.id }
        );
      });

      // Ctrl+wheel (and trackpad pinch, which Chrome delivers as ctrl+wheel) zooms the time axis
      // around the pointer; a plain wheel keeps panning through time via the "inside" dataZoom's
      // own moveOnMouseWheel below. ECharts checks each wheel behavior independently, so
      // `moveOnMouseWheel: true` fires on every wheel event whatever ctrlKey says, and a
      // config-only `zoomOnMouseWheel: 'ctrl'` would zoom AND pan on every ctrl+wheel. Capturing
      // the event here, before it reaches zrender's own wheel listener, and handling ctrl+wheel
      // entirely ourselves avoids that; preventDefault also stops the browser's page zoom.
      const onWheel = (event: WheelEvent) => {
        if (!event.ctrlKey) return;
        event.preventDefault();
        event.stopPropagation();
        const rect = el.getBoundingClientRect();
        const pointerTime = instance.convertFromPixel({ yAxisIndex: 0 }, event.clientY - rect.top);
        if (typeof pointerTime !== "number" || Number.isNaN(pointerTime)) return;
        const option: unknown = instance.getOption();
        const dataZoomOption =
          option &&
          typeof option === "object" &&
          "dataZoom" in option &&
          Array.isArray(option.dataZoom)
            ? option.dataZoom[0]
            : undefined;
        const { start: boundStart, end: boundEnd } = boundsRef.current;
        const curStart =
          dataZoomOption &&
          typeof dataZoomOption === "object" &&
          typeof dataZoomOption.startValue === "number"
            ? dataZoomOption.startValue
            : boundStart;
        const curEnd =
          dataZoomOption &&
          typeof dataZoomOption === "object" &&
          typeof dataZoomOption.endValue === "number"
            ? dataZoomOption.endValue
            : boundEnd;
        const factor = event.deltaY < 0 ? 1 / 1.15 : 1.15; // scroll up zooms in
        const newStart = Math.max(boundStart, pointerTime - (pointerTime - curStart) * factor);
        const newEnd = Math.min(boundEnd, pointerTime + (curEnd - pointerTime) * factor);
        if (newEnd - newStart < 60_000) return; // never collapse below a minute
        instance.dispatchAction({ type: "dataZoom", startValue: newStart, endValue: newEnd });
      };
      el.addEventListener("wheel", onWheel, { capture: true, passive: false });

      // Brush: manual zrender pointer tracking, not ECharts' built-in brush component, which
      // intercepts every mousedown (it calls preventDefault whether or not a drag starts) and so
      // makes clicking a dot to open its drill-down and setting a brush window mutually
      // exclusive. `convertFromPixel` takes the one y offset an axis-only finder expects.
      let dragStart: { y: number } | null = null;
      const zr = instance.getZr();
      const onPointerDown = (event: { offsetY: number }): void => {
        dragStart = { y: event.offsetY };
      };
      const onPointerMove = (event: { offsetY: number }): void => {
        if (dragStart === null || Math.abs(event.offsetY - dragStart.y) < 4) return;
        setDragBand({ y0: dragStart.y, y1: event.offsetY });
      };
      const onPointerUp = (event: { offsetY: number }): void => {
        const start = dragStart;
        dragStart = null;
        setDragBand(null);
        if (start === null || Math.abs(event.offsetY - start.y) < 4) return; // a click
        const t0 = instance.convertFromPixel({ yAxisIndex: 0 }, start.y);
        const t1 = instance.convertFromPixel({ yAxisIndex: 0 }, event.offsetY);
        if (typeof t0 !== "number" || typeof t1 !== "number") return;
        if (Number.isNaN(t0) || Number.isNaN(t1)) return;
        justBrushedRef.current = true;
        onBrushRef.current({
          start: new Date(Math.min(t0, t1)).toISOString(),
          end: new Date(Math.max(t0, t1)).toISOString(),
        });
      };
      // A drag released outside the chart never reaches zrender's mouseup: cancel it.
      const onWindowMouseUp = (): void => {
        dragStart = null;
        setDragBand(null);
      };
      zr.on("mousedown", onPointerDown);
      zr.on("mousemove", onPointerMove);
      zr.on("mouseup", onPointerUp);
      window.addEventListener("mouseup", onWindowMouseUp);

      cleanup = () => {
        observer.disconnect();
        el.removeEventListener("wheel", onWheel, { capture: true });
        zr.off("mousedown", onPointerDown);
        zr.off("mousemove", onPointerMove);
        zr.off("mouseup", onPointerUp);
        window.removeEventListener("mouseup", onWindowMouseUp);
        instance.dispose();
      };
    });

    return () => {
      disposed = true;
      cleanup?.();
      setChart(undefined);
    };
  }, []);

  // Data/option updates: pushed onto the live instance rather than re-creating the chart, so
  // zoom/pan state survives a filter change.
  useEffect(() => {
    if (chart === undefined) return;
    const encodeHTML = encodeHTMLRef.current;

    const mergeSeriesData = prs
      .filter((pr): pr is DeliveryPR & { merged_at: string } => pr.merged_at !== null)
      .map((pr) => {
        const key = colorKeyFor(pr, colorBy);
        const laneIdx = lanes ? (laneIndex.get(keyToLaneKey.get(key) ?? key) ?? 1) : 1;
        const x = laneIdx + 0.2 + jitterFor(pr.id) * 0.6;
        return {
          value: [x, new Date(pr.merged_at).getTime()],
          id: pr.id,
          kind: "pr" as const,
          tip: `${pr.id}: ${pr.title}`,
          symbol: pr.rework ? "diamond" : "circle",
          symbolSize: pr.rework ? 9 : 7,
          itemStyle: { color: colorScale.get(key) ?? PLACEHOLDER_COLOR },
        };
      });

    const deploySeriesData = deploys.flatMap((run) => {
      const t = run.production?.completed_at ?? run.completed_at;
      if (t === null) return [];
      const size = 6 + (14 - 6) * (Math.sqrt(run.prs.length) / Math.sqrt(maxDeployPRs));
      return [
        {
          value: [GLOBAL_DEPLOY_X, new Date(t).getTime()],
          id: run.id,
          kind: "deploy" as const,
          tip: `Run #${run.id}: ${run.prs.length} PR(s) shipped`,
          symbolSize: size,
        },
      ];
    });

    const failureSeriesData = failures.flatMap((run) => {
      const rootJob = run.root_failing_job;
      const step = rootJob?.name ?? run.production?.conclusion ?? "failure";
      const t = rootJob?.completed_at ?? run.production?.completed_at ?? run.completed_at;
      if (t === null) return [];
      return [
        {
          value: [GLOBAL_FAILURE_X, new Date(t).getTime()],
          id: run.id,
          kind: "failure" as const,
          tip: `Run #${run.id} failed: ${step}`,
        },
      ];
    });

    const waitingSeriesData = waitingPoints.map((point) => [
      GLOBAL_WAITING_X0 + (GLOBAL_WAITING_X1 - GLOBAL_WAITING_X0) * (point.count / maxWaiting),
      new Date(point.at).getTime(),
    ]);

    const initialEnd = windowEndMs;
    const initialStart = Math.max(windowStartMs, initialEnd - INITIAL_VISIBLE_MS);

    // Only reset the visible zoom/pan range when the window itself changes (first mount, or a
    // new brush selection) — a facet/colorBy/lanes toggle or a live refetch re-runs this effect
    // with the same window and must leave the reader's scroll position alone. A brush-set window
    // shows in full (`justBrushedRef`) instead of slicing to the last INITIAL_VISIBLE_MS the way
    // the default window does on first paint.
    const windowKey = `${timeWindow.start}|${timeWindow.end}`;
    const isNewWindow = lastWindowKeyRef.current !== windowKey;
    lastWindowKeyRef.current = windowKey;
    let zoomRange: { startValue?: number; endValue?: number } = {};
    if (isNewWindow) {
      if (justBrushedRef.current) {
        zoomRange = { startValue: windowStartMs, endValue: windowEndMs };
        justBrushedRef.current = false;
      } else {
        zoomRange = { startValue: initialStart, endValue: initialEnd };
      }
    }

    chart.setOption(
      {
        animation: false,
        grid: { left: LEFT_AXIS_WIDTH, right: RIGHT_SLIDER_WIDTH, top: 8, bottom: 8 },
        xAxis: { type: "value", min: 0, max: totalLanes, show: false },
        yAxis: {
          type: "time",
          min: windowStartMs,
          max: windowEndMs,
          axisLine: { lineStyle: { color: palette.axis } },
          // Object form: echarts picks the tick granularity to fit the current zoom level (day
          // ticks zoomed out, hour ticks once zoomed in); this only supplies the format per
          // granularity, so scrolled-back dates stay dated (month + day).
          axisLabel: {
            color: palette.label,
            fontSize: 10,
            formatter: {
              year: "{yyyy}",
              month: "{MMM} {d}",
              day: "{MMM} {d}",
              hour: "{MMM} {d} {HH}:{mm}",
              minute: "{HH}:{mm}",
              second: "{HH}:{mm}:{ss}",
              millisecond: "{HH}:{mm}:{ss}",
              none: "{MMM} {d} {HH}:{mm}",
            },
          },
          splitLine: { lineStyle: { color: palette.split } },
        },
        dataZoom: [
          {
            type: "inside",
            yAxisIndex: 0,
            zoomOnMouseWheel: false,
            moveOnMouseWheel: true,
            moveOnMouseMove: false,
            ...zoomRange,
          },
          {
            type: "slider",
            yAxisIndex: 0,
            right: 4,
            width: 12,
            showDetail: false,
            brushSelect: false,
            ...zoomRange,
          },
        ],
        tooltip: {
          trigger: "item",
          formatter: (params: unknown) => {
            const data =
              typeof params === "object" && params !== null && "data" in params
                ? params.data
                : undefined;
            const point = chartPoint(data);
            return point === null ? "" : encodeHTML(point.tip);
          },
        },
        series: [
          {
            name: "Waiting to deploy",
            type: "line",
            data: waitingSeriesData,
            showSymbol: false,
            step: "end",
            lineStyle: { color: WAITING_COLOR, width: 1.5 },
            silent: true,
            z: 1,
          },
          {
            name: "Deploys",
            type: "scatter",
            data: deploySeriesData,
            itemStyle: { color: DEPLOY_COLOR, borderColor: palette.outline, borderWidth: 1 },
            z: 3,
          },
          {
            name: "Pipeline failures",
            type: "scatter",
            symbol: "triangle",
            data: failureSeriesData,
            itemStyle: { color: FAILURE_COLOR, borderColor: palette.outline, borderWidth: 1 },
            symbolSize: 12,
            z: 3,
          },
          { name: "PR merges", type: "scatter", data: mergeSeriesData, z: 2 },
        ],
      },
      { notMerge: false }
    );
  }, [
    chart,
    prs,
    colorBy,
    lanes,
    laneIndex,
    keyToLaneKey,
    colorScale,
    deploys,
    failures,
    maxDeployPRs,
    waitingPoints,
    maxWaiting,
    windowStartMs,
    windowEndMs,
    totalLanes,
    timeWindow,
    palette,
  ]);

  return (
    <div className="flex min-h-0 w-full flex-1 flex-col select-none">
      {/* Lane header row: plain HTML, not echarts, so labels stay crisp and always visible
          without syncing with the chart's zoom and pan. Padding matches the echarts grid's
          left/right margins so columns line up. */}
      <div
        className={`flex border-b pb-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
        style={{ paddingLeft: LEFT_AXIS_WIDTH, paddingRight: RIGHT_SLIDER_WIDTH }}
      >
        <div
          className="flex-1 px-1 text-center leading-tight"
          style={{ minWidth: MIN_LANE_WIDTH_PX }}
          title="Successful deploys, pipeline failures, waiting-to-deploy PRs"
        >
          Deploys/fails
        </div>
        {laneList.map((lane) => (
          <div
            className="min-w-0 flex-1 px-1 text-center leading-tight"
            key={lane.key}
            style={{ minWidth: MIN_LANE_WIDTH_PX }}
            title={`${lane.label} (${lane.count})`}
          >
            <div className="line-clamp-2 break-words">{lane.label}</div>
            <div className="text-[10px] tabular-nums">{lane.count}</div>
          </div>
        ))}
      </div>
      <div className="relative min-h-0 w-full flex-1">
        <div
          className={`absolute inset-0 touch-none rounded-b-lg ${surfaceBg}`}
          data-testid="delivery-chart"
          ref={containerRef}
        />
        {dragBand === null ? null : (
          <div
            className={`pointer-events-none absolute border-y ${brushBand}`}
            data-testid="delivery-brush-band"
            style={{
              left: LEFT_AXIS_WIDTH,
              right: RIGHT_SLIDER_WIDTH,
              top: Math.min(dragBand.y0, dragBand.y1),
              height: Math.abs(dragBand.y1 - dragBand.y0),
            }}
          />
        )}
      </div>
    </div>
  );
}

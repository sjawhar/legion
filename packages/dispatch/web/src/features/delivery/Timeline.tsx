import type { ECElementEvent, ECharts, EChartsOption } from "echarts";
import { type ReactNode, useEffect, useRef } from "react";

import type { DeliveryPR, DeliveryRun } from "../../api/types";
import { buildColorScale, type ColorFacet, colorKeyFor } from "./lib/colorScale";
import { PLACEHOLDER_LABELS } from "./lib/facets";

// echarts is loaded only here, dynamically, so it ships with the /delivery route's own chunk and
// never the app shell — no other module in this feature imports it.

export type TimelineSelection =
  | { kind: "pr"; id: string }
  | { kind: "deploy"; id: number }
  | { kind: "failure"; id: number };

interface Props {
  prs: readonly DeliveryPR[];
  runs: readonly DeliveryRun[];
  colorBy: ColorFacet;
  onSelect: (selection: TimelineSelection) => void;
  /** Fired when the user zooms/pans the x axis, so the page can refetch a new window. */
  onBrush: (window: { from: string; to: string }) => void;
}

const DEPLOY_COLOR = "#34d399";
const FAILURE_COLOR = "#f87171";
const MIN_DEPLOY_SYMBOL = 8;
const MAX_DEPLOY_SYMBOL = 28;
const DATAZOOM_DEBOUNCE_MS = 300;

/** Up to this many real (non-placeholder) lane values get their own row; the rest fold into one
 *  "Other" lane so the chart stays readable with a high-cardinality facet (e.g. author). */
const MAX_REAL_LANES = 8;

interface Lane {
  key: string;
  label: string;
}

function buildLanes(prs: readonly DeliveryPR[], colorBy: ColorFacet): Lane[] {
  const counts = new Map<string, number>();
  for (const pr of prs) {
    const key = colorKeyFor(pr, colorBy);
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  const real = Array.from(counts.entries())
    .filter(([key]) => !(key in PLACEHOLDER_LABELS))
    .sort((a, b) => b[1] - a[1])
    .map(([key]) => key);
  const placeholders = Array.from(counts.keys()).filter((key) => key in PLACEHOLDER_LABELS);
  const shown = real.slice(0, MAX_REAL_LANES);
  const overflow = real.length > MAX_REAL_LANES;
  const lanes: Lane[] = shown.map((key) => ({ key, label: key }));
  if (overflow) lanes.push({ key: "__other__", label: "Other" });
  for (const key of placeholders) lanes.push({ key, label: PLACEHOLDER_LABELS[key] ?? key });
  return lanes;
}

function laneIndex(lanes: Lane[], key: string): number {
  const index = lanes.findIndex((lane) => lane.key === key);
  return index === -1 ? lanes.length - 1 : index;
}

/** Deploys and pipeline failures sit in two rows above every PR lane. */
function buildOption(
  prs: readonly DeliveryPR[],
  runs: readonly DeliveryRun[],
  colorBy: ColorFacet,
  lanes: Lane[],
  colorScale: Map<string, string>
): EChartsOption {
  const deployLaneY = lanes.length;
  const failureLaneY = lanes.length + 1;
  const laneNames = [...lanes.map((lane) => lane.label), "Deploys", "Failures"];

  const mergeData = prs
    .filter((pr): pr is DeliveryPR & { merged_at: string } => pr.merged_at !== null)
    .map((pr) => ({
      value: [Date.parse(pr.merged_at), laneIndex(lanes, colorKeyFor(pr, colorBy))],
      itemStyle: { color: colorScale.get(colorKeyFor(pr, colorBy)) },
      symbol: pr.rework ? "diamond" : "circle",
      symbolSize: 8,
      name: pr.id,
    }));

  const deployData = runs
    .filter((run) => run.conclusion === "success")
    .map((run) => ({
      value: [Date.parse(run.started_at), deployLaneY],
      itemStyle: { color: DEPLOY_COLOR },
      symbolSize: Math.min(MAX_DEPLOY_SYMBOL, Math.max(MIN_DEPLOY_SYMBOL, run.prs.length * 4)),
      name: String(run.id),
    }));

  const failureData = runs
    .filter((run) => run.conclusion === "failure")
    .map((run) => ({
      value: [Date.parse(run.started_at), failureLaneY],
      itemStyle: { color: FAILURE_COLOR },
      symbolSize: 10,
      name: String(run.id),
    }));

  return {
    animation: false,
    grid: { left: 90, right: 44, top: 24, bottom: 56 },
    tooltip: { trigger: "item" },
    xAxis: { type: "time" },
    yAxis: {
      type: "value",
      min: -0.5,
      max: laneNames.length - 0.5,
      interval: 1,
      axisLabel: { formatter: (value: number) => laneNames[Math.round(value)] ?? "" },
      splitLine: { show: false },
    },
    dataZoom: [
      { type: "inside", xAxisIndex: 0 },
      { type: "slider", xAxisIndex: 0, height: 20 },
    ],
    series: [
      { name: "merges", type: "scatter", data: mergeData },
      { name: "deploys", type: "scatter", data: deployData },
      { name: "failures", type: "scatter", data: failureData },
    ],
  };
}

/** The delivery timeline chart: merges by lane (coloured by `colorBy`), successful deploys and
 *  pipeline failures each in their own row above the lanes, zoomable/pannable via `dataZoom`. */
export function Timeline({ prs, runs, colorBy, onSelect, onBrush }: Props): ReactNode {
  const containerRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<ECharts | undefined>(undefined);
  // The chart mounts exactly once (below); onSelect/onBrush are read through these refs, kept
  // current every render, rather than closed over directly, so a parent passing a new callback
  // identity each render (DeliveryPage's onBrush closes over its own current filter state) never
  // leaves the mount effect's listeners calling a stale one.
  const onSelectRef = useRef(onSelect);
  onSelectRef.current = onSelect;
  const onBrushRef = useRef(onBrush);
  onBrushRef.current = onBrush;

  useEffect(() => {
    let disposed = false;
    let chart: ECharts | undefined;
    let resize: (() => void) | undefined;
    let debounce: number | undefined;

    void import("echarts").then((echarts) => {
      if (disposed || containerRef.current === null) return;
      chart = echarts.init(containerRef.current);
      chartRef.current = chart;

      chart.on("click", (params: ECElementEvent) => {
        const { name, seriesName } = params;
        if (name === undefined || seriesName === undefined) return;
        if (seriesName === "merges") onSelectRef.current({ kind: "pr", id: name });
        else if (seriesName === "deploys")
          onSelectRef.current({ kind: "deploy", id: Number(name) });
        else if (seriesName === "failures")
          onSelectRef.current({ kind: "failure", id: Number(name) });
      });

      chart.on("datazoom", () => {
        clearTimeout(debounce);
        debounce = window.setTimeout(() => {
          const option = chart?.getOption();
          const configured = option?.dataZoom;
          const zoom = Array.isArray(configured) ? configured[0] : configured;
          if (zoom?.startValue === undefined || zoom.endValue === undefined) return;
          onBrushRef.current({
            from: new Date(zoom.startValue).toISOString(),
            to: new Date(zoom.endValue).toISOString(),
          });
        }, DATAZOOM_DEBOUNCE_MS);
      });

      resize = () => chart?.resize();
      window.addEventListener("resize", resize);
    });

    return () => {
      disposed = true;
      clearTimeout(debounce);
      if (resize !== undefined) window.removeEventListener("resize", resize);
      chart?.dispose();
    };
  }, []);

  useEffect(() => {
    const chart = chartRef.current;
    if (chart === undefined) return;
    const lanes = buildLanes(prs, colorBy);
    const colorScale = buildColorScale(prs, colorBy);
    chart.setOption(buildOption(prs, runs, colorBy, lanes, colorScale), { notMerge: true });
  }, [prs, runs, colorBy]);

  return <div className="h-[420px] w-full" ref={containerRef} />;
}

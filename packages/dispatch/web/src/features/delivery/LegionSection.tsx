// The Delivery page's first section (INFRA-1077's spec, R3, R4 and the Design's "The Delivery
// page", section 1): Legion, issue to merge — the four metrics of the issues that finished in the
// page's window, with a project picker and a toggle for all issues, roots or children.
//
// Every number comes from `GET /api/v1/legion/metrics` and nothing is computed here: the server
// holds the rules (the work time less parked and queued hours, the seven bars and their five
// segments in their one precedence, the review rounds and send-backs, the four classes of filing),
// and this draws what it answers. Each metric goes through `Chart` (`Chart.tsx`), so each has the
// graph-or-table toggle and the compare range every chart of a measure over a range has; the
// comparison is the page's own, read as a second call of the same route, the way `PipelineSection`
// reads its own route twice.
//
// The page gives the section the window and the comparison it gives the Pipeline section and the
// delivery timeline (`DeliverySectionProps`), so every chart on the page follows one window
// control and one compare range. The route takes two instants rather than the page's preset, so a
// preset is resolved to a range once per choice (`metricsRange`).
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { legionMetricsQuery, legionSettingsQuery } from "../../api/queries";
import type {
  LegionMetricsResponse,
  LegionMetricsScope,
  LegionReworkIssue,
  LegionSegments,
  LegionWorkTimeIssue,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  surfaceBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import {
  Chart,
  type ChartCompareSource,
  type ChartRange,
  type ChartSeries,
  chartCompare,
  type DeliverySectionProps,
  fieldClass,
  PENDING,
} from "./Chart";
import { formatDuration, pct } from "./lib/format";
import { DAY_MS, liveWindow, type WindowChoice } from "./lib/window";
import { type SegmentedOption, SegmentedToggle } from "./SegmentedToggle";

const CARD = `flex min-w-0 flex-col gap-1.5 rounded border px-3 py-2 ${surfaceBg} ${borderDefault}`;

/** The three populations the section's toggle counts, in the order it offers them. */
const SCOPES: readonly SegmentedOption<LegionMetricsScope>[] = [
  { value: "all", label: "All issues" },
  { value: "roots", label: "Roots" },
  { value: "children", label: "Children" },
];

/** The five segments of a bar, in the precedence the server books an hour by (the spec's "Rule
 *  details — Precedence"), each a run of bars over the seven worker types. */
const SEGMENTS: readonly { readonly key: keyof LegionSegments; readonly label: string }[] = [
  { key: "waiting_on_person", label: "Waiting on a person" },
  { key: "in_a_turn", label: "In a turn" },
  { key: "ci_wait", label: "CI wait" },
  { key: "waiting_on_children", label: "Waiting on children" },
  { key: "idle", label: "Idle" },
];

/** The four classes of filing, in the order the card lists them (`metrics.classes`). */
const CLASSES: readonly { readonly key: string; readonly label: string }[] = [
  { key: "in_tree_at_design", label: "In the tree, at design" },
  { key: "in_tree_later", label: "In the tree, later" },
  { key: "out_of_tree", label: "Out of the tree" },
  { key: "by_the_controller", label: "By the controller" },
];

/** Hours as the page's other durations read ("3.2h"), which `formatDuration` writes in minutes. */
const hours = (value: number): string => formatDuration(value * 60);

const asCount = (value: number): string => `${value}`;

/** The top of a run of bars: the largest value it draws, and 1 for a run of nothing, since a
 *  scale whose top is zero draws no bar at all. */
const topOf = (values: readonly number[]): number => Math.max(1, ...values);

/** The range the metrics route reads for a window choice: a custom range as the reader named it,
 *  and a preset as its own days ending at `nowMs`. The route takes two instants, so a preset is
 *  resolved once per choice rather than on every render, which would read the route again every
 *  time anything else on the page moved. */
export function metricsRange(choice: WindowChoice, nowMs: number): ChartRange {
  const live = liveWindow(choice);
  if (!("preset" in live)) return { kind: "custom", from: live.from, to: live.to };
  return {
    kind: "custom",
    from: new Date(nowMs - live.preset * DAY_MS).toISOString(),
    to: new Date(nowMs).toISOString(),
  };
}

/** Metric 1's chart: each finished issue's work time, and the hours taken out of it — the time it
 *  sat parked and the time it sat back in the queue. One row per issue, in the answer's own order
 *  (the server reads its issues by key). */
export function workTimeSeries(report: LegionMetricsResponse): ChartSeries[] {
  const issues = report.work_time.issues;
  const max = topOf(issues.map((issue) => issue.hours));
  const run = (label: string, value: (issue: LegionWorkTimeIssue) => number): ChartSeries => ({
    kind: "spark",
    format: hours,
    label,
    max,
    points: issues.map((issue) => ({ label: issue.key, partial: false, value: value(issue) })),
  });
  return [
    run("Work time", (issue) => issue.hours),
    run("Parked", (issue) => issue.parked_hours),
    run("Queued", (issue) => issue.queued_hours),
  ];
}

/** Metric 2's chart: each worker type's bar, split into its five segments, and the median hours
 *  per issue beside them. One row per worker type; a bar's whole time is its five segments, so
 *  the table holds every hour the card counts. */
export function phaseTimeSeries(report: LegionMetricsResponse): ChartSeries[] {
  const bars = report.phase_time.bars;
  const max = topOf(bars.map((bar) => bar.hours));
  // The seventh bar is the phases no role works, which reads as "no role" rather than its key.
  const points = (value: (bar: (typeof bars)[number]) => number) =>
    bars.map((bar) => ({
      label: bar.role === "no_role" ? "no role" : bar.role,
      partial: false,
      value: value(bar),
    }));
  return [
    ...SEGMENTS.map(
      (segment): ChartSeries => ({
        kind: "spark",
        format: hours,
        label: segment.label,
        max,
        points: points((bar) => bar.segments[segment.key]),
      })
    ),
    {
      kind: "spark",
      format: hours,
      label: "Median per issue",
      max: topOf(bars.map((bar) => bar.median_hours_per_issue)),
      points: points((bar) => bar.median_hours_per_issue),
    },
  ];
}

/** Metric 3's chart: each finished issue's review rounds, and the implementing and testing hours
 *  it spent after its first review. One row per issue. */
export function reworkSeries(report: LegionMetricsResponse): ChartSeries[] {
  const issues = report.rework.issues;
  const after = topOf(
    issues.flatMap((issue) => [
      issue.implementing_hours_after_first_review,
      issue.testing_hours_after_first_review,
    ])
  );
  const run = (
    label: string,
    format: (value: number) => string,
    max: number,
    value: (issue: LegionReworkIssue) => number
  ): ChartSeries => ({
    kind: "spark",
    format,
    label,
    max,
    points: issues.map((issue) => ({ label: issue.key, partial: false, value: value(issue) })),
  });
  const rounds = topOf(issues.map((issue) => issue.review_rounds));
  return [
    run("Review rounds", asCount, rounds, (issue) => issue.review_rounds),
    run("Implementing after the first review", hours, after, (issue) => {
      return issue.implementing_hours_after_first_review;
    }),
    run("Testing after the first review", hours, after, (issue) => {
      return issue.testing_hours_after_first_review;
    }),
  ];
}

/** Metric 4's chart: what Legion sessions filed each week, by class, against the issues that
 *  finished that week. One row per UTC week, named by the Monday it begins on. */
export function splittingSeries(report: LegionMetricsResponse): ChartSeries[] {
  const weeks = report.splitting.weeks;
  const max = topOf(weeks.flatMap((week) => [week.finished, ...Object.values(week.filed)]));
  const run = (label: string, value: (week: (typeof weeks)[number]) => number): ChartSeries => ({
    kind: "spark",
    format: asCount,
    label,
    max,
    points: weeks.map((week) => ({
      label: week.week.slice(0, 10),
      partial: false,
      value: value(week),
    })),
  });
  return [
    ...CLASSES.map((filing) => run(filing.label, (week) => week.filed[filing.key] ?? 0)),
    run("Issues finished", (week) => week.finished),
  ];
}

/** The Delivery page's `#legion` section: the four Legion metrics of one project over the page's
 *  window, each through `Chart`. A deployment where no project names a Legion daemon says so and
 *  reads nothing. */
export function LegionSection({
  compare,
  onCompare,
  window: chosen,
}: DeliverySectionProps): ReactNode {
  const settings = useQuery(legionSettingsQuery());
  const projects = useMemo(
    () => (settings.data ?? []).map((setting) => setting.project),
    [settings.data]
  );
  // The project the reader picked, while it still names a daemon; otherwise the first that does,
  // which is the project the route itself answers with no `project` of its own.
  const [picked, setPicked] = useState<string | null>(null);
  const project = (picked !== null && projects.includes(picked) ? picked : projects[0]) ?? "";
  const [scope, setScope] = useState<LegionMetricsScope>("all");
  // The window as two instants, resolved when the page's choice changes and not on every render:
  // a preset's end is the minute it was resolved in, so a re-render reads no new range.
  const range = useMemo(
    () => metricsRange(chosen, Math.floor(Date.now() / 60_000) * 60_000),
    [chosen]
  );
  const query = useQuery({
    ...legionMetricsQuery({ from: range.from, project, scope, to: range.to }),
    enabled: project !== "",
  });
  // The comparison's own read, as a range of its own: while nothing is compared the query names
  // the section's own range and is disabled, so it reads nothing.
  const compared = useQuery({
    ...legionMetricsQuery({
      from: (compare ?? range).from,
      project,
      scope,
      to: (compare ?? range).to,
    }),
    enabled: compare !== null && project !== "",
  });

  return (
    <section aria-labelledby="legion-heading" className="flex flex-col gap-3" id="legion">
      <header className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2">
        <h2 className={`text-lg font-semibold ${textPrimaryOnCanvas}`} id="legion-heading">
          Legion: issue to merge
        </h2>
        {projects.length === 0 ? null : (
          <div className="flex flex-wrap items-center gap-2">
            <select
              aria-label="Legion project"
              className={fieldClass}
              data-legion-metrics-project
              onChange={(event) => setPicked(event.target.value)}
              value={project}
            >
              {projects.map((key) => (
                <option key={key} value={key}>
                  {key}
                </option>
              ))}
            </select>
            <div data-legion-metrics-scope={scope}>
              <SegmentedToggle
                label="Legion issues"
                onChange={setScope}
                options={SCOPES}
                value={scope}
              />
            </div>
          </div>
        )}
      </header>
      {settings.isPending ? <p className={textMutedOnCanvas}>Loading Legion projects…</p> : null}
      {projects.length === 0 && !settings.isPending ? (
        <p className={`text-sm ${textSecondaryOnCanvas}`} data-legion-metrics-unconfigured>
          No project names a Legion daemon, so there is nothing to measure here. The Settings page
          names one.
        </p>
      ) : null}
      {query.isPending && project !== "" ? (
        <p className={textMutedOnCanvas}>Loading the Legion metrics…</p>
      ) : null}
      {query.isError ? (
        <QueryError
          message="Couldn't load the Legion metrics."
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      ) : null}
      {query.data === undefined ? null : (
        <LegionMetrics
          compare={{ data: compared.data, onRange: onCompare, range: compare }}
          report={query.data}
        />
      )}
    </section>
  );
}

/** The four cards for one answer of the route. `compare` is the page's comparison, which every
 *  card offers and one second answer serves; a host that reads no second range passes none and
 *  the cards show their graph-or-table control alone. */
export function LegionMetrics({
  compare,
  report,
}: {
  compare?: ChartCompareSource<LegionMetricsResponse>;
  report: LegionMetricsResponse;
}): ReactNode {
  // What each card draws, and what the compare range's answer draws beside it: built once per
  // answer rather than on every render, since `Chart` builds its table of every number from the
  // series it is handed.
  const series = useMemo(
    () => ({
      phaseTime: phaseTimeSeries(report),
      rework: reworkSeries(report),
      splitting: splittingSeries(report),
      workTime: workTimeSeries(report),
    }),
    [report]
  );
  const comparedData = compare?.data;
  const compared = useMemo(
    () =>
      comparedData === undefined
        ? undefined
        : {
            phaseTime: phaseTimeSeries(comparedData),
            rework: reworkSeries(comparedData),
            splitting: splittingSeries(comparedData),
            workTime: workTimeSeries(comparedData),
          },
    [comparedData]
  );
  const range: ChartRange = { kind: "custom", from: report.from, to: report.to };
  const rework = report.rework;
  const splitting = report.splitting;

  return (
    <div className="@container">
      <div className="grid min-w-0 grid-cols-1 gap-2 @min-[56rem]:grid-cols-2">
        <MetricCard
          figures={[
            { label: "Median", value: hours(report.work_time.median_hours) },
            { label: "p90", value: hours(report.work_time.p90_hours) },
            { label: "Issues finished", value: `${report.finished}` },
          ]}
          metric="work_time"
          title="Work time to merge"
        >
          <Chart
            compare={chartCompare(compare, compared?.workTime)}
            label="Work time to merge"
            range={range}
            rows={{ header: "Issue", over: "issue" }}
            series={series.workTime}
          />
        </MetricCard>
        <MetricCard
          figures={[{ label: "Idle share", value: pct(report.phase_time.idle_share) }]}
          metric="phase_time"
          title="Phase time by worker type"
        >
          <Chart
            compare={chartCompare(compare, compared?.phaseTime)}
            label="Phase time by worker type"
            range={range}
            rows={{ header: "Worker type", over: "worker type" }}
            series={series.phaseTime}
          />
        </MetricCard>
        <MetricCard
          figures={[
            { label: "Median review rounds", value: rework.median_review_rounds.toFixed(1) },
            { label: "Sent back at least once", value: pct(rework.sent_back_share) },
            {
              label: "Median hours after the first review",
              value: hours(rework.median_hours_after_first_review),
            },
          ]}
          metric="rework"
          title="Rework"
        >
          <Chart
            compare={chartCompare(compare, compared?.rework)}
            label="Rework"
            range={range}
            rows={{ header: "Issue", over: "issue" }}
            series={series.rework}
          />
        </MetricCard>
        <MetricCard
          figures={[
            { label: "Issues filed", value: `${splitting.filed.length}` },
            { label: "Roots finished", value: `${splitting.finished_roots}` },
            { label: "Children per root", value: splitting.children_per_root.toFixed(1) },
            {
              label: "Median work time, roots with children",
              value: hours(splitting.median_hours_roots_with_children),
            },
            {
              label: "Median work time, roots without children",
              value: hours(splitting.median_hours_roots_without_children),
            },
          ]}
          metric="splitting"
          title="Splitting and follow-ups"
        >
          <Chart
            compare={chartCompare(compare, compared?.splitting)}
            label="Splitting and follow-ups"
            range={range}
            rows={{ header: "Week", over: "UTC week" }}
            series={series.splitting}
          />
        </MetricCard>
      </div>
    </div>
  );
}

/** One metric: its headline numbers and its chart. */
function MetricCard({
  children,
  figures,
  metric,
  title,
}: {
  children: ReactNode;
  figures: readonly { readonly label: string; readonly value: string }[];
  metric: string;
  title: string;
}): ReactNode {
  return (
    <div className={CARD} data-legion-metric={metric}>
      <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>{title}</h3>
      <dl className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        {figures.map((figure) => (
          <div className="flex items-baseline gap-1.5" key={figure.label}>
            <dt className={`text-xs ${textMutedOnSurface}`}>{figure.label}</dt>
            <dd
              className={`text-base font-semibold ${textPrimaryOnCanvas}`}
              data-legion-figure={figure.label}
            >
              {figure.value === "" ? PENDING : figure.value}
            </dd>
          </div>
        ))}
      </dl>
      {children}
    </div>
  );
}

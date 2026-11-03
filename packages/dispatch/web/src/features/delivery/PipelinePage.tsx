// The prototype's web/src/pages/Pipeline.tsx, ported as it is: the header line, six cards, the
// slowest-jobs table and the failed-in-24-hours table, each with the prototype's data-* attributes.
// Every number, every target and every day comes from GET /api/v1/delivery/pipeline; nothing is
// computed here but the two lead-time cards' met/missed, which the prototype judges on the page as
// this does, against the targets the response carries. Differences, each from LEGION-567's slice 3
// plan or for the same reason as the measures panel's: Dispatch's sidebar replaces the
// prototype's row of page links; the cards take the prototype's column counts at the prototype's
// content widths (one column, two from 46rem, six from 78rem: its `md:grid-cols-2
// xl:grid-cols-6` less its `px-4` page padding) measured on the page's own width rather than the
// viewport, since beside Dispatch's sidebar the viewport rule put six cards in too little room for
// their figures; a pipeline whose settings name no staging end-to-end gate (or no staging deploy)
// omits that card; the prototype's note counting runs whose jobs are not fetched yet is gone, so a
// run whose jobs are not stored (intake and reconcile write a run and its jobs separately, and a
// run whose jobs listing GitHub refuses is marked jobs-unfetchable and never gets any) counts in
// the PR CI card and adds nothing to the two job tables, unremarked; the failing-jobs note names
// the roll-up job by its role, since the job's name is a setting; and the colours are the theme's
// tokens.
//
// `?at=` is the test and screenshot hook: the page forwards it to the route, which then names the
// seven days ending on at's day and looks back 24 hours from it, so a fixture dated in the past
// renders as "today", and a failure's age is measured from it. The page never writes it.
//
// Each card's bars go through `Chart` (INFRA-1077), which gives every chart of a measure over a
// range its graph-or-table and compare controls. The route answers the seven days ending on one
// day and no other length, so the reader compares the seven days just before the shown ones or
// nothing (`pickable: false`), and the page reads that comparison as a second subscription
// pinned to the day before the first shown one. The slowest-jobs table's trend keeps its bars
// alone: its row already holds today's and the seven days' median, p90 and count.
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { isDeliveryNotConfigured } from "../../api/client";
import { deliveryPipelineQuery } from "../../api/queries";
import type {
  DeliveryLeadTimeCell,
  DeliveryLeadTimeDay,
  DeliveryPipelineResponse,
  DeliveryTiming,
  DeliveryTimingDay,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  dangerText,
  linkText,
  surfaceBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { Chart, type ChartCompare, type ChartRange, type ChartSeries, daysRange } from "./Chart";
import { formatDue, formatDuration, formatRate } from "./lib/format";
import { formatAge } from "./lib/freshness";
import { SourceFreshness } from "./SourceFreshness";
import { SpreadBars } from "./SpreadBars";
import { TargetBadge } from "./TargetBadge";

const CARD = `flex min-w-0 flex-col gap-1.5 rounded border px-3 py-2 ${surfaceBg} ${borderDefault}`;

/** `/pipeline`: how long PR checks, the staging end-to-end gate and the two deploys take, and how
 *  often each ends in success, over the last seven UTC days. */
export function PipelinePage(): ReactNode {
  useDocumentTitle("Pipeline · Dispatch");
  const [params] = useSearchParams();
  const at = params.get("at");
  const query = useQuery(
    deliveryPipelineQuery(at === null ? { kind: "pipeline" } : { kind: "pipeline", at })
  );
  // The page's "now": a pinned at, else the clock, so a failure's age is from the moment the
  // failing-jobs table looked back from.
  const nowMs = at === null ? Date.now() : Date.parse(at);
  // When the answer on screen was read, kept across a refetch, as the Delivery page keeps it.
  const answeredAtRef = useRef(0);
  if (query.dataUpdatedAt > 0) answeredAtRef.current = query.dataUpdatedAt;
  // The range the reader compares the shown days with, and its own read of the route: the cards
  // offer it, and one answer serves every card. Its `at` is a second before the first shown
  // day's midnight, which is the last instant of the day the preceding seven end on. While the
  // reader compares nothing the query names the page's own view and is disabled, so it reads
  // nothing and subscribes nothing.
  const [compareRange, setCompareRange] = useState<ChartRange | null>(null);
  const compared = useQuery({
    ...deliveryPipelineQuery(
      compareRange === null
        ? at === null
          ? { kind: "pipeline" }
          : { kind: "pipeline", at }
        : { kind: "pipeline", at: new Date(Date.parse(compareRange.to) - 1000).toISOString() }
    ),
    enabled: compareRange !== null,
  });

  return (
    <section className="flex flex-col gap-3">
      <header className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h1 className={`text-xl font-bold ${textPrimaryOnCanvas}`}>Pipeline</h1>
        {query.data === undefined ? null : (
          <div className={`text-xs ${textMutedOnCanvas}`}>
            Generated {new Date(answeredAtRef.current).toLocaleString()}
          </div>
        )}
      </header>
      {query.isPending ? <p className={textMutedOnCanvas}>Loading pipeline…</p> : null}
      {isDeliveryNotConfigured(query.error) ? (
        <p className={`text-sm ${textSecondaryOnCanvas}`}>
          The Pipeline page reads the delivery timeline's deploy repository and workflows.{" "}
          <Link className={`underline ${linkText}`} to="/delivery">
            Set up the delivery timeline
          </Link>
        </p>
      ) : query.isError ? (
        <QueryError
          message="Couldn't load the pipeline."
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      ) : null}
      {query.data === undefined ? null : (
        <PipelineView
          compare={{
            onRange: setCompareRange,
            pipeline: compared.data,
            range: compareRange,
          }}
          nowMs={nowMs}
          pipeline={query.data}
          readAtMs={answeredAtRef.current}
        />
      )}
    </section>
  );
}

/** What a card's chart compares the shown days with: the range the reader picked, how they pick
 *  another, and the route's answer for it once it lands. */
export interface PipelineCompare {
  readonly range: ChartRange | null;
  readonly onRange: (next: ChartRange | null) => void;
  readonly pipeline: DeliveryPipelineResponse | undefined;
}

/** What a stage card's chart draws: each day's run time, median and p90, and the share of the
 *  same day's results that succeeded. */
function timingSeries(days: readonly DeliveryTimingDay[]): ChartSeries[] {
  return [
    { kind: "spread", days, format: formatDuration, label: "Run time", noun: "runs" },
    {
      kind: "spark",
      format: (v) => `${formatRate(v)} success`,
      label: "Success",
      max: 1,
      points: days.map((d) => ({ label: d.day, partial: d.partial, value: d.success_rate })),
    },
  ];
}

/** What a lead-time card's chart draws: each merge day's lead time, median and p90, against the
 *  target the response carries. */
function leadTimeSeries(days: readonly DeliveryLeadTimeDay[], target: number): ChartSeries[] {
  return [
    { kind: "spread", days, format: formatDuration, label: "Lead time", noun: "PRs", target },
  ];
}

/** The page's body for one answer of the route; `readAtMs` is when that answer was read, and
 *  `nowMs` the moment a failure's age is measured from. `compare` is the page's comparison,
 *  which every card offers and one second answer serves; a host that reads no second range
 *  passes none and the cards show their graph-or-table control alone. */
export function PipelineView({
  compare,
  pipeline,
  readAtMs,
  nowMs,
}: {
  compare?: PipelineCompare;
  pipeline: DeliveryPipelineResponse;
  readAtMs: number;
  nowMs: number;
}): ReactNode {
  const first = pipeline.days[0];
  const last = pipeline.days[pipeline.days.length - 1];
  if (first === undefined || last === undefined) throw new Error("the pipeline has no days");
  const { pr_checks: checks } = pipeline;
  const range = daysRange(first, last);
  // The seven days just before the shown ones are the only comparison the route can answer, so
  // a card names that one and no range of the reader's own.
  const cardCompare = (series: (answer: DeliveryPipelineResponse) => ChartSeries[] | null) =>
    compare === undefined
      ? undefined
      : ({
          onRange: compare.onRange,
          pickable: false,
          range: compare.range,
          series:
            compare.pipeline === undefined ? undefined : (series(compare.pipeline) ?? undefined),
        } satisfies ChartCompare);

  return (
    <>
      <SourceFreshness freshness={pipeline.freshness} readAtMs={readAtMs} />
      <p className={`text-xs ${textMutedOnCanvas}`}>
        Last 7 days, {formatDue(first)} to {formatDue(last)} (UTC), by the day each run started;
        today is partial and drawn faded. Not filtered by the timeline&apos;s window or facets.
        Times are wall clock. Success is success over success plus failure; cancelled runs and jobs
        are counted beside it, not in it.
      </p>

      <div className="@container">
        <div
          className="grid grid-cols-1 gap-3 @min-[46rem]:grid-cols-2 @min-[78rem]:grid-cols-6"
          data-section="stage-cards"
        >
          <TimingCard
            compare={cardCompare((answer) => timingSeries(answer.pr_checks.run.days))}
            detail="PR Checks on a pull request, from run start to its last job"
            metric="pr_ci"
            range={range}
            timing={checks.run}
            title="PR CI"
          />
          {pipeline.staging_e2e === null ? null : (
            <TimingCard
              compare={cardCompare((answer) =>
                answer.staging_e2e === null ? null : timingSeries(answer.staging_e2e.days)
              )}
              detail={pipeline.staging_e2e.job}
              metric="staging_e2e"
              range={range}
              timing={pipeline.staging_e2e}
              title="Staging end-to-end gate"
            />
          )}
          {pipeline.staging_apply === null ? null : (
            <TimingCard
              compare={cardCompare((answer) =>
                answer.staging_apply === null ? null : timingSeries(answer.staging_apply.days)
              )}
              detail={pipeline.staging_apply.job}
              metric="staging_apply"
              range={range}
              timing={pipeline.staging_apply}
              title="Deploy to staging"
            />
          )}
          <TimingCard
            compare={cardCompare((answer) => timingSeries(answer.production_apply.days))}
            detail={pipeline.production_apply.job}
            metric="production_apply"
            range={range}
            timing={pipeline.production_apply}
            title="Deploy to production"
          />
          <LeadTimeCard
            compare={cardCompare((answer) =>
              leadTimeSeries(
                answer.merge_to_production.days,
                answer.targets.merge_to_production_minutes
              )
            )}
            detail="DORA lead time, PRs by the day they merged"
            judged="longest"
            metric="merge_to_production"
            range={range}
            target={pipeline.targets.merge_to_production_minutes}
            timing={pipeline.merge_to_production}
            title="Merge to production"
          />
          <LeadTimeCard
            compare={cardCompare((answer) =>
              leadTimeSeries(
                answer.opened_to_merge.days,
                answer.targets.opened_to_merge_median_minutes
              )
            )}
            detail="Opened to merged, every merged PR, by the day it merged"
            judged="median"
            metric="opened_to_merge"
            range={range}
            target={pipeline.targets.opened_to_merge_median_minutes}
            timing={pipeline.opened_to_merge}
            title="PR open to merge"
          />
        </div>
      </div>

      <section aria-label="PR CI slowest jobs" className="mt-1" data-section="slowest-jobs">
        <h2 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>PR CI: slowest jobs</h2>
        <p className={`text-xs ${textMutedOnCanvas}`}>
          Ranked by their 7-day median; a job needs 50 timed results in the 7 days to be ranked.
        </p>
        {checks.slowest_jobs.length === 0 ? (
          <p className={`mt-1 text-xs ${textMutedOnCanvas}`}>No job has 50 timed results yet.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="mt-1 w-full max-w-6xl text-xs tabular-nums">
              <thead className={textMutedOnCanvas}>
                <tr>
                  <th className="px-2 py-1 text-left font-normal">Job</th>
                  <th className="px-2 py-1 text-right font-normal">Today median</th>
                  <th className="px-2 py-1 text-right font-normal">Today p90</th>
                  <th className="px-2 py-1 text-right font-normal">Today</th>
                  <th className="px-2 py-1 text-right font-normal">7-day median</th>
                  <th className="px-2 py-1 text-right font-normal">7-day p90</th>
                  <th className="px-2 py-1 text-right font-normal">7 days</th>
                  <th className="px-2 py-1 text-right font-normal">Success</th>
                  <th className="w-40 px-2 py-1 text-left font-normal">Trend</th>
                </tr>
              </thead>
              <tbody className={textSecondaryOnCanvas}>
                {checks.slowest_jobs.map((job) => {
                  const today = lastDay(job);
                  return (
                    <tr className={`border-t ${borderDefault}`} data-job={job.name} key={job.name}>
                      <td className="px-2 py-1">{job.name}</td>
                      <td className="px-2 py-1 text-right font-semibold">
                        {formatDuration(today.median_minutes)}
                      </td>
                      <td className="px-2 py-1 text-right">{formatDuration(today.p90_minutes)}</td>
                      <td className={`px-2 py-1 text-right ${textMutedOnCanvas}`}>{today.n}</td>
                      <td className="px-2 py-1 text-right font-semibold">
                        {formatDuration(job.total.median_minutes)}
                      </td>
                      <td className="px-2 py-1 text-right">
                        {formatDuration(job.total.p90_minutes)}
                      </td>
                      <td className={`px-2 py-1 text-right ${textMutedOnCanvas}`}>{job.total.n}</td>
                      <td className="px-2 py-1 text-right">{formatRate(job.total.success_rate)}</td>
                      <td className="px-2 py-1">
                        <SpreadBars days={job.days} format={formatDuration} noun="results" />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section aria-label="PR CI failing jobs" className="mt-1" data-section="failing-jobs">
        <h2 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>
          PR CI: jobs that failed in the last 24 hours
        </h2>
        <p className={`text-xs ${textMutedOnCanvas}`}>
          Every failed job but the roll-up job the settings name, most failures first; a failure is
          often the pull request&apos;s own change.
        </p>
        {checks.failing_jobs.length === 0 ? (
          <p className={`mt-1 text-xs ${textMutedOnCanvas}`}>No job failed in the last 24 hours.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="mt-1 w-full max-w-4xl text-xs tabular-nums">
              <thead className={textMutedOnCanvas}>
                <tr>
                  <th className="px-2 py-1 text-left font-normal">Job</th>
                  <th className="px-2 py-1 text-right font-normal">Failures</th>
                  <th className="px-2 py-1 text-right font-normal">Of runs</th>
                  <th className="px-2 py-1 text-right font-normal">Failure share</th>
                  <th className="px-2 py-1 text-left font-normal">Last failure</th>
                </tr>
              </thead>
              <tbody className={textSecondaryOnCanvas}>
                {checks.failing_jobs.map((job) => (
                  <tr
                    className={`border-t ${borderDefault}`}
                    data-failing-job={job.name}
                    key={job.name}
                  >
                    <td className="px-2 py-1">{job.name}</td>
                    <td className={`px-2 py-1 text-right font-semibold ${dangerText}`}>
                      {job.failures}
                    </td>
                    <td className={`px-2 py-1 text-right ${textMutedOnCanvas}`}>{job.results}</td>
                    <td className="px-2 py-1 text-right">
                      {formatRate(job.failures / job.results)}
                    </td>
                    <td className="px-2 py-1">
                      <a
                        className={`hover:underline ${linkText}`}
                        href={job.last_run_url}
                        rel="noreferrer"
                        target="_blank"
                      >
                        {formatAge(Math.max(0, (nowMs - Date.parse(job.last_failed_at)) / 1000))}
                      </a>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </>
  );
}

/** Today: the last shown day. */
function lastDay<T>(timing: { readonly days: readonly T[] }): T {
  const today = timing.days[timing.days.length - 1];
  if (today === undefined) throw new Error("a pipeline timing has no days");
  return today;
}

function Figure(props: {
  label: string;
  median: number | null;
  p90: number | null;
  n: number;
  noun: string;
}): ReactNode {
  return (
    <div className="min-w-0">
      <div className={`text-[11px] ${textMutedOnSurface}`}>{props.label}</div>
      <div className="flex items-baseline gap-1">
        <span className={`text-lg font-semibold ${textPrimaryOnSurface}`}>
          {formatDuration(props.median)}
        </span>
        <span className={`text-[11px] ${textMutedOnSurface}`}>median</span>
      </div>
      <div className={`text-xs ${textSecondaryOnSurface}`}>
        p90 {formatDuration(props.p90)} · {props.n}{" "}
        {props.n === 1 ? props.noun.replace(/s$/, "") : props.noun}
      </div>
    </div>
  );
}

function CardTitle({ title, detail }: { title: string; detail: string }): ReactNode {
  return (
    <div className="min-w-0">
      <div className={`text-sm font-semibold ${textPrimaryOnSurface}`}>{title}</div>
      <div className={`truncate text-[11px] ${textMutedOnSurface}`} title={detail}>
        {detail}
      </div>
    </div>
  );
}

function TimingCard(props: {
  metric: string;
  title: string;
  detail: string;
  timing: DeliveryTiming;
  range: ChartRange;
  compare: ChartCompare | undefined;
}): ReactNode {
  const { timing } = props;
  const today: DeliveryTimingDay = lastDay(timing);
  return (
    <div className={CARD} data-metric={props.metric}>
      <CardTitle detail={props.detail} title={props.title} />
      <div className="grid grid-cols-2 gap-2">
        <Figure
          label="Today"
          median={today.median_minutes}
          n={today.n}
          noun="runs"
          p90={today.p90_minutes}
        />
        <Figure
          label="Last 7 days"
          median={timing.total.median_minutes}
          n={timing.total.n}
          noun="runs"
          p90={timing.total.p90_minutes}
        />
      </div>
      <Chart
        compare={props.compare}
        label={props.title}
        range={props.range}
        series={timingSeries(timing.days)}
      />
      <div className={`text-xs ${textSecondaryOnSurface}`} data-health>
        Success today <span className="font-semibold">{formatRate(today.success_rate)}</span>
        <span className={textMutedOnSurface}>
          {" "}
          ({today.success} of {today.success + today.failure})
        </span>
        , 7 days <span className="font-semibold">{formatRate(timing.total.success_rate)}</span>
        <span className={textMutedOnSurface}>
          {" "}
          ({timing.total.success} of {timing.total.success + timing.total.failure})
        </span>
      </div>
      <div className={`text-[11px] ${textMutedOnSurface}`}>
        {timing.total.cancelled} cancelled in 7 days, not counted
      </div>
    </div>
  );
}

/** One of the two lead-time cards: merge to production is judged on the seven days' longest
 *  change, PR open to merge on their median, each strictly under the response's target (the
 *  prototype's Pipeline.tsx MergeCard and OpenedToMergeCard). */
function LeadTimeCard(props: {
  metric: string;
  title: string;
  detail: string;
  timing: { readonly days: readonly DeliveryLeadTimeDay[]; readonly total: DeliveryLeadTimeCell };
  target: number;
  judged: "longest" | "median";
  range: ChartRange;
  compare: ChartCompare | undefined;
}): ReactNode {
  const { timing, target, judged } = props;
  const today = lastDay(timing);
  const total = timing.total;
  const judgedValue = judged === "longest" ? total.max_minutes : total.median_minutes;
  const met = judgedValue === null ? null : judgedValue < target;
  return (
    <div className={CARD} data-metric={props.metric}>
      <div className="flex items-start justify-between gap-2">
        <CardTitle detail={props.detail} title={props.title} />
        <TargetBadge met={met} metLabel="target met" missedLabel="target missed" />
      </div>
      <div className="grid grid-cols-2 gap-2">
        <Figure
          label="Today"
          median={today.median_minutes}
          n={today.n}
          noun="PRs"
          p90={today.p90_minutes}
        />
        <Figure
          label="Last 7 days"
          median={total.median_minutes}
          n={total.n}
          noun="PRs"
          p90={total.p90_minutes}
        />
      </div>
      <Chart
        compare={props.compare}
        label={props.title}
        range={props.range}
        series={leadTimeSeries(timing.days, target)}
      />
      <div className={`text-xs ${textSecondaryOnSurface}`}>
        {judged === "longest" ? "Longest" : "Median"} today{" "}
        <span className="font-semibold">
          {formatDuration(judged === "longest" ? today.max_minutes : today.median_minutes)}
        </span>
        , 7 days{" "}
        <span className={`font-semibold ${met === false ? dangerText : ""}`}>
          {formatDuration(judgedValue)}
        </span>
      </div>
      <div className={`text-[11px] ${textMutedOnSurface}`}>
        {judged === "longest"
          ? `target under ${target}m for every change (the dashed line)`
          : `target median under ${target}m (the dashed line)`}
      </div>
    </div>
  );
}

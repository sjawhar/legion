// The prototype's web/src/components/DoraPanel.tsx, ported as it is: the six KPI cards, the
// headline strip, and the "Definitions & alternatives" fold with its eight definition cards and
// per-day table. Every number, every target and whether each is met come from
// GET /api/v1/delivery/measures; nothing is computed here. Three differences, each from LEGION-567's
// slice 2 plan: the change-failure-rate card says flags arrive with flag confirmation while the
// server stores none; the P0 card links each issue inside Dispatch and dates its count by the
// response's computed_at; and the card grid reflows on narrow screens, where the prototype is
// desktop-only.
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import type { DeliveryMeasuresResponse } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import {
  approvalPill,
  borderDefault,
  dangerText,
  linkText,
  successText,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textPrimaryOnSurfaceMuted,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { formatMinutes, pct } from "./lib/format";
import { Sparkbars } from "./Sparkbars";

const UNOWNED_P0_SHOWN = 6;

/** What a pending read shows in each number's place. */
const PENDING = "\u2026";

/** The change-failure-rate card's breakdown: deploys with a confirmed break, and how many more
 *  have only unreviewed (pending) flags. `confirmedOrPending` counts each deploy once (the
 *  prototype's web/src/lib/dora.ts:297-305). */
export function failureRateBreakdown(confirmed: number, confirmedOrPending: number): string {
  const onlyPending = confirmedOrPending - confirmed;
  const breaks = `${confirmed} ${confirmed === 1 ? "deploy" : "deploys"} with a confirmed break`;
  return onlyPending === 0
    ? `${breaks}; none with only unreviewed flags`
    : `${breaks}; ${onlyPending} more with only unreviewed flags`;
}

function tone(met: boolean | null): string {
  if (met === null) return textMutedOnCanvas;
  return met ? successText : dangerText;
}

function Badge({ met }: { met: boolean | null }): ReactNode {
  const [label, state] =
    met === null
      ? ["no data", "draft" as const]
      : met
        ? ["\u2713 met", "approved" as const]
        : ["\u2717 missed", "changes_requested" as const];
  return (
    <span
      className={`shrink-0 rounded border px-1.5 py-0.5 text-[10px] font-medium whitespace-nowrap ${approvalPill[state]}`}
    >
      {label}
    </span>
  );
}

function KpiCard(props: {
  title: string;
  target: string;
  // Undefined while the read is pending: no badge rather than a verdict nobody has reached.
  met: boolean | null | undefined;
  children: ReactNode;
}): ReactNode {
  return (
    <div
      className={`flex min-w-0 flex-col gap-1 rounded border px-2.5 py-2 ${surfaceBg} ${borderDefault}`}
      data-kpi={props.title}
    >
      <div className="flex items-center justify-between gap-2">
        <span className={`truncate text-xs ${textMutedOnSurface}`}>{props.title}</span>
        {props.met === undefined ? null : <Badge met={props.met} />}
      </div>
      {props.children}
      <div className={`text-[11px] ${textMutedOnSurface}`}>target {props.target}</div>
    </div>
  );
}

function BigNumber({ className, children }: { className: string; children: string }): ReactNode {
  return <span className={`text-lg font-semibold ${className}`}>{children}</span>;
}

function DefinitionCard(props: { title: string; value: string; children: ReactNode }): ReactNode {
  return (
    <div className={`rounded p-3 ${surfaceMutedBg}`}>
      <div className={`text-xs uppercase ${textMutedOnSurfaceMuted}`}>{props.title}</div>
      <div className={`text-xl font-semibold ${textPrimaryOnSurfaceMuted}`}>{props.value}</div>
      <div className={`text-xs ${textMutedOnSurfaceMuted}`}>{props.children}</div>
    </div>
  );
}

/** The delivery measures for the page's window and facets: Sami's six KPI targets, then one
 *  compact strip of the DORA headline numbers; every alternative definition and breakdown lives
 *  behind the "Definitions & alternatives" fold, so the timeline keeps the bulk of the page.
 *  `data` is undefined while the read is pending, when the cards show their frame. */
export function MeasuresPanel({ data }: { data: DeliveryMeasuresResponse | undefined }): ReactNode {
  const [open, setOpen] = useState(false);
  const show = (format: (response: DeliveryMeasuresResponse) => string): string =>
    data === undefined ? PENDING : format(data);
  const toneOf = (met: (response: DeliveryMeasuresResponse) => boolean | null): string =>
    data === undefined ? textMutedOnSurface : tone(met(data));
  const targets = data?.targets;
  const status = data?.status;
  const measures = data?.measures;

  const strip = [
    {
      label: "Deploy freq",
      value: show((d) => `${d.measures.deploy_frequency.per_day.toFixed(2)}/day`),
    },
    {
      label: "Lead time (merge\u2192prod)",
      value: show((d) => formatMinutes(d.measures.lead_time.merge_to_production.median_minutes)),
    },
    { label: "CFR (per PR)", value: show((d) => pct(d.measures.change_failure_rate.per_pr.rate)) },
    {
      label: "Time to restore",
      value: show((d) => formatMinutes(d.measures.time_to_restore.median_minutes)),
    },
    { label: "Rework share", value: show((d) => pct(d.measures.rework_share)) },
  ];

  return (
    <section className={`rounded border ${borderDefault}`}>
      <fieldset
        aria-label="KPI targets"
        className="grid min-w-0 grid-cols-2 gap-2 px-3 pt-2 text-sm md:grid-cols-3 xl:grid-cols-6"
      >
        <KpiCard
          met={status?.deploys_per_day}
          target={targets === undefined ? PENDING : `\u2265 ${targets.deploys_per_day}/day`}
          title="Deploys a day"
        >
          <div className="flex items-baseline gap-2">
            <BigNumber className={toneOf((d) => d.status.deploys_per_day)}>
              {show((d) => `${d.measures.deploy_frequency.per_day.toFixed(1)}/day`)}
            </BigNumber>
            {measures === undefined ? null : (
              <span className={`text-xs ${textMutedOnSurface}`}>
                {measures.deploy_frequency.successful_deploys} deploys
              </span>
            )}
          </div>
          {measures === undefined || targets === undefined ? null : (
            <Sparkbars
              format={(v) => `${v} deploys`}
              max={targets.deploys_per_day}
              met={(v) => v >= targets.deploys_per_day}
              points={measures.daily.map((d) => ({
                label: d.day,
                value: d.deploys,
                partial: d.partial,
              }))}
              target={targets.deploys_per_day}
            />
          )}
        </KpiCard>

        <KpiCard
          met={status?.change_failure_rate}
          target={
            targets === undefined ? PENDING : `< ${pct(targets.change_failure_rate, 0)} per deploy`
          }
          title="Change failure rate"
        >
          <div className="flex items-baseline gap-2">
            {/* Two decimals: 4.96% meets the target, 5.00% does not. */}
            <BigNumber className={toneOf((d) => d.status.change_failure_rate)}>
              {show((d) => pct(d.measures.change_failure_rate.per_deploy.rate, 2))}
            </BigNumber>
            <span className={`text-xs ${textMutedOnSurface}`}>confirmed</span>
          </div>
          {data === undefined ? null : data.flags_source === "none" ? (
            <div className={`text-xs ${textMutedOnCanvas}`}>
              Flags arrive with flag confirmation
            </div>
          ) : (
            <>
              <div className={`text-xs ${textMutedOnSurface}`}>
                up to{" "}
                <span
                  className={`font-semibold ${tone(data.status.change_failure_rate_upper_bound)}`}
                >
                  {pct(data.measures.change_failure_rate.per_deploy.upper_bound_rate, 2)}
                </span>{" "}
                if every pending flag is confirmed
              </div>
              <div className={`text-[11px] ${textMutedOnSurface}`}>
                {failureRateBreakdown(
                  data.measures.change_failure_rate.per_deploy.confirmed,
                  data.measures.change_failure_rate.per_deploy.confirmed_or_pending
                )}
              </div>
            </>
          )}
        </KpiCard>

        <KpiCard
          met={status?.merge_to_production}
          target={
            targets === undefined
              ? PENDING
              : `< ${targets.merge_to_production_minutes}m for every change`
          }
          title={"Merge \u2192 production"}
        >
          <div className="flex flex-wrap items-baseline gap-x-3">
            <span className="flex items-baseline gap-1">
              <BigNumber className={toneOf((d) => d.status.merge_to_production_median)}>
                {show((d) =>
                  formatMinutes(d.measures.lead_time.merge_to_production.median_minutes)
                )}
              </BigNumber>
              <span className={`text-xs ${textMutedOnSurface}`}>median</span>
            </span>
            <span className="flex items-baseline gap-1">
              <BigNumber className={toneOf((d) => d.status.merge_to_production)}>
                {show((d) => formatMinutes(d.measures.lead_time.merge_to_production.max_minutes))}
              </BigNumber>
              <span className={`text-xs ${textMutedOnSurface}`}>max</span>
            </span>
          </div>
          <div className={`text-[11px] ${textMutedOnSurface}`}>
            PRs merged in the window and deployed
          </div>
        </KpiCard>

        <KpiCard
          met={status?.opened_to_merge}
          target={
            targets === undefined ? PENDING : `median < ${targets.opened_to_merge_median_minutes}m`
          }
          title={"PR open \u2192 merge"}
        >
          <div className="flex flex-wrap items-baseline gap-x-3">
            <span className="flex items-baseline gap-1">
              <BigNumber className={toneOf((d) => d.status.opened_to_merge)}>
                {show((d) => formatMinutes(d.measures.lead_time.opened_to_merge.median_minutes))}
              </BigNumber>
              <span className={`text-xs ${textMutedOnSurface}`}>median</span>
            </span>
            <span className="flex items-baseline gap-1">
              <BigNumber className={textPrimaryOnSurface}>
                {show((d) => formatMinutes(d.measures.lead_time.opened_to_merge.p90_minutes))}
              </BigNumber>
              <span className={`text-xs ${textMutedOnSurface}`}>p90</span>
            </span>
          </div>
          <div className={`text-[11px] ${textMutedOnSurface}`}>every PR merged in the window</div>
        </KpiCard>

        <KpiCard
          met={status?.deploy_run_success}
          target={
            targets === undefined ? PENDING : `\u2265 ${pct(targets.deploy_run_success_rate, 0)}`
          }
          title="Deploy runs reaching production"
        >
          <div className="flex items-baseline gap-2">
            <BigNumber className={toneOf((d) => d.status.deploy_run_success)}>
              {show((d) =>
                d.measures.deploy_run_success.rate === null
                  ? "n/a"
                  : pct(d.measures.deploy_run_success.rate)
              )}
            </BigNumber>
            {measures === undefined ? null : (
              <span className={`text-xs ${textMutedOnSurface}`}>
                {measures.deploy_run_success.reached_production} of{" "}
                {measures.deploy_run_success.concluded} concluded
              </span>
            )}
          </div>
          {measures === undefined || targets === undefined ? null : (
            <>
              <Sparkbars
                format={pct}
                max={1}
                met={(v) => v >= targets.deploy_run_success_rate}
                points={measures.daily.map((d) => ({
                  label: d.day,
                  value: d.run_success_rate,
                  partial: d.partial,
                }))}
                target={targets.deploy_run_success_rate}
              />
              <div className={`text-[11px] ${textMutedOnSurface}`}>
                {measures.deploy_run_success.cancelled} cancelled runs not counted
              </div>
            </>
          )}
        </KpiCard>

        <KpiCard
          met={status?.unowned_p0}
          target={targets === undefined ? PENDING : `${targets.unowned_p0}`}
          title="P0 issues with no owner"
        >
          <div className="flex items-baseline gap-2">
            <BigNumber className={toneOf((d) => d.status.unowned_p0)}>
              {show((d) => `${d.unowned_p0.length}`)}
            </BigNumber>
            {data === undefined ? null : (
              <span className={`text-xs ${textMutedOnSurface}`}>
                no claim or route, as of{" "}
                {new Date(data.computed_at).toLocaleTimeString([], {
                  hour: "2-digit",
                  minute: "2-digit",
                })}
              </span>
            )}
          </div>
          {data === undefined ? null : (
            <div className="flex flex-wrap gap-x-2 text-xs">
              {data.unowned_p0.slice(0, UNOWNED_P0_SHOWN).map((issue) => (
                <Link
                  className={`hover:underline ${linkText}`}
                  key={issue.key}
                  title={issue.title}
                  to={buildIssuePath({ key: issue.key, kind: "issue" })}
                >
                  {issue.key}
                </Link>
              ))}
              {data.unowned_p0.length > UNOWNED_P0_SHOWN ? (
                <span
                  className={textMutedOnSurface}
                  title={data.unowned_p0
                    .slice(UNOWNED_P0_SHOWN)
                    .map((issue) => issue.key)
                    .join(", ")}
                >
                  +{data.unowned_p0.length - UNOWNED_P0_SHOWN} more
                </span>
              ) : null}
            </div>
          )}
        </KpiCard>
      </fieldset>

      <div
        className="flex items-center gap-6 overflow-x-auto px-3 py-2 text-sm"
        data-testid="measures-strip"
      >
        {strip.map((stat) => (
          <div className="flex items-baseline gap-2 whitespace-nowrap" key={stat.label}>
            <span className={`text-lg font-semibold ${textPrimaryOnSurface}`}>{stat.value}</span>
            <span className={`text-xs ${textMutedOnCanvas}`}>{stat.label}</span>
          </div>
        ))}
        {data === undefined ? null : (
          <div className="ml-auto shrink-0 whitespace-nowrap">
            <DisclosureToggle
              expanded={open}
              label="Definitions & alternatives"
              onToggle={() => setOpen(!open)}
              textClassName={linkText}
            />
          </div>
        )}
      </div>

      {data === undefined || !open ? null : <Definitions data={data} />}
    </section>
  );
}

/** The fold's eight definition cards and the per-day table, newest first. */
function Definitions({ data }: { data: DeliveryMeasuresResponse }): ReactNode {
  const { measures, targets } = data;
  const frequency = measures.deploy_frequency;
  const lead = measures.lead_time;
  const perPR = measures.change_failure_rate.per_pr;
  const perDeploy = measures.change_failure_rate.per_deploy;
  return (
    <>
      <div
        className={`grid grid-cols-2 gap-4 border-t px-3 pt-3 pb-3 text-sm md:grid-cols-3 ${borderDefault}`}
      >
        <DefinitionCard title="Deploy frequency" value={`${frequency.per_day.toFixed(2)}/day`}>
          {frequency.successful_deploys} successful ({frequency.deploys_with_prs} shipped a PR,{" "}
          {frequency.with_prs_per_day.toFixed(2)}/day)
        </DefinitionCard>

        <DefinitionCard
          title={"Lead time (merge \u2192 prod)"}
          value={formatMinutes(lead.merge_to_production.median_minutes)}
        >
          p90 {formatMinutes(lead.merge_to_production.p90_minutes)}, max{" "}
          {formatMinutes(lead.merge_to_production.max_minutes)}
        </DefinitionCard>

        <DefinitionCard
          title={"Lead time (first commit \u2192 prod)"}
          value={formatMinutes(lead.first_commit_to_production.median_minutes)}
        >
          p90 {formatMinutes(lead.first_commit_to_production.p90_minutes)}
        </DefinitionCard>

        <DefinitionCard
          title={"Lead time (PR opened \u2192 prod)"}
          value={formatMinutes(lead.opened_to_production.median_minutes)}
        >
          p90 {formatMinutes(lead.opened_to_production.p90_minutes)}
        </DefinitionCard>

        <DefinitionCard title="Change failure rate (per PR)" value={pct(perPR.rate)}>
          {perPR.confirmed} confirmed ({perPR.reverts} reverts), {perPR.pending} pending,{" "}
          {perPR.rejected} rejected of {perPR.total} PRs. Incidents: no source yet.
        </DefinitionCard>

        <DefinitionCard title="Change failure rate (per deploy)" value={pct(perDeploy.rate)}>
          {perDeploy.confirmed} of {perDeploy.total} deploys; {pct(perDeploy.upper_bound_rate)} if
          every pending flag is confirmed
        </DefinitionCard>

        <DefinitionCard
          title="Time to restore"
          value={formatMinutes(measures.time_to_restore.median_minutes)}
        >
          median, broken PR deploy {"\u2192"} fix deploy
        </DefinitionCard>

        <DefinitionCard title="Rework share" value={pct(measures.rework_share)}>
          of merged PRs in the current filter/window
        </DefinitionCard>
      </div>

      <div className="px-3 pb-3 text-sm">
        <div className={`mb-1 text-xs uppercase ${textMutedOnCanvas}`}>
          Per day (UTC), newest first
        </div>
        <div className={`max-h-48 overflow-y-auto rounded border ${borderDefault}`}>
          <table className="w-full text-xs tabular-nums">
            <thead className={`sticky top-0 ${surfaceBg} ${textMutedOnSurface}`}>
              <tr>
                <th className="px-2 py-1 text-left font-normal">Day</th>
                <th className="px-2 py-1 text-right font-normal">Deploys</th>
                <th className="px-2 py-1 text-right font-normal">Concluded runs</th>
                <th className="px-2 py-1 text-right font-normal">Reached production</th>
                <th className="px-2 py-1 text-right font-normal">Run success</th>
                <th className="px-2 py-1 text-right font-normal">Cancelled</th>
              </tr>
            </thead>
            <tbody>
              {[...measures.daily].reverse().map((d) => (
                <tr
                  className={`border-t ${borderDefault} ${d.partial ? textMutedOnSurface : textPrimaryOnSurface}`}
                  key={d.day}
                >
                  <td className="px-2 py-0.5">
                    {d.day}
                    {d.partial ? " (partial)" : ""}
                  </td>
                  <td
                    className={`px-2 py-0.5 text-right ${d.partial ? "" : tone(d.deploys >= targets.deploys_per_day)}`}
                  >
                    {d.deploys}
                  </td>
                  <td className="px-2 py-0.5 text-right">{d.concluded}</td>
                  <td className="px-2 py-0.5 text-right">{d.reached_production}</td>
                  <td
                    className={`px-2 py-0.5 text-right ${
                      d.partial || d.run_success_rate === null
                        ? ""
                        : tone(d.run_success_rate >= targets.deploy_run_success_rate)
                    }`}
                  >
                    {d.run_success_rate === null ? "n/a" : pct(d.run_success_rate)}
                  </td>
                  <td className="px-2 py-0.5 text-right">{d.cancelled}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </>
  );
}

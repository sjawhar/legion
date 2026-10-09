import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { deliveryRunQuery } from "../../api/queries";
import type {
  DeliveryComponent,
  DeliveryPR,
  DeliveryRun,
  DeliveryRunJobDetail,
} from "../../api/types";
import {
  borderStrong,
  dangerText,
  dismissButtonText,
  inlineWarningText,
  linkText,
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { NO_SESSION, PLACEHOLDER_LABELS } from "./lib/facets";
import { leadTimeMinutes } from "./lib/prList";
import type { TimelineSelection } from "./Timeline";

const sectionLabel = `mt-2 text-xs uppercase ${textMutedOnSurface}`;

function fmt(iso: string | null): string {
  return iso === null ? "\u2014" : new Date(iso).toLocaleString();
}

function jobDurationMs(job: DeliveryRunJobDetail): number | null {
  if (job.started_at === null || job.completed_at === null) return null;
  return new Date(job.completed_at).getTime() - new Date(job.started_at).getTime();
}

function PRDetail({
  pr,
  components,
}: {
  pr: DeliveryPR;
  components: Readonly<Record<string, DeliveryComponent>>;
}): ReactNode {
  const leadTime = leadTimeMinutes(pr);
  let componentText = "No issue";
  if (pr.issue !== null) {
    componentText =
      pr.components.length === 0
        ? "No component"
        : pr.components.map((id) => components[id]?.title ?? id).join(", ");
  }
  return (
    <div className="flex flex-col gap-2">
      <h3 className={`pr-6 text-lg font-semibold ${textPrimaryOnSurface}`}>
        {pr.id}{" "}
        {pr.rework ? (
          <span className={`ml-2 align-middle text-xs ${inlineWarningText}`}>rework</span>
        ) : null}
      </h3>
      <p className={textSecondaryOnSurface}>{pr.title}</p>
      <a className={`${linkText} underline`} href={pr.url} rel="noreferrer" target="_blank">
        Open on GitHub
      </a>
      <dl className={`grid grid-cols-2 gap-x-4 gap-y-1 text-sm ${textPrimaryOnSurface}`}>
        <dt className={textMutedOnSurface}>Author</dt>
        <dd>{pr.author}</dd>
        <dt className={textMutedOnSurface}>Dispatch issue</dt>
        <dd>
          {pr.issue === null ? (
            "\u2014"
          ) : (
            <Link
              className={`${linkText} underline`}
              to={buildIssuePath({ key: pr.issue, kind: "issue" })}
            >
              {pr.issue}
              {pr.issue_title === null ? "" : ` \u2014 ${pr.issue_title}`}
            </Link>
          )}
        </dd>
        <dt className={textMutedOnSurface}>Priority</dt>
        <dd>{pr.issue === null ? "No issue" : (pr.priority ?? "No priority")}</dd>
        <dt className={textMutedOnSurface}>Components</dt>
        <dd>{componentText}</dd>
        <dt className={textMutedOnSurface}>Parent agent(s)</dt>
        <dd>{pr.parent_agent ?? PLACEHOLDER_LABELS[NO_SESSION]}</dd>
        <dt className={textMutedOnSurface}>Sessions</dt>
        <dd className="truncate" title={pr.sessions.join(", ")}>
          {pr.sessions.length === 0 ? "\u2014" : pr.sessions.join(", ")}
        </dd>
        <dt className={textMutedOnSurface}>Merged</dt>
        <dd>{fmt(pr.merged_at)}</dd>
        <dt className={textMutedOnSurface}>Deployed</dt>
        <dd>{fmt(pr.deployed_at)}</dd>
        <dt className={textMutedOnSurface}>Lead time (merge→prod)</dt>
        <dd>{leadTime === undefined ? "not yet deployed" : `${Math.round(leadTime)} min`}</dd>
      </dl>
      {pr.partial ? (
        <p className={`text-sm ${textMutedOnSurface}`}>
          Still completing: some fields await the next reconcile pass.
        </p>
      ) : null}
      {pr.unfetchable_reason === null ? null : (
        <p className={`text-sm ${textMutedOnSurface}`}>
          Can no longer be fetched from GitHub: {pr.unfetchable_reason}
        </p>
      )}
    </div>
  );
}

/** Every job of the run, with each result, read once the drill-down opens. */
function RunJobs({ runId, durations }: { runId: number; durations: boolean }): ReactNode {
  const detail = useQuery(deliveryRunQuery(runId));
  if (detail.isPending) return <div className={`text-sm ${textMutedOnSurface}`}>Loading…</div>;
  if (detail.isError) {
    return <div className={`text-sm ${textMutedOnSurface}`}>The run's jobs could not be read.</div>;
  }
  return detail.data.jobs.map((job) => {
    const durationMs = jobDurationMs(job);
    return (
      <div className="flex justify-between gap-2 text-sm" key={job.name}>
        <span className={textPrimaryOnSurface}>{job.name}</span>
        <span className={job.conclusion === "failure" ? dangerText : textSecondaryOnSurface}>
          {job.conclusion}
          {durations && durationMs !== null ? ` (${Math.round(durationMs / 1000)}s)` : ""}
        </span>
      </div>
    );
  });
}

function DeployDetail({ run }: { run: DeliveryRun }): ReactNode {
  return (
    <div className="flex flex-col gap-2">
      <h3 className={`pr-6 text-lg font-semibold ${textPrimaryOnSurface}`}>
        Deploy: run #{run.id}
      </h3>
      <a className={`${linkText} underline`} href={run.url} rel="noreferrer" target="_blank">
        Open run
      </a>
      <div>
        <div className={sectionLabel}>PRs shipped ({run.prs.length})</div>
        {run.prs.map((pr) => (
          <div className={`text-sm ${textPrimaryOnSurface}`} key={pr.id}>
            {pr.id} {`\u2014 ${pr.title}`}
          </div>
        ))}
      </div>
      <div>
        <div className={sectionLabel}>Jobs</div>
        <RunJobs durations runId={run.id} />
      </div>
    </div>
  );
}

function FailureDetail({ run }: { run: DeliveryRun }): ReactNode {
  return (
    <div className="flex flex-col gap-2">
      <h3 className={`pr-6 text-lg font-semibold ${dangerText}`}>
        Pipeline failure: run #{run.id}
      </h3>
      <a className={`${linkText} underline`} href={run.url} rel="noreferrer" target="_blank">
        Open run
      </a>
      <div>
        <div className={sectionLabel}>Failed jobs</div>
        {run.failed_jobs.map((job) => (
          <div className={`text-sm ${dangerText}`} key={job.name}>
            {job.name} at {fmt(job.completed_at)}
          </div>
        ))}
        {run.failed_jobs.length === 0 && run.production?.conclusion === "failure" ? (
          <div className={`text-sm ${dangerText}`}>
            the production job at {fmt(run.production.completed_at)}
          </div>
        ) : null}
      </div>
      <div>
        <div className={sectionLabel}>All jobs</div>
        <RunJobs durations={false} runId={run.id} />
      </div>
    </div>
  );
}

/** The timeline and the list's shared drill-down: a PR, a deploy, or a pipeline failure, in a
 *  full-height panel at the right edge with a close button. */
export function DrillDown({
  prs,
  runs,
  components,
  selection,
  onClose,
}: {
  prs: readonly DeliveryPR[];
  runs: readonly DeliveryRun[];
  components: Readonly<Record<string, DeliveryComponent>>;
  selection: TimelineSelection | null;
  onClose: () => void;
}): ReactNode {
  if (selection === null) return null;

  let body: ReactNode;
  if (selection.kind === "pr") {
    const pr = prs.find((candidate) => candidate.id === selection.id);
    body =
      pr === undefined ? (
        <div>PR {selection.id} not found.</div>
      ) : (
        <PRDetail components={components} pr={pr} />
      );
  } else {
    const run = runs.find((candidate) => candidate.id === selection.id);
    if (run === undefined) body = <div>Run {selection.id} not found.</div>;
    else if (selection.kind === "deploy") body = <DeployDetail run={run} />;
    else body = <FailureDetail run={run} />;
  }

  return (
    <aside
      aria-label="Details"
      className={`fixed top-0 right-0 z-30 h-full w-96 max-w-full overflow-y-auto border-l p-4 shadow-xl ${borderStrong} ${surfaceBg}`}
    >
      <button
        aria-label="Close details"
        className={`float-right ${dismissButtonText}`}
        onClick={onClose}
        type="button"
      >
        ✕
      </button>
      {body}
    </aside>
  );
}

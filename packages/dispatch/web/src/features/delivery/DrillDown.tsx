import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { DeliveryPR, DeliveryRun } from "../../api/types";
import { linkText, textMutedOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { DEPLOYED_LABELS } from "./lib/facets";
import { githubPrUrl } from "./lib/links";
import type { TimelineSelection } from "./Timeline";

function fmt(iso: string | null): ReactNode {
  return iso === null ? (
    <span className={textMutedOnCanvas}>{"\u2014"}</span>
  ) : (
    <Timestamp at={iso} />
  );
}

/** The priority/components a PR's linked Dispatch issue carries. `DeliveryPR` itself carries
 *  neither (LEGION-567's plan API keeps them off the wire, since only the server has the joined
 *  issue data) — fetched lazily, once the drill-down opens on a PR with a linked issue. */
function IssueDetails({ issueKey }: { issueKey: string }): ReactNode {
  const issue = useQuery({ queryKey: ["issue", issueKey], queryFn: () => api.getIssue(issueKey) });
  if (issue.isPending) return <p className={textMutedOnCanvas}>Loading issue…</p>;
  if (issue.isError) return <p className={textMutedOnCanvas}>Issue details unavailable.</p>;
  const { priority, components } = issue.data;
  return (
    <p className={textSecondaryOnCanvas}>
      Priority: {priority === null ? "None" : `P${priority}`} · Components:{" "}
      {components.ids.length === 0 ? "None" : components.ids.join(", ")}
    </p>
  );
}

function PRDetail({ pr }: { pr: DeliveryPR }): ReactNode {
  return (
    <div className="flex flex-col gap-2 text-sm">
      <h3 className={`font-semibold ${textSecondaryOnCanvas}`}>{pr.title}</h3>
      <p>
        <a
          className={linkText}
          href={githubPrUrl(pr.repo, pr.number)}
          rel="noreferrer"
          target="_blank"
        >
          {pr.id} on GitHub
        </a>
      </p>
      {pr.issue === null ? (
        <p className={textMutedOnCanvas}>No Dispatch issue linked.</p>
      ) : (
        <>
          <p>
            <Link className={linkText} to={buildIssuePath({ key: pr.issue, kind: "issue" })}>
              {pr.issue}
            </Link>
          </p>
          <IssueDetails issueKey={pr.issue} />
        </>
      )}
      <p className={textSecondaryOnCanvas}>Author: {pr.author}</p>
      <p className={textSecondaryOnCanvas}>
        Parent agent: {pr.parent_agent ?? "None"}
        {pr.sessions.length === 0 ? "" : ` (sessions: ${pr.sessions.join(", ")})`}
      </p>
      <p className={textSecondaryOnCanvas}>Merged: {fmt(pr.merged_at)}</p>
      <p className={textSecondaryOnCanvas}>
        {DEPLOYED_LABELS[pr.deployed_status]}
        {pr.deploy_run === null ? "" : ` · deploy run ${pr.deploy_run}`}
        {pr.deployed_at === null ? "" : <> · {fmt(pr.deployed_at)}</>}
      </p>
      {pr.rework ? <p className={textSecondaryOnCanvas}>Rework (fix/revert/hotfix).</p> : null}
      {pr.partial ? (
        <p className={textMutedOnCanvas}>
          Still completing: some fields await the next reconcile pass.
        </p>
      ) : null}
      {pr.unfetchable_reason === null ? null : (
        <p className={textMutedOnCanvas}>
          Can no longer be fetched from GitHub: {pr.unfetchable_reason}
        </p>
      )}
    </div>
  );
}

function DeployDetail({ run, prs }: { run: DeliveryRun; prs: readonly DeliveryPR[] }): ReactNode {
  const shipped = run.prs
    .map((id) => prs.find((pr) => pr.id === id))
    .filter((pr) => pr !== undefined);
  return (
    <div className="flex flex-col gap-2 text-sm">
      <h3 className={`font-semibold ${textSecondaryOnCanvas}`}>Deploy run {run.id}</h3>
      <p>
        <a className={linkText} href={run.url} rel="noreferrer" target="_blank">
          Run on GitHub Actions
        </a>
      </p>
      <p className={textSecondaryOnCanvas}>Started: {fmt(run.started_at)}</p>
      <p className={textSecondaryOnCanvas}>Completed: {fmt(run.completed_at)}</p>
      <p className={textSecondaryOnCanvas}>Shipped {run.prs.length} PR(s):</p>
      <ul className="list-disc pl-5">
        {shipped.map((pr) => (
          <li key={pr.id}>
            <a
              className={linkText}
              href={githubPrUrl(pr.repo, pr.number)}
              rel="noreferrer"
              target="_blank"
            >
              {pr.id}
            </a>{" "}
            {pr.title}
          </li>
        ))}
        {run.prs.length > shipped.length ? (
          <li className={textMutedOnCanvas}>
            {run.prs.length - shipped.length} more outside the current window.
          </li>
        ) : null}
      </ul>
    </div>
  );
}

function FailureDetail({ run }: { run: DeliveryRun }): ReactNode {
  return (
    <div className="flex flex-col gap-2 text-sm">
      <h3 className={`font-semibold ${textSecondaryOnCanvas}`}>Pipeline failure: run {run.id}</h3>
      <p>
        <a className={linkText} href={run.url} rel="noreferrer" target="_blank">
          Run on GitHub Actions
        </a>
      </p>
      <p className={textSecondaryOnCanvas}>Started: {fmt(run.started_at)}</p>
      {run.root_failing_job === null ? null : (
        <p className={textSecondaryOnCanvas}>
          Root failing job: {run.root_failing_job.name}
          {run.root_failing_job.completed_at === null ? (
            ""
          ) : (
            <> · {fmt(run.root_failing_job.completed_at)}</>
          )}
        </p>
      )}
      <p className={textSecondaryOnCanvas}>Failed jobs:</p>
      <ul className="list-disc pl-5">
        {run.failed_jobs.map((job) => (
          <li key={job.name}>
            {job.name}
            {job.completed_at === null ? "" : <> · {fmt(job.completed_at)}</>}
          </li>
        ))}
      </ul>
    </div>
  );
}

/** The timeline/list's shared drill-down: a PR, a deploy, or a pipeline failure, per the
 *  selection's `kind` — the prototype's three selection kinds, ported as-is. */
export function DrillDown({
  prs,
  runs,
  selection,
  onClose,
}: {
  prs: readonly DeliveryPR[];
  runs: readonly DeliveryRun[];
  selection: TimelineSelection | null;
  onClose: () => void;
}): ReactNode {
  if (selection === null) return null;

  let body: ReactNode;
  if (selection.kind === "pr") {
    const pr = prs.find((candidate) => candidate.id === selection.id);
    body =
      pr === undefined ? (
        <p>PR {selection.id} not found in the current window.</p>
      ) : (
        <PRDetail pr={pr} />
      );
  } else {
    const run = runs.find((candidate) => candidate.id === selection.id);
    if (run === undefined) {
      body = <p>Run {selection.id} not found in the current window.</p>;
    } else if (selection.kind === "deploy") {
      body = <DeployDetail prs={prs} run={run} />;
    } else {
      body = <FailureDetail run={run} />;
    }
  }

  return (
    <aside className="flex w-96 shrink-0 flex-col gap-3 border-l p-4" aria-label="Details">
      <button className={`self-end text-xs ${textMutedOnCanvas}`} onClick={onClose} type="button">
        Close
      </button>
      {body}
    </aside>
  );
}

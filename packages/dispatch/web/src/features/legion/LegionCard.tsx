// One issue on the Legion view: everything the card shows comes from `GET /api/v1/legion/board`,
// which is what the sampler recorded of the project's daemon. Nothing here writes: the daemon owns
// a Legion issue's status and phase, so the card's priority is a badge rather than the Issues
// board's editor.
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { LegionBoardCard } from "../../api/types";
import { Pill } from "../../components/Pill";
import {
  borderDefault,
  card,
  cardHoverBorder,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { formatPhaseTime } from "./lib/format";

/** One fact of the card: its name, the value, and the `data-` hook an end-to-end test reads it
 *  by. The name is written out rather than abbreviated, so a screen reader hears "Slot 2" and not
 *  "2". */
function Fact({
  hook,
  label,
  value,
}: {
  readonly hook: string;
  readonly label: string;
  readonly value: string;
}): ReactNode {
  return (
    <span className={`inline-flex gap-1 ${textSecondaryOnSurface}`} data-legion-fact={hook}>
      <span className={textMutedOnSurface}>{label}</span>
      <span>{value}</span>
    </span>
  );
}

/** One card of the Legion view: the issue's key and title, its priority, how long it has been in
 *  its phase, the slot it holds, the roles in a turn now and its pull request's CI verdict, plus
 *  what holds it where it is — the issues a blocker link names, what it waits on from a person,
 *  and how many issues of its tree have not started and so have no card of their own.
 *
 *  `nowMs` is the moment the time in the phase is measured from, which the view passes to every
 *  card so one board reads one clock. */
export function LegionCard({
  card: issue,
  nowMs,
}: {
  readonly card: LegionBoardCard;
  readonly nowMs: number;
}): ReactNode {
  return (
    <article
      aria-label={`${issue.key} ${issue.title}`}
      className={`rounded-xl border p-3 ${card} ${cardHoverBorder}`}
      data-legion-card={issue.key}
    >
      <Link
        className={`flex flex-col text-sm ${linkText} ${linkHoverText}`}
        to={buildIssuePath({ key: issue.key, kind: "issue" })}
      >
        <span className="self-start font-semibold">{issue.key}</span>
        <span className={`mt-1 self-start ${textPrimaryOnSurface}`}>{issue.title}</span>
      </Link>
      <div className="mt-2 flex flex-wrap items-center gap-2 text-xs">
        <span data-legion-fact="priority">
          <Pill>{issue.priority === null ? "No priority" : `P${issue.priority}`}</Pill>
        </span>
        <Fact
          hook="phase-time"
          label="In phase"
          value={formatPhaseTime(issue.phase_since, nowMs)}
        />
        <Fact hook="slot" label="Slot" value={issue.slot === null ? "none" : String(issue.slot)} />
        <Fact
          hook="roles"
          label="In a turn"
          value={issue.roles.length === 0 ? "nobody" : issue.roles.join(", ")}
        />
        <Fact
          hook="checks"
          label="CI"
          value={issue.checks_verdict === null ? "no verdict" : issue.checks_verdict}
        />
      </div>
      {issue.blocked_by.length === 0 ? null : (
        <p
          className={`mt-2 border-t pt-2 text-xs ${borderDefault} ${textSecondaryOnSurface}`}
          data-legion-fact="blocked-by"
        >
          Blocked by {issue.blocked_by.join(", ")}
        </p>
      )}
      {issue.waiting_on === null ? null : (
        <p
          className={`mt-2 border-t pt-2 text-xs ${borderDefault} ${textSecondaryOnSurface}`}
          data-legion-fact="waiting-on"
        >
          Waiting on {issue.waiting_on.kind}
          {issue.waiting_on.since === null
            ? ""
            : ` since ${new Date(issue.waiting_on.since).toLocaleString()}`}
          {issue.waiting_on.answered_at === null ? "" : "; answered"}
        </p>
      )}
      {issue.children === 0 ? null : (
        <p className={`mt-2 text-xs ${textMutedOnSurface}`} data-legion-fact="children">
          {issue.children} issue
          {issue.children === 1 ? " of this tree has" : "s of this tree have"} not started
        </p>
      )}
    </article>
  );
}

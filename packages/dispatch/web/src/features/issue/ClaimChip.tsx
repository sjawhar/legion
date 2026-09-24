import type { ReactNode } from "react";

import type { IssueClaim } from "../../api/types";
import { pillClassName } from "../../components/Pill";
import { useAgents } from "../conversation/useAgents";
import { actorLabel } from "../refs/actor";
import { Timestamp } from "../refs/Timestamp";

/** What a claim is judged against: the live agent registry as `useAgents` returns it. */
export interface AgentRegistry {
  readonly isError: boolean;
  readonly isPending: boolean;
  readonly titles: ReadonlyMap<string, string>;
}

/**
 * Whether a claim's session has lapsed: the registry has loaded and does not list it, which is
 * exactly when the server lets any agent take the issue. A human's claim never lapses - there
 * is no session to end, and only a human releases or forces it.
 *
 * The loaded test is the whole point. A chip paints the stamped title before the registry
 * answers, so "the registry has not arrived" and "the holder is gone" look identical to a bare
 * `titles.has(id)`; treating the first as lapsed would mark every claim on the page free for a
 * moment, and mark them all free for good whenever the listener is unreachable.
 */
export function claimHasLapsed(claim: IssueClaim | null, registry: AgentRegistry): boolean {
  if (claim === null || claim.actor.kind !== "session") {
    return false;
  }
  if (registry.isPending || registry.isError) {
    return false;
  }
  return !registry.titles.has(claim.actor.id);
}

/**
 * "Claimed by <holder> · <age>" wherever an issue is shown: the issue header's state row, a
 * List row, a Board card. The holder is named by `actorLabel` from `@legion/contracts` — the
 * agent registry's live title, else the title stamped on the claim, else `session:<8>…` — and
 * the agent tools call that same function over the same registry, so a session reading
 * `dispatch_read` and a human reading this page see one name for one holder. The registry is
 * read here rather than passed in, so no surface can forget to pass it. The age keeps itself current through the shared `Timestamp` tick, and the
 * whole label with the absolute time is in the `title`, which also says what a claim is: the
 * chip sits among status, priority and assignee, and nothing else on the page defines it. An
 * unclaimed issue renders nothing at all, so a board of unclaimed work stays quiet.
 *
 * A holder the registry no longer lists reads "· not running" in the muted tone, because that
 * claim is free for the taking and a chip that looked held would hide pickable work — the same
 * judgement the Unclaimed filter makes through `claimHasLapsed`.
 *
 * A session title can be any length, and no name may widen the page (the rule
 * `IssueHeader.tsx` cites for the metadata rail). `pillClassName` sets `shrink-0`, and no class
 * reliably beats it: `shrink` is in the same Tailwind group, and `[flex-shrink:1]` measured a
 * computed `flex-shrink` of 0 on the built page, so the shrink is an inline style, which
 * cannot lose whatever the stylesheet order. The chip also caps itself at the row's width.
 * Only the holder's name ellipsizes:
 * "Claimed by", the age and "not running" stay whole, because a chip reading "Claimed…" would
 * say nothing.
 */
export function ClaimChip({ claim }: { claim: IssueClaim | null }): ReactNode {
  const holdsSession = claim?.actor.kind === "session";
  const registry = useAgents(holdsSession);
  if (claim === null) {
    return null;
  }
  const holder = actorLabel(claim.actor, registry.titles);
  const lapsed = claimHasLapsed(claim, registry);
  const since = `Claimed by ${holder} · since ${new Date(claim.at).toLocaleString()}.`;
  const explains =
    "A claim marks who is implementing this issue, so two agents never take the same work.";
  return (
    <span
      className={`${pillClassName(lapsed ? "label" : "selected-label")} min-w-0 max-w-full gap-1`}
      data-testid="issue-claim"
      style={{ flexShrink: 1 }}
      title={
        lapsed
          ? `${since} ${explains} That session is no longer running, so anyone may claim it.`
          : `${since} ${explains}`
      }
    >
      <span className="shrink-0">Claimed by</span>
      <span className="min-w-0 truncate">{holder}</span>
      <span aria-hidden="true" className="shrink-0">
        ·
      </span>
      <Timestamp at={claim.at} className="shrink-0" />
      {lapsed ? (
        <>
          <span aria-hidden="true" className="shrink-0">
            ·
          </span>
          <span className="shrink-0" data-testid="issue-claim-lapsed">
            not running
          </span>
        </>
      ) : null}
    </span>
  );
}

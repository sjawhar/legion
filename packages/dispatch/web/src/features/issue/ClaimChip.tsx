import type { ReactNode } from "react";

import type { IssueClaim } from "../../api/types";
import { pillClassName } from "../../components/Pill";
import { useAgents } from "../conversation/useAgents";
import { actorLabel } from "../refs/actor";
import { Timestamp } from "../refs/Timestamp";

/**
 * "Claimed by <holder> · <age>" wherever an issue is shown: the issue header's state row, a
 * List row, a Board card. The holder is named by `actorLabel` from `@legion/contracts` — the
 * agent registry's live title, else the title stamped on the claim, else `session:<8>…` — and
 * the agent tools call that same function over the same registry, so a session reading
 * `dispatch_read` and a human reading this page see one name for one holder. The registry is
 * read here rather than passed in, so no surface can forget to pass it. The age keeps itself current through the shared `Timestamp` tick, and the
 * whole label with the absolute time is in the `title`. An unclaimed issue renders nothing at
 * all, so a board of unclaimed work stays quiet.
 *
 * A session title can be any length, and no name may widen the page (the rule
 * `IssueHeader.tsx` cites for the metadata rail). `pillClassName` sets `shrink-0`, and no class
 * reliably beats it: `shrink` is in the same Tailwind group, and `[flex-shrink:1]` measured a
 * computed `flex-shrink` of 0 on the built page, so the shrink is an inline style, which
 * cannot lose whatever the stylesheet order. The chip also caps itself at the row's width.
 * Only the holder's name ellipsizes:
 * "Claimed by" and the age stay whole, because a chip reading "Claimed…" would say nothing.
 */
export function ClaimChip({ claim }: { claim: IssueClaim | null | undefined }): ReactNode {
  const holdsSession = claim?.actor.kind === "session";
  const { titles } = useAgents(holdsSession);
  if (claim === null || claim === undefined) {
    return null;
  }
  const holder = actorLabel(claim.actor, titles);
  return (
    <span
      className={`${pillClassName("selected-label")} min-w-0 max-w-full gap-1`}
      data-testid="issue-claim"
      style={{ flexShrink: 1 }}
      title={`Claimed by ${holder} · since ${new Date(claim.at).toLocaleString()}`}
    >
      <span className="shrink-0">Claimed by</span>
      <span className="min-w-0 truncate">{holder}</span>
      <span aria-hidden="true" className="shrink-0">
        ·
      </span>
      <Timestamp at={claim.at} className="shrink-0" />
    </span>
  );
}

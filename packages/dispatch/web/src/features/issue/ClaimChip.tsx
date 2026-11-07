import { claimHolds } from "@legion/contracts";
import type { ReactNode } from "react";

import type { IssueClaim } from "../../api/types";
import { pillClassName } from "../../components/Pill";
import { AgentName } from "../refs/AgentName";
import { type AgentRegistry, useAgentRegistry } from "../refs/agent-registry";
import { Timestamp } from "../refs/Timestamp";
import { PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * Whether a claim's session has lapsed: the registry's latest read succeeded and does not list
 * it (`not-running`), which is exactly when the server lets any agent take the issue. A human's
 * claim never lapses - there is no session to end, and only a human releases or forces it. The
 * judgement is `claimHolds` in `@legion/contracts`, the one the agent tools print as
 * "not running" too.
 *
 * The loaded test is the whole point. A chip paints the stamped title before the registry
 * answers, so "the registry has not arrived" and "the holder is gone" look identical to a bare
 * lookup; the registry's `unknown` state (no successful read yet, or the latest read failed) is
 * never lapsed, or every claim on the page would read free for a moment, and free for good
 * whenever the listener is unreachable. So `claimHolds` gets the registry's titles only once a
 * read has landed.
 */
export function claimHasLapsed(
  claim: IssueClaim,
  registry: Pick<AgentRegistry, "state" | "titles">
): boolean {
  return (
    claimHolds(
      claim,
      registry.state(claim.actor.id) === "unknown" ? undefined : registry.titles
    ) === "lapsed"
  );
}

/**
 * "Claimed by <holder> · <age>" wherever an issue is shown: the issue header's state row, a
 * List row, a Board card. The holder is drawn by `AgentName`, as every agent is: its current
 * registry title, else the title stamped on the claim, else `session:<8>…`, with its running
 * state and a link to its live conversation. The age keeps itself current through the shared
 * `Timestamp` tick, and on a surface that raises no tooltip - the issue page
 * (dispatch://INFRA-1196 R21) - the instant itself follows it in words, because the time a
 * claim was taken is what decides whether it is stale and a tooltip saying so reaches no
 * reader on a phone or on the keyboard. Where a tooltip does carry it, a Board card and a List
 * row, it is not drawn as well: it would say the same thing twice and widen a chip those hosts
 * have no room for (`Timestamp.tsx`). An unclaimed issue renders nothing at all, so a board of
 * unclaimed work stays quiet.
 *
 * A holder the registry no longer lists reads "· not running" in the muted tone, beside its
 * name's own not-running mark, because that claim is free for the taking and a chip that looked
 * held would hide pickable work — the same judgement the Unclaimed filter makes through
 * `claimHasLapsed`.
 *
 * A session title can be any length, and no name may widen the page (the rule
 * `IssueHeader.tsx` cites for the metadata rail). `pillClassName` sets `shrink-0`, and no class
 * reliably beats it: `shrink` is in the same Tailwind group, and `[flex-shrink:1]` measured a
 * computed `flex-shrink` of 0 on the built page, so the shrink is an inline style, which
 * cannot lose whatever the stylesheet order. The chip also caps itself at the row's width.
 * Only the holder's name ellipsizes:
 * "Claimed by", the age and "not running" stay whole, because a chip reading "Claimed…" would
 * say nothing. The name is an inline link, so it keeps the chip's line box at a phone width.
 */
export function ClaimChip({ claim }: { claim: IssueClaim | null }): ReactNode {
  const registry = useAgentRegistry(claim?.actor.kind === "session");
  // No agent text is read here: the holder is drawn by `AgentName` and the instant by `Timestamp`.
  if (claim === null) {
    return null;
  }
  const lapsed = claimHasLapsed(claim, registry);
  return (
    <span
      className={`${pillClassName(lapsed ? "label" : "selected-label")} min-w-0 max-w-full gap-1`}
      data-testid="issue-claim"
      style={{ flexShrink: 1 }}
    >
      <span className="shrink-0" {...pageChrome}>
        {PROPERTY_LABELS.claim}
      </span>
      <AgentName actor={claim.actor} className="min-w-0 truncate" inline />
      <span aria-hidden="true" className="shrink-0" {...pageChrome}>
        ·
      </span>
      {/* The instant itself where no tooltip carries it: how long a session has held the issue
          is what decides whether its claim is stale, and on the issue page a tooltip saying so
          reaches no reader on a phone or on the keyboard (dispatch://INFRA-1196 R21). */}
      <Timestamp at={claim.at} className="shrink-0" exact />
      {lapsed ? (
        <>
          <span aria-hidden="true" className="shrink-0" {...pageChrome}>
            ·
          </span>
          <span className="shrink-0" data-testid="issue-claim-lapsed" {...pageChrome}>
            {CONTROL_LABELS.not_running}
          </span>
        </>
      ) : null}
    </span>
  );
}

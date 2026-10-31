import {
  type ClaimHolderLiveness,
  type ClaimHolding,
  claimHolds,
  claimTakeableAt,
} from "@legion/contracts";
import type { ReactNode } from "react";

import { AGENT_LIST_REFILLING } from "../../api/client";
import type { IssueClaim } from "../../api/types";
import { pillClassName } from "../../components/Pill";
import { AgentName, useAgentText } from "../refs/AgentName";
import { type AgentRegistry, useAgentRegistry } from "../refs/agent-registry";
import { Timestamp } from "../refs/Timestamp";

/**
 * What the registry says about a claim's holder, as `claimHolds` judges it. The registry lists
 * the unresponsive sessions beside the live ones and marks each row's state, so membership is
 * no longer the whole answer: a holder that has missed its heartbeats without ending still
 * holds the issue (`unresponsive`), and only a session the latest successful read does not list
 * at all has lapsed, which is exactly when the server lets any agent take the issue. A human's
 * claim never lapses - there is no session to end, and only a human releases or forces it.
 *
 * The current test is the whole point. A chip paints the stamped title before the registry
 * answers, so "the registry has not arrived" and "the holder is gone" look identical to a bare
 * lookup; the registry's `unknown` state (no successful read yet, or the latest read failed) is
 * never lapsed, or every claim on the page would read free for a moment, and free for good
 * whenever the listener is unreachable or refilling. So `claimHolds` gets the registry's titles
 * only once a read has landed and the latest one succeeded.
 */
export function claimHolding(
  claim: IssueClaim,
  registry: Pick<AgentRegistry, "state" | "titles">
): ClaimHolding {
  const state = registry.state(claim.actor.id);
  return claimHolds(claim, state === "unknown" ? undefined : registry.titles, {
    holder_state:
      state === "unresponsive" ? "unresponsive" : state === "not-running" ? "gone" : null,
  });
}

/** Whether a claim is free for any agent to take: its session is gone, judged by
 *  `claimHolding`. An unresponsive holder is not - it is still working the issue. */
export function claimHasLapsed(
  claim: IssueClaim,
  registry: Pick<AgentRegistry, "state" | "titles">
): boolean {
  return claimHolding(claim, registry) === "lapsed";
}

/**
 * "Claimed by <holder> · <age>" wherever an issue is shown: the issue header's state row, a
 * List row, a Board card. The holder is drawn by `AgentName`, as every agent is: its current
 * registry title, else the title stamped on the claim, else `session:<8>…`, with its running
 * state and a link to its live conversation. The age keeps itself current through the shared
 * `Timestamp` tick, and the whole label with the absolute time is in the `title`, which also
 * says what a claim is: the chip sits among status, priority and assignee, and nothing else on
 * the page defines it. An unclaimed issue renders nothing at all, so a board of unclaimed work
 * stays quiet.
 *
 * A holder the registry no longer lists reads "· not running" in the muted tone, beside its
 * name's own not-running mark, because that claim is free for the taking and a chip that looked
 * held would hide pickable work — the same judgement the Unclaimed filter makes through
 * `claimHasLapsed`. A holder that has only missed its heartbeats reads "· unresponsive" and
 * keeps the held tone: that issue is not free, and the chip names the moment it becomes free so
 * a reader waiting on it knows how long (`liveness`, the issue read's own `takeable_at`, which
 * the server refuses a take by). Without a `liveness` - a List row, a Board card, whose rows
 * carry no such read - the state is the registry's and the takeable moment is the holder's last
 * heartbeat plus the shared grace, the same sum the server uses.
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
export function ClaimChip({
  claim,
  liveness,
}: {
  claim: IssueClaim | null;
  liveness?: ClaimHolderLiveness;
}): ReactNode {
  const registry = useAgentRegistry(claim?.actor.kind === "session");
  const text = useAgentText();
  if (claim === null) {
    return null;
  }
  // The issue read judged the holder against its own record; a row without one is judged by the
  // registry, which lists every session the listener still holds, live or unresponsive, and
  // whose heartbeat then dates the wait.
  const read: ClaimHolderLiveness = liveness?.holder_state == null ? {} : liveness;
  const judged = read.holder_state == null;
  const holding = judged ? claimHolding(claim, registry) : claimHolds(claim, registry.titles, read);
  const lapsed = holding === "lapsed";
  const unresponsive = holding === "unresponsive";
  const takeableAt = !unresponsive
    ? null
    : claimTakeableAt(
        judged ? { holder_state: "unresponsive" } : read,
        registry.lastSeen(claim.actor.id)
      );
  const since = `Claimed by ${text.label(claim.actor)} · since ${new Date(claim.at).toLocaleString()}.`;
  const explains =
    "A claim marks who is implementing this issue, so two agents never take the same work.";
  const lapsedNote = lapsed ? " That session is no longer running, so anyone may claim it." : "";
  const unresponsiveNote = !unresponsive
    ? ""
    : ` That session has missed its heartbeats without ending, so it keeps this issue${
        takeableAt === null
          ? "; another agent may take it once the listener gives it up"
          : ` until ${new Date(takeableAt).toLocaleString()}, when another agent may take it`
      }.`;
  // A session claim the registry could not judge says so, rather than pass for a held one.
  const uncheckedNote =
    claim.actor.kind === "session" && registry.isError
      ? ` Couldn't check whether that session is still running: ${
          registry.refilling ? AGENT_LIST_REFILLING : "Envoy listener unreachable."
        }`
      : "";
  return (
    <span
      className={`${pillClassName(lapsed ? "label" : "selected-label")} min-w-0 max-w-full gap-1`}
      data-testid="issue-claim"
      style={{ flexShrink: 1 }}
      title={`${since} ${explains}${lapsedNote}${unresponsiveNote}${uncheckedNote}`}
    >
      <span className="shrink-0">Claimed by</span>
      <AgentName actor={claim.actor} className="min-w-0 truncate" inline />
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
      {unresponsive ? (
        <>
          <span aria-hidden="true" className="shrink-0">
            ·
          </span>
          <span className="shrink-0" data-testid="issue-claim-unresponsive">
            unresponsive
            {takeableAt === null ? null : (
              <>
                , takeable <Timestamp at={takeableAt} />
              </>
            )}
          </span>
        </>
      ) : null}
    </span>
  );
}

import { claimHolds } from "@legion/contracts";
import type { ReactNode } from "react";

import { AGENT_LIST_REFILLING } from "../../api/client";
import type { IssueClaim } from "../../api/types";
import { pillClassName } from "../../components/Pill";
import { AgentName, useAgentText } from "../refs/AgentName";
import { type AgentRegistry, useAgentRegistry } from "../refs/agent-registry";
import { Timestamp } from "../refs/Timestamp";

/**
 * Whether a claim's session has lapsed: the registry's latest read succeeded and does not list
 * it (`not-running`), which is exactly when the server lets any agent take the issue. A human's
 * claim never lapses - there is no session to end, and only a human releases or forces it. The
 * judgement is `claimHolds` in `@legion/contracts`, the one the agent tools print as
 * "not running" too.
 *
 * The current test is the whole point. A chip paints the stamped title before the registry
 * answers, so "the registry has not arrived" and "the holder is gone" look identical to a bare
 * lookup; the registry's `unknown` state (no successful read yet, or the latest read failed) is
 * never lapsed, or every claim on the page would read free for a moment, and free for good
 * whenever the listener is unreachable or refilling. So `claimHolds` gets the registry's titles
 * only once a read has landed and the latest one succeeded.
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
 * `Timestamp` tick, and the whole label with the absolute time is in the `title`, which also
 * says what a claim is: the chip sits among status, priority and assignee, and nothing else on
 * the page defines it. An unclaimed issue renders nothing at all, so a board of unclaimed work
 * stays quiet.
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
  const text = useAgentText();
  if (claim === null) {
    return null;
  }
  const lapsed = claimHasLapsed(claim, registry);
  const since = `Claimed by ${text.label(claim.actor)} · since ${new Date(claim.at).toLocaleString()}.`;
  const explains =
    "A claim marks who is implementing this issue, so two agents never take the same work.";
  const lapsedNote = lapsed ? " That session is no longer running, so anyone may claim it." : "";
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
      title={`${since} ${explains}${lapsedNote}${uncheckedNote}`}
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
    </span>
  );
}

import { type ClaimHolderLiveness, claimHolds } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useState } from "react";

import { issueQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { textMutedOnSurface } from "../../theme/classes";
import { claimHolding } from "../issue/ClaimChip";
import { useAgentText } from "../refs/AgentName";
import { type AgentRegistry, useAgentRegistry } from "../refs/agent-registry";
import { useWrites } from "../writes/write-store";
import { type SendToAgent, sendToAgent } from "./send-to-agent";

/** The group the offer's one row sits in, and the row's own id, which the bar's arrows and
 *  `aria-activedescendant` read. */
export const SEND_TO_AGENT_GROUP_ID = "search-group-send-to-agent";
export const SEND_TO_AGENT_OPTION_ID = "search-option-send-to-agent";

/** Where the offer stands: nothing pressed, the send out, the message sent, or a refusal in the
 *  person's words. */
type OfferState =
  | { readonly kind: "idle" }
  | { readonly kind: "sending"; readonly words: string }
  | { readonly agent: string; readonly kind: "sent"; readonly words: string }
  | { readonly kind: "refused"; readonly refusal: string; readonly words: string };

/** What the offer is at this moment. */
export interface SendToAgentOffer {
  /** The row the bar lists. It is gone from the press onward: one approval sends one message,
   *  so there is no second press to make, and the line below says what became of it. A refused
   *  send puts the row back, because then nothing was sent. */
  readonly row: { readonly label: string; readonly select: () => void } | undefined;
  /** What the bar says under the list about the message it sent or could not send. */
  readonly said: ReactNode;
}

/**
 * The session a live agent holds this issue's claim with, or undefined where no such agent does:
 * no claim, a person's claim, a holder that has stopped or missed its heartbeats, or a registry
 * this browser could not read. It is the judgement the chip on the issue itself makes
 * (`claimHolding`), from the issue read's own verdict on the holder where it gave one and from
 * the live registry otherwise, so the bar and the chip never disagree about who is on the issue.
 */
function liveHolderOf(
  issue: IssueDetails | undefined,
  registry: Pick<AgentRegistry, "state" | "titles">
): string | undefined {
  if (issue === undefined) {
    return undefined;
  }
  const claim = issue.claim;
  if (claim === null || claim.actor.kind !== "session") {
    return undefined;
  }
  const read: ClaimHolderLiveness = issue.holder_state === null ? {} : issue;
  const holding =
    read.holder_state == null
      ? claimHolding(claim, registry)
      : claimHolds(claim, registry.titles, read);
  return holding === "holds" ? claim.actor.id : undefined;
}

/**
 * The bar's offer of the words a person typed as a message to the agent already on this issue,
 * one row among the actions the bar lists rather than a surface of its own.
 *
 * It always asks: the row is what sends, whatever any confidence in the words would be, because
 * a send spends that agent a turn (dispatch://INFRA-1042/ask/4310b2c0-df6f-424b-b0b6-b75e23a60fb6).
 * Nothing is sent until the person presses it, and one press sends one message.
 */
export function useSendToAgentOffer({
  issue,
  send = sendToAgent,
  words,
}: {
  /** The issue the page under the bar is on, or undefined on every other page. */
  readonly issue: string | undefined;
  /** Write seam for tests; defaults to the real send. */
  readonly send?: SendToAgent;
  /** The words in the box, as the person typed them. */
  readonly words: string;
}): SendToAgentOffer | undefined {
  const writes = useWrites();
  const detail = useQuery({ ...issueQuery(issue), enabled: issue !== undefined && words !== "" });
  const claim = detail.data?.claim;
  const registry = useAgentRegistry(claim?.actor.kind === "session");
  const text = useAgentText();
  const [state, setState] = useState<OfferState>({ kind: "idle" });
  // A box the person has typed on since is another request, so what the words before it sent is
  // no longer this offer's answer.
  if (state.kind !== "idle" && state.words !== words) {
    setState({ kind: "idle" });
  }
  const here: OfferState =
    state.kind !== "idle" && state.words === words ? state : { kind: "idle" };
  const holder = liveHolderOf(detail.data, registry);
  // Nothing to offer and nothing to say: no words, or no live agent on the issue. Once a press
  // has been made the offer stays, so a poll that finds the agent gone does not take back the
  // line saying what was already sent.
  if (issue === undefined || words === "" || (holder === undefined && here.kind === "idle")) {
    return undefined;
  }
  const approve = (session: string) => {
    setState({ kind: "sending", words });
    send(writes, { issue, session, words }).then(
      () => {
        setState({ agent: text.label({ id: session, kind: "session" }), kind: "sent", words });
      },
      (error: unknown) => {
        // Dispatch's own refusal where it gave one, and whatever else stopped the send
        // otherwise: a reader told only "could not" has nothing to act on.
        const refusal =
          error instanceof Error && error.message !== ""
            ? error.message
            : "the message did not reach Dispatch.";
        setState({ kind: "refused", refusal, words });
      }
    );
  };
  const offered = holder !== undefined && (here.kind === "idle" || here.kind === "refused");
  return {
    row:
      offered && holder !== undefined
        ? { label: `Send this to the agent on ${issue}`, select: () => approve(holder) }
        : undefined,
    said: <SendToAgentSaid issue={issue} state={here} />,
  };
}

/** The one line the bar says about the message it is sending, sent, or could not send. */
function SendToAgentSaid({
  issue,
  state,
}: {
  readonly issue: string;
  readonly state: OfferState;
}): ReactNode {
  if (state.kind === "idle") {
    return null;
  }
  const line = `shrink-0 px-3 py-2 text-xs ${textMutedOnSurface}`;
  if (state.kind === "sending") {
    return (
      <p className={line} role="status">
        Sending to the agent on {issue}…
      </p>
    );
  }
  if (state.kind === "refused") {
    return (
      <p className={line} data-testid="search-send-to-agent-said" role="alert">
        Could not send it to the agent on {issue}: {state.refusal}
      </p>
    );
  }
  return (
    <p className={line} data-testid="search-send-to-agent-said" role="status">
      Sent to {state.agent}, the agent on {issue}.
    </p>
  );
}

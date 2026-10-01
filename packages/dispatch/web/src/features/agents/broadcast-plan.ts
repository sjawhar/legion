import { DELIVERY_CAPABILITIES, MAX_BROADCAST_RECIPIENTS } from "@legion/contracts";

import type { Agent, CreateBroadcastInput, MessageDeliveryMode } from "../../api/types";
import { MODE_LABELS } from "../conversation/delivery";
import { sessionLabel } from "../refs/actor";
/** A session a broadcast would leave out, with its composer-facing reason named through
 *  `MODE_LABELS`. The server's stored diagnostic keeps the wire name
 *  (`packages/dispatch/AGENTS.md`). */
export interface BroadcastExclusionPlan {
  /** The live session the chosen mode leaves out; absent when it has left the registry. */
  readonly agent: Agent | undefined;
  readonly reason: string;
  readonly sessionID: string;
}

/** The sessions a broadcast of the current selection reaches, and the selected ones it leaves out. */
export interface BroadcastPlan {
  readonly excluded: readonly BroadcastExclusionPlan[];
  /** The session ids the send will name, in the order it names them. A restored send can name
   *  a session the registry no longer holds, which no `Agent` stands for. */
  readonly recipients: readonly string[];
}

/** What the composer shows about the send it would make: Send's label, the one notice line, and
 *  why Send refuses, if it does. */
export interface BroadcastSendState {
  readonly label: string;
  /** The composer's one notice line, highest first: the limit, nobody reached, or the Excluded
   *  line; null when none applies. */
  readonly notice: string | null;
  /** Why Send refuses, or null while it can be pressed. */
  readonly refusal: string | null;
  /** Whether `notice` is the refusal, rather than a line beside a Send that can be pressed or
   *  refuses for an empty message. */
  readonly refusalOnNotice: boolean;
}

/** The registry by session id. */
function sessionsById(agents: readonly Agent[]): Map<string, Agent> {
  return new Map(agents.map((agent) => [agent.session_id, agent]));
}

/** Why the live registry leaves a selected session out of a send in `delivery`, or undefined when
 *  it can take it. */
function liveExclusion(
  sessionID: string,
  agent: Agent | undefined,
  delivery: MessageDeliveryMode
): BroadcastExclusionPlan | undefined {
  if (agent === undefined) return { agent: undefined, reason: "no live session", sessionID };
  if (!agent.capabilities.includes(delivery)) {
    return { agent, reason: `does not advertise ${MODE_LABELS[delivery]}`, sessionID };
  }
  return undefined;
}

/**
 * What sending the current selection would do: the sessions it reaches, and the selected
 * sessions it leaves out. A session that does not advertise the chosen mode is excluded
 * rather than switched to another one - the mode is part of what the sender said - and a
 * selection kept across a session going away excludes it too, which is exactly the judgment
 * the server repeats against its own registry read when the send arrives.
 */
export function broadcastPlan(
  selected: ReadonlySet<string>,
  agents: readonly Agent[],
  delivery: MessageDeliveryMode
): BroadcastPlan {
  const live = sessionsById(agents);
  const excluded: BroadcastExclusionPlan[] = [];
  const recipients: string[] = [];
  for (const sessionID of selected) {
    const exclusion = liveExclusion(sessionID, live.get(sessionID), delivery);
    if (exclusion === undefined) recipients.push(sessionID);
    else excluded.push(exclusion);
  }
  return { excluded, recipients };
}

/** Why a selected session that has come back is left out of a restored send: it was not
 *  reachable when the refused send was pressed, so the request being re-sent does not name it. */
const NOT_IN_RESTORED_SEND = "not in the refused send; edit to include it";

/**
 * What re-sending a refused request word for word would do: it reaches exactly the sessions it
 * named, whatever the registry says now. A selected session the request does not name is
 * excluded, so the composer never shows as reached a session the request will not ask for: with
 * NOT_IN_RESTORED_SEND when it has come back since and an edit - which drops the restored request
 * for the live plan - would include it, and otherwise with the reason the live plan would give.
 * A named session that has left since is still a recipient here: the request asks for it, and the
 * server excludes it against its own registry and names it on the broadcast.
 */
function restoredBroadcastPlan(
  restored: CreateBroadcastInput,
  selected: ReadonlySet<string>,
  agents: readonly Agent[]
): BroadcastPlan {
  const live = sessionsById(agents);
  const named = new Set(restored.session_ids);
  const excluded: BroadcastExclusionPlan[] = [];
  for (const sessionID of selected) {
    if (named.has(sessionID)) continue;
    const agent = live.get(sessionID);
    excluded.push(
      liveExclusion(sessionID, agent, restored.delivery) ?? {
        agent,
        reason: NOT_IN_RESTORED_SEND,
        sessionID,
      }
    );
  }
  return { excluded, recipients: [...restored.session_ids] };
}

/** What the composer would send, and the plan it shows for it. */
export interface ComposedBroadcast {
  readonly input: CreateBroadcastInput;
  readonly plan: BroadcastPlan;
}

/**
 * The composer's one restored-or-live decision. A restored request goes out word for word and is
 * described as it is (`restoredBroadcastPlan`); otherwise the live plan of the current composition
 * names the sessions, under the composition's key.
 */
export function composedBroadcast(
  composition: {
    readonly delivery: MessageDeliveryMode;
    readonly draft: string;
    readonly restored: CreateBroadcastInput | null;
    readonly selected: ReadonlySet<string>;
    readonly sendKey: string;
  },
  agents: readonly Agent[]
): ComposedBroadcast {
  const { delivery, draft, restored, selected, sendKey } = composition;
  if (restored !== null) {
    return { input: restored, plan: restoredBroadcastPlan(restored, selected, agents) };
  }
  const plan = broadcastPlan(selected, agents, delivery);
  return {
    input: { body: draft, delivery, idempotency_key: sendKey, session_ids: plan.recipients },
    plan,
  };
}

/**
 * The Excluded line: every selected session the send leaves out, with its reason, and, when the
 * mode is why nobody is reached, the mode that would reach the most of them (`hint`).
 */
function exclusionLine(excluded: readonly BroadcastExclusionPlan[], hint: string | null): string {
  const named = excluded
    .map((item) => `${sessionLabel(item.sessionID, item.agent?.title)} (${item.reason})`)
    .join(", ");
  const line = `Excluded: ${named}. Nothing is sent to them, and no other mode is substituted.`;
  return hint === null ? line : `${line} ${hint}`;
}

/**
 * What the composer says about the send, and where. The notice slot holds one line, highest
 * first: the recipient limit, then the Excluded line. Over the limit, or with nobody to reach,
 * that line is why Send refuses; otherwise an Excluded line is context beside Send, and an empty
 * message is Send's own reason, with no line of its own. A button that only counts - `Send to 0`
 * - reads as a number, not a refusal, so a selection none of whom this mode reaches reads
 * `No recipient`. When the mode is what leaves everyone out, the Excluded line ends by naming the
 * mode that reaches the most of them - by what it would do, not by the control that picks it, so
 * it holds however the mode is chosen. The limit outranks an empty message, and the cause it
 * hides is the empty box, which the reader can see; the limit hiding the Excluded line hides no
 * name, since every excluded chip carries its reason. An empty selection has no composer, so it
 * is no case here.
 */
export function broadcastSendState(
  { excluded, recipients }: BroadcastPlan,
  delivery: MessageDeliveryMode,
  body: string
): BroadcastSendState {
  const label = `Send to ${recipients.length}`;
  if (recipients.length > MAX_BROADCAST_RECIPIENTS) {
    const limit = `At most ${MAX_BROADCAST_RECIPIENTS} recipients per broadcast; this one would reach ${recipients.length}.`;
    return { label, notice: limit, refusal: limit, refusalOnNotice: true };
  }
  if (recipients.length > 0) {
    return {
      label,
      notice: excluded.length === 0 ? null : exclusionLine(excluded, null),
      refusal: body.trim() === "" ? "Type a message first." : null,
      refusalOnNotice: false,
    };
  }
  // No recipient means every selected session is excluded, and the composer mounts only with a
  // selection, so `excluded` is the whole selection here.
  const lacking = excluded.flatMap((item) => (item.agent === undefined ? [] : [item.agent]));
  let best: { count: number; mode: MessageDeliveryMode } | undefined;
  for (const mode of DELIVERY_CAPABILITIES) {
    if (mode === delivery) continue;
    const count = lacking.filter((agent) => agent.capabilities.includes(mode)).length;
    if (count > (best?.count ?? 0)) best = { count, mode };
  }
  const hint =
    best === undefined
      ? null
      : excluded.length === 1
        ? `Sending as ${MODE_LABELS[best.mode]} would reach it.`
        : `Sending as ${MODE_LABELS[best.mode]} would reach ${best.count} of them.`;
  const nobody = exclusionLine(excluded, hint);
  return { label: "No recipient", notice: nobody, refusal: nobody, refusalOnNotice: true };
}

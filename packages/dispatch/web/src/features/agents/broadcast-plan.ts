import { DELIVERY_CAPABILITIES, MAX_BROADCAST_RECIPIENTS } from "@legion/contracts";

import type { Agent, MessageDeliveryMode } from "../../api/types";
import { sessionLabel } from "../refs/actor";

/** A session a broadcast would leave out, worded the way the server reports it, so the
 *  composer and the create response say the same thing. */
export interface BroadcastExclusionPlan {
  /** The live session the chosen mode leaves out; absent when it has left the registry. */
  readonly agent: Agent | undefined;
  readonly reason: string;
  readonly sessionID: string;
}

/** The sessions a broadcast of the current selection reaches, and the selected ones it leaves out. */
export interface BroadcastPlan {
  readonly excluded: readonly BroadcastExclusionPlan[];
  readonly recipients: readonly Agent[];
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
  const live = new Map(agents.map((agent) => [agent.session_id, agent]));
  const excluded: BroadcastExclusionPlan[] = [];
  const recipients: Agent[] = [];
  for (const sessionID of selected) {
    const agent = live.get(sessionID);
    if (agent === undefined) {
      excluded.push({ agent: undefined, reason: "no live session", sessionID });
      continue;
    }
    if (!agent.capabilities.includes(delivery)) {
      excluded.push({ agent, reason: `does not advertise ${delivery}`, sessionID });
      continue;
    }
    recipients.push(agent);
  }
  return { excluded, recipients };
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
        ? `Sending as ${best.mode} would reach it.`
        : `Sending as ${best.mode} would reach ${best.count} of them.`;
  const nobody = exclusionLine(excluded, hint);
  return { label: "No recipient", notice: nobody, refusal: nobody, refusalOnNotice: true };
}

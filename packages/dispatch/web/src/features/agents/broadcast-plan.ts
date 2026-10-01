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

/**
 * What Send says about the send it would make, one case each: it can be pressed (`ready`), or it
 * refuses for an empty message, for the recipient limit, or because the chosen mode reaches
 * nobody selected. Every refusal carries its `reason`; the label is a count except when there is
 * nobody to count.
 */
export type BroadcastSendState =
  | { readonly kind: "ready"; readonly label: string }
  | { readonly kind: "empty"; readonly label: string; readonly reason: string }
  | { readonly kind: "limit"; readonly label: string; readonly reason: string }
  | { readonly kind: "nobody"; readonly label: string; readonly reason: string };

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
export function exclusionLine(
  excluded: readonly BroadcastExclusionPlan[],
  hint: string | null = null
): string {
  const named = excluded
    .map((item) => `${sessionLabel(item.sessionID, item.agent?.title)} (${item.reason})`)
    .join(", ");
  const line = `Excluded: ${named}. Nothing is sent to them, and no other mode is substituted.`;
  return hint === null ? line : `${line} ${hint}`;
}

/**
 * Which of its reasons stops Send, if any. A button that only counts - `Send to 0` - reads as a
 * number, not a refusal, so a selection none of whom this mode reaches reads `No recipient`, and
 * its reason is the Excluded line, which names each session and why. When the mode is what leaves
 * everyone out, that line ends by naming the mode that reaches the most of them - by what it would
 * do, not by the control that picks it, so it holds however the mode is chosen. The limit outranks
 * an empty message, and the cause it hides is the empty box, which the reader can see. The limit's
 * reason is also its notice line, word for word. An empty selection has no composer, so it is no
 * case here.
 */
export function broadcastSendState(
  { excluded, recipients }: BroadcastPlan,
  delivery: MessageDeliveryMode,
  body: string
): BroadcastSendState {
  const label = `Send to ${recipients.length}`;
  if (recipients.length > MAX_BROADCAST_RECIPIENTS) {
    return {
      kind: "limit",
      label,
      reason: `At most ${MAX_BROADCAST_RECIPIENTS} recipients per broadcast; this one would reach ${recipients.length}.`,
    };
  }
  if (recipients.length > 0) {
    return body.trim() === ""
      ? { kind: "empty", label, reason: "Type a message first." }
      : { kind: "ready", label };
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
  return { kind: "nobody", label: "No recipient", reason: exclusionLine(excluded, hint) };
}

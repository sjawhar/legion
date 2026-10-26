import { DELIVERY_CAPABILITIES, MAX_BROADCAST_RECIPIENTS } from "@legion/contracts";

import { AGENT_LIST_REFILLING } from "../../api/client";
import type { Agent, CreateBroadcastInput, MessageDeliveryMode } from "../../api/types";
import { MODE_LABELS } from "../conversation/delivery";
import type { AgentPart } from "../refs/AgentName";

/** A session a broadcast would leave out, with its composer-facing reason named through
 *  `MODE_LABELS`. The server's stored diagnostic keeps the wire name
 *  (`packages/dispatch/AGENTS.md`). */
export interface BroadcastExclusionPlan {
  /** The live session the chosen mode leaves out; absent when it has left the registry. */
  readonly agent: Agent | undefined;
  readonly reason: string;
  readonly sessionID: string;
}

/** A recipient the agent list could not vouch for: a selected session missing from a list whose
 *  latest read failed, with the composer-facing wording of that unknown. */
export interface BroadcastUncheckedRecipient {
  readonly reason: string;
  readonly sessionID: string;
}

/** The sessions a broadcast of the current selection reaches, and the selected ones it leaves out. */
export interface BroadcastPlan {
  readonly excluded: readonly BroadcastExclusionPlan[];
  /** The session ids the send will name, in the order it names them. A restored send can name
   *  a session the registry no longer holds, which no `Agent` stands for. */
  readonly recipients: readonly string[];
  /** The recipients the list could not vouch for, because it is not current and does not hold
   *  them. Each is still named in the send: the server reads each id's liveness itself. */
  readonly unchecked: readonly BroadcastUncheckedRecipient[];
}

/** The agent list a plan is judged against, as `useAgents` answers it: the rows of its last
 *  successful read, whether its latest read succeeded (`current`), and whether that read failed
 *  because Envoy is refilling the list (`refilling`). */
export interface BroadcastRoster {
  readonly agents: readonly Agent[];
  readonly current: boolean;
  readonly refilling: boolean;
}

/** What the composer shows about the send it would make: Send's label, the one notice line, and
 *  why Send refuses, if it does. Both lines are `AgentPart`s, so an excluded session keeps its
 *  identity until the composer draws it through `AgentName`. */
export interface BroadcastSendState {
  readonly label: string;
  /** The composer's one notice line, highest first: the limit, nobody reached, or the Excluded
   *  line; null when none applies. */
  readonly notice: readonly AgentPart[] | null;
  /** Why Send refuses, or null while it can be pressed. */
  readonly refusal: readonly AgentPart[] | null;
  /** Whether `notice` is the refusal, rather than a line beside a Send that can be pressed or
   *  refuses for an empty message. */
  readonly refusalOnNotice: boolean;
}

/** The registry by session id. */
function sessionsById(agents: readonly Agent[]): Map<string, Agent> {
  return new Map(agents.map((agent) => [agent.session_id, agent]));
}

/** That a list could not vouch for a selected session: it would have to claim an absence - that
 *  the session has gone, or that it does not advertise the mode - and its latest read failed. */
const UNCHECKED = "unchecked";

/** Why the live registry leaves a selected session out of a send in `delivery`, undefined when
 *  it can take it, or UNCHECKED when only an absence claim would leave it out and the list is
 *  not `current`. A session the list does not hold and a capability its row does not carry are
 *  both absences, and only a current list says either: a refilling list's rows are its last
 *  successful read, which says nothing of what a session advertises now. */
function liveExclusion(
  sessionID: string,
  agent: Agent | undefined,
  delivery: MessageDeliveryMode,
  current: boolean
): BroadcastExclusionPlan | typeof UNCHECKED | undefined {
  const reason =
    agent === undefined
      ? "no live session"
      : agent.capabilities.includes(delivery)
        ? undefined
        : `does not advertise ${MODE_LABELS[delivery]}`;
  if (reason === undefined) return undefined;
  return current ? { agent, reason, sessionID } : UNCHECKED;
}

/** What the composer says of a selected session a list that is not current cannot vouch for:
 *  whether it runs, and so what it takes, is unknown, not that it has gone or cannot take the
 *  mode, and a refilling list says so in the shared words. */
function uncheckedReason(refilling: boolean): string {
  return refilling
    ? `couldn't check whether it is running. ${AGENT_LIST_REFILLING}`
    : "couldn't check whether it is running";
}

/**
 * What sending the current selection would do: the sessions it reaches, and the selected
 * sessions it leaves out. A session that does not advertise the chosen mode is excluded
 * rather than switched to another one - the mode is part of what the sender said - and a
 * selection kept across a session going away excludes it too, which is exactly the judgment
 * the server repeats against its own registry read when the send arrives. Both are absences,
 * judged only against a `current` list: a selected session missing from a list whose latest read
 * failed (a refilling one, say), or listed there without the mode, is unknown, not left out, so
 * it stays a recipient - `unchecked`, with that unknown as its wording - and the server, which
 * reads each selected id's liveness and capabilities at send time, excludes it then if it really
 * has gone or cannot take the mode.
 */
export function broadcastPlan(
  selected: ReadonlySet<string>,
  { agents, current, refilling }: BroadcastRoster,
  delivery: MessageDeliveryMode
): BroadcastPlan {
  const live = sessionsById(agents);
  const excluded: BroadcastExclusionPlan[] = [];
  const recipients: string[] = [];
  const unchecked: BroadcastUncheckedRecipient[] = [];
  for (const sessionID of selected) {
    const exclusion = liveExclusion(sessionID, live.get(sessionID), delivery, current);
    if (exclusion === undefined) {
      recipients.push(sessionID);
    } else if (exclusion === UNCHECKED) {
      recipients.push(sessionID);
      unchecked.push({ reason: uncheckedReason(refilling), sessionID });
    } else {
      excluded.push(exclusion);
    }
  }
  return { excluded, recipients, unchecked };
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
 * A list that is not current says neither that a session it does not hold has gone nor that a
 * session it lists lacks the mode, so such a session is NOT_IN_RESTORED_SEND too: the live plan an
 * edit composes would name it, `unchecked`.
 * A named session that has left since is still a recipient here: the request asks for it, and the
 * server excludes it against its own registry and names it on the broadcast; one a list that is
 * not current cannot vouch for is `unchecked`.
 */
function restoredBroadcastPlan(
  restored: CreateBroadcastInput,
  selected: ReadonlySet<string>,
  { agents, current, refilling }: BroadcastRoster
): BroadcastPlan {
  const live = sessionsById(agents);
  const named = new Set(restored.session_ids);
  const excluded: BroadcastExclusionPlan[] = [];
  for (const sessionID of selected) {
    if (named.has(sessionID)) continue;
    const agent = live.get(sessionID);
    const exclusion = liveExclusion(sessionID, agent, restored.delivery, current);
    excluded.push(
      exclusion === undefined || exclusion === UNCHECKED
        ? { agent, reason: NOT_IN_RESTORED_SEND, sessionID }
        : exclusion
    );
  }
  const unchecked = restored.session_ids.flatMap((sessionID) =>
    liveExclusion(sessionID, live.get(sessionID), restored.delivery, current) === UNCHECKED
      ? [{ reason: uncheckedReason(refilling), sessionID }]
      : []
  );
  return { excluded, recipients: [...restored.session_ids], unchecked };
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
  roster: BroadcastRoster
): ComposedBroadcast {
  const { delivery, draft, restored, selected, sendKey } = composition;
  if (restored !== null) {
    return { input: restored, plan: restoredBroadcastPlan(restored, selected, roster) };
  }
  const plan = broadcastPlan(selected, roster, delivery);
  return {
    input: { body: draft, delivery, idempotency_key: sendKey, session_ids: plan.recipients },
    plan,
  };
}

/** `Excluded: <session> (<reason>), …`: each session a send leaves out, with its reason, as an
 *  agent part for `AgentParts` to draw. The composer's Excluded line and the sent broadcast's
 *  page both start from it. */
export function excludedParts(
  excluded: readonly { sessionId: string; storedTitle: string | undefined; reason: string }[]
): AgentPart[] {
  const parts: AgentPart[] = ["Excluded: "];
  excluded.forEach(({ reason, sessionId, storedTitle }, index) => {
    if (index > 0) parts.push(", ");
    parts.push({ sessionId, storedTitle }, ` (${reason})`);
  });
  return parts;
}

/**
 * The Excluded line: every selected session the send leaves out, with its reason, and, when the
 * mode is why nobody is reached, the mode that would reach the most of them (`hint`).
 */
function exclusionLine(
  excluded: readonly BroadcastExclusionPlan[],
  hint: string | null
): AgentPart[] {
  const parts = excludedParts(
    excluded.map((item) => ({
      reason: item.reason,
      sessionId: item.sessionID,
      storedTitle: item.agent?.title,
    }))
  );
  const tail = ". Nothing is sent to them, and no other mode is substituted.";
  parts.push(hint === null ? tail : `${tail} ${hint}`);
  return parts;
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
    return { label, notice: [limit], refusal: [limit], refusalOnNotice: true };
  }
  if (recipients.length > 0) {
    return {
      label,
      notice: excluded.length === 0 ? null : exclusionLine(excluded, null),
      refusal: body.trim() === "" ? ["Type a message first."] : null,
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

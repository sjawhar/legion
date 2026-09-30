import type { MessageRead } from "@legion/contracts";
import type { DispatchDelivery, RenderInboundResult } from "@legion/envoy-client/delivery";

/**
 * A person's direct message from Dispatch's Agents page, delivered as the session's own user turn
 * (LEGION-394): the message a person types at the terminal, not an Envoy card. The frame that says
 * a person wrote it is not proof of that: the listener takes an envelope's source from whoever
 * sends it, and every session holds that token. So a frame is only a candidate
 * (`isUserTurnCandidate`), the session reads the message back from Dispatch with its own bearer,
 * and it takes the turn only when Dispatch's record confirms it (`confirmUserTurn`). Anything
 * else keeps today's card, which is the delivery.
 */

/**
 * Transcript entry naming a Dispatch message this session took as its own user turn. A replayed
 * frame carries the same attempt as the first, so this, rebuilt on every start, is what keeps it
 * from becoming a second turn after the plugin restarts.
 */
export const DISPATCH_USER_TURN_ENTRY = "envoy-dispatch-user-turn";

/** The message id a transcript entry records, when it is a `DISPATCH_USER_TURN_ENTRY`. */
export function userTurnEntryMessageID(entry: unknown): string | undefined {
  if (typeof entry !== "object" || entry === null) return undefined;
  if (!("type" in entry) || entry.type !== "custom") return undefined;
  if (!("customType" in entry) || entry.customType !== DISPATCH_USER_TURN_ENTRY) return undefined;
  if (!("data" in entry) || typeof entry.data !== "object" || entry.data === null) return undefined;
  if (!("message_id" in entry.data) || typeof entry.data.message_id !== "string") return undefined;
  return entry.data.message_id;
}

/**
 * Whether a frame could be a person's direct Send or Aside, worth reading back from Dispatch: a
 * Dispatch `message.created` delivery (renderInbound builds a message delivery for no other event)
 * on no issue, whose actor is a person, in aside or steer. An issue message and a comment mention
 * keep their envelope, since their flow replies on the issue; a BTW is a side turn.
 */
export function isUserTurnCandidate(
  rendered: RenderInboundResult
): rendered is RenderInboundResult & { readonly delivery: DispatchDelivery } {
  const { delivery } = rendered;
  return (
    rendered.envelope?.source === "dispatch" &&
    delivery?.resource === "message" &&
    delivery.issueKey === null &&
    rendered.dispatchActor?.kind === "user" &&
    (delivery.mode === "aside" || delivery.mode === "steer")
  );
}

/** What a confirmed message becomes: its stored body, sent as Send (steer) or Aside. */
export interface ConfirmedUserTurn {
  readonly body: string;
  readonly mode: "aside" | "steer";
}

/**
 * Whether Dispatch's read of the thread a delivery's message belongs to (`GET
 * /api/v1/messages/{id}?session=`) confirms it as a person's own message to this session: the
 * stored author is a person, it belongs to no issue, the thread's root is aimed at this session,
 * no broadcast sent it (a broadcast keeps its envelope and its reply counts; a Dispatch that does
 * not say is not taken as saying no), and Dispatch recorded the attempt the frame names as
 * delivered to this session. The mode is that stored attempt's, never the frame's. The read is
 * untrusted JSON, so a shape this does not expect throws, which the caller answers with the card.
 */
export function confirmUserTurn(
  thread: MessageRead,
  delivery: DispatchDelivery,
  sessionID: string
): ConfirmedUserTurn | undefined {
  const root = thread.message;
  const message =
    root.id === delivery.id ? root : thread.replies.find((reply) => reply.id === delivery.id);
  if (
    message === undefined ||
    message.author.kind !== "user" ||
    typeof message.body !== "string" ||
    message.issue_key !== null ||
    root.target !== `session:${sessionID}` ||
    root.broadcast_id !== null ||
    message.broadcast_id !== null
  ) {
    return undefined;
  }
  const attempt = message.deliveries.find(
    (candidate) => candidate.attempt === delivery.attempt && candidate.session_id === sessionID
  );
  if (attempt?.delivery !== "aside" && attempt?.delivery !== "steer") return undefined;
  return { body: message.body, mode: attempt.delivery };
}

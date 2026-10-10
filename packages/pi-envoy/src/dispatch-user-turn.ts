import { DISPATCH_BODY_MAX } from "@legion/contracts";
import type { DispatchDelivery, RenderInboundResult } from "@legion/envoy-client/delivery";
import type { UserInputResult } from "@legion/pi-shared/pi-types";
import { z } from "zod";

/**
 * A person's direct message from Dispatch's Agents page, delivered as the session's own user turn
 * (LEGION-394): the message a person types at the terminal, not an Envoy card. The frame that says
 * a person wrote it is not proof of that: the listener takes an envelope's source from whoever
 * sends it, and every session holds that token. So a frame is only a candidate
 * (`isUserTurnCandidate`), and the session takes the turn only once Dispatch records that this
 * session accepted that very attempt (`POST /api/v1/messages/{id}/deliveries/{attempt}/accept`):
 * only that accept's success makes a turn, and it answers the body Dispatch stored, which is what
 * the session injects (`turnFromAccept`). The session's own checks can only keep a card: it rules
 * out frames no person's direct message could be, and never accepts an attempt it already
 * delivered, as a card or as a turn (`handledAttempts`). Anything else keeps today's card, which
 * is the delivery.
 */

/**
 * Whether a frame could be a person's direct Send or Aside, worth asking Dispatch to accept: a
 * Dispatch `message.created` delivery (renderInbound builds a message delivery for no other event)
 * on no issue, whose actor is a person, in aside or steer, and that names no broadcast. An issue
 * message and a comment mention keep their envelope, since their flow replies on the issue; a BTW
 * is a side turn; a broadcast keeps its envelope and its reply counts, and a frame that falsely
 * claims one only yields the card. It rules frames out and never in: Dispatch decides the rest.
 */
export function isUserTurnCandidate(
  rendered: RenderInboundResult
): rendered is RenderInboundResult & { readonly delivery: DispatchDelivery } {
  const { delivery } = rendered;
  return (
    rendered.envelope?.source === "dispatch" &&
    delivery?.resource === "message" &&
    delivery.issueKey === null &&
    delivery.broadcastId === undefined &&
    rendered.dispatchActor?.kind === "user" &&
    (delivery.mode === "aside" || delivery.mode === "steer")
  );
}

/** What an accepted attempt becomes: the body Dispatch stored, sent as Send (steer) or Aside,
 *  and the message it delivers, which tags its turn on the live stream. */
export interface AcceptedUserTurn {
  readonly messageId: string;
  readonly body: string;
  readonly mode: "aside" | "steer";
}

/** The fields of the accept's answer the session injects and tags, and nothing else. */
const AcceptedDeliverySchema = z.object({
  message_id: z.string(),
  body: z.string(),
  delivery: z.enum(["aside", "steer"]),
});

/**
 * The turn Dispatch's 200 from the accept route says to inject: the message's stored body in the
 * stored attempt's mode, never the frame's text or mode. The answer is JSON off the network, so
 * one without that shape injects nothing.
 */
export function turnFromAccept(accepted: unknown): AcceptedUserTurn | undefined {
  const parsed = AcceptedDeliverySchema.safeParse(accepted);
  if (!parsed.success) return undefined;
  return { body: parsed.data.body, messageId: parsed.data.message_id, mode: parsed.data.delivery };
}

/**
 * Transcript entry recording one attempt of a person's direct message this session delivered, as
 * a card or as a turn: `{ message_id, attempt }`. Written before the card or turn goes out and
 * read back from every entry of the session file, so a frame naming a delivered attempt - a
 * replay, or one forged inside the accept's minute - is a card with no accept call, even after a
 * restart or a switch to another branch of the tree.
 */
export const HANDLED_ATTEMPT_ENTRY = "envoy-dispatch-handled-attempt";

/** One attempt's key in the set of handled attempts: a message id and an attempt number. */
export function handledAttemptKey(messageId: string, attempt: number): string {
  return `${messageId}#${attempt}`;
}

/** The attempts a session's entries record it handled (`HANDLED_ATTEMPT_ENTRY`), by key. */
export function handledAttempts(entries: readonly unknown[]): Set<string> {
  const handled = new Set<string>();
  for (const entry of entries) {
    if (typeof entry !== "object" || entry === null) continue;
    if (!("type" in entry) || entry.type !== "custom") continue;
    if (!("customType" in entry) || entry.customType !== HANDLED_ATTEMPT_ENTRY) continue;
    if (!("data" in entry) || typeof entry.data !== "object" || entry.data === null) continue;
    const { data } = entry;
    if (!("message_id" in data) || typeof data.message_id !== "string") continue;
    if (!("attempt" in data) || typeof data.attempt !== "number") continue;
    handled.add(handledAttemptKey(data.message_id, data.attempt));
  }
  return handled;
}

/** What became of a person's text sent to `pi.sendUserInput`: the host's answer, the error it
 *  failed with, or, on a host without the method, that host's version. */
export type TypedInputOutcome =
  | UserInputResult
  | { readonly error: string }
  | { readonly hostVersion: string };

/**
 * What the session answers on Dispatch for a person's text, capped to a Dispatch body, or
 * undefined when there is nothing to say: a prompt or skill the model now works on, a command that
 * printed nothing, and, on a host without `pi.sendUserInput`, text that is not a command, which the
 * caller sends as before. The attempt is already the session's user turn, so the answer is a reply
 * rather than an error, and the conversation shows it.
 */
export function typedInputReply(text: string, outcome: TypedInputOutcome): string | undefined {
  const typed = /^\/\S+/.exec(text)?.[0];
  const command = typed ?? "This message";
  let reply: string | undefined;
  if ("error" in outcome) reply = `${command} was not run: ${outcome.error}`;
  else if ("hostVersion" in outcome) {
    if (typed !== undefined) {
      reply = `${typed} was not run: this session's Oh My Pi ${outcome.hostVersion} cannot run commands sent from Dispatch; nothing was sent.`;
    }
  } else if (outcome.handled === "terminal-only") {
    reply = `${command} runs only in the session's own terminal; nothing was sent.`;
  } else if (outcome.handled === "unavailable") {
    reply = `${command} was not run: this session's host cannot run typed input from Dispatch; nothing was sent.`;
  } else if (outcome.handled === "command" && outcome.output?.trim()) reply = outcome.output;
  if (reply === undefined || reply.length <= DISPATCH_BODY_MAX) return reply;
  return `${reply.slice(0, DISPATCH_BODY_MAX - 1)}…`;
}

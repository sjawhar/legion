import type { DispatchDelivery, RenderInboundResult } from "@legion/envoy-client/delivery";
import type { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { z } from "zod";
import { type AcceptedUserTurn, handledAttemptKey } from "./dispatch-user-turn";

/**
 * The accept step every Envoy plugin takes before a frame becomes a person's own user turn
 * (INFRA-1026), written once so a fix to the rule lands in every host: the Oh My Pi extension
 * (`acceptedUserTurn`, `packages/pi-envoy/extensions/envoy.ts`) and the Claude Code plugin
 * (`accepted`, `packages/claude-envoy/src/user-turn-router.ts`) both call it, and each supplies
 * the record of handled attempts its host keeps, its Dispatch client, the session id it accepts
 * as, and the way it says why a frame kept its card.
 *
 * The order of the steps is the security property, and it is why this is one function rather
 * than a rule each host writes out:
 *
 *   - a candidate frame (`isUserTurnCandidate`). The listener takes an envelope's source from
 *     whoever publishes it and every session on the bus holds that token, so a frame saying a
 *     person wrote it proves nothing: the host's own checks can only keep a card.
 *   - an attempt this session has not already handled, as a card or as a turn.
 *   - the attempt recorded as handled **before** Dispatch is asked, so whatever the frame
 *     becomes - a turn, or the card every refusal, error and timeout keeps - no later frame
 *     naming that attempt is accepted: a replay, or one forged inside the accept's minute for a
 *     message that arrived as a card. The record is not skipped when the accept answers 404:
 *     that route answers 404 for a transient database error too, and forgetting the attempt
 *     would let a later forged frame spend it.
 *   - the accept itself, which is what makes a turn: Dispatch recording that this session
 *     accepted that very attempt.
 *   - the stored body Dispatch answers with (`turnFromAccept`), never the frame's own text.
 */

/** Why a frame kept its card, for the host to say in its own words. Dispatch not being
 *  configured is no refusal: there is nothing to accept with, and nothing to report. */
export type UserTurnRefusal =
  /** The attempt is one this session already delivered, as a card or as a turn. */
  | { readonly reason: "handled"; readonly delivery: DispatchDelivery }
  /** Dispatch accepted the attempt and answered without the stored body to inject. */
  | { readonly reason: "no-body"; readonly delivery: DispatchDelivery }
  /** The accept call failed, timed out, or Dispatch refused it. */
  | { readonly reason: "failed"; readonly delivery: DispatchDelivery; readonly error: unknown };

/** What a frame Dispatch accepted became: the delivery the host answers on, and the turn it
 *  injects. */
export interface AcceptedUserTurnDelivery {
  readonly delivery: DispatchDelivery;
  readonly turn: AcceptedUserTurn;
}

/** What a host supplies to the accept step; everything about it that differs between hosts. */
export interface UserTurnAcceptOptions {
  /** The frame as the host rendered it. */
  readonly rendered: RenderInboundResult;
  /** Whether this session has already handled the attempt `key` names (`handledAttemptKey`):
   *  the Oh My Pi extension reads its transcript's entries, the Claude Code plugin its state
   *  file. */
  readonly handled: (key: string) => boolean;
  /** Records the attempt as handled, awaited before Dispatch is asked so no later frame naming
   *  it can be accepted. The delivery comes with it for a host whose record names the message
   *  and the attempt rather than the key. */
  readonly handle: (key: string, delivery: DispatchDelivery) => void | Promise<void>;
  /** The session's Dispatch for this one call, bounded by the host's accept timeout; undefined
   *  when Dispatch is not configured, which keeps the card and says nothing. */
  readonly dispatch: () => DispatchClient | undefined;
  /** The id this session accepts as, read at the call so a `/clear` moves it. */
  readonly actorId: string;
  /** Where the host says why a frame kept its card. */
  readonly warn: (refusal: UserTurnRefusal) => void;
}

/**
 * Whether a frame could be a person's direct Send or Aside, worth asking Dispatch to accept: a
 * Dispatch `message.created` delivery (renderInbound builds a message delivery for no other event)
 * on no issue, whose actor is a person, in aside or steer, and that names no broadcast. An issue
 * message and a comment mention keep their envelope, since their flow replies on the issue; a BTW
 * is a side turn; a broadcast keeps its envelope and its reply counts, and a frame that falsely
 * claims one only yields the card. It rules frames out and never in: Dispatch decides the rest.
 *
 * It is the first of the steps and is not exported, because a host that could take it alone
 * could take the steps in another order, which is what this module exists to prevent.
 */
function isUserTurnCandidate(
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

/** The fields of the accept's answer the session injects and tags, and nothing else. */
const AcceptedDeliverySchema = z.object({
  message_id: z.string(),
  body: z.string(),
  delivery: z.enum(["aside", "steer"]),
});

/**
 * The turn Dispatch's 200 from the accept route says to inject: the message's stored body in the
 * stored attempt's mode, never the frame's text or mode. The answer is JSON off the network, so
 * one without that shape injects nothing. It is the last of the steps and is not exported, for
 * the reason `isUserTurnCandidate` is not: only an accept this module made reaches it.
 */
function turnFromAccept(accepted: unknown): AcceptedUserTurn | undefined {
  const parsed = AcceptedDeliverySchema.safeParse(accepted);
  if (!parsed.success) return undefined;
  return { body: parsed.data.body, messageId: parsed.data.message_id, mode: parsed.data.delivery };
}

/**
 * The turn Dispatch says this frame is, or undefined for every frame that keeps its card, which
 * the caller then delivers as the card it always was.
 */
export async function acceptUserTurn(
  options: UserTurnAcceptOptions
): Promise<AcceptedUserTurnDelivery | undefined> {
  const { rendered } = options;
  if (!isUserTurnCandidate(rendered)) return undefined;
  const { delivery } = rendered;
  const key = handledAttemptKey(delivery.id, delivery.attempt);
  if (options.handled(key)) {
    options.warn({ delivery, reason: "handled" });
    return undefined;
  }
  await options.handle(key, delivery);
  try {
    const dispatch = options.dispatch();
    if (dispatch === undefined) return undefined;
    const accepted = await dispatch.acceptMessageDelivery(delivery.id, delivery.attempt, {
      actor: { id: options.actorId, kind: "session" },
    });
    const turn = turnFromAccept(accepted);
    if (turn === undefined) {
      options.warn({ delivery, reason: "no-body" });
      return undefined;
    }
    return { delivery, turn };
  } catch (error) {
    options.warn({ delivery, error, reason: "failed" });
    return undefined;
  }
}

import type { UserInputResult } from "@legion/pi-shared/pi-types";

/**
 * A person's direct message from Dispatch's Agents page, delivered as the session's own user turn
 * (LEGION-394): the message a person types at the terminal, not an Envoy card. The frame that says
 * a person wrote it is not proof of that: the listener takes an envelope's source from whoever
 * sends it, and every session holds that token. So the session takes the turn only once Dispatch
 * records that this session accepted that very attempt
 * (`POST /api/v1/messages/{id}/deliveries/{attempt}/accept`): only that accept's success makes a
 * turn, and it answers the body Dispatch stored, which is what the session injects. Those steps
 * and their order are `acceptUserTurn`'s (`./accept-user-turn`), written once for every host.
 *
 * What this file holds is what a host keeps of its own and hands to that step or says after it:
 * the attempts it already delivered, as a card or as a turn (`HANDLED_ATTEMPT_ENTRY`,
 * `handledAttemptKey`, `handledAttempts`), and what it answers a person on Dispatch for their
 * typed text (`typedInputReply`). A frame the accept does not make a turn keeps today's card,
 * which is the delivery.
 */

/** What an accepted attempt becomes: the body Dispatch stored, sent as Send (steer) or Aside,
 *  and the message it delivers, which tags its turn on the live stream. */
export interface AcceptedUserTurn {
  readonly messageId: string;
  readonly body: string;
  readonly mode: "aside" | "steer";
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
 * What the session answers on Dispatch for a person's text, or undefined when there is nothing to
 * say: a prompt, skill or command that printed nothing, and, on a host without `pi.sendUserInput`,
 * text that is not a command, which the caller sends as before. What a command printed is the
 * answer whether or not it then submitted a prompt (`/force:<tool> <prompt>` prints its
 * confirmation first). The attempt is already the session's user turn, so the answer is a reply
 * rather than an error, and the conversation shows it. The reply is written whole: Dispatch holds
 * it to its own bounds and refuses a longer one, and the caller posts that refusal.
 */
export function typedInputReply(text: string, outcome: TypedInputOutcome): string | undefined {
  const typed = /^\/\S+/.exec(text)?.[0];
  const command = typed ?? "This message";
  if ("error" in outcome) return `${command} was not run: ${outcome.error}`;
  if ("hostVersion" in outcome) {
    return typed === undefined
      ? undefined
      : `${typed} was not run: this session's agent, version ${outcome.hostVersion}, cannot run commands sent from Dispatch; nothing was sent.`;
  }
  if (outcome.handled === "terminal-only") {
    return `${command} runs only in the session's own terminal; nothing was sent.`;
  }
  if (outcome.handled === "unavailable") {
    return `${command} was not run: this session's host cannot run typed input from Dispatch; nothing was sent.`;
  }
  return outcome.output?.trim() ? outcome.output : undefined;
}

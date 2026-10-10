import { envoyPluginInterface } from "./interface";

/**
 * The user turns this process sent into one session and the user messages found for them. A turn
 * sent through `pi.sendUserInput` carries its Dispatch message id as the host's `tag`, which the
 * user message it submits keeps, so that message is found by its tag even when the host rewrote
 * its text (a prompt template, a file command). A turn sent through `pi.sendUserMessage`, on a
 * host without `sendUserInput`, is linked to nothing the host records, so it is found by its
 * text: the first user message that equals it, until the session's run ends. A message found is
 * remembered under its host timestamp, so every handler that asks about it gets the same answer
 * whichever asks first: envoy.ts's stream recorder tags its frames, and Legion's phase-stall
 * check (`extensions/legion.ts`) counts it as an inbound event rather than the daemon's
 * assignment.
 *
 * The record is the `injectedUserTurns` member of the versioned interface (`interface.ts`):
 * envoy.ts writes what legion.ts reads, and each plugin bundles its own copy of this module.
 */
export interface SessionUserTurns {
  readonly sent: { readonly body: string; readonly messageId: string }[];
  readonly found: Map<number, string>;
}

/** A user message's own text: its text parts alone, which is what a sent prompt's text became. */
function userMessageText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const pieces: string[] = [];
  for (const part of content) {
    if (typeof part !== "object" || part === null) continue;
    if ("type" in part && part.type === "text" && "text" in part && typeof part.text === "string") {
      pieces.push(part.text);
    }
  }
  return pieces.join("\n");
}

/** Records that Dispatch message `messageId`, whose body is `body`, was just sent into
 *  `sessionID` as its user's own turn. */
export function noteInjectedUserTurn(sessionID: string, body: string, messageId: string): void {
  const turns = envoyPluginInterface().injectedUserTurns;
  const session: SessionUserTurns = turns.get(sessionID) ?? { found: new Map(), sent: [] };
  session.sent.push({ body, messageId });
  turns.set(sessionID, session);
}

/**
 * The Dispatch message a user message `sessionID` records delivered, if it is a turn this process
 * sent in: the sent turn its tag names, else the first unfound sent turn with its text, which is
 * consumed, so the same words arriving again are someone else's; the answer is remembered for that
 * message.
 */
export function matchInjectedUserTurn(sessionID: string, message: unknown): string | undefined {
  if (typeof message !== "object" || message === null) return undefined;
  if (!("role" in message) || message.role !== "user") return undefined;
  if (!("timestamp" in message) || typeof message.timestamp !== "number") return undefined;
  const session = envoyPluginInterface().injectedUserTurns.get(sessionID);
  if (session === undefined) return undefined;
  const known = session.found.get(message.timestamp);
  if (known !== undefined) return known;
  const tag = "tag" in message && typeof message.tag === "string" ? message.tag : undefined;
  const text = userMessageText("content" in message ? message.content : undefined);
  const tagged = tag === undefined ? -1 : session.sent.findIndex((sent) => sent.messageId === tag);
  const index = tagged === -1 ? session.sent.findIndex((sent) => sent.body === text) : tagged;
  const sent = session.sent[index];
  if (sent === undefined) return undefined;
  session.sent.splice(index, 1);
  session.found.set(message.timestamp, sent.messageId);
  return sent.messageId;
}

/** `sessionID`'s run ended: a sent turn not yet found is not looked for again, and nothing found
 *  is asked about again. The one expiry of the record. */
export function endInjectedUserTurns(sessionID: string): void {
  envoyPluginInterface().injectedUserTurns.delete(sessionID);
}

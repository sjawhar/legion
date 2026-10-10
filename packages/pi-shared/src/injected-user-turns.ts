import { envoyPluginInterface } from "./interface";

/**
 * The user turns this process sent into one session and the messages found for them. A turn sent
 * through `pi.sendUserInput` (`noteTypedUserTurn`) carries its Dispatch message id as the host's
 * `tag`, which every message the input submits keeps: a user message, even one whose text the host
 * rewrote (a prompt template, a file command), and the `skill-prompt` custom message a `/skill:`
 * submits. Such a turn is found by its tag alone, never by a message's text, so a command that
 * submitted nothing leaves no turn the same words can take; it is kept until its message is found
 * or the host answers that the input submitted none (`dropInjectedUserTurn`), across the run's end,
 * since the host can run it as a turn of its own after that run. A turn sent through
 * `pi.sendUserMessage` (`noteInjectedUserTurn`), on a host without `sendUserInput`, is linked to
 * nothing the host records, so it is found by its text: the first user message that equals it,
 * until the session's run ends. A message found is remembered under its host timestamp until the
 * run ends, so every handler that asks about it gets the same answer whichever asks first: envoy.ts's
 * stream recorder tags its frames, and Legion's phase-stall check (`extensions/legion.ts`) counts it
 * as an inbound event rather than the daemon's assignment.
 *
 * The record is the `injectedUserTurns` member of the versioned interface (`interface.ts`):
 * envoy.ts writes what legion.ts reads, and each plugin bundles its own copy of this module.
 */
export interface SessionUserTurns {
  /** `body` is a `sendUserMessage` turn's text, which finds it; a typed turn has none. */
  readonly sent: { readonly messageId: string; readonly body?: string }[];
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

function sessionTurns(sessionID: string): SessionUserTurns {
  const turns = envoyPluginInterface().injectedUserTurns;
  const session: SessionUserTurns = turns.get(sessionID) ?? { found: new Map(), sent: [] };
  turns.set(sessionID, session);
  return session;
}

/** Records that Dispatch message `messageId`, whose body is `body`, was just sent into
 *  `sessionID` through `pi.sendUserMessage` as its user's own turn. */
export function noteInjectedUserTurn(sessionID: string, body: string, messageId: string): void {
  sessionTurns(sessionID).sent.push({ body, messageId });
}

/** Records that Dispatch message `messageId` was just sent into `sessionID` through
 *  `pi.sendUserInput`, tagged with its id. */
export function noteTypedUserTurn(sessionID: string, messageId: string): void {
  sessionTurns(sessionID).sent.push({ messageId });
}

/** The host answered that the typed turn `messageId` submitted no message: it is no longer one of
 *  `sessionID`'s sent turns. */
export function dropInjectedUserTurn(sessionID: string, messageId: string): void {
  const session = envoyPluginInterface().injectedUserTurns.get(sessionID);
  if (session === undefined) return;
  const index = session.sent.findIndex((sent) => sent.messageId === messageId);
  if (index !== -1) session.sent.splice(index, 1);
}

/**
 * The Dispatch message a message `sessionID` records delivered, if it is a turn this process sent
 * in: a user message or a custom message whose tag names a sent turn, else a user message whose
 * text equals the first unfound `sendUserMessage` turn's. The turn found is consumed, so the same
 * words arriving again are someone else's, and the answer is remembered for that message.
 */
export function matchInjectedUserTurn(sessionID: string, message: unknown): string | undefined {
  if (typeof message !== "object" || message === null) return undefined;
  if (!("role" in message) || (message.role !== "user" && message.role !== "custom")) {
    return undefined;
  }
  if (!("timestamp" in message) || typeof message.timestamp !== "number") return undefined;
  const session = envoyPluginInterface().injectedUserTurns.get(sessionID);
  if (session === undefined) return undefined;
  const known = session.found.get(message.timestamp);
  if (known !== undefined) return known;
  const tag = "tag" in message && typeof message.tag === "string" ? message.tag : undefined;
  let index = tag === undefined ? -1 : session.sent.findIndex((sent) => sent.messageId === tag);
  if (index === -1 && message.role === "user") {
    const text = userMessageText("content" in message ? message.content : undefined);
    index = session.sent.findIndex((sent) => sent.body === text);
  }
  const sent = session.sent[index];
  if (sent === undefined) return undefined;
  session.sent.splice(index, 1);
  session.found.set(message.timestamp, sent.messageId);
  return sent.messageId;
}

/** `sessionID`'s run ended: a `sendUserMessage` turn not yet found is not looked for again, and
 *  nothing found is asked about again; a typed turn not yet found is kept, since only its own
 *  message carries its tag. */
export function endInjectedUserTurns(sessionID: string): void {
  const turns = envoyPluginInterface().injectedUserTurns;
  const typed = turns.get(sessionID)?.sent.filter((sent) => sent.body === undefined) ?? [];
  if (typed.length === 0) turns.delete(sessionID);
  else turns.set(sessionID, { found: new Map(), sent: typed });
}

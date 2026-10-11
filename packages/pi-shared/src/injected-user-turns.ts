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
 * until the session's run ends. A message found is remembered until the run ends under the
 * identity the stream keys it by, its host timestamp and, when the host kept one, its tag (two
 * Sends can be stamped in the same millisecond), so every handler that asks about it gets the same
 * answer whichever asks first: envoy.ts's stream recorder tags its frames, and Legion's
 * phase-stall check (`extensions/legion.ts`) counts it as an inbound event rather than the
 * daemon's assignment.
 *
 * Every match asks under the session id live when the message arrives, so when an instance moves
 * off a session id it forgets the turns it sent in there (`forgetInjectedUserTurns`): it never asks
 * under that id again. The record is process-wide, and an ACP host serves several top-level
 * sessions in one process, each with its own instance, so an instance forgets only an id it noted
 * turns under; another instance still serving that id keeps its turns.
 *
 * The record is the `injectedUserTurns` member of the versioned interface (`interface.ts`):
 * envoy.ts writes what legion.ts reads, and each plugin bundles its own copy of this module.
 */
export type SentUserTurn =
  | { readonly via: "typed"; readonly messageId: string }
  | { readonly via: "text"; readonly messageId: string; readonly body: string };

export interface SessionUserTurns {
  /** Typed input is found by tag; `sendUserMessage` input is found by body text. */
  readonly sent: SentUserTurn[];
  /** What each message asked about was found to be, by `<timestamp>:<tag>` for a tagged message
   *  and `<timestamp>` for an untagged one. */
  readonly found: Map<string, string>;
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
  sessionTurns(sessionID).sent.push({ body, messageId, via: "text" });
}

/** Records that Dispatch message `messageId` was just sent into `sessionID` through
 *  `pi.sendUserInput`, tagged with its id. */
export function noteTypedUserTurn(sessionID: string, messageId: string): void {
  sessionTurns(sessionID).sent.push({ messageId, via: "typed" });
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
  const tag = messageTag(message);
  const memo = tag === undefined ? `${message.timestamp}` : `${message.timestamp}:${tag}`;
  const known = session.found.get(memo);
  if (known !== undefined) return known;
  let index = tag === undefined ? -1 : session.sent.findIndex((sent) => sent.messageId === tag);
  if (index === -1 && message.role === "user") {
    const text = userMessageText("content" in message ? message.content : undefined);
    index = session.sent.findIndex((sent) => sent.via === "text" && sent.body === text);
  }
  const sent = session.sent[index];
  if (sent === undefined) return undefined;
  session.sent.splice(index, 1);
  session.found.set(memo, sent.messageId);
  return sent.messageId;
}

/** The tag the host kept on `message`, the Dispatch message id of the typed input it submitted. */
export function messageTag(message: unknown): string | undefined {
  if (typeof message !== "object" || message === null || !("tag" in message)) return undefined;
  return typeof message.tag === "string" ? message.tag : undefined;
}

/** `sessionID`'s run ended: a `sendUserMessage` turn not yet found is not looked for again, and
 *  nothing found is asked about again; a typed turn not yet found is kept, since only its own
 *  message carries its tag. */
export function endInjectedUserTurns(sessionID: string): void {
  const turns = envoyPluginInterface().injectedUserTurns;
  const typed = turns.get(sessionID)?.sent.filter((sent) => sent.via === "typed") ?? [];
  if (typed.length === 0) turns.delete(sessionID);
  else turns.set(sessionID, { found: new Map(), sent: typed });
}

/** An instance that noted turns under `sessionID` no longer serves it: every turn sent into it and
 *  every answer given for it go, since that instance never matches under the id again. */
export function forgetInjectedUserTurns(sessionID: string): void {
  envoyPluginInterface().injectedUserTurns.delete(sessionID);
}

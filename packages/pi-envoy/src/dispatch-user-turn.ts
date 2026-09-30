import type { DispatchDelivery, RenderInboundResult } from "@legion/envoy-client/delivery";
import { z } from "zod";

/**
 * A person's direct message from Dispatch's Agents page, delivered as the session's own user turn
 * (LEGION-394): the message a person types at the terminal, not an Envoy card. The frame that says
 * a person wrote it is not proof of that: the listener takes an envelope's source from whoever
 * sends it, and every session holds that token. So a frame is only a candidate
 * (`isUserTurnCandidate`), the session reads the message back from Dispatch with its own bearer
 * (`confirmUserTurn`), and it takes the turn only once Dispatch records that this session accepted
 * that very attempt (`POST /api/v1/messages/{id}/deliveries/{attempt}/accept`), which Dispatch
 * allows once per message, for a person's attempt of the last minute. Anything else keeps today's
 * card, which is the delivery.
 */

/**
 * Whether a frame could be a person's direct Send or Aside, worth reading back from Dispatch: a
 * Dispatch `message.created` delivery (renderInbound builds a message delivery for no other event)
 * on no issue, whose actor is a person, in aside or steer, and that names no broadcast. An issue
 * message and a comment mention keep their envelope, since their flow replies on the issue; a BTW
 * is a side turn; a broadcast keeps its envelope and its reply counts, and a frame that falsely
 * claims one only yields the card.
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

/** What a confirmed message becomes: its stored body, sent as Send (steer) or Aside. */
export interface ConfirmedUserTurn {
  readonly messageId: string;
  readonly attempt: number;
  readonly body: string;
  readonly mode: "aside" | "steer";
}

/** The fields of one stored message the confirmation reads, and nothing else. */
const StoredMessageSchema = z.object({
  id: z.string(),
  author: z.object({ kind: z.string() }),
  body: z.string(),
  issue_key: z.string().nullable(),
  target: z.string().nullable(),
  // Absent from a Dispatch older than the field, which is not the same as saying no broadcast.
  broadcast_id: z.string().nullable().optional(),
  deliveries: z.array(
    z.object({ attempt: z.number().int(), session_id: z.string(), delivery: z.string() })
  ),
});

/** `GET /api/v1/messages/{id}?session=`'s answer: the thread root and every reply. */
const MessageThreadSchema = z.object({
  message: StoredMessageSchema,
  replies: z.array(StoredMessageSchema),
});

/**
 * Whether Dispatch's read of the thread a delivery's message belongs to (`GET
 * /api/v1/messages/{id}?session=`) confirms it as a person's own message to this session: the
 * stored author is a person, it belongs to no issue, the thread's root is aimed at this session,
 * no broadcast sent it (a Dispatch that does not say is not taken as saying no), and Dispatch
 * recorded the attempt the frame names as delivered to this session. The mode is that stored
 * attempt's, never the frame's. The read is untrusted JSON, so anything that is not the shape
 * above confirms nothing.
 */
export function confirmUserTurn(
  read: unknown,
  delivery: DispatchDelivery,
  sessionID: string
): ConfirmedUserTurn | undefined {
  const parsed = MessageThreadSchema.safeParse(read);
  if (!parsed.success) return undefined;
  const { message: root, replies } = parsed.data;
  const message = root.id === delivery.id ? root : replies.find(({ id }) => id === delivery.id);
  if (
    message === undefined ||
    message.author.kind !== "user" ||
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
  return {
    attempt: attempt.attempt,
    body: message.body,
    messageId: message.id,
    mode: attempt.delivery,
  };
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

/**
 * The user turns this process sent into one session and the user messages found for them. The
 * host links a sent prompt to nothing it records, so a sent turn is found by its text: the first
 * user message that equals it, until the session's run ends. A message found is remembered under
 * its host timestamp, so every handler that asks about it gets the same answer whichever asks
 * first: envoy.ts's stream recorder tags its frames, and Legion's phase-stall check
 * (`extensions/legion.ts`) counts it as an inbound event rather than the daemon's assignment.
 */
interface SessionUserTurns {
  readonly sent: { readonly body: string; readonly messageId: string }[];
  readonly found: Map<number, string>;
}

interface GlobalInjectedUserTurnStore {
  [key: symbol]: Map<string, SessionUserTurns> | undefined;
}

// Process-wide, because each manifest entry loads its own copy of this module: envoy.ts writes
// what legion.ts reads.
const INJECTED_USER_TURNS = Symbol.for("legion.pi-envoy.injected-user-turns");

function injectedUserTurns(): Map<string, SessionUserTurns> {
  const store = globalThis as typeof globalThis & GlobalInjectedUserTurnStore;
  const turns = store[INJECTED_USER_TURNS] ?? new Map<string, SessionUserTurns>();
  store[INJECTED_USER_TURNS] = turns;
  return turns;
}

/** Records that Dispatch message `messageId`, whose body is `body`, was just sent into
 *  `sessionID` as its user's own turn. */
export function noteInjectedUserTurn(sessionID: string, body: string, messageId: string): void {
  const turns = injectedUserTurns();
  const session: SessionUserTurns = turns.get(sessionID) ?? { found: new Map(), sent: [] };
  session.sent.push({ body, messageId });
  turns.set(sessionID, session);
}

/**
 * The Dispatch message a user message `sessionID` records delivered, if it is a turn this process
 * sent in: the first unfound sent turn with its text is consumed, so the same words arriving again
 * are someone else's, and the answer is remembered for that message.
 */
export function matchInjectedUserTurn(sessionID: string, message: unknown): string | undefined {
  if (typeof message !== "object" || message === null) return undefined;
  if (!("role" in message) || message.role !== "user") return undefined;
  if (!("timestamp" in message) || typeof message.timestamp !== "number") return undefined;
  const session = injectedUserTurns().get(sessionID);
  if (session === undefined) return undefined;
  const known = session.found.get(message.timestamp);
  if (known !== undefined) return known;
  const text = userMessageText("content" in message ? message.content : undefined);
  const index = session.sent.findIndex((sent) => sent.body === text);
  const sent = session.sent[index];
  if (sent === undefined) return undefined;
  session.sent.splice(index, 1);
  session.found.set(message.timestamp, sent.messageId);
  return sent.messageId;
}

/** `sessionID`'s run ended: a sent turn not yet found is not looked for again, and nothing found
 *  is asked about again. The one expiry of the record. */
export function endInjectedUserTurns(sessionID: string): void {
  injectedUserTurns().delete(sessionID);
}

/** Test seam: `bun test` runs every file in one process, and the record is process-wide. */
export function resetInjectedUserTurnsForTests(): void {
  injectedUserTurns().clear();
}

import { ApiError, api } from "../../api/client";
import type {
  AskOption,
  AskUrgency,
  CreateAgentMessageInput,
  CreateCommentInput,
  DeliveryCapability,
  Message,
} from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import type {
  AcceptedMention,
  ComposerAnchor,
  ComposerKind,
  ComposerOwner,
  ReplyTarget,
} from "./composer-model";

/** A send's own copy of the draft, taken when it starts: what the request is built from, and
 *  what a refusal hands back. Mentions are offsets into that exact text. */
export interface SentDraft {
  readonly body: string;
  readonly mentions: readonly AcceptedMention[];
  readonly replacement: string;
}

/**
 * A send, whole, as it stood when Send started it: where it goes (the owner, the reply it answers,
 * its kind and anchor), what it carries (the draft and an ask's fields), and, for an edit, which
 * comment it saves and how. TanStack gives a pending mutation each new render's options and calls
 * `mutationFn` only once `onMutate` has resolved, so a request read from the latest render follows
 * whatever changed in Send's own task - a host's Reply or issue pick, a kind switch, a newer
 * anchor. Built from this value alone, the request is the one the reader sent, in every host,
 * whether or not the host holds its own controls while the send is out.
 */
export interface SentRequest {
  readonly anchor: ComposerAnchor | undefined;
  readonly ask: {
    readonly multiple: boolean;
    readonly options: AskOption[];
    readonly urgency: AskUrgency;
  };
  readonly draft: SentDraft;
  readonly edit:
    | {
        readonly id: string;
        readonly save: (id: string, body: string) => Promise<unknown>;
      }
    | undefined;
  readonly kind: ComposerKind;
  readonly owner: ComposerOwner;
  /** The mode a message with no delivery command goes in when it answers no thread
   *  (`plainDelivery`, from the addressed sessions' capabilities as Send found them). */
  readonly plainDelivery: DeliveryCapability;
  readonly replyTo: ReplyTarget | null;
  /** The key that names this send to Dispatch where its route takes one - a message to one
   *  session (`idempotency_key`) - so that a repeat of the request is that one message there.
   *  Minted when Send builds the request, and kept by a Retry that sends the same message
   *  (`sendKeyFor`). */
  readonly sendKey: string;
}

/** The command, at the start of a body, that sends it in a mode other than the plain one (Send,
 *  unless the session refuses Send: `plainDelivery`). `deliveryPlan` strips it from what goes out,
 *  and the composer names it where it tells the reader how to send in another mode or what a bare
 *  command still needs. */
export const DELIVERY_COMMANDS = { aside: "/aside", btw: "/btw" } as const satisfies Record<
  Exclude<DeliveryCapability, "steer">,
  string
>;

function parseDelivery(body: string): { body: string; delivery: DeliveryCapability } {
  for (const mode of ["btw", "aside"] as const) {
    const command = `${DELIVERY_COMMANDS[mode]} `;
    if (body.startsWith(command)) return { body: body.slice(command.length), delivery: mode };
  }
  return { body, delivery: "steer" };
}

export interface DeliveryPlan {
  readonly body: string;
  readonly delivery: DeliveryCapability | undefined;
}

export function deliveryPlan(
  body: string,
  inherited: DeliveryCapability | undefined
): DeliveryPlan {
  const parsed = parseDelivery(body);
  const hasCommand = parsed.body !== body;
  return {
    body: inherited === undefined || !hasCommand ? body : parsed.body,
    delivery: inherited === undefined ? undefined : hasCommand ? parsed.delivery : inherited,
  };
}

export function mentionText(text: string): string {
  return `@${text}`;
}

/** The accepted mentions the body still carries: each target once, and only while its span reads
 *  exactly as it was accepted. A span the reader edited is their prose, not a mention. */
export function survivingMentions(
  body: string,
  mentions: readonly AcceptedMention[]
): AcceptedMention[] {
  const seen = new Set<string>();
  const surviving: AcceptedMention[] = [];
  for (const mention of mentions) {
    if (
      seen.has(mention.target) ||
      mention.start < 0 ||
      mention.end > body.length ||
      mention.start >= mention.end ||
      body.slice(mention.start, mention.end) !== mentionText(mention.text)
    ) {
      continue;
    }
    seen.add(mention.target);
    surviving.push(mention);
  }
  return surviving;
}

/** How long a send holds what takes the reader away from its composer - a host's Back, Escape or
 *  Collapse thread - before it lets go: a request the server never answers would hold them for
 *  good. The request goes on past it, and its answer is still the send's outcome. */
export const SEND_DEADLINE_MS = 30_000;

/** A send the server did not answer within `SEND_DEADLINE_MS`. It is not a refusal: the request
 *  goes on, and `answer` is still its outcome. */
export class SendDeadlineError extends Error {
  readonly answer: Promise<unknown>;

  constructor(answer: Promise<unknown>) {
    super(`the server has not answered within ${SEND_DEADLINE_MS / 1000} seconds`);
    this.answer = answer;
    this.name = "SendDeadlineError";
  }
}

/** How long one attempt of a keyed send to a session may go unanswered before it is given up and
 *  made again under the same key. */
export const KEYED_SEND_ATTEMPT_MS = 15_000;

/** What a keyed send to a session says once it is past `SEND_DEADLINE_MS`: still the server's, as
 *  any late send is, and still being tried under its key, which Dispatch takes once. */
export const KEYED_SEND_LATE_TEXT =
  "Still sending — Dispatch has not confirmed it yet. It keeps trying, and the message is delivered once.";

/** The wait before a keyed send's first retry, doubling after each failed attempt up to
 *  `KEYED_SEND_MAX_BACKOFF_MS`. */
const KEYED_SEND_FIRST_BACKOFF_MS = 1_000;
const KEYED_SEND_MAX_BACKOFF_MS = 30_000;

/** A message to one session as Dispatch receives it, but for its key: what Dispatch compares to
 *  decide whether two requests under one key are one message. */
type AgentMessage = Omit<CreateAgentMessageInput, "idempotency_key">;

/** The message a send to a session puts on the wire: its body with any delivery command stripped,
 *  the mode it names or the reply's thread or the session takes, and the reply it answers. */
function agentMessage({
  draft,
  plainDelivery,
  replyTo,
}: Pick<SentRequest, "draft" | "plainDelivery" | "replyTo">): AgentMessage {
  const inheritedDelivery = replyTo?.thread?.delivery ?? plainDelivery;
  const plan = deliveryPlan(draft.body, inheritedDelivery);
  return {
    body: plan.body,
    delivery: plan.delivery ?? inheritedDelivery,
    ...(replyTo === null ? {} : { in_reply_to: replyTo.id }),
  };
}

/** The key `next` goes under. A Retry that sends the refused send's message again - the same
 *  session, body, mode and reply - keeps the refused send's key: the refusal may follow a request
 *  Dispatch did act on, and the key is how Dispatch answers the repeat with that message rather
 *  than making a second. Anything else is another message, under a key of its own. */
export function sendKeyFor(
  refused: SentRequest | undefined,
  next: Omit<SentRequest, "sendKey">
): string {
  if (
    refused?.owner.kind !== "session" ||
    next.owner.kind !== "session" ||
    refused.owner.sessionId !== next.owner.sessionId
  ) {
    return newSendKey();
  }
  const before = agentMessage(refused);
  const after = agentMessage(next);
  return before.body === after.body &&
    before.delivery === after.delivery &&
    before.in_reply_to === after.in_reply_to
    ? refused.sendKey
    : newSendKey();
}

/** Waits `ms`, and says whether it waited the whole of it: `false` once `signal` aborts, at once. */
function pause(ms: number, signal: AbortSignal | undefined): Promise<boolean> {
  if (signal?.aborted === true) return Promise.resolve(false);
  const { promise, resolve } = Promise.withResolvers<boolean>();
  const stop = () => {
    clearTimeout(timer);
    resolve(false);
  };
  const timer = setTimeout(() => {
    signal?.removeEventListener("abort", stop);
    resolve(true);
  }, ms);
  signal?.addEventListener("abort", stop, { once: true });
  return promise;
}

/** A keyed send to a session that stopped trying before Dispatch confirmed it: the reader pressed
 *  Stop trying, or left what held the send. Not Dispatch's refusal: the message may have reached
 *  it, and a Retry under the same key is answered with that message rather than a second one. */
export class SendStoppedError extends Error {
  constructor() {
    super(
      "Dispatch has not confirmed this message. Retry sends it again under the same key, so it is delivered at most once."
    );
    this.name = "SendStoppedError";
  }
}

/**
 * Sends `input` to session `sessionId` under `key` until Dispatch decides it. Each attempt is held
 * to `KEYED_SEND_ATTEMPT_MS`, and one that ends without Dispatch's verdict - the fetch failed, the
 * attempt timed out, a 5xx, a 429 - is made again under the same key after 1 s, 2 s, 4 s … never
 * more than 30 s apart, so however many of them reach Dispatch, it makes the message once. Resolves
 * with the message on Dispatch's 2xx; rejects with any other 4xx, its refusal. `signal` stops the
 * trying between attempts - a wait ends at once, an attempt in flight is let finish, since it may
 * already have reached Dispatch and its answer is still the send's - and the send then rejects
 * with `SendStoppedError`.
 */
export async function sendKeyedAgentMessage(
  sessionId: string,
  input: AgentMessage,
  key: string,
  signal?: AbortSignal
): Promise<Message> {
  const keyed = { ...input, idempotency_key: key };
  let backoff = KEYED_SEND_FIRST_BACKOFF_MS;
  for (;;) {
    try {
      return await api.createAgentMessage(sessionId, keyed, { deadlineMs: KEYED_SEND_ATTEMPT_MS });
    } catch (error) {
      if (error instanceof ApiError && error.status !== 429 && error.status < 500) throw error;
    }
    if (!(await pause(backoff, signal))) throw new SendStoppedError();
    backoff = Math.min(backoff * 2, KEYED_SEND_MAX_BACKOFF_MS);
  }
}

/** `sendRequest`, answered by the server, or rejected with a `SendDeadlineError` carrying the
 *  request's own answer once `SEND_DEADLINE_MS` passes without one. `stop` stops a keyed send's
 *  trying (`sendKeyedAgentMessage`). */
export function sendWithinDeadline(sent: SentRequest, stop?: AbortSignal): Promise<unknown> {
  const answer = sendRequest(sent, stop);
  const { promise, reject, resolve } = Promise.withResolvers<unknown>();
  const deadline = setTimeout(() => reject(new SendDeadlineError(answer)), SEND_DEADLINE_MS);
  answer.then(resolve, reject).finally(() => clearTimeout(deadline));
  return promise;
}

/** Sends a `SentRequest`. It sits outside the composer, so no later render's props or state can
 *  reach the request it builds. A message to one session goes under the request's key, and is
 *  tried until Dispatch decides it or `stop` aborts (`sendKeyedAgentMessage`). */
export async function sendRequest(sent: SentRequest, stop?: AbortSignal): Promise<unknown> {
  const { anchor, ask, draft, edit, kind, owner, plainDelivery, replyTo } = sent;
  const { body, mentions, replacement } = draft;
  if (edit !== undefined) return edit.save(edit.id, body.trim());
  const selection =
    anchor === undefined ? undefined : { artifact: anchor.artifact, mark_id: anchor.mark_id };
  if (kind === "ask") {
    if (owner.kind === "session")
      throw new Error("The direct session channel does not support asks.");
    const input = {
      anchor: selection,
      multiple: ask.multiple,
      options: ask.options,
      question: body.trim(),
      urgency: ask.urgency,
    };
    return owner.kind === "issue"
      ? api.createAsk(owner.issueKey, input)
      : api.createArtifactAsk(owner.artifactId, input);
  }
  if (owner.kind === "session") {
    return sendKeyedAgentMessage(owner.sessionId, agentMessage(sent), sent.sendKey, stop);
  }
  if (replyTo?.parentKind === "message") {
    if (owner.kind !== "issue") throw new Error("Legacy message replies belong to an issue.");
    const plan = deliveryPlan(body, replyTo.thread?.delivery);
    return api.createMessage(owner.issueKey, {
      body: plan.body,
      in_reply_to: replyTo.id,
      ...(replyTo.thread === undefined
        ? {}
        : { delivery: plan.delivery, target: replyTo.thread.target }),
    });
  }
  const targets = survivingMentions(body, mentions).map((mention) => mention.target);
  const baseBody = kind === "suggestion" && body.trim() === "" ? "Suggested replacement." : body;
  const plan = deliveryPlan(baseBody, targets.length === 0 ? undefined : plainDelivery);
  const comment: CreateCommentInput = {
    body: plan.body,
    ...(replyTo === null ? {} : { reply_to: replyTo.id }),
    ...(kind === "suggestion" ? { suggestion: { replace_with: replacement } } : {}),
    ...(targets.length === 0
      ? {}
      : {
          delivery: plan.delivery ?? plainDelivery,
          mentions: targets.map((target) => ({ target })),
        }),
  };
  const input = replyTo === null ? { ...comment, anchor: selection } : comment;
  return owner.kind === "issue"
    ? api.createComment(owner.issueKey, input)
    : api.createArtifactComment(owner.artifactId, input);
}

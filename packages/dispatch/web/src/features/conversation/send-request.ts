import { api, type WriteBounds } from "../../api/client";
import type {
  AskOption,
  AskUrgency,
  CreateAgentMessageInput,
  CreateCommentInput,
  DeliveryCapability,
} from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import { retriesUnderItsKey } from "../writes/keyed-write";
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
    readonly options: readonly AskOption[];
    readonly urgency: AskUrgency;
  };
  readonly draft: SentDraft;
  readonly edit:
    | {
        readonly id: string;
        readonly save: (id: string, body: string, bounds: WriteBounds) => Promise<unknown>;
      }
    | undefined;
  readonly kind: ComposerKind;
  readonly owner: ComposerOwner;
  /** The mode a message with no delivery command goes in when it answers no thread
   *  (`plainDelivery`, from the addressed sessions' capabilities as Send found them). */
  readonly plainDelivery: DeliveryCapability;
  /** A mode the reader picked outright - the live view's `Send as` - in which a message to one
   *  session goes as written: no `/btw` or `/aside` at its start is read as a command, since the
   *  live view sends a session its commands verbatim. */
  readonly pickedDelivery?: DeliveryCapability;
  readonly replyTo: ReplyTarget | null;
  /** The key that names this send to Dispatch (`Idempotency-Key`), so that every attempt of it,
   *  and a reload's, is that one write there. Minted when Send builds the request, and kept by a
   *  Retry of a send that ended without Dispatch's verdict (`requestFor`). */
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

/** What a send says once it is past `SEND_DEADLINE_MS`: still the server's, as any late send is,
 *  and still being tried under its key, which Dispatch takes once. */
export const KEYED_SEND_LATE_TEXT =
  "Still sending — Dispatch has not confirmed it yet. It keeps trying, and the message is delivered once.";

/** The message a send to a session puts on the wire: its body with any delivery command stripped,
 *  the mode it names or the reply's thread or the session takes, and the reply it answers - or, in
 *  a mode the reader picked outright, the body as written in that mode. */
function agentMessage({
  draft,
  pickedDelivery,
  plainDelivery,
  replyTo,
}: Pick<
  SentRequest,
  "draft" | "pickedDelivery" | "plainDelivery" | "replyTo"
>): CreateAgentMessageInput {
  const reply = replyTo === null ? {} : { in_reply_to: replyTo.id };
  if (pickedDelivery !== undefined) return { body: draft.body, delivery: pickedDelivery, ...reply };
  const inheritedDelivery = replyTo?.thread?.delivery ?? plainDelivery;
  const plan = deliveryPlan(draft.body, inheritedDelivery);
  return { body: plan.body, delivery: plan.delivery ?? inheritedDelivery, ...reply };
}

/** Whether `next`, an edited draft, is still the refused send's message on the wire: to the same
 *  session, with the same body, mode and reply. Dispatch compares the wire, so such a message is
 *  the same write under the same key. */
function sameMessage(refused: SentRequest, next: Omit<SentRequest, "sendKey">): boolean {
  if (
    refused.owner.kind !== "session" ||
    next.owner.kind !== "session" ||
    refused.owner.sessionId !== next.owner.sessionId
  ) {
    return false;
  }
  const before = agentMessage(refused);
  const after = agentMessage(next);
  return (
    before.body === after.body &&
    before.delivery === after.delivery &&
    before.in_reply_to === after.in_reply_to
  );
}

/** Whether `next` is the refused send unedited: the same draft - its text, accepted mentions and a
 *  suggestion's replacement, and an ask's fields - to the same place, in the same kind. */
export function unedited(refused: SentRequest, next: Omit<SentRequest, "sendKey">): boolean {
  return (
    refused.kind === next.kind &&
    JSON.stringify(refused.owner) === JSON.stringify(next.owner) &&
    refused.anchor?.artifact === next.anchor?.artifact &&
    refused.anchor?.mark_id === next.anchor?.mark_id &&
    refused.replyTo?.id === next.replyTo?.id &&
    refused.edit?.id === next.edit?.id &&
    refused.draft.body === next.draft.body &&
    refused.draft.replacement === next.draft.replacement &&
    JSON.stringify(refused.draft.mentions) === JSON.stringify(next.draft.mentions) &&
    (next.kind !== "ask" || JSON.stringify(refused.ask) === JSON.stringify(next.ask))
  );
}

/**
 * What Send or Retry sends, given `refused`, the request of the refusal it replaces, if any, and
 * `refusal`, what ended it. A send that ended without Dispatch's verdict - stopped, or interrupted
 * by a reload - may have landed, so its unedited Retry is the refused request itself, its key
 * included, whichever surface made it and whichever presses Retry, and Dispatch answers it with
 * what the first did; an edited one keeps the key only while it is still that message on the wire
 * (`sameMessage`). A refusal Dispatch gave is its verdict on that key, so any Retry after it goes
 * under a new key: for a message to one session, unedited, the refused request as it was sent - its
 * mode, picked or plain - so a message the live view sent in a mode it picked never goes out again
 * in the Agents row's mode; for anything else, and an edited draft, the request as the composer
 * shows it now, a comment in the modes its mentioned sessions advertise now.
 */
export function requestFor(
  refused: SentRequest | undefined,
  refusal: Error | undefined,
  next: Omit<SentRequest, "sendKey">
): SentRequest {
  const underItsKey = refused !== undefined && retriesUnderItsKey(refusal);
  if (
    refused !== undefined &&
    (underItsKey || refused.owner.kind === "session") &&
    unedited(refused, next)
  ) {
    return underItsKey ? refused : { ...refused, sendKey: newSendKey() };
  }
  return {
    ...next,
    sendKey: underItsKey && sameMessage(refused, next) ? refused.sendKey : newSendKey(),
  };
}

/** One attempt of `sent`, an edit or a send to an issue or a document, under `bounds`: built from
 *  that value alone, so no later render's props or state can reach the request it makes. */
function attempt(
  sent: SentRequest & { readonly owner: Exclude<ComposerOwner, { kind: "session" }> },
  bounds: WriteBounds
): Promise<unknown> {
  const { anchor, ask, draft, kind, owner, plainDelivery, replyTo } = sent;
  const { body, mentions, replacement } = draft;
  const selection =
    anchor === undefined ? undefined : { artifact: anchor.artifact, mark_id: anchor.mark_id };
  if (kind === "ask") {
    const input = {
      anchor: selection,
      multiple: ask.multiple,
      options: [...ask.options],
      question: body.trim(),
      urgency: ask.urgency,
    };
    return owner.kind === "issue"
      ? api.createAsk(owner.issueKey, input, bounds)
      : api.createArtifactAsk(owner.artifactId, input, bounds);
  }
  if (replyTo?.parentKind === "message" && owner.kind === "issue") {
    const plan = deliveryPlan(body, replyTo.thread?.delivery);
    return api.createMessage(
      owner.issueKey,
      {
        body: plan.body,
        in_reply_to: replyTo.id,
        ...(replyTo.thread === undefined
          ? {}
          : { delivery: plan.delivery, target: replyTo.thread.target }),
      },
      bounds
    );
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
    ? api.createComment(owner.issueKey, input, bounds)
    : api.createArtifactComment(owner.artifactId, input, bounds);
}

/**
 * One attempt of a `SentRequest`, built from that value alone, so no later render's props or state
 * can reach the request it makes, whichever surface made it and whenever the store makes it again
 * (`held-sends.ts`): an edit saves its comment, a message to one session goes as `agentMessage`
 * puts it, and anything else is an ask, a message reply or a comment on its issue or document. A
 * request no route takes - an ask to a session, a message reply on a document - is refused at once,
 * an error of the page's own.
 */
export function sendAttempt(sent: SentRequest): (bounds: WriteBounds) => Promise<unknown> {
  const { edit, kind, owner, replyTo } = sent;
  if (edit !== undefined) return (bounds) => edit.save(edit.id, sent.draft.body.trim(), bounds);
  if (owner.kind === "session") {
    if (kind === "ask") {
      return () => Promise.reject(new Error("The direct session channel does not support asks."));
    }
    const message = agentMessage(sent);
    return (bounds) => api.createAgentMessage(owner.sessionId, message, bounds);
  }
  if (replyTo?.parentKind === "message" && owner.kind === "artifact") {
    return () => Promise.reject(new Error("Legacy message replies belong to an issue."));
  }
  return (bounds) => attempt({ ...sent, owner }, bounds);
}

import { api } from "../../api/client";
import type {
  AskOption,
  AskUrgency,
  CreateCommentInput,
  DeliveryCapability,
} from "../../api/types";
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
  readonly replyTo: ReplyTarget | null;
}

/** The command, at the start of a body, that sends it in a mode other than Send, the default one.
 *  `deliveryPlan` strips it from what goes out, and the composer names it where it tells the
 *  reader how to send in another mode or what a bare command still needs. */
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

/** `sendRequest`, answered by the server, or rejected with a `SendDeadlineError` carrying the
 *  request's own answer once `SEND_DEADLINE_MS` passes without one. */
export function sendWithinDeadline(sent: SentRequest): Promise<unknown> {
  const answer = sendRequest(sent);
  const { promise, reject, resolve } = Promise.withResolvers<unknown>();
  const deadline = setTimeout(() => reject(new SendDeadlineError(answer)), SEND_DEADLINE_MS);
  answer.then(resolve, reject).finally(() => clearTimeout(deadline));
  return promise;
}

/** Sends a `SentRequest`. It sits outside the composer, so no later render's props or state can
 *  reach the request it builds. */
export async function sendRequest({
  anchor,
  ask,
  draft,
  edit,
  kind,
  owner,
  replyTo,
}: SentRequest): Promise<unknown> {
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
    const inheritedDelivery = replyTo?.thread?.delivery ?? "steer";
    const plan = deliveryPlan(body, inheritedDelivery);
    const reply = replyTo === null ? {} : { in_reply_to: replyTo.id };
    return api.createAgentMessage(owner.sessionId, {
      body: plan.body,
      delivery: plan.delivery ?? inheritedDelivery,
      ...reply,
    });
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
  const plan = deliveryPlan(baseBody, targets.length === 0 ? undefined : "steer");
  const comment: CreateCommentInput = {
    body: plan.body,
    ...(replyTo === null ? {} : { reply_to: replyTo.id }),
    ...(kind === "suggestion" ? { suggestion: { replace_with: replacement } } : {}),
    ...(targets.length === 0
      ? {}
      : {
          delivery: plan.delivery ?? "steer",
          mentions: targets.map((target) => ({ target })),
        }),
  };
  const input = replyTo === null ? { ...comment, anchor: selection } : comment;
  return owner.kind === "issue"
    ? api.createComment(owner.issueKey, input)
    : api.createArtifactComment(owner.artifactId, input);
}

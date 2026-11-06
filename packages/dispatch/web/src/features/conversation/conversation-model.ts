import type {
  Comment,
  CommentDelivery,
  HandoffSummary,
  IssueAskThreadRow,
  Message,
  MessageRead,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { conversationItemId } from "../issue/pins";
import { rowAnsweredWithError } from "./delivery";

/** Stored delivery rows include `pending`; `answeredWithError` is read off the row the same way
 *  a receipt's is (`DeliveryOutcome`). */
export type CommentDeliveryAttempt = CommentDelivery & { readonly answeredWithError: boolean };

/** Which conversations the reader asked for. Closed is an answered or resolved ask, a comment
 *  thread somebody resolved or whose suggestion was accepted or rejected, or a phase handoff,
 *  which is a finished record from the moment it is written; a message thread is never closed,
 *  so it is listed under All and under Open. */
export type ConversationFilter = "all" | "closed" | "open";

interface ConversationBase {
  /** `ask:<id>`, `comment:<id>`, `message:<id>` or `handoff:<id>`: the card's `data-turn`, the
   *  id its pin is stored under (`features/issue/pins.ts`), and the tie-break that makes the
   *  order total. */
  readonly id: string;
  /** The conversation's newest activity: its root, its newest reply, and for an ask its answer
   *  and its resolution. An edit is not activity and never moves a card. */
  readonly at: string;
  readonly closed: boolean;
  /** The newest activity is at or after the reader's unread boundary. */
  readonly unread: boolean;
}

/** One card of the Conversation: an ask with its replies and its answer, a comment thread with
 *  its replies, a message thread, targeted or not, or one version of a phase handoff. Each is
 *  built from the issue's own entity list, so a conversation older than any window of the event
 *  log is listed like every other. */
export type Conversation =
  | (ConversationBase & { readonly kind: "ask"; readonly ask: IssueAskThreadRow })
  | (ConversationBase & {
      readonly kind: "comment";
      readonly root: Comment;
      readonly replies: readonly Comment[];
    })
  | (ConversationBase & {
      readonly kind: "message";
      readonly root: Message;
      readonly replies: readonly Message[];
    })
  | (ConversationBase & {
      readonly kind: "targeted-message";
      readonly root: Message;
      readonly replies: readonly Message[];
      /** The first reply a session sent: the card reads "Answered by" that session. */
      readonly answer?: Message;
    })
  | (ConversationBase & {
      readonly kind: "handoff";
      /** The record without its body: the card fetches the body when the reader expands it. */
      readonly handoff: HandoffSummary;
    });

/** A message thread's card: the two kinds that render a root message, its deliveries and its
 *  replies. */
export type MessageConversation = Extract<Conversation, { kind: "message" | "targeted-message" }>;

export interface ConversationInput {
  readonly asks: readonly IssueAskThreadRow[];
  readonly comments: readonly Comment[];
  readonly messages: readonly MessageRead[];
  /** Every phase handoff of the issue, each version its own entry. */
  readonly handoffs: readonly HandoffSummary[];
  /** `created_at` of the first event the reader has not read. Without one nothing is unread:
   *  either they have read the issue through its head, or its watermark has not been read. */
  readonly unreadFrom?: string;
}

export function commentDeliveries(
  deliveries: readonly CommentDelivery[]
): CommentDeliveryAttempt[] {
  return deliveries.map((row) => ({ ...row, answeredWithError: rowAnsweredWithError(row) }));
}

/** Whether a comment thread is done with: resolved, or its suggestion accepted or rejected. */
function isClosedComment(comment: Comment): boolean {
  return comment.resolved || (comment.suggestion !== null && comment.suggestion.accepted !== null);
}

/** The later of two moments, ignoring a moment the record does not carry. */
function newest(at: string, candidate: string | null | undefined): string {
  if (candidate === null || candidate === undefined || candidate === "") {
    return at;
  }
  return compareTimestamps(candidate, at) > 0 ? candidate : at;
}

function isUnread(at: string, unreadFrom: string | undefined): boolean {
  return unreadFrom !== undefined && compareTimestamps(at, unreadFrom) >= 0;
}

/** The comment at the top of `comment`'s reply chain: the first with no `reply_to`, or the last
 *  one present when a parent is missing. A cyclic chain stops at the first repeated comment. */
export function rootOf(byId: ReadonlyMap<string, Comment>, comment: Comment): Comment {
  const visited = new Set<string>();
  let root = comment;
  while (root.reply_to !== null && !visited.has(root.id)) {
    visited.add(root.id);
    const parent = byId.get(root.reply_to);
    if (parent === undefined) {
      break;
    }
    root = parent;
  }
  return root;
}

function askConversation(ask: IssueAskThreadRow, unreadFrom: string | undefined): Conversation {
  let at = ask.created_at;
  for (const reply of ask.thread.replies) {
    at = newest(at, reply.created_at);
  }
  at = newest(at, ask.last_reply?.created_at);
  at = newest(at, ask.answer?.at);
  at = newest(at, ask.resolution?.at);
  return {
    ask,
    at,
    closed: ask.state !== "open",
    id: conversationItemId("ask", ask.id),
    kind: "ask",
    unread: isUnread(at, unreadFrom),
  };
}

/** One card per root comment, including a root nobody has replied to. A comment under an ask
 *  belongs to that ask's card, which renders the exchange itself. */
function commentConversations(
  comments: readonly Comment[],
  unreadFrom: string | undefined
): Conversation[] {
  const byId = new Map(comments.map((comment) => [comment.id, comment]));
  const threads = new Map<string, { replies: Comment[]; root: Comment }>();
  for (const comment of comments) {
    if (comment.ask_id !== null) {
      continue;
    }
    const root = rootOf(byId, comment);
    if (root.reply_to !== null || root.ask_id !== null) {
      continue;
    }
    const thread = threads.get(root.id) ?? { replies: [], root };
    if (comment.id !== root.id) {
      thread.replies.push(comment);
    }
    threads.set(root.id, thread);
  }
  return [...threads.values()].map(({ replies, root }) => {
    replies.sort(
      (left, right) =>
        compareTimestamps(left.created_at, right.created_at) || left.id.localeCompare(right.id)
    );
    let at = root.created_at;
    for (const reply of replies) {
      at = newest(at, reply.created_at);
    }
    return {
      at,
      closed: isClosedComment(root),
      id: conversationItemId("comment", root.id),
      kind: "comment" as const,
      replies,
      root,
      unread: isUnread(at, unreadFrom),
    };
  });
}

/** One card per root message, targeted or not: a targeted root keeps its delivery card, with
 *  its replies beneath it, rather than becoming a second turn. */
function messageConversation(read: MessageRead, unreadFrom: string | undefined): Conversation {
  const replies = [...read.replies].sort(
    (left, right) =>
      compareTimestamps(left.created_at, right.created_at) || left.id.localeCompare(right.id)
  );
  let at = read.message.created_at;
  for (const reply of replies) {
    at = newest(at, reply.created_at);
  }
  const base = {
    at,
    closed: false,
    id: conversationItemId("message", read.message.id),
    replies,
    root: read.message,
    unread: isUnread(at, unreadFrom),
  };
  if (read.message.target === null) {
    return { ...base, kind: "message" };
  }
  const answer = replies.find((reply) => reply.author.kind === "session");
  return { ...base, kind: "targeted-message", ...(answer === undefined ? {} : { answer }) };
}

/** One card per handoff version. Nobody replies to a handoff and no later write changes it, so
 *  its activity is its own `created_at` and it is closed from the start. */
function handoffConversation(
  handoff: HandoffSummary,
  unreadFrom: string | undefined
): Conversation {
  return {
    at: handoff.created_at,
    closed: true,
    handoff,
    id: conversationItemId("handoff", handoff.id),
    kind: "handoff",
    unread: isUnread(handoff.created_at, unreadFrom),
  };
}

/** One labelled row of an expanded handoff: a top-level field of its body, or one entry of its
 *  `proof` list. */
export interface HandoffField {
  /** The top-level field the row comes from: its own name, or `proof` for a proof entry. */
  readonly field: string;
  /** The field's name, or `proof <n>` for the nth proof entry, counted from 1. */
  readonly label: string;
  readonly text: string;
  /** `text` is indented JSON (an array or an object) rather than a plain value. */
  readonly json: boolean;
}

/** A value as a row shows it: a string as itself, an array or an object as indented JSON, and a
 *  number, a boolean or null as its JSON text. */
function handoffValue(value: unknown): Pick<HandoffField, "json" | "text"> {
  if (typeof value === "string") {
    return { json: false, text: value };
  }
  if (value !== null && typeof value === "object") {
    return { json: true, text: JSON.stringify(value, null, 2) };
  }
  return { json: false, text: JSON.stringify(value) };
}

/** The rows a handoff's body expands to, in the order its session wrote the fields, with a
 *  `proof` list split into one row per entry. Undefined when the body is not a JSON object,
 *  which the card then shows as the raw text it is. */
export function handoffFields(body: string): HandoffField[] | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    return undefined;
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return undefined;
  }
  return Object.entries(parsed).flatMap(([field, value]): HandoffField[] =>
    field === "proof" && Array.isArray(value)
      ? value.map((entry, index) => ({
          field,
          label: `proof ${index + 1}`,
          ...handoffValue(entry),
        }))
      : [{ field, label: field, ...handoffValue(value) }]
  );
}

/**
 * The issue's conversations, newest activity first. Each of the four reads returns the issue's
 * whole set, so the list is complete however old a conversation is; ties break on the card's own
 * id, so the order is total and nothing shuffles between renders.
 */
export function buildConversations({
  asks,
  comments,
  messages,
  handoffs,
  unreadFrom,
}: ConversationInput): Conversation[] {
  const conversations = [
    ...asks.map((ask) => askConversation(ask, unreadFrom)),
    ...commentConversations(comments, unreadFrom),
    ...messages.map((read) => messageConversation(read, unreadFrom)),
    ...handoffs.map((handoff) => handoffConversation(handoff, unreadFrom)),
  ];
  return conversations.sort(
    (left, right) => compareTimestamps(right.at, left.at) || left.id.localeCompare(right.id)
  );
}

/** Whether `conversation` belongs in the bucket `filter` names: an ask is closed once it is
 *  answered, resolved or retracted, a comment thread once it is resolved or its suggestion is
 *  decided, a handoff always is, and a message thread never is. A predicate rather than a list
 *  pass, so a caller that also keeps conversations of its own (the Conversation's threads held
 *  in hand) walks the list once and keeps its order. */
export function matchesFilter(conversation: Conversation, filter: ConversationFilter): boolean {
  return filter === "all" || conversation.closed === (filter === "closed");
}

/** Where a deep link lands: the conversation that holds the item, and the reply inside it when
 *  the link names a reply rather than the root. */
export interface ConversationFocus {
  readonly id: string;
  readonly replyId?: string;
}

/**
 * The conversation an item id belongs to. A reply under an ask resolves to that ask, a reply in
 * a thread to its root, and a handoff id to that version's card, read off the issue's own lists -
 * each is its whole set, so no per-item fetch can tell the tab anything these do not.
 */
export function focusConversation(
  itemId: string | undefined,
  { asks, comments, handoffs, messages }: Omit<ConversationInput, "unreadFrom">
): ConversationFocus | undefined {
  if (itemId === undefined) {
    return undefined;
  }
  if (asks.some((ask) => ask.id === itemId)) {
    return { id: conversationItemId("ask", itemId) };
  }
  const comment = comments.find((row) => row.id === itemId);
  if (comment !== undefined) {
    if (comment.ask_id !== null) {
      return { id: conversationItemId("ask", comment.ask_id), replyId: comment.id };
    }
    const root = rootOf(new Map(comments.map((row) => [row.id, row])), comment);
    return root.id === comment.id
      ? { id: conversationItemId("comment", root.id) }
      : { id: conversationItemId("comment", root.id), replyId: comment.id };
  }
  for (const { message, replies } of messages) {
    if (message.id === itemId) {
      return { id: conversationItemId("message", message.id) };
    }
    if (replies.some((reply) => reply.id === itemId)) {
      return { id: conversationItemId("message", message.id), replyId: itemId };
    }
  }
  if (handoffs.some((handoff) => handoff.id === itemId)) {
    return { id: conversationItemId("handoff", itemId) };
  }
  return undefined;
}

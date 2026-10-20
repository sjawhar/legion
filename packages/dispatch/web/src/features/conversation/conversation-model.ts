import type { Actor, Ask, CommentDelivery, CommentMention, Event } from "../../api/types";
import { activityDescription, dateKey, dayLabel, sameActor } from "../activity/activity-model";
import { receiptAnsweredWithError, rowAnsweredWithError } from "./delivery";

const GROUP_WINDOW_MS = 5 * 60 * 1000;

export type MessageEvent = Extract<Event, { type: "message.created" | "message.answered" }>;
export type TargetedMessageEvent = Extract<Event, { type: "message.created" }>;
export type MessageDeliveryEvent = Extract<Event, { type: "message.delivery" }>;
export type MessageAnsweredEvent = Extract<Event, { type: "message.answered" }>;
export type CommentEvent = Extract<
  Event,
  {
    type:
      | "comment.created"
      | "comment.anchor_refreshed"
      | "comment.answered"
      | "comment.resolved"
      | "comment.reopened"
      | "comment.edited"
      | "suggestion.accepted"
      | "suggestion.rejected";
  }
>;
export type CommentDeliveryEvent = Extract<Event, { type: "comment.delivery" }>;
export type AskEvent = Extract<
  Event,
  {
    type:
      | "ask.opened"
      | "ask.anchor_refreshed"
      | "ask.edited"
      | "ask.handed_back"
      | "ask.answered"
      | "ask.resolved";
  }
>;

interface Turn {
  id: string;
  at: string;
  seq: number;
  lastSeq: number;
  pinEventId: number;
  author: Actor;
}

/** A reply in a message thread: a message whose parent is loaded, shown nested under the
 *  thread's root in the order it was sent. A reply a human sent into a targeted thread was
 *  delivered too, so it carries its own attempts. */
export interface ThreadReply {
  id: string;
  event: MessageEvent;
  author: Actor;
  at: string;
  seq: number;
  deliveries: MessageDeliveryEvent[];
}
/** Stored initial delivery rows include `pending`; later delivery events replace their row.
 *  `answeredWithError` is read off whichever of the two it came from (`DeliveryOutcome`). */
export type CommentDeliveryAttempt = CommentDelivery & { readonly answeredWithError: boolean };

/** A message turn owns its thread: the replies beneath it and when the thread last moved. The
 *  turn sits in the conversation at `lastSeq`, so a reply on an old thread brings the thread to
 *  the top the way a new message would. */
interface Thread {
  replies: ThreadReply[];
  lastAt: string;
}

/** A reply nested below a comment root, including its independent mention deliveries. */
export interface CommentReply {
  at: string;
  author: Actor;
  deliveries: CommentDeliveryAttempt[];
  event: CommentEvent;
  id: string;
  seq: number;
}

interface CommentThread {
  replies: CommentReply[];
  lastAt: string;
}

export type ConversationItem =
  | (Turn & Thread & { kind: "message"; event: MessageEvent; continued: boolean })
  | (Turn &
      Thread & {
        kind: "targeted-message";
        event: TargetedMessageEvent;
        deliveries: MessageDeliveryEvent[];
        /** The first reply a session sent: the card reads "Answered by" that session. */
        answer?: ThreadReply;
      })
  | (Turn &
      CommentThread & {
        kind: "comment";
        event: CommentEvent;
        deliveries: CommentDeliveryAttempt[];
        continued: boolean;
      })
  | (Turn & { kind: "ask"; ask: Ask })
  | (Turn & { kind: "activity"; event: Event; description: string })
  | { kind: "day-divider"; id: string; date: string; label: string }
  | { kind: "unread-divider"; id: "unread-divider" };

export interface ConversationInput {
  /** Live session titles from the agent registry, so an activity line names a session the
   *  same way every other surface does. */
  readonly titles?: ReadonlyMap<string, string>;
  events: Event[];
  lastReadSeq: number;
  /** Local YYYY-MM-DD. */
  today: string;
}

function isAskEvent(event: Event): event is AskEvent {
  return (
    event.type === "ask.opened" ||
    event.type === "ask.anchor_refreshed" ||
    event.type === "ask.edited" ||
    event.type === "ask.handed_back" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved"
  );
}

function isConversationComment(event: Event): event is CommentEvent {
  return (
    (event.type === "comment.created" ||
      event.type === "comment.anchor_refreshed" ||
      event.type === "comment.answered" ||
      event.type === "comment.resolved" ||
      event.type === "comment.reopened" ||
      event.type === "comment.edited" ||
      event.type === "suggestion.accepted" ||
      event.type === "suggestion.rejected") &&
    event.payload.ask_id === null
  );
}

// A comment event recorded before 2026-09-18 carries no `mentions` or `deliveries`
// (`CommentEventPayload` in @legion/contracts); read either through these, never directly.
export function commentMentions(payload: CommentEvent["payload"]): readonly CommentMention[] {
  return payload.mentions ?? [];
}

export function commentDeliveries(payload: CommentEvent["payload"]): CommentDeliveryAttempt[] {
  return (payload.deliveries ?? []).map((row) => ({
    ...row,
    answeredWithError: rowAnsweredWithError(row),
  }));
}

/** A retracted ask was withdrawn by its asker; it is history, not a decision to read, so the
 *  conversation hides it with the rest of the activity unless the reader asks to see it. */
export function isRetractedAsk(ask: Pick<Ask, "state" | "resolution">): boolean {
  return ask.state === "resolved" && ask.resolution?.kind === "retracted";
}

type MessageThreadRoot = Extract<ConversationItem, { kind: "message" | "targeted-message" }>;

/** Where a turn sits in the conversation: a thread at its latest activity, anything else where
 *  it happened. */
function orderSeq(turn: ConversationItem & { seq: number }): number {
  return turn.kind === "message" || turn.kind === "targeted-message" || turn.kind === "comment"
    ? turn.lastSeq
    : turn.seq;
}

export function buildConversationItems({
  events,
  lastReadSeq,
  today,
  titles,
}: ConversationInput): ConversationItem[] {
  const ordered = [...events].sort((left, right) => left.seq - right.seq);
  type AskItem = Extract<ConversationItem, { kind: "ask" }>;
  type CommentRoot = Extract<ConversationItem, { kind: "comment" }>;
  const askItems = new Map<string, AskItem>();
  const messageThreadOf = new Map<string, MessageThreadRoot>();
  const messageNodeOf = new Map<
    string,
    Extract<MessageThreadRoot, { kind: "targeted-message" }> | ThreadReply
  >();
  const commentThreadOf = new Map<string, CommentRoot>();
  const commentNodeOf = new Map<string, CommentRoot | CommentReply>();
  const turns: Exclude<ConversationItem, { kind: "day-divider" | "unread-divider" }>[] = [];
  let previousIssueStatus: string | undefined;

  for (const event of ordered) {
    if (isAskEvent(event)) {
      const askId = event.payload.id;
      const existing = askItems.get(askId);
      if (existing === undefined) {
        const item: AskItem = {
          kind: "ask",
          id: `ask:${askId}`,
          ask: event.payload,
          author: event.payload.author,
          at: event.created_at,
          seq: event.seq,
          lastSeq: event.seq,
          pinEventId: event.payload.opened_event_id,
        };
        askItems.set(askId, item);
        turns.push(item);
      } else {
        existing.ask = event.payload;
        existing.lastSeq = event.seq;
      }
      // A rewording or a hand-back is also an activity line; other ask events are the card alone.
      if (event.type !== "ask.edited" && event.type !== "ask.handed_back") continue;
    }

    if (isConversationComment(event)) {
      const existing = commentNodeOf.get(event.payload.id);
      const existingRoot = commentThreadOf.get(event.payload.id);
      if (existing !== undefined && existingRoot !== undefined) {
        existing.event = event;
        existing.author = event.payload.author;
        existing.at = event.payload.created_at;
        if (event.payload.deliveries !== undefined) {
          existing.deliveries = commentDeliveries(event.payload);
        }
        existingRoot.lastAt = event.created_at;
        existingRoot.lastSeq = event.seq;
        continue;
      }

      const parentId = event.payload.reply_to;
      const root =
        parentId === null || parentId === undefined ? undefined : commentThreadOf.get(parentId);
      if (root !== undefined) {
        const reply: CommentReply = {
          at: event.payload.created_at,
          author: event.payload.author,
          deliveries: commentDeliveries(event.payload),
          event,
          id: `comment:${event.payload.id}`,
          seq: event.seq,
        };
        root.replies.push(reply);
        root.lastAt = event.created_at;
        root.lastSeq = event.seq;
        commentThreadOf.set(event.payload.id, root);
        commentNodeOf.set(event.payload.id, reply);
        continue;
      }
      const rootComment: CommentRoot = {
        at: event.payload.created_at,
        author: event.payload.author,
        continued: false,
        deliveries: commentDeliveries(event.payload),
        event,
        id: `comment:${event.payload.id}`,
        kind: "comment",
        lastAt: event.created_at,
        lastSeq: event.seq,
        pinEventId: event.id,
        replies: [],
        seq: event.seq,
      };
      commentThreadOf.set(event.payload.id, rootComment);
      commentNodeOf.set(event.payload.id, rootComment);
      turns.push(rootComment);
      continue;
    }
    if (event.type === "comment.delivery") {
      const node = commentNodeOf.get(event.payload.comment_id);
      const root = commentThreadOf.get(event.payload.comment_id);
      if (node !== undefined && root !== undefined) {
        const attempt: CommentDeliveryAttempt = {
          answeredWithError: receiptAnsweredWithError(event),
          attempt: event.payload.attempt,
          comment_id: event.payload.comment_id,
          created_at: event.created_at,
          delivery: event.payload.delivery,
          duplicate: event.payload.duplicate,
          envelope_id: null,
          error: event.payload.error ?? null,
          reply_id: event.payload.reply_id,
          resolve_error: null,
          session_id: event.payload.session_id,
          state: event.payload.state,
          target: event.payload.target,
        };
        const prior = node.deliveries.findIndex(
          (candidate) =>
            candidate.target === attempt.target && candidate.attempt === attempt.attempt
        );
        if (prior < 0) {
          node.deliveries.push(attempt);
        } else {
          node.deliveries.splice(prior, 1, attempt);
        }
        root.lastAt = event.created_at;
        root.lastSeq = event.seq;
        continue;
      }
    }

    if (event.type === "message.created" || event.type === "message.answered") {
      const parentId = event.payload.in_reply_to;
      const root =
        parentId === null || parentId === undefined ? undefined : messageThreadOf.get(parentId);
      if (root !== undefined) {
        const reply: ThreadReply = {
          id: `message:${event.payload.id}`,
          event,
          author: event.actor,
          at: event.created_at,
          seq: event.seq,
          deliveries: [],
        };
        root.replies.push(reply);
        root.lastSeq = event.seq;
        root.lastAt = event.created_at;
        if (
          root.kind === "targeted-message" &&
          root.answer === undefined &&
          event.actor.kind === "session"
        ) {
          root.answer = reply;
        }
        messageThreadOf.set(event.payload.id, root);
        messageNodeOf.set(event.payload.id, reply);
        continue;
      }
      const thread = { replies: [], lastAt: event.created_at };
      const base = {
        ...thread,
        author: event.actor,
        at: event.created_at,
        seq: event.seq,
        lastSeq: event.seq,
      };
      const item: MessageThreadRoot =
        event.type === "message.created" && event.payload.target !== null
          ? {
              ...base,
              kind: "targeted-message",
              id: `message:${event.payload.id}`,
              event,
              deliveries: [],
              pinEventId: event.id,
            }
          : {
              ...base,
              kind: "message",
              id: `message:${event.id}`,
              event,
              continued: false,
              pinEventId: event.id,
            };
      messageThreadOf.set(event.payload.id, item);
      if (item.kind === "targeted-message") messageNodeOf.set(event.payload.id, item);
      turns.push(item);
      continue;
    }
    if (event.type === "message.delivery") {
      const node = messageNodeOf.get(event.payload.message_id);
      const root = messageThreadOf.get(event.payload.message_id);
      if (node !== undefined && root !== undefined) {
        node.deliveries.push(event);
        root.lastSeq = event.seq;
        root.lastAt = event.created_at;
        continue;
      }
    }

    const base = {
      at: event.created_at,
      seq: event.seq,
      lastSeq: event.seq,
      pinEventId: event.id,
      author: event.actor,
    };
    turns.push({
      ...base,
      kind: "activity",
      id: `activity:${event.id}`,
      event,
      description: activityDescription(event, previousIssueStatus, titles),
    });
    if (
      event.type === "issue.created" ||
      event.type === "issue.updated" ||
      event.type === "issue.closed"
    ) {
      previousIssueStatus = event.payload.status;
    }
  }

  turns.sort((left, right) => orderSeq(left) - orderSeq(right));
  const anyRead = turns.some((turn) => orderSeq(turn) <= lastReadSeq);
  const anyUnread = turns.some((turn) => orderSeq(turn) > lastReadSeq);
  const items: ConversationItem[] = [];
  let currentDay: string | undefined;
  let unreadPlaced = !(anyRead && anyUnread);
  let previous: { author: Actor; atMs: number } | undefined;

  for (const turn of [...turns].reverse()) {
    const day = dateKey(
      turn.kind === "message" || turn.kind === "targeted-message" || turn.kind === "comment"
        ? turn.lastAt
        : turn.at
    );
    if (day !== currentDay) {
      currentDay = day;
      items.push({
        kind: "day-divider",
        id: `day:${day}`,
        date: day,
        label: dayLabel(day, today),
      });
      previous = undefined;
    }
    if (!unreadPlaced && orderSeq(turn) <= lastReadSeq) {
      items.push({ kind: "unread-divider", id: "unread-divider" });
      unreadPlaced = true;
      previous = undefined;
    }
    if (
      (turn.kind === "message" && turn.replies.length === 0) ||
      (turn.kind === "comment" && turn.replies.length === 0)
    ) {
      const atMs = new Date(turn.at).getTime();
      turn.continued =
        previous !== undefined &&
        sameActor(previous.author, turn.author) &&
        previous.atMs - atMs < GROUP_WINDOW_MS;
      previous = { author: turn.author, atMs };
    } else if (turn.kind !== "activity") {
      previous = undefined;
    }
    items.push(turn);
  }

  return items;
}

export interface ConversationVisibility {
  showActivity: boolean;
  /** Retracted asks are withdrawn history and stay hidden until asked for. */
  showRetracted: boolean;
}

function isHiddenItem(item: ConversationItem | undefined, show: ConversationVisibility): boolean {
  if (item === undefined) return false;
  if (item.kind === "activity") return !show.showActivity;
  if (item.kind === "ask") return !show.showRetracted && isRetractedAsk(item.ask);
  return false;
}

export function countRetractedAsks(items: ConversationItem[]): number {
  return items.filter((item) => item.kind === "ask" && isRetractedAsk(item.ask)).length;
}

export function visibleConversationItems(
  items: ConversationItem[],
  show: ConversationVisibility
): ConversationItem[] {
  if (show.showActivity && show.showRetracted) return items;

  const kept: ConversationItem[] = [];
  for (let index = 0; index < items.length; index += 1) {
    const item = items[index] as ConversationItem;
    if (isHiddenItem(item, show)) continue;
    if (item.kind === "day-divider") {
      let next = index + 1;
      while (next < items.length && isHiddenItem(items[next], show)) next += 1;
      if (next >= items.length || items[next]?.kind === "day-divider") continue;
    }
    kept.push(item);
  }
  return kept;
}

import type { Actor, Ask, Event } from "../../api/types";
import { describeAskResolution } from "../refs/actor";

export const GROUP_WINDOW_MS = 5 * 60 * 1000;

export type MessageEvent = Extract<Event, { type: "message.created" | "message.answered" }>;
export type TargetedMessageEvent = Extract<Event, { type: "message.created" }>;
export type MessageDeliveryEvent = Extract<Event, { type: "message.delivery" }>;
export type MessageAnsweredEvent = Extract<Event, { type: "message.answered" }>;
export type CommentEvent = Extract<Event, { type: "comment.created" }>;
export type AskEvent = Extract<
  Event,
  { type: "ask.opened" | "ask.edited" | "ask.answered" | "ask.resolved" }
>;

interface Turn {
  id: string;
  at: string;
  seq: number;
  lastSeq: number;
  pinEventId: number;
  author: Actor;
}

export type ConversationItem =
  | (Turn & { kind: "message"; event: MessageEvent; continued: boolean })
  | (Turn & {
      kind: "targeted-message";
      event: TargetedMessageEvent;
      deliveries: MessageDeliveryEvent[];
      answer?: MessageAnsweredEvent;
    })
  | (Turn & { kind: "comment"; event: CommentEvent; continued: boolean })
  | (Turn & { kind: "ask"; ask: Ask })
  | (Turn & { kind: "activity"; event: Event; description: string })
  | { kind: "day-divider"; id: string; date: string; label: string }
  | { kind: "unread-divider"; id: "unread-divider" };

export interface ConversationInput {
  events: Event[];
  lastReadSeq: number;
  /** Local YYYY-MM-DD. */
  today: string;
}

export function shortSessionId(id: string): string {
  return `session:${id.slice(0, 8)}…`;
}

export function dateKey(iso: string): string {
  const date = new Date(iso);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

function localDate(key: string): Date {
  const [year, month, day] = key.split("-").map(Number);
  return new Date(year ?? 1970, (month ?? 1) - 1, day ?? 1);
}

export function dayLabel(date: string, today: string): string {
  if (date === today) return "Today";

  const yesterday = localDate(today);
  yesterday.setDate(yesterday.getDate() - 1);
  if (date === dateKey(yesterday.toISOString())) return "Yesterday";

  const sameYear = date.slice(0, 4) === today.slice(0, 4);
  return new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "long",
    weekday: "long",
    ...(sameYear ? {} : { year: "numeric" }),
  }).format(localDate(date));
}

function isAskEvent(event: Event): event is AskEvent {
  return (
    event.type === "ask.opened" ||
    event.type === "ask.edited" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved"
  );
}

function isConversationComment(event: Event): event is CommentEvent {
  return (
    event.type === "comment.created" &&
    event.payload.anchor === null &&
    event.payload.ask_id === null &&
    event.payload.reply_to === null
  );
}

export function activityDescription(event: Event): string {
  switch (event.type) {
    case "issue.created":
      return "created the issue";
    case "issue.updated":
      return "updated the issue";
    case "issue.closed":
      return "closed the issue";
    case "artifact.created":
      return `added ${event.payload.artifact.name}`;
    case "artifact.version":
      return `saved ${event.payload.name} v${event.payload.version.number}`;
    case "comment.created":
      if (event.payload.ask_id !== null) {
        return `replied to “${event.payload.ask_question ?? "an ask"}”`;
      }
      if (event.payload.reply_to !== null) return "replied to a comment";
      return `commented on ${event.payload.artifact_name}: “${event.payload.anchor?.quote ?? ""}”`;
    case "comment.resolved":
      return `resolved a comment on ${event.payload.artifact_name}`;
    case "comment.reopened":
      return `reopened a comment on ${event.payload.artifact_name}`;
    case "artifact.approved":
      return `approved ${event.payload.name} v${event.payload.version}`;
    case "artifact.changes_requested":
      return `requested changes on ${event.payload.name} v${event.payload.version}`;
    case "comment.edited":
      return `edited a comment on ${event.payload.artifact_name}`;
    case "suggestion.accepted":
      return `accepted a suggestion on ${event.payload.artifact_name}`;
    case "suggestion.rejected":
      return `rejected a suggestion on ${event.payload.artifact_name}`;
    case "ask.resolved":
      return describeAskResolution(event.payload.resolution);
    case "ask.opened":
      return `asked “${event.payload.question}”`;
    case "ask.edited":
      return `edited the question "${event.payload.question}"`;
    case "ask.answered":
      return `answered “${event.payload.question}”`;
    case "message.created":
      return "sent a message";
    case "message.delivery":
      return "delivered a message";
    case "message.answered":
      return "answered a message";
    case "child.status":
      return `moved ${event.payload.child_key} from ${event.payload.from} to ${event.payload.to}`;
    case "subscription.removed":
      return `unsubscribed ${shortSessionId(event.payload.session_id)} from notifications`;
    case "block.repaired":
      return `repaired server-owned state on block ${event.payload.block_id}`;
  }
}

export function buildConversationItems({
  events,
  lastReadSeq,
  today,
}: ConversationInput): ConversationItem[] {
  const ordered = [...events].sort((left, right) => left.seq - right.seq);
  type AskItem = Extract<ConversationItem, { kind: "ask" }>;
  type TargetedMessageItem = Extract<ConversationItem, { kind: "targeted-message" }>;
  const askItems = new Map<string, AskItem>();
  const targetedMessages = new Map<string, TargetedMessageItem>();
  const turns: Exclude<ConversationItem, { kind: "day-divider" | "unread-divider" }>[] = [];

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
      if (event.type !== "ask.edited") continue;
    }

    if (event.type === "message.created" && event.payload.target !== null) {
      const item: TargetedMessageItem = {
        kind: "targeted-message",
        id: `message:${event.payload.id}`,
        event,
        deliveries: [],
        author: event.actor,
        at: event.created_at,
        seq: event.seq,
        lastSeq: event.seq,
        pinEventId: event.id,
      };
      targetedMessages.set(event.payload.id, item);
      turns.push(item);
      continue;
    }
    if (event.type === "message.delivery") {
      const item = targetedMessages.get(event.payload.message_id);
      if (item !== undefined) {
        item.deliveries.push(event);
        item.lastSeq = event.seq;
        continue;
      }
    }
    if (event.type === "message.answered" && event.payload.in_reply_to !== null) {
      const item = targetedMessages.get(event.payload.in_reply_to);
      if (item !== undefined) {
        item.answer = event;
        item.lastSeq = event.seq;
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
    if (event.type === "message.created" || event.type === "message.answered") {
      turns.push({
        ...base,
        kind: "message",
        id: `message:${event.id}`,
        event,
        continued: false,
      });
    } else if (isConversationComment(event)) {
      turns.push({
        ...base,
        kind: "comment",
        id: `comment:${event.id}`,
        event,
        continued: false,
      });
    } else {
      turns.push({
        ...base,
        kind: "activity",
        id: `activity:${event.id}`,
        event,
        description: activityDescription(event),
      });
    }
  }

  const anyRead = turns.some((turn) => turn.seq <= lastReadSeq);
  const anyUnread = turns.some((turn) => turn.seq > lastReadSeq);
  const items: ConversationItem[] = [];
  let currentDay: string | undefined;
  let unreadPlaced = !(anyRead && anyUnread);
  let previous: { author: Actor; atMs: number } | undefined;

  for (const turn of [...turns].reverse()) {
    const day = dateKey(turn.at);
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
    if (!unreadPlaced && turn.seq <= lastReadSeq) {
      items.push({ kind: "unread-divider", id: "unread-divider" });
      unreadPlaced = true;
      previous = undefined;
    }
    if (turn.kind === "message" || turn.kind === "comment") {
      const atMs = new Date(turn.at).getTime();
      turn.continued =
        previous !== undefined &&
        previous.author.kind === turn.author.kind &&
        previous.author.id === turn.author.id &&
        previous.atMs - atMs < GROUP_WINDOW_MS;
      previous = { author: turn.author, atMs };
    } else if (turn.kind !== "activity") {
      previous = undefined;
    }
    items.push(turn);
  }

  return items;
}

export function visibleConversationItems(
  items: ConversationItem[],
  showActivity: boolean
): ConversationItem[] {
  if (showActivity) return items;

  const kept: ConversationItem[] = [];
  for (let index = 0; index < items.length; index += 1) {
    const item = items[index] as ConversationItem;
    if (item.kind === "activity") continue;
    if (item.kind === "day-divider") {
      let next = index + 1;
      while (next < items.length && items[next]?.kind === "activity") next += 1;
      if (next >= items.length || items[next]?.kind === "day-divider") continue;
    }
    kept.push(item);
  }
  return kept;
}

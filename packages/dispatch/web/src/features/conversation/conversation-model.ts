import type { Actor, Ask, CommentDelivery, Event } from "../../api/types";
import { describeAskResolution, shortSessionId } from "../refs/actor";

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
  { type: "ask.opened" | "ask.anchor_refreshed" | "ask.edited" | "ask.answered" | "ask.resolved" }
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
/** Stored initial delivery rows include `pending`; later delivery events replace their row. */
export type CommentDeliveryAttempt = CommentDelivery;

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
  events: Event[];
  lastReadSeq: number;
  /** Local YYYY-MM-DD. */
  today: string;
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
    event.type === "ask.anchor_refreshed" ||
    event.type === "ask.edited" ||
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

export function activityDescription(event: Event, previousStatus?: string): string {
  switch (event.type) {
    case "project.created":
      return `created project ${event.payload.key}`;
    case "project.updated":
      return `updated project ${event.payload.key}`;
    case "settings.repo_project.updated":
      return `${event.payload.deleted ? "removed" : "updated"} repository mapping ${event.payload.mapping.repo}`;
    case "settings.architecture_source.updated":
      return `${event.payload.deleted ? "removed" : "updated"} architecture source ${event.payload.source.repo}`;
    case "architecture.synced":
      return `synced the architecture model at ${event.payload.commit.slice(0, 12)}`;
    case "architecture.sync_failed":
      return `failed to sync the architecture model: ${event.payload.error}`;
    case "user_state.updated":
      return "updated user state";
    case "issue.created":
      return "created the issue";
    case "issue.updated":
      return previousStatus === "done" && event.payload.status !== "done"
        ? `reopened the issue into ${event.payload.status}`
        : "updated the issue";
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
    case "comment.answered":
      return "answered a comment";
    case "comment.delivery":
      return `delivered a comment mention to ${event.payload.target}`;
    case "comment.resolved":
      return `resolved a comment on ${event.payload.artifact_name}`;
    case "comment.anchor_refreshed":
      return event.payload.anchor?.orphaned
        ? `made a comment on ${event.payload.artifact_name} lose its quote`
        : `re-anchored a comment on ${event.payload.artifact_name} after an edit`;
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
    case "ask.anchor_refreshed":
      return event.payload.anchor?.orphaned
        ? `made an ask lose its quote: “${event.payload.question}”`
        : `re-anchored an ask after an edit: “${event.payload.question}”`;
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
    case "child.added":
      return `added child ${event.payload.child_key}`;
    case "child.removed":
      return `removed child ${event.payload.child_key}`;
    case "subscription.remove_requested":
      return `requested unsubscribe for ${shortSessionId(event.payload.session_id)}`;
    case "subscription.removed":
      return `unsubscribed ${shortSessionId(event.payload.session_id)} from notifications`;
    case "ask.follower_added":
      return event.actor.kind === "session" && event.actor.id === event.payload.session_id
        ? "followed an ask"
        : `added ${shortSessionId(event.payload.session_id)} to an ask's followers`;
    case "ask.follower_removed":
      return event.actor.kind === "session" && event.actor.id === event.payload.session_id
        ? "unfollowed an ask"
        : `removed ${shortSessionId(event.payload.session_id)} from an ask's followers`;
    case "block.repaired":
      return `repaired server-owned state on block ${event.payload.block_id}`;
    case "block.invalid":
      return `marked decision ${event.payload.block_id} malformed: ${event.payload.reason}`;
  }
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
      if (event.type !== "ask.edited") continue;
    }

    if (isConversationComment(event)) {
      const existing = commentNodeOf.get(event.payload.id);
      const existingRoot = commentThreadOf.get(event.payload.id);
      if (existing !== undefined && existingRoot !== undefined) {
        existing.event = event;
        existing.author = event.payload.author;
        existing.at = event.payload.created_at;
        existing.deliveries = [...event.payload.deliveries];
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
          deliveries: [...event.payload.deliveries],
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
        deliveries: [...event.payload.deliveries],
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
          attempt: event.payload.attempt,
          comment_id: event.payload.comment_id,
          created_at: event.created_at,
          delivery: event.payload.delivery,
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
      description: activityDescription(event, previousIssueStatus),
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

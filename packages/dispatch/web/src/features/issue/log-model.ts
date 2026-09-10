import type { Event } from "../../api/types";
import { describeAskResolution } from "../refs/actor";

type AskEvent = Extract<Event, { type: "ask.opened" | "ask.answered" | "ask.resolved" }>;

export type LogItem =
  | { kind: "ask"; event: AskEvent; folded: false }
  | { kind: "event"; event: Event; folded: boolean }
  | { kind: "new-divider" };

const pinnedItemPrefix = "pinned_items:";

function isAskEvent(event: Event): event is AskEvent {
  return (
    event.type === "ask.opened" || event.type === "ask.answered" || event.type === "ask.resolved"
  );
}

export function eventItemId(event: Event): string {
  return `event:${event.id}`;
}

export function isPinnedEvent(dismissed: string[], event: Event): boolean {
  return dismissed.includes(`${pinnedItemPrefix}${eventItemId(event)}`);
}

export function pinnedEventIds(dismissed: string[]): string[] {
  const prefix = `${pinnedItemPrefix}event:`;
  return dismissed
    .filter((item) => item.startsWith(prefix))
    .map((item) => item.slice(prefix.length));
}

export function dismissedEventIds(dismissed: string[]): string[] {
  const prefix = "event:";
  return dismissed
    .filter((item) => item.startsWith(prefix))
    .map((item) => item.slice(prefix.length));
}

export function eventDescription(event: Event): string {
  if (event.type === "ask.resolved") {
    return describeAskResolution(event.payload.resolution);
  }
  if (event.type === "ask.answered") {
    const question = event.payload.question;
    return `Ask answered: ${typeof question === "string" ? question : "ask"}`;
  }
  if (event.type === "comment.resolved") {
    return "Comment resolved";
  }
  if (event.type === "message.created") {
    const body = event.payload.body;
    return typeof body === "string" ? body : "Message created";
  }
  if (event.type === "ask.opened") {
    const question = event.payload.question;
    return `Ask opened: ${typeof question === "string" ? question : "ask"}`;
  }
  if (event.type === "comment.created") {
    const body = event.payload.body;
    return typeof body === "string" ? body : "Comment created";
  }
  if (event.type === "issue.created") {
    return "Issue created";
  }
  if (event.type === "issue.updated") {
    return "Issue updated";
  }
  if (event.type === "issue.closed") {
    return "Issue closed";
  }
  if (event.type === "artifact.created") {
    return "Artifact created";
  }
  if (event.type === "artifact.version") {
    return "Artifact version saved";
  }
  if (event.type === "suggestion.accepted") {
    return "Suggestion accepted";
  }
  if (event.type === "suggestion.rejected") {
    return "Suggestion rejected";
  }
  return "Child status changed";
}

export function buildLogItems(
  events: Event[],
  dismissed: string[],
  lastReadSeq: number
): LogItem[] {
  const hidden = new Set(dismissed.filter((item) => item.startsWith("event:")));
  const askEvents = new Map<string, AskEvent[]>();
  for (const event of events) {
    if (isAskEvent(event)) {
      const group = askEvents.get(event.payload.id) ?? [];
      group.push(event);
      askEvents.set(event.payload.id, group);
    }
  }
  const items: LogItem[] = [];
  const seenAsks = new Set<string>();
  let dividerAdded = false;
  let unreadAbove = false;

  for (const event of [...events].sort((left, right) => right.seq - left.seq)) {
    const askEvent = isAskEvent(event);
    if (askEvent) {
      if (seenAsks.has(event.payload.id)) {
        continue;
      }
      seenAsks.add(event.payload.id);
      if (askEvents.get(event.payload.id)?.some((item) => hidden.has(eventItemId(item)))) {
        continue;
      }
    } else if (hidden.has(eventItemId(event))) {
      continue;
    }
    if (!dividerAdded && unreadAbove && event.seq <= lastReadSeq) {
      items.push({ kind: "new-divider" });
      dividerAdded = true;
    }
    if (event.seq > lastReadSeq) {
      unreadAbove = true;
    }
    if (askEvent) {
      items.push({ event, folded: false, kind: "ask" });
      continue;
    }
    items.push({
      event,
      folded:
        event.type === "comment.resolved" ||
        event.type === "suggestion.accepted" ||
        event.type === "suggestion.rejected",
      kind: "event",
    });
  }

  return items;
}

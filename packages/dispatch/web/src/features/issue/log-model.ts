import type { Event } from "../../api/types";
import { describeAskResolution } from "../refs/actor";

export type LogItem = { kind: "event"; event: Event; folded: boolean } | { kind: "new-divider" };

const pinnedItemPrefix = "pinned_items:";

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
  const items: LogItem[] = [];
  let dividerAdded = false;
  let unreadAbove = false;

  for (const event of [...events].sort((left, right) => right.seq - left.seq)) {
    if (hidden.has(eventItemId(event))) {
      continue;
    }
    if (!dividerAdded && unreadAbove && event.seq <= lastReadSeq) {
      items.push({ kind: "new-divider" });
      dividerAdded = true;
    }
    if (event.seq > lastReadSeq) {
      unreadAbove = true;
    }
    items.push({
      event,
      folded:
        event.type === "ask.answered" ||
        event.type === "ask.resolved" ||
        event.type === "comment.resolved" ||
        event.type === "suggestion.accepted" ||
        event.type === "suggestion.rejected",
      kind: "event",
    });
  }

  return items;
}

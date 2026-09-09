import type { Event } from "../../api/types";

export type LogItem = { kind: "event"; event: Event; folded: boolean } | { kind: "new-divider" };

const pinnedItemPrefix = "pinned_items:";

export function eventItemId(event: Event): string {
  return `event:${event.id}`;
}

export function isPinnedEvent(dismissed: string[], event: Event): boolean {
  return dismissed.includes(`${pinnedItemPrefix}${eventItemId(event)}`);
}

export function pinnedEventIds(dismissed: string[]): string[] {
  return dismissed
    .filter((item) => item.startsWith(pinnedItemPrefix))
    .map((item) => item.slice(pinnedItemPrefix.length));
}

export function setEventPinned(dismissed: string[], event: Event, pinned: boolean): string[] {
  const marker = `${pinnedItemPrefix}${eventItemId(event)}`;
  if (pinned) {
    return dismissed.includes(marker) ? dismissed : [...dismissed, marker];
  }
  return dismissed.filter((item) => item !== marker);
}

export function dismissEvent(dismissed: string[], event: Event): string[] {
  const item = eventItemId(event);
  return dismissed.includes(item) ? dismissed : [...dismissed, item];
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
        event.type === "comment.resolved" ||
        event.type === "suggestion.accepted" ||
        event.type === "suggestion.rejected",
      kind: "event",
    });
  }

  return items;
}

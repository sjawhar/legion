import type { Event } from "../../api/types";

const pinnedItemPrefix = "pinned_items:";

export function eventItemId(event: Pick<Event, "id">): string {
  return `event:${event.id}`;
}

export function isPinnedEvent(dismissed: string[], eventId: number): boolean {
  return dismissed.includes(`${pinnedItemPrefix}${eventItemId({ id: eventId })}`);
}

export function pinnedEventIds(dismissed: string[]): string[] {
  const prefix = `${pinnedItemPrefix}event:`;
  return dismissed
    .filter((item) => item.startsWith(prefix))
    .map((item) => item.slice(prefix.length));
}

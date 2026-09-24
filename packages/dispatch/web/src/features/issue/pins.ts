import type { Event, UserIssueState, UserState } from "../../api/types";

const pinnedItemPrefix = "pinned_items:";

export function eventItemId(event: Pick<Event, "id">): string {
  return `event:${event.id}`;
}

/** The `dismissed` entry that records `id` (an `eventItemId`) as pinned. */
export function pinnedItemMarker(id: string): string {
  return `${pinnedItemPrefix}${id}`;
}

export function isPinnedEvent(dismissed: string[], eventId: number): boolean {
  return dismissed.includes(pinnedItemMarker(eventItemId({ id: eventId })));
}

export function pinnedEventIds(dismissed: string[]): string[] {
  const prefix = `${pinnedItemPrefix}event:`;
  return dismissed
    .filter((item) => item.startsWith(prefix))
    .map((item) => item.slice(prefix.length));
}

/** The viewer's row for an issue, defaulted: absent state and an unvisited issue read alike. */
export function stateForIssue(state: UserState | undefined, issueKey: string): UserIssueState {
  return state?.[issueKey] ?? { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };
}

const MINUTE_MS = 60 * 1000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

/** Formats an ask's elapsed open time for compact human-blocker surfaces. */
export function formatAskAge(createdAt: string, now = Date.now()): string {
  const openedAt = Date.parse(createdAt);
  if (Number.isNaN(openedAt)) return createdAt;

  const elapsed = Math.max(0, now - openedAt);
  if (elapsed < HOUR_MS) return `${Math.floor(elapsed / MINUTE_MS)}m`;
  if (elapsed < 2 * DAY_MS) return `${Math.floor(elapsed / HOUR_MS)}h`;
  return `${Math.floor(elapsed / DAY_MS)}d`;
}

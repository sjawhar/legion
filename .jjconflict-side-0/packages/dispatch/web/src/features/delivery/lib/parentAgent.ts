// How the page labels a Parent agent value. The server answers a session group as `group:<id>` (a
// rename keeps every filter and link naming it), a placeholder as its token, and any other session
// as its title, so only a group needs a lookup. Pure: no React.
import type { DeliverySessionGroup } from "../../../api/types";
import { PLACEHOLDER_LABELS } from "./facets";

/** The prefix of a session group's Parent agent value (`delivery.SessionGroupValuePrefix`). */
export const SESSION_GROUP_VALUE_PREFIX = "group:";

/** A Parent agent value as the page shows it: a group by its name, a placeholder in words, a
 *  session as the server wrote it. A group the answer no longer lists shows its raw value. */
export function parentAgentLabel(value: string, groups: readonly DeliverySessionGroup[]): string {
  const placeholder = PLACEHOLDER_LABELS[value];
  if (placeholder !== undefined) return placeholder;
  if (!value.startsWith(SESSION_GROUP_VALUE_PREFIX)) return value;
  const id = value.slice(SESSION_GROUP_VALUE_PREFIX.length);
  return groups.find((group) => group.id === id)?.name ?? value;
}

// Applies a live delta (`GET /ws/delivery`'s `delta` frame, LEGION-690) to the body the page holds,
// and its revision changes to the revisions the page holds. Pure: no React, no cache. Dispatch
// sends only what moved, so every field, list item, map entry and count the delta does not name
// stays the very object it was, and a delta that names nothing returns the body itself, which
// re-renders nothing.
import type {
  DeliveryLiveBody,
  DeliveryLiveDelta,
  DeliveryLiveRevChanges,
  DeliveryLiveRevs,
  DeliveryPR,
  DeliveryRun,
  DeliverySessionGroup,
} from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";

type Item = { readonly id: string | number };

/** The order Dispatch lists each live list in, ties broken by id so a merge of upserts always
 *  lands in one order: pull requests by merge time (`ListPullRequestsInWindow`), runs by their head
 *  commit's time (`ListRuns`, `order by head_commit_at`), session groups by name, case aside
 *  (`ListSessionGroups`, `order by lower(g.name), g.id`). */
const ORDERS = {
  prs: (a: DeliveryPR, b: DeliveryPR) =>
    compareNullableTimestamps(a.merged_at, b.merged_at) || compareText(a.id, b.id),
  runs: (a: DeliveryRun, b: DeliveryRun) => compareTimestamps(a.head_at, b.head_at) || a.id - b.id,
  session_groups: (a: DeliverySessionGroup, b: DeliverySessionGroup) =>
    compareText(a.name.toLowerCase(), b.name.toLowerCase()) || compareText(a.id, b.id),
} as const;

function compareText(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** A window pull request always has a merge time; null sorts last, should one ever arrive. */
function compareNullableTimestamps(a: string | null, b: string | null): number {
  if (a === null || b === null) return a === b ? 0 : a === null ? 1 : -1;
  return compareTimestamps(a, b);
}

/** `items` with `upsert` merged in by id and `remove` dropped, re-sorted by `order`. */
function mergeList<T extends Item>(
  items: readonly T[],
  upsert: readonly T[],
  remove: readonly string[],
  order: (a: T, b: T) => number
): T[] {
  const byId = new Map(items.map((item) => [String(item.id), item]));
  for (const id of remove) byId.delete(id);
  for (const item of upsert) byId.set(String(item.id), item);
  return [...byId.values()].sort(order);
}

/** `entries` with each key set to its change's value, or deleted when the change is null. */
function mergeEntries<V>(
  entries: Readonly<Record<string, V>>,
  changes: Readonly<Record<string, V | null>>
): Record<string, V> {
  const next = { ...entries };
  for (const [key, value] of Object.entries(changes)) {
    if (value === null) delete next[key];
    else next[key] = value;
  }
  return next;
}

/** A two-level map with each first-level entry merged by `mergeEntries`. */
function mergeNestedEntries<V>(
  entries: Readonly<Record<string, Readonly<Record<string, V>>>>,
  changes: Readonly<Record<string, Readonly<Record<string, V | null>>>>
): Record<string, Readonly<Record<string, V>>> {
  const next = { ...entries };
  for (const [key, values] of Object.entries(changes)) {
    next[key] = mergeEntries(entries[key] ?? {}, values);
  }
  return next;
}

/** `body` with `delta` applied: its lists' upserts merged in by id and removes dropped, each list
 *  in Dispatch's order; its maps' and counts' entries set, or deleted where null; and its `set`
 *  fields replaced whole. An empty delta returns `body` itself. */
export function applyDelta<B extends DeliveryLiveBody>(body: B, delta: DeliveryLiveDelta): B {
  const lists = Object.entries(delta.items);
  const maps = Object.entries(delta.maps);
  const counts = Object.entries(delta.counts);
  const set = Object.entries(delta.set);
  if (lists.length + maps.length + counts.length + set.length === 0) return body;

  // DeliveryLiveBody is a union of response bodies with no shared field shape. The delta names
  // fields by the server's field classes, so this generic merge reads and writes the body by name.
  const held = body as unknown as Record<string, unknown>;
  const next: Record<string, unknown> = { ...held };
  for (const [list, changes] of lists) {
    if (changes === undefined) continue;
    const order = Object.hasOwn(ORDERS, list)
      ? (ORDERS[list as keyof typeof ORDERS] as (a: Item, b: Item) => number)
      : undefined;
    if (order === undefined) throw new Error(`Delivery live list ${list} has no client order`);
    next[list] = mergeList(
      held[list] as readonly Item[],
      changes.upsert as readonly Item[],
      changes.remove,
      order
    );
  }
  for (const [map, changes] of maps) {
    if (changes === undefined) continue;
    next[map] = mergeEntries(held[map] as Readonly<Record<string, unknown>>, changes);
  }
  for (const [name, changes] of counts) {
    if (changes === undefined) continue;
    next[name] = mergeNestedEntries(
      held[name] as Readonly<Record<string, Readonly<Record<string, number>>>>,
      changes
    );
  }
  for (const [field, value] of set) next[field] = value;
  // Every field the delta could name has been replaced with the same JSON shape the server sent.
  return next as unknown as B;
}

/** `revs` with a delta's revision changes applied, section by section: each key set to its new
 *  revision, or deleted where it is null. */
export function applyRevs(
  revs: DeliveryLiveRevs,
  changes: DeliveryLiveRevChanges
): DeliveryLiveRevs {
  const counts: Record<string, Readonly<Record<string, Readonly<Record<string, string>>>>> = {
    ...revs.counts,
  };
  for (const [name, facets] of Object.entries(changes.counts ?? {})) {
    if (facets === undefined) continue;
    counts[name] = mergeNestedEntries(counts[name] ?? {}, facets);
  }
  return {
    items: mergeNestedEntries(revs.items, changes.items ?? {}),
    maps: mergeNestedEntries(revs.maps, changes.maps ?? {}),
    counts,
    fields: { ...revs.fields, ...changes.fields },
  };
}

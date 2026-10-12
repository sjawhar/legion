import { expect, test } from "bun:test";
import after from "../fixtures/delivery-live/after.json";
import delta from "../fixtures/delivery-live/delta.json";
import error from "../fixtures/delivery-live/error.json";
import heartbeat from "../fixtures/delivery-live/heartbeat.json";
import snapshot from "../fixtures/delivery-live/snapshot.json";
import type {
  DeliveryLiveCounts,
  DeliveryLiveDelta,
  DeliveryLiveError,
  DeliveryLiveHeartbeat,
  DeliveryLiveItemChanges,
  DeliveryLiveList,
  DeliveryLiveMap,
  DeliveryLiveRevChanges,
  DeliveryLiveRevs,
  DeliveryLiveServerFrame,
  DeliveryLiveSnapshot,
  DeliveryTimelineResponse,
} from "./dispatch-api";

// The frames in fixtures/delivery-live are written by the Dispatch server's own diff
// (TestTheLiveFixturesAreWhatTheDiffWrites, packages/envoy/internal/dispatch/api), so each is what
// GET /ws/delivery sends. These tests hold them to the DeliveryLive* contract key for key: each
// list below must name every key of its type, or the type check fails, so a key the contract
// gains or loses fails here until the server writes it too.

/** `list`, which must hold every member of U. */
function allOf<U extends string>() {
  return <const L extends readonly U[]>(
    list: L & ([Exclude<U, L[number]>] extends [never] ? unknown : never)
  ): L => list;
}

const SNAPSHOT_KEYS = allOf<keyof DeliveryLiveSnapshot>()([
  "type",
  "sub",
  "seq",
  "changed_at",
  "body",
  "revs",
]);
const DELTA_KEYS = allOf<keyof DeliveryLiveDelta>()([
  "type",
  "sub",
  "seq",
  "changed_at",
  "set",
  "items",
  "maps",
  "counts",
  "revs",
]);
const ERROR_KEYS = allOf<keyof DeliveryLiveError>()(["type", "sub", "code", "message"]);
const HEARTBEAT_KEYS = allOf<keyof DeliveryLiveHeartbeat>()(["type"]);
const SERVER_FRAME_TYPES = allOf<DeliveryLiveServerFrame["type"]>()([
  "snapshot",
  "delta",
  "freshness",
  "error",
  "heartbeat",
]);
const TIMELINE_KEYS = allOf<keyof DeliveryTimelineResponse>()([
  "window",
  "prs",
  "waiting",
  "runs",
  "facet_counts",
  "color_counts",
  "components",
  "issue_titles",
  "freshness",
  "measures",
  "session_groups",
  "session_titles",
  "session_pr_counts",
]);
const REVS_KEYS = allOf<keyof DeliveryLiveRevs>()(["items", "maps", "counts", "fields"]);
const REV_CHANGE_KEYS = allOf<keyof DeliveryLiveRevChanges>()([
  "items",
  "maps",
  "counts",
  "fields",
]);
const ITEM_CHANGE_KEYS = allOf<keyof DeliveryLiveItemChanges<DeliveryLiveList>>()([
  "upsert",
  "remove",
]);
const LISTS = allOf<DeliveryLiveList>()(["prs", "runs", "session_groups"]);
const MAPS = allOf<DeliveryLiveMap>()([
  "issue_titles",
  "session_titles",
  "session_pr_counts",
  "components",
]);
const COUNTS = allOf<DeliveryLiveCounts>()(["facet_counts", "color_counts"]);

const REVISION = /^[0-9a-f]{16}$/;

const sorted = (keys: readonly string[]) => [...keys].sort();
const keysOf = (value: object) => sorted(Object.keys(value));

/** Every leaf of a revisions section, `depth` levels of keys deep. */
function leaves(section: unknown, depth: number): unknown[] {
  if (depth === 0) return [section];
  return Object.values(section as Record<string, unknown>).flatMap((value) =>
    leaves(value, depth - 1)
  );
}

test("a snapshot frame is the timeline's GET body with a revision for every key it holds", () => {
  for (const frame of [snapshot, after]) {
    expect(keysOf(frame)).toEqual(sorted(SNAPSHOT_KEYS));
    expect(frame.type).toBe("snapshot");
    expect(frame.seq).toMatch(/^\d+$/);
    expect(keysOf(frame.body)).toEqual(sorted(TIMELINE_KEYS));
    expect(keysOf(frame.revs)).toEqual(sorted(REVS_KEYS));
    expect(keysOf(frame.revs.items)).toEqual(sorted(LISTS));
    expect(keysOf(frame.revs.maps)).toEqual(sorted(MAPS));
    expect(keysOf(frame.revs.counts)).toEqual(sorted(COUNTS));
    for (const list of LISTS) {
      const ids = frame.body[list].map((item: { id: string | number }) => String(item.id));
      expect(sorted(ids)).toEqual(keysOf(frame.revs.items[list]));
    }
    for (const map of MAPS) expect(keysOf(frame.revs.maps[map])).toEqual(keysOf(frame.body[map]));
    for (const counts of COUNTS) {
      expect(keysOf(frame.revs.counts[counts])).toEqual(keysOf(frame.body[counts]));
    }
    expect(keysOf(frame.revs.fields)).toEqual(["measures", "waiting", "window"]);
    for (const revision of [
      ...leaves(frame.revs.items, 2),
      ...leaves(frame.revs.maps, 2),
      ...leaves(frame.revs.counts, 3),
      ...leaves(frame.revs.fields, 1),
    ]) {
      expect(revision).toMatch(REVISION);
    }
  }
});

test("a delta frame carries lists by id, maps by key, counts two levels deep, and null for a key that left", () => {
  expect(keysOf(delta)).toEqual(sorted(DELTA_KEYS));
  expect(delta.type).toBe("delta");
  expect(delta.seq).toMatch(/^\d+$/);
  for (const [list, changes] of Object.entries(delta.items)) {
    expect(LISTS).toContain(list as DeliveryLiveList);
    expect(keysOf(changes)).toEqual(sorted(ITEM_CHANGE_KEYS));
  }
  for (const map of Object.keys(delta.maps)) expect(MAPS).toContain(map as DeliveryLiveMap);
  for (const counts of Object.keys(delta.counts)) {
    expect(COUNTS).toContain(counts as DeliveryLiveCounts);
  }
  for (const count of leaves(delta.counts, 3)) {
    expect(count === null || Number.isInteger(count)).toBe(true);
  }
  // This fixture's change moves at least one of each, so every section is exercised.
  expect(delta.maps.issue_titles).toEqual({ "ACME-2": null });
  // octocat's count holds at 2 (#1 left, #4 merged), so only hubot's, which fell to none, is sent.
  expect(delta.counts.facet_counts.author).toEqual({ hubot: null });
  expect(delta.items.prs.remove).toEqual(["acme/widgets#1", "acme/widgets#2"]);
  expect(Object.keys(delta.set).sort()).toEqual(["measures", "waiting", "window"]);

  for (const section of Object.keys(delta.revs)) {
    expect(REV_CHANGE_KEYS).toContain(section as keyof DeliveryLiveRevChanges);
  }
  for (const revision of [
    ...leaves(delta.revs.items, 2),
    ...leaves(delta.revs.maps, 2),
    ...leaves(delta.revs.counts, 3),
  ]) {
    expect(revision === null || REVISION.test(String(revision))).toBe(true);
  }
  for (const revision of leaves(delta.revs.fields, 1)) expect(revision).toMatch(REVISION);
});

test("an error frame names its code and its message", () => {
  expect(keysOf(error)).toEqual(sorted(ERROR_KEYS));
  expect(error).toEqual({
    type: "error",
    sub: "timeline-1",
    code: "DELIVERY_NOT_CONFIGURED",
    message:
      "delivery is not configured; set delivery_settings through PUT /api/v1/settings/delivery",
  });
});

test("a heartbeat frame carries its type and nothing else", () => {
  expect(keysOf(heartbeat)).toEqual(sorted(HEARTBEAT_KEYS));
  expect(heartbeat).toEqual({ type: "heartbeat" });
  expect(SERVER_FRAME_TYPES).toContain(heartbeat.type as DeliveryLiveServerFrame["type"]);
});

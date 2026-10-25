import { expect, test } from "bun:test";
import after from "../fixtures/live/after.json";
import agent from "../fixtures/live/agent.json";
import delta from "../fixtures/live/delta.json";
import ended from "../fixtures/live/ended.json";
import error from "../fixtures/live/error.json";
import event from "../fixtures/live/event.json";
import eventsStart from "../fixtures/live/events_start.json";
import heartbeat from "../fixtures/live/heartbeat.json";
import snapshot from "../fixtures/live/snapshot.json";
import { AGENT_STREAM_PROTOCOL, type AgentStreamResponder } from "./agent-stream";
import {
  type DeliveryLiveCounts,
  type DeliveryLiveDelta,
  type DeliveryLiveFreshness,
  type DeliveryLiveItemChanges,
  type DeliveryLiveList,
  type DeliveryLiveMap,
  type DeliveryLiveResume,
  type DeliveryLiveRevChanges,
  type DeliveryLiveRevs,
  type DeliveryLiveSnapshot,
  type DeliveryTimelineResponse,
  type DispatchEvent,
  type EventsLiveResume,
  LIVE_SOCKET_MAX_AGE_CLOSE,
  LIVE_SOCKET_MAX_AGE_MS,
  LIVE_SOCKET_MAX_SUBSCRIPTIONS,
  LIVE_SOCKET_MIN_AGE_MS,
  type LiveAgentFrame,
  type LiveClientFrame,
  type LiveEnded,
  type LiveError,
  type LiveEventFrame,
  type LiveEventsStart,
  type LiveHeartbeat,
  type LiveServerFrame,
  type LiveSubscribe,
  type LiveUnsubscribe,
  type LiveView,
} from "./dispatch-api";

// The frames in fixtures/live are written by the Dispatch server's own diff and event feed
// (TestTheLiveFixturesAreWhatTheDiffWrites, packages/envoy/internal/dispatch/api), so each is what
// the live socket sends. These tests hold them to the Live* and DeliveryLive* contract key for
// key: each list below must name every key of its type, or the type check fails, so a key the
// contract gains or loses fails here until the server writes it too.

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
/** Every frame Dispatch sends, by its type, with the keys it carries: a frame type without a row,
 *  or a row naming a key its type lacks or missing one it has, fails the type check. */
const SERVER_FRAME_KEYS = {
  snapshot: SNAPSHOT_KEYS,
  delta: DELTA_KEYS,
  freshness: allOf<keyof DeliveryLiveFreshness>()(["type", "freshness", "stale"]),
  events_start: allOf<keyof LiveEventsStart>()(["type", "sub", "since"]),
  event: allOf<keyof LiveEventFrame>()(["type", "sub", "id", "event"]),
  agent: allOf<keyof LiveAgentFrame>()(["type", "sub", "event", "data"]),
  ended: allOf<keyof LiveEnded>()(["type", "sub", "reason"]),
  error: allOf<keyof LiveError>()(["type", "sub", "code", "message"]),
  heartbeat: allOf<keyof LiveHeartbeat>()(["type"]),
} satisfies Record<LiveServerFrame["type"], readonly string[]>;
/** Every frame a page sends, by its type, with the keys it carries. */
const CLIENT_FRAME_KEYS = {
  subscribe: allOf<keyof LiveSubscribe>()(["type", "sub", "view", "resume"]),
  unsubscribe: allOf<keyof LiveUnsubscribe>()(["type", "sub"]),
} satisfies Record<LiveClientFrame["type"], readonly string[]>;
const VIEW_KINDS = allOf<LiveView["kind"]>()([
  "timeline",
  "measures",
  "pipeline",
  "events",
  "agent",
]);
const DELIVERY_RESUME_KEYS = allOf<keyof DeliveryLiveResume>()(["seq", "revs"]);
const EVENTS_RESUME_KEYS = allOf<keyof EventsLiveResume>()(["since"]);
const AGENT_EVENTS = allOf<LiveAgentFrame["event"]>()(["responder", "replay", "frame"]);
const RESPONDER_KEYS = allOf<keyof AgentStreamResponder>()(["v", "responding"]);
const ENDED_REASONS = allOf<LiveEnded["reason"]>()(["behind", "dropped"]);
/** Every key of the event an event frame carries. */
const EVENT_KEYS = allOf<keyof DispatchEvent>()([
  "id",
  "issue_key",
  "artifact_id",
  "project",
  "seq",
  "type",
  "actor",
  "notify",
  "created_at",
  "payload",
]);
/** The frames that are not one subscription's: freshness goes to every subscriber of the socket,
 *  heartbeat to the socket itself. */
const SOCKET_WIDE: Partial<Record<LiveServerFrame["type"], true>> = {
  freshness: true,
  heartbeat: true,
};
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
  expect(keysOf(error)).toEqual(sorted(SERVER_FRAME_KEYS.error));
  expect(error).toEqual({
    type: "error",
    sub: "timeline-1",
    code: "DELIVERY_NOT_CONFIGURED",
    message:
      "delivery is not configured; set delivery_settings through PUT /api/v1/settings/delivery",
  });
});

test("a heartbeat frame carries its type and nothing else", () => {
  expect(keysOf(heartbeat)).toEqual(sorted(SERVER_FRAME_KEYS.heartbeat));
  expect(heartbeat).toEqual({ type: "heartbeat" });
  expect(Object.keys(SERVER_FRAME_KEYS)).toContain(heartbeat.type);
});

test("an events_start frame names the decimal id its subscription starts after", () => {
  expect(keysOf(eventsStart)).toEqual(sorted(SERVER_FRAME_KEYS.events_start));
  expect(eventsStart.type).toBe("events_start");
  expect(eventsStart.since).toMatch(/^\d+$/);
});

test("an event frame carries the event's decimal id and the event itself", () => {
  expect(keysOf(event)).toEqual(sorted(SERVER_FRAME_KEYS.event));
  expect(event.type).toBe("event");
  expect(event.id).toBe(String(event.event.id));
  expect(keysOf(event.event)).toEqual(sorted(EVENT_KEYS));
});

test("an ended frame names its subscription and why Dispatch ended it", () => {
  expect(keysOf(ended)).toEqual(sorted(SERVER_FRAME_KEYS.ended));
  expect(ended.type).toBe("ended");
  expect(ENDED_REASONS).toContain(ended.reason as LiveEnded["reason"]);
});

test("an agent frame names its event, and its responder verdict is the one Dispatch writes", () => {
  expect(keysOf(agent)).toEqual(sorted(SERVER_FRAME_KEYS.agent));
  expect(agent.type).toBe("agent");
  expect(AGENT_EVENTS).toContain(agent.event as LiveAgentFrame["event"]);
  // A replay's or a frame's data is the session's own JSON, which Dispatch relays unread; the
  // responder verdict is the one agent frame whose data Dispatch writes itself.
  expect(agent.event).toBe("responder");
  expect(keysOf(agent.data)).toEqual(sorted(RESPONDER_KEYS));
  expect(agent.data).toEqual({ v: AGENT_STREAM_PROTOCOL, responding: true });
});

test("every frame names its type, and every frame of one subscription names the subscription", () => {
  for (const [type, keys] of Object.entries({ ...SERVER_FRAME_KEYS, ...CLIENT_FRAME_KEYS })) {
    expect(keys).toContain("type");
    if (SOCKET_WIDE[type as LiveServerFrame["type"]] !== true) expect(keys).toContain("sub");
  }
  for (const list of [
    ...Object.values(SERVER_FRAME_KEYS),
    ...Object.values(CLIENT_FRAME_KEYS),
    VIEW_KINDS,
    AGENT_EVENTS,
    ENDED_REASONS,
  ]) {
    expect(new Set(list).size).toBe(list.length);
  }
});

test("a resume's keys tell a delivery view's resume from an events view's", () => {
  // Dispatch refuses a resume whose shape is not its view's kind, so the two share no key.
  for (const key of DELIVERY_RESUME_KEYS) expect(EVENTS_RESUME_KEYS).not.toContain(key);
});

test("a socket ends at an age between thirty minutes and an hour, with a close code of its own", () => {
  expect(LIVE_SOCKET_MIN_AGE_MS).toBe(30 * 60 * 1000);
  expect(LIVE_SOCKET_MAX_AGE_MS).toBe(60 * 60 * 1000);
  // An application code, apart from 4000, the page's own close of a silent socket.
  expect(LIVE_SOCKET_MAX_AGE_CLOSE).toBeGreaterThan(4000);
  expect(LIVE_SOCKET_MAX_AGE_CLOSE).toBeLessThan(5000);
  expect(Number.isInteger(LIVE_SOCKET_MAX_SUBSCRIPTIONS)).toBe(true);
  expect(LIVE_SOCKET_MAX_SUBSCRIPTIONS).toBeGreaterThan(0);
});

import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import {
  architectureSourcesQuery,
  inboxQuery,
  projectsQuery,
  userStateQuery,
} from "../api/queries";
import { applyEventInvalidations, mergeEventPages, prependEventToLog } from "../api/sse";
import type { Event, EventType } from "../api/types";

/**
 * The pre-#1248 reference implementation, copied verbatim from `main`'s
 * `packages/dispatch/web/src/api/sse.ts`. It is the oracle this PR's behaviour is compared
 * against: every event must invalidate exactly what main invalidated, except the single
 * documented subtraction (the Inbox query for plain messages and non-ask comments).
 */
function mainArtifactId(event: Event): string | undefined {
  return event.type === "artifact.version" ? event.payload.artifact_id : undefined;
}

function mainAppendDocumentKey(keys: (readonly unknown[])[], event: Event): void {
  let id: string | undefined;
  switch (event.type) {
    case "ask.opened":
    case "ask.edited":
    case "ask.answered":
    case "ask.resolved":
    case "ask.anchor_refreshed":
      id = event.payload.block_artifact?.id ?? event.payload.anchor?.artifact_id;
      break;
    case "comment.created":
    case "comment.delivery":
    case "comment.answered":
    case "comment.resolved":
    case "comment.reopened":
    case "comment.edited":
    case "comment.anchor_refreshed":
    case "suggestion.accepted":
    case "suggestion.rejected":
      id = "anchor" in event.payload ? event.payload.anchor?.artifact_id : undefined;
      break;
    default:
      throw new Error(`${event.type} events do not name a document`);
  }
  if (id !== undefined) {
    keys.push(["artifact", id]);
  }
}

function mainPayloadString(event: Event, key: string): string | undefined {
  const payload = event.payload;
  if (typeof payload !== "object" || payload === null || !(key in payload)) {
    return undefined;
  }
  const value = Reflect.get(payload, key);
  return typeof value === "string" ? value : undefined;
}

function mainAppendAskDetailKeys(keys: (readonly unknown[])[], event: Event): void {
  const id =
    event.type === "ask.follower_added" || event.type === "ask.follower_removed"
      ? mainPayloadString(event, "ask_id")
      : mainPayloadString(event, "id");
  if (id !== undefined) {
    keys.push(["ask", id], ["ask-thread", id]);
  }
}

function mainAppendCommentDetailKeys(keys: (readonly unknown[])[], event: Event): void {
  const id =
    event.type === "comment.delivery" ? event.payload.comment_id : mainPayloadString(event, "id");
  if (id !== undefined) {
    keys.push(["comment", id]);
  }
  const askID = mainPayloadString(event, "ask_id");
  if (askID !== undefined) {
    keys.push(["ask", askID], ["ask-thread", askID]);
  }
}

function mainIsCommentLikeEvent(event: Event): boolean {
  return (
    event.type === "comment.created" ||
    event.type === "comment.delivery" ||
    event.type === "comment.answered" ||
    event.type === "comment.resolved" ||
    event.type === "comment.reopened" ||
    event.type === "comment.edited" ||
    event.type === "comment.anchor_refreshed" ||
    event.type === "suggestion.accepted" ||
    event.type === "suggestion.rejected"
  );
}

function mainIsMessageEvent(event: Event): boolean {
  return (
    event.type === "message.created" ||
    event.type === "message.delivery" ||
    event.type === "message.answered"
  );
}

function mainEventQueryKeys(event: Event, signedInLogin?: string): (readonly unknown[])[] {
  if (
    event.type === "project.created" ||
    event.type === "project.updated" ||
    event.type === "settings.repo_project.updated"
  ) {
    if (event.project === undefined) {
      throw new Error("project event is missing its project");
    }
    return [
      projectsQuery().queryKey,
      ["project", event.project],
      ["issues", "project", event.project],
      ["repo-projects"],
    ];
  }
  if (
    event.type === "settings.architecture_source.updated" ||
    event.type === "architecture.synced" ||
    event.type === "architecture.sync_failed"
  ) {
    if (event.project === undefined) {
      throw new Error("project event is missing its project");
    }
    return [
      architectureSourcesQuery().queryKey,
      ["architecture-source", event.project],
      ["components", event.project],
      ["architecture", event.project],
    ];
  }
  if (event.type === "user_state.updated") {
    return mainPayloadString(event, "login") === signedInLogin
      ? [userStateQuery().queryKey, inboxQuery().queryKey]
      : [];
  }
  if (event.type === "subscription.remove_requested") {
    return [];
  }
  if (event.issue_key === null && mainIsMessageEvent(event)) {
    const target = mainPayloadString(event, "target");
    if (
      target === undefined ||
      !target.startsWith("session:") ||
      target.length === "session:".length
    ) {
      throw new Error("issue-less message event is missing its session target");
    }
    return [["agents", target.slice("session:".length), "messages"]];
  }
  if (event.issue_key === null) {
    if (event.artifact_id === null || event.artifact_id === undefined) {
      throw new Error("document event is missing its artifact id");
    }
    if (event.project === undefined) {
      throw new Error("document event is missing its project");
    }
    const keys: (readonly unknown[])[] = [
      ["artifact", event.artifact_id],
      ["artifact-ref"],
      ["project", event.project, "artifacts"],
      projectsQuery().queryKey,
    ];
    if (event.type.startsWith("ask.")) {
      mainAppendAskDetailKeys(keys, event);
      keys.push(inboxQuery().queryKey);
    }
    if (mainIsCommentLikeEvent(event)) {
      mainAppendCommentDetailKeys(keys, event);
    }
    if (event.type === "artifact.approved" || event.type === "artifact.changes_requested") {
      keys.push(inboxQuery().queryKey);
    }
    if (event.type === "subscription.removed") {
      keys.push(["subscribers", event.artifact_id]);
    }
    return keys;
  }

  const keys: (readonly unknown[])[] = [
    ["issue", event.issue_key],
    ["events", event.issue_key],
    ["issues"],
    userStateQuery().queryKey,
    inboxQuery().queryKey,
  ];

  if (
    event.type === "issue.created" ||
    event.type === "issue.updated" ||
    event.type === "issue.closed"
  ) {
    if (event.project !== undefined) {
      keys.push(
        ["issues", "project", event.project],
        ["components", event.project],
        ["architecture", event.project]
      );
    }
    if (event.type === "issue.updated") {
      keys.push(["issue"]);
    }
    return keys;
  }
  if (event.type.startsWith("issue.")) {
    return keys;
  }

  if (event.type === "artifact.created" || event.type === "artifact.version") {
    keys.push(["artifacts", event.issue_key]);
    const id = mainArtifactId(event);
    if (id !== undefined) {
      keys.push(["artifact", id]);
    }
    keys.push(["comments", event.issue_key]);
    return keys;
  }

  if (event.type === "artifact.approved" || event.type === "artifact.changes_requested") {
    keys.push(
      ["artifacts", event.issue_key],
      ["artifact", event.payload.artifact_id],
      ["asks", event.issue_key]
    );
    if (event.payload.ask_id !== null) {
      keys.push(["ask-thread", event.payload.ask_id]);
    }
    return keys;
  }

  if (
    event.type === "ask.opened" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved" ||
    event.type === "ask.edited" ||
    event.type === "ask.anchor_refreshed"
  ) {
    keys.push(["asks", event.issue_key]);
    if (event.type !== "ask.edited" && event.type !== "ask.anchor_refreshed") {
      keys.push(projectsQuery().queryKey);
    }
    mainAppendDocumentKey(keys, event);
    mainAppendAskDetailKeys(keys, event);
    return keys;
  }
  if (event.type === "ask.follower_added" || event.type === "ask.follower_removed") {
    mainAppendAskDetailKeys(keys, event);
    return keys;
  }
  if (event.type === "block.repaired" || event.type === "block.invalid") {
    keys.push(["asks", event.issue_key], projectsQuery().queryKey);
    return keys;
  }

  if (mainIsCommentLikeEvent(event)) {
    keys.push(["comments", event.issue_key]);
    if (mainPayloadString(event, "ask_id") !== undefined) {
      keys.push(["asks", event.issue_key]);
    }
    mainAppendDocumentKey(keys, event);
    mainAppendCommentDetailKeys(keys, event);
    return keys;
  }

  if (mainIsMessageEvent(event)) {
    const target = mainPayloadString(event, "target");
    if (target?.startsWith("session:") && target.length > "session:".length) {
      keys.push(["agents", target.slice("session:".length), "messages"]);
    }
    keys.push(["messages", event.issue_key]);
    return keys;
  }

  if (event.type === "subscription.removed") {
    keys.push(["subscribers", event.issue_key]);
    return keys;
  }

  keys.push(["children", event.issue_key]);
  return keys;
}

function event(
  type: Event["type"],
  payload: Record<string, unknown> = {},
  overrides: Partial<Event> = {}
): Event {
  return {
    actor: { kind: "user", id: "alice" },
    created_at: "2026-09-09T00:00:00Z",
    id: 12,
    issue_key: "CORE-1",
    notify: true,
    payload,
    project: "CORE",
    seq: 4,
    type,
    ...overrides,
  } as Event;
}

const documentOwner = { artifact_id: "artifact-1", issue_key: null, project: "CORE" } as const;

/**
 * One representative event per contract type, plus the payload variants that change routing.
 * Keyed by `EventType` so a new event type fails to compile here instead of going unchecked.
 */
const eventsByType: Record<EventType, readonly Event[]> = {
  "project.created": [event("project.created", {}, { issue_key: null })],
  "project.updated": [event("project.updated", {}, { issue_key: null })],
  "settings.repo_project.updated": [
    event("settings.repo_project.updated", {}, { issue_key: null }),
  ],
  "settings.architecture_source.updated": [
    event("settings.architecture_source.updated", {}, { issue_key: null }),
  ],
  "architecture.synced": [
    event("architecture.synced", { commit: "c0ffee", components: 2 }, { issue_key: null }),
  ],
  "architecture.sync_failed": [
    event("architecture.sync_failed", { error: "broken" }, { issue_key: null }),
  ],
  "user_state.updated": [
    event("user_state.updated", { login: "alice" }, { issue_key: null }),
    event("user_state.updated", { login: "bob" }, { issue_key: null }),
  ],
  "issue.created": [event("issue.created")],
  "issue.updated": [event("issue.updated")],
  "issue.closed": [event("issue.closed")],
  "artifact.created": [event("artifact.created", { artifact: { id: "artifact-1" } })],
  "artifact.version": [event("artifact.version", { artifact_id: "artifact-1" })],
  "artifact.approved": [event("artifact.approved", { artifact_id: "artifact-1", ask_id: "ask-1" })],
  "artifact.changes_requested": [
    event("artifact.changes_requested", { artifact_id: "artifact-1", ask_id: null }),
  ],
  "ask.opened": [
    event("ask.opened", { id: "ask-1", anchor: { artifact_id: "artifact-1" } }),
    event("ask.opened", { id: "ask-1", anchor: null }, documentOwner),
  ],
  "ask.anchor_refreshed": [
    event("ask.anchor_refreshed", { id: "ask-1", anchor: { artifact_id: "artifact-1" } }),
  ],
  "ask.edited": [event("ask.edited", { id: "ask-1", anchor: null })],
  "ask.answered": [event("ask.answered", { id: "ask-1", anchor: null })],
  "ask.resolved": [event("ask.resolved", { id: "ask-1", anchor: null })],
  "ask.follower_added": [event("ask.follower_added", { ask_id: "ask-1", session_id: "s1" })],
  "ask.follower_removed": [event("ask.follower_removed", { ask_id: "ask-1", session_id: "s1" })],
  "block.repaired": [event("block.repaired", { block_id: "b-1" })],
  "block.invalid": [event("block.invalid", { block_id: "b-1" })],
  "comment.created": [
    event("comment.created", { id: "comment-1", anchor: { artifact_id: "artifact-1" } }),
    event("comment.created", { id: "comment-1", ask_id: "ask-1", anchor: null }),
    event("comment.created", { id: "comment-1", anchor: null }, documentOwner),
  ],
  "comment.anchor_refreshed": [
    event("comment.anchor_refreshed", { id: "comment-1", anchor: { artifact_id: "artifact-1" } }),
  ],
  "comment.delivery": [event("comment.delivery", { comment_id: "comment-1" })],
  "comment.answered": [event("comment.answered", { id: "comment-1", anchor: null })],
  "comment.resolved": [event("comment.resolved", { id: "comment-1", anchor: null })],
  "comment.reopened": [event("comment.reopened", { id: "comment-1", anchor: null })],
  "comment.edited": [event("comment.edited", { id: "comment-1", anchor: null })],
  "suggestion.accepted": [event("suggestion.accepted", { id: "comment-1", anchor: null })],
  "suggestion.rejected": [event("suggestion.rejected", { id: "comment-1", anchor: null })],
  "message.created": [
    event("message.created", {}),
    event("message.created", { target: "session:planner" }),
    event("message.created", { target: "session:planner" }, { artifact_id: null, issue_key: null }),
  ],
  "message.delivery": [event("message.delivery", { target: "session:planner" })],
  "message.answered": [event("message.answered", {})],
  "child.status": [event("child.status", { child_key: "CORE-2" })],
  "child.added": [event("child.added", { child_key: "CORE-2" })],
  "child.removed": [event("child.removed", { child_key: "CORE-2" })],
  "subscription.remove_requested": [event("subscription.remove_requested", { session_id: "s1" })],
  "subscription.removed": [
    event("subscription.removed", { session_id: "s1" }),
    event("subscription.removed", { session_id: "s1" }, documentOwner),
  ],
};

const representativeEvents: readonly Event[] = Object.values(eventsByType).flat();

/**
 * The documented additions to main's behaviour, each one strictly more invalidation than main:
 * ancestors and descendants whose rendered rollups and inherited fields move without an event
 * of their own, and the sidebar's per-project open-ask badge.
 */
function additions(incoming: Event): unknown[][] {
  if (incoming.issue_key === null) {
    return [];
  }
  if (incoming.type === "issue.created" || incoming.type === "issue.closed") {
    return [["issue"], [...projectsQuery().queryKey]];
  }
  if (incoming.type === "issue.updated") {
    return [[...projectsQuery().queryKey]];
  }
  if (
    incoming.type === "child.status" ||
    incoming.type === "child.added" ||
    incoming.type === "child.removed"
  ) {
    return [["issue"]];
  }
  return [];
}

/** The documented subtraction: an issue-scoped message or a comment that replies to no ask. */
function subtractsInbox(incoming: Event): boolean {
  if (incoming.issue_key === null) {
    return false;
  }
  if (mainIsMessageEvent(incoming)) {
    return true;
  }
  return mainIsCommentLikeEvent(incoming) && mainPayloadString(incoming, "ask_id") === undefined;
}

function invalidatedKeys(incoming: Event): unknown[][] {
  const keys: unknown[][] = [];
  applyEventInvalidations(
    {
      invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
        keys.push([...queryKey]);
        return Promise.resolve();
      },
    },
    incoming,
    "alice"
  );
  return keys;
}

test("every event invalidates main's keys, minus the Inbox subtraction, plus the additions", () => {
  for (const incoming of representativeEvents) {
    const oracle = mainEventQueryKeys(incoming, "alice").map((key) => [...key]);
    const kept = subtractsInbox(incoming) ? oracle.filter((key) => key[0] !== "inbox") : oracle;
    expect(invalidatedKeys(incoming)).toEqual([...kept, ...additions(incoming)]);
  }
});

test("the subtraction removes the Inbox refresh and nothing else", () => {
  const subtracted = representativeEvents.filter(subtractsInbox);
  expect(subtracted.length).toBeGreaterThan(0);
  for (const incoming of subtracted) {
    const before = mainEventQueryKeys(incoming, "alice").map((key) => [...key]);
    expect(before).toContainEqual(["inbox"]);
    expect(invalidatedKeys(incoming)).not.toContainEqual(["inbox"]);
  }
});

test("an ask reply keeps the ask-thread key the delayed-Inbox freshness marker reads", () => {
  const keys = invalidatedKeys(event("comment.created", { id: "comment-1", ask_id: "ask-1" }));
  expect(keys).toContainEqual(["ask-thread", "ask-1"]);
  expect(keys).toContainEqual(["inbox"]);
});

test("SSE event prepends a newer event to the loaded log page", () => {
  const client = new QueryClient();
  client.setQueryData(["events", "CORE-1"], {
    pageParams: [null],
    pages: [[event("message.created", {}, { id: 7, seq: 7 })]],
  });
  prependEventToLog(client, event("message.created", {}, { id: 8, seq: 8 }));
  expect(client.getQueryData<{ pages: Event[][] }>(["events", "CORE-1"])?.pages[0]).toEqual([
    event("message.created", {}, { id: 8, seq: 8 }),
    event("message.created", {}, { id: 7, seq: 7 }),
  ]);
});

test("a log page that predates streamed events keeps them in front of it", () => {
  const seq = (n: number) => event("message.created", { body: `m${n}` }, { id: n, seq: n });
  expect(
    mergeEventPages(
      { pageParams: [null], pages: [[seq(9), seq(8), seq(7)]] },
      { pageParams: [null], pages: [[seq(7), seq(6)]] }
    )
  ).toEqual({ pageParams: [null], pages: [[seq(9), seq(8), seq(7), seq(6)]] });
  // A response at or beyond the cached head is the newer truth and replaces the page whole -
  // including an event the stream had at an older shape.
  const edited = event("message.created", { body: "m9 edited" }, { id: 9, seq: 9 });
  expect(
    mergeEventPages(
      { pageParams: [null], pages: [[seq(9), seq(8)]] },
      { pageParams: [null], pages: [[edited, seq(8), seq(7)]] }
    )
  ).toEqual({ pageParams: [null], pages: [[edited, seq(8), seq(7)]] });
  expect(mergeEventPages(undefined, { pageParams: [null], pages: [[seq(7)]] })).toEqual({
    pageParams: [null],
    pages: [[seq(7)]],
  });
});

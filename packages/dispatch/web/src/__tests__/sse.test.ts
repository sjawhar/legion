import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import {
  architectureSourcesQuery,
  inboxQuery,
  projectsQuery,
  userStateQuery,
} from "../api/queries";
import { coalescePrefixKeys } from "../api/query-refresh";
import { eventQueryKeys, mergeEventPages, prependEventToLog } from "../api/sse";
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
  "artifact.version": [
    event("artifact.version", { artifact_id: "artifact-1" }),
    // A project document whose new version cites an ask: the document has no issue, and the
    // ask's rows — its document's ask list and the Inbox — are what the write moved.
    event(
      "artifact.version",
      {
        artifact_id: "artifact-1",
        references_changed: [{ kind: "ask", id: "ask-9", artifact_id: "artifact-9" }],
      },
      documentOwner
    ),
  ],
  "artifact.approved": [
    event("artifact.approved", { artifact_id: "artifact-1", ask_id: "ask-1" }),
    event("artifact.approved", { artifact_id: "artifact-1", ask_id: "ask-1" }, documentOwner),
  ],
  "artifact.changes_requested": [
    event("artifact.changes_requested", { artifact_id: "artifact-1", ask_id: null }),
  ],
  "ask.opened": [
    event("ask.opened", { id: "ask-1", anchor: { artifact_id: "artifact-1" } }),
    event("ask.opened", { id: "ask-1", anchor: null }, documentOwner),
    // A question that cites an issue moves that issue's own header count.
    event("ask.opened", {
      id: "ask-1",
      anchor: null,
      references_changed: [{ kind: "issue", id: "CORE-4" }],
    }),
  ],
  // Every ask type carries a document-scoped row as well: the ask branch on that path is an
  // explicit member list, and only a document-scoped row reaches it, so without one per member
  // the oracle cannot see a member being dropped.
  "ask.anchor_refreshed": [
    event("ask.anchor_refreshed", { id: "ask-1", anchor: { artifact_id: "artifact-1" } }),
    event("ask.anchor_refreshed", { id: "ask-1", anchor: null }, documentOwner),
  ],
  "ask.edited": [
    event("ask.edited", { id: "ask-1", anchor: null }),
    event("ask.edited", { id: "ask-1", anchor: null }, documentOwner),
  ],
  "ask.answered": [
    event("ask.answered", { id: "ask-1", anchor: null }),
    event("ask.answered", { id: "ask-1", anchor: null }, documentOwner),
  ],
  "ask.resolved": [
    event("ask.resolved", { id: "ask-1", anchor: null }),
    event("ask.resolved", { id: "ask-1", anchor: null }, documentOwner),
  ],
  "ask.follower_added": [
    event("ask.follower_added", { ask_id: "ask-1", session_id: "s1" }),
    event("ask.follower_added", { ask_id: "ask-1", session_id: "s1" }, documentOwner),
  ],
  "ask.follower_removed": [
    event("ask.follower_removed", { ask_id: "ask-1", session_id: "s1" }),
    event("ask.follower_removed", { ask_id: "ask-1", session_id: "s1" }, documentOwner),
  ],
  "block.repaired": [event("block.repaired", { block_id: "b-1" })],
  "block.invalid": [event("block.invalid", { block_id: "b-1" })],
  "comment.created": [
    event("comment.created", { id: "comment-1", anchor: { artifact_id: "artifact-1" } }),
    event("comment.created", { id: "comment-1", ask_id: "ask-1", anchor: null }),
    event("comment.created", { id: "comment-1", anchor: null }, documentOwner),
    // A comment that replies to no ask but cites one: the Inbox row carrying that ask's count
    // moves, so the subtraction cannot apply, and only that ask's rows refresh.
    event("comment.created", {
      id: "comment-1",
      anchor: null,
      references_changed: [{ kind: "ask", id: "ask-9", issue_key: "CORE-4" }],
    }),
    // A comment citing an ask on a project document: its row is in that document's ask list.
    event("comment.created", {
      id: "comment-1",
      anchor: null,
      references_changed: [{ kind: "ask", id: "ask-9", artifact_id: "artifact-9" }],
    }),
    // A comment citing an issue moves no ask row, so the Inbox stays subtracted.
    event("comment.created", {
      id: "comment-1",
      anchor: null,
      references_changed: [{ kind: "issue", id: "CORE-4" }],
    }),
  ],
  "comment.anchor_refreshed": [
    event("comment.anchor_refreshed", { id: "comment-1", anchor: { artifact_id: "artifact-1" } }),
  ],
  "comment.delivery": [event("comment.delivery", { comment_id: "comment-1" })],
  "comment.answered": [event("comment.answered", { id: "comment-1", anchor: null })],
  "comment.resolved": [event("comment.resolved", { id: "comment-1", anchor: null })],
  "comment.reopened": [event("comment.reopened", { id: "comment-1", anchor: null })],
  "comment.edited": [
    event("comment.edited", { id: "comment-1", anchor: null }),
    // An edit that dropped the mention names the ask it stopped citing.
    event("comment.edited", {
      id: "comment-1",
      anchor: null,
      references_changed: [{ kind: "ask", id: "ask-9", issue_key: "CORE-4" }],
    }),
  ],
  "suggestion.accepted": [event("suggestion.accepted", { id: "comment-1", anchor: null })],
  "suggestion.rejected": [event("suggestion.rejected", { id: "comment-1", anchor: null })],
  "message.created": [
    event("message.created", {}),
    event("message.created", { target: "session:planner" }),
    event("message.created", { target: "session:planner" }, { artifact_id: null, issue_key: null }),
    event("message.created", {
      references_changed: [
        { kind: "issue", id: "CORE-4" },
        { kind: "ask", id: "ask-9", issue_key: "CORE-4" },
      ],
    }),
    // More counted targets than an event carries: nothing is named and every list refreshes.
    event("message.created", { references_changed_truncated: true }),
  ],
  "message.delivery": [event("message.delivery", { target: "session:planner" })],
  "message.answered": [
    event("message.answered", {}),
    event("message.answered", { in_reply_to: "message-1", thread_target: "session:planner" }),
    event("message.answered", { target: "session:planner", thread_target: "session:planner" }),
    event("message.answered", {
      references_changed: [{ kind: "ask", id: "ask-9", issue_key: "CORE-4" }],
    }),
  ],
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
 * The events whose server handler rewrites the reference index (`refs.Replace` / `refs.Stamp` in
 * `packages/envoy/internal/dispatch`) or moves a structural `graph_edges` row: an issue write
 * (its body, parent, or components), a child link, a document upload or version, an ask's
 * creation, edit, anchor refresh or follower change, and every comment or message body — an
 * answer's reply body is indexed exactly like any other write. Listed here independently of the
 * implementation, so a handler that starts or stops writing references fails this file.
 *
 * `architecture.synced` is deliberately absent: an import replaces every `part_of` and
 * `depends_on` row (`architecture/importer.go`), but those edges point at components and no
 * surface renders a component's backlinks, so nothing holds a stale panel to refresh. Add it
 * here the day a component page grows one.
 */
const referenceWrites: ReadonlySet<EventType> = new Set([
  "artifact.created",
  "artifact.version",
  "ask.anchor_refreshed",
  "ask.edited",
  "ask.follower_added",
  "ask.follower_removed",
  "ask.opened",
  "child.added",
  "child.removed",
  "comment.answered",
  "comment.anchor_refreshed",
  "comment.created",
  "comment.edited",
  "issue.created",
  "issue.updated",
  "message.answered",
  "message.created",
]);

/** The rows a changed target's backlink count sits on, written independently of `sse.ts`. */
function countedRowKeys(incoming: Event): unknown[][] {
  if (truncatedTargets(incoming)) {
    return [["issue"], ["asks"], ["ask-thread"], ["artifact"], ["inbox"]];
  }
  const keys: unknown[][] = [];
  for (const target of changedTargets(incoming)) {
    if (target.kind === "issue") {
      keys.push(["issue", target.id]);
      continue;
    }
    // An answered card reads its ask from the thread, which is fetched once and never goes
    // stale on its own.
    keys.push(["ask", target.id], ["ask-thread", target.id]);
    if (typeof target.issue_key === "string") {
      keys.push(["asks", target.issue_key]);
    } else if (typeof target.artifact_id === "string") {
      keys.push(["artifact", target.artifact_id, "asks"]);
    }
    // The Inbox lists every open ask, whoever owns it — including one on a project document.
    keys.push(["inbox"]);
  }
  return keys;
}

function rollupAdditions(incoming: Event): unknown[][] {
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
  // The conversation a reply belongs to is its thread root's, which main read only from the
  // reply's own `target` - absent on a session's reply to a message aimed at that session.
  const threadTarget = mainPayloadString(incoming, "thread_target");
  if (
    mainIsMessageEvent(incoming) &&
    threadTarget !== undefined &&
    threadTarget !== mainPayloadString(incoming, "target") &&
    threadTarget.startsWith("session:") &&
    threadTarget.length > "session:".length
  ) {
    return [["agents", threadTarget.slice("session:".length), "messages"]];
  }
  return [];
}

/** The targets the server named on this event; an event from before the field carries none. */
function changedTargets(incoming: Event): { [key: string]: unknown }[] {
  const payload: unknown = incoming.payload;
  if (typeof payload !== "object" || payload === null) {
    return [];
  }
  const changed: unknown = Reflect.get(payload, "references_changed");
  return Array.isArray(changed) ? (changed as { [key: string]: unknown }[]) : [];
}

function truncatedTargets(incoming: Event): boolean {
  const payload: unknown = incoming.payload;
  return typeof payload === "object" && payload !== null
    ? Reflect.get(payload, "references_changed_truncated") === true
    : false;
}

/** The documented subtraction: an issue-scoped message, or a comment that replies to no ask.
 *  It applies to the baseline; a write that cited an ask brings the Inbox back through the
 *  rows that carry that ask's count. */
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
  return eventQueryKeys(incoming, "alice").map((key) => [...key]);
}

/** The set of rows an event refreshes, independent of queue order and repetition. */
function keySet(keys: readonly (readonly unknown[])[]): string[] {
  return [...new Set(keys.map((key) => JSON.stringify(key)))].sort();
}

// One case per corpus entry, so a failure names the event and the rest still run.
const corpus = representativeEvents.map(
  (incoming, index) => [`${incoming.type}#${index}`, incoming] as const
);

test.each(
  corpus
)("%s maps to main's keys, minus the Inbox subtraction, plus the additions", (_name, incoming) => {
  const oracle = mainEventQueryKeys(incoming, "alice").map((key) => [...key]);
  const kept = subtractsInbox(incoming) ? oracle.filter((key) => key[0] !== "inbox") : oracle;
  // The rows a cited node's count sits on are appended once: a key the baseline already
  // refreshes is not repeated, which is what puts the Inbox back after the subtraction.
  const base = [...kept, ...rollupAdditions(incoming)];
  const counted = countedRowKeys(incoming);
  const references = referenceWrites.has(incoming.type) ? [["references"]] : [];
  // Which rows refresh is the contract; the order they are queued in and whether a key is
  // repeated are not, since the stream's pending map is keyed on the serialised key.
  expect(keySet(invalidatedKeys(incoming))).toEqual(keySet([...base, ...counted, ...references]));
});

test("the corpus contains events the subtraction applies to", () => {
  expect(representativeEvents.filter(subtractsInbox).length).toBeGreaterThan(0);
});

// A write that cited an ask restores the Inbox through the rows that carry its count, so the
// subtraction is observable only on the writes that cited nothing counted.
function keepsTheInboxSubtraction([, incoming]: (typeof corpus)[number]): boolean {
  return subtractsInbox(incoming) && countedRowKeys(incoming).length === 0;
}

test.each(
  corpus.filter(keepsTheInboxSubtraction)
)("%s loses the Inbox refresh and nothing else", (_name, incoming) => {
  const before = mainEventQueryKeys(incoming, "alice").map((key) => [...key]);
  expect(before).toContainEqual(["inbox"]);
  expect(invalidatedKeys(incoming)).not.toContainEqual(["inbox"]);
});

test("a queued key keeps only the broadest of its family, and never a sibling's", () => {
  expect(
    coalescePrefixKeys([
      ["issue", "CORE-1"],
      ["issue"],
      ["inbox"],
      ["artifact"],
      ["artifact-ref"],
      ["artifact", "a-1"],
    ])
  ).toEqual([["issue"], ["inbox"], ["artifact"], ["artifact-ref"]]);
  // Nothing is ever emptied: the shortest queued key of a family always survives.
  expect(coalescePrefixKeys([["issues"]])).toEqual([["issues"]]);
  // An element-wise comparison, so a longer key that only shares a string prefix stays.
  expect(coalescePrefixKeys([["ask"], ["ask-thread", "ask-1"]])).toEqual([
    ["ask"],
    ["ask-thread", "ask-1"],
  ]);
});

// An Inbox row carries its ask's `referenced_by_count`, so the subtraction's premise — a plain
// message cannot change an Inbox row — holds only while the write cited no ask. The refresh is
// the cited node's own rows, never a sweep of every list.
test("a write refreshes the rows of the nodes it cited, and a plain one refreshes none", () => {
  const askTarget = [{ kind: "ask", id: "ask-9", issue_key: "CORE-4" }];
  for (const cited of [
    event("message.created", { references_changed: askTarget }),
    event("comment.created", { id: "comment-1", anchor: null, references_changed: askTarget }),
  ]) {
    const keys = invalidatedKeys(cited);
    expect(keys).toContainEqual(["inbox"]);
    expect(keys).toContainEqual(["ask", "ask-9"]);
    expect(keys).toContainEqual(["asks", "CORE-4"]);
    // The cited issue's own list is never swept: only the rows that carry the moved count.
    expect(keys).not.toContainEqual(["issue"]);
    expect(keys).not.toContainEqual(["asks"]);
  }

  const citedIssue = invalidatedKeys(
    event("message.created", {
      references_changed: [{ kind: "issue", id: "CORE-4" }],
    })
  );
  expect(citedIssue).toContainEqual(["issue", "CORE-4"]);
  // No ask row moved, so #1248's Inbox saving still applies.
  expect(citedIssue).not.toContainEqual(["inbox"]);

  const truncated = invalidatedKeys(
    event("message.created", { references_changed_truncated: true })
  );
  expect(truncated).toContainEqual(["issue"]);
  expect(truncated).toContainEqual(["asks"]);
  expect(truncated).toContainEqual(["inbox"]);

  for (const plain of [
    event("message.created", {}),
    event("comment.created", { id: "comment-1", anchor: null }),
  ]) {
    const keys = invalidatedKeys(plain);
    expect(keys).not.toContainEqual(["inbox"]);
    expect(keys.some((key) => key[0] === "ask")).toBe(false);
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

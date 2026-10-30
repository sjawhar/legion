import { describe, expect, test } from "bun:test";
import {
  type Actor,
  type Agent,
  type Anchor,
  type AnchorInput,
  type AnswerAskInput,
  type ArtifactBlock,
  type ArtifactText,
  type Ask,
  type AskAnsweredEventPayload,
  AskEditedEventPayloadSchema,
  AskEventPayloadSchema,
  type AskRead,
  type AskThread,
  answerNotKept,
  type BlockTypeSchema,
  type ChangedReference,
  type Comment,
  CommentEventPayloadSchema,
  type CreateAgentMessageInput,
  type CreateCommentInput,
  type CreateMessageInput,
  type CreateProjectInput,
  DELIVERY_CAPABILITIES,
  type DeliveryCapability,
  DISPATCH_SERVER_STARTED_SUBJECT,
  type DispatchEvent,
  DispatchEventSchema,
  DispatchServerStartedPayloadSchema,
  DispatchTargetedCommentPayloadSchema,
  DispatchTargetedMessagePayloadSchema,
  type EditCommentInput,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_MAX,
  IDEMPOTENCY_KEY_PATTERN,
  IDEMPOTENCY_KEY_REUSED,
  type IdempotencyLandedCode,
  IssueDeployedEventPayloadSchema,
  IssueEventPayloadSchema,
  type ItemBlocks,
  type Message,
  MessageDeliveryEventPayloadSchema,
  type MessageDeliveryMode,
  MessageEventPayloadSchema,
  type MessageReplyInput,
  type MyAnswersResponse,
  type MyRepliesResponse,
  NO_ROUTE_RETRY_DELAYS_MS,
  type ReferenceChangesPayload,
  type ReplyBlock,
  ReplyBlockSchema,
  retriesUnderKey,
  retryNoRoute,
  type Sleep,
  serviceSubjectLabel,
  sleep,
  type UserStateUpdatedEventPayload,
  type UserThreadStateUpdatedEventPayload,
} from "./dispatch-api";

test("delivery modes are exactly the closed capability list", () => {
  expect([...DELIVERY_CAPABILITIES]).toEqual(["aside", "btw", "steer"]);
  // The event payload's delivery mode admits every capability and nothing else.
  for (const mode of DELIVERY_CAPABILITIES) {
    expect(MessageDeliveryEventPayloadSchema.parse({ delivery: mode }).delivery).toBe(mode);
  }
  expect(MessageDeliveryEventPayloadSchema.safeParse({ delivery: "shout" }).success).toBe(false);
  // A delivery mode is a capability and vice versa (compile-time).
  const asCapability: DeliveryCapability = "steer" satisfies MessageDeliveryMode;
  const asMode: MessageDeliveryMode = asCapability satisfies DeliveryCapability;
  expect(asMode).toBe("steer");
});

// A page mints a v4 UUID per press; a script may mint its own key in the same alphabet. The
// pattern is the whole header value, so a key with another character or one too long is refused
// before Dispatch looks it up.
test("an idempotency key is letters, digits, '.', '_', ':' and '-', at most 128 of them", () => {
  for (const key of ["0b9c1e7a-55f4-4d2e-9a6b-3c2f1d0e8a71", "cli:run-7.a_b", "k".repeat(128)]) {
    expect(IDEMPOTENCY_KEY_PATTERN.test(key)).toBe(true);
  }
  for (const key of ["", "k".repeat(IDEMPOTENCY_KEY_MAX + 1), "with space", "slash/key", "é"]) {
    expect(IDEMPOTENCY_KEY_PATTERN.test(key)).toBe(false);
  }
});

// The page and the `dispatch` command retry by this one rule, so a gateway's 408 is asked again
// under the key by both (the server stores no 408) and a 422 or a KEY_COMMITTED by neither.
describe("retriesUnderKey", () => {
  test("asks again after every answer Dispatch does not store, and after a key still in flight", () => {
    for (const status of [500, 502, 503, 504, 599, 408, 429]) {
      expect(retriesUnderKey(status, undefined)).toBe(true);
    }
    expect(retriesUnderKey(409, IDEMPOTENCY_KEY_IN_FLIGHT)).toBe(true);
  });

  test("takes every other answer as Dispatch's verdict", () => {
    for (const status of [200, 201, 204, 400, 401, 403, 404, 410, 413]) {
      expect(retriesUnderKey(status, undefined)).toBe(false);
    }
    expect(retriesUnderKey(409, IDEMPOTENCY_KEY_COMMITTED)).toBe(false);
    expect(retriesUnderKey(409, undefined)).toBe(false);
    expect(retriesUnderKey(422, IDEMPOTENCY_KEY_REUSED)).toBe(false);
  });
});

describe("answerNotKept", () => {
  const notKept = { code: IDEMPOTENCY_ANSWER_NOT_KEPT, error: "answer too large to keep" };

  test("is a replayed 2xx whose body names ANSWER_NOT_KEPT, with or without its reason", () => {
    expect(answerNotKept(201, true, notKept)).toBe(true);
    expect(answerNotKept(200, true, { code: IDEMPOTENCY_ANSWER_NOT_KEPT })).toBe(true);
  });

  test("is never an answer that was not replayed, a non-2xx, or another body", () => {
    // An answer run afresh is the resource itself, whatever its fields say.
    expect(answerNotKept(201, false, notKept)).toBe(false);
    expect(answerNotKept(409, true, notKept)).toBe(false);
    expect(answerNotKept(201, true, { code: "SOMETHING_ELSE" })).toBe(false);
    for (const body of [undefined, null, "ANSWER_NOT_KEPT", [IDEMPOTENCY_ANSWER_NOT_KEPT]]) {
      expect(answerNotKept(201, true, body)).toBe(false);
    }
  });

  test("names the two landed codes in one type", () => {
    const landed: readonly IdempotencyLandedCode[] = [
      IDEMPOTENCY_KEY_COMMITTED,
      IDEMPOTENCY_ANSWER_NOT_KEPT,
    ];
    expect(landed).toEqual(["KEY_COMMITTED", "ANSWER_NOT_KEPT"]);
  });
});

test("models Agent activity and open-ask aggregates", () => {
  const agent = {
    capabilities: ["btw"],
    dir: "/workspaces/planner",
    host: "example-host-a",
    last_activity: "2026-09-13T13:00:00Z",
    last_seen: 1_726_231_200_000,
    machine_id: "host-a",
    open_asks: 2,
    roles: ["planner"],
    session_id: "planner-session",
    title: "Planner",
  } satisfies Agent;

  expect(agent.open_asks).toBe(2);
  expect(agent.last_activity).toBe("2026-09-13T13:00:00Z");
});
test("accepts the typed artifact version event payload", () => {
  const event = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-09T00:00:00Z",
    id: 42,
    issue_key: "DSP-1",
    notify: true,
    payload: {
      artifact_id: "artifact-1",
      name: "spec.md",
      version: { number: 2, summary: null },
    },
    seq: 7,
    type: "artifact.version",
  };

  expect(DispatchEventSchema.safeParse(event)).toMatchObject({ success: true });
});

test("preserves labels on an issue update event payload", () => {
  expect(IssueEventPayloadSchema.parse({ labels: ["frontend", "urgent"] })).toEqual({
    labels: ["frontend", "urgent"],
  });
});

test("preserves a coarse priority on an issue update event payload", () => {
  expect(IssueEventPayloadSchema.parse({ priority: 0 })).toEqual({ priority: 0 });
  expect(IssueEventPayloadSchema.parse({ priority: null })).toEqual({ priority: null });
});

test("keeps what an issue write changed, its ancestors and its thread logins, and accepts an event without them", () => {
  const payload = {
    status: "done",
    changed: ["status"],
    ancestors: ["CORE-2", "CORE-1"],
    thread_logins: ["alice@example.com"],
  };
  expect(IssueEventPayloadSchema.parse(payload)).toEqual(payload);
  expect(IssueEventPayloadSchema.parse({ title: "Older event" })).toEqual({ title: "Older event" });
});

test("keeps the thread logins on comment, message and ask event payloads", () => {
  const thread_logins = ["alice@example.com", "bob@example.com"];
  expect(CommentEventPayloadSchema.parse({ id: "comment-1", thread_logins })).toMatchObject({
    thread_logins,
  });
  expect(MessageEventPayloadSchema.parse({ id: "message-1", thread_logins })).toMatchObject({
    thread_logins,
  });
  expect(AskEventPayloadSchema.parse({ opened_event_id: 3, thread_logins })).toMatchObject({
    thread_logins,
  });
  expect(MessageEventPayloadSchema.parse({ id: "message-2" }).thread_logins).toBeUndefined();
});

test("models the deployed event exactly as the delivery timeline writes it", () => {
  const payload = {
    completed_at: "2024-01-01T02:10:00Z",
    number: 501,
    repo: "acme/widgets",
    run_id: 900,
  };
  expect(IssueDeployedEventPayloadSchema.parse(payload)).toEqual(payload);
  const event: DispatchEvent = {
    actor: { id: "delivery-timeline", kind: "system" },
    created_at: "2024-01-01T03:00:00Z",
    id: 91,
    issue_key: "DEPL-1",
    notify: false,
    payload,
    seq: 4,
    type: "issue.deployed",
  };
  expect(event.type).toBe("issue.deployed");
  // Strict: a field the Go writer does not emit, a missing one, or a time that is not RFC 3339
  // is refused rather than passed through.
  expect(IssueDeployedEventPayloadSchema.safeParse({ ...payload, extra: true }).success).toBe(
    false
  );
  expect(IssueDeployedEventPayloadSchema.safeParse({ ...payload, run_id: undefined }).success).toBe(
    false
  );
  expect(
    IssueDeployedEventPayloadSchema.safeParse({ ...payload, completed_at: "yesterday" }).success
  ).toBe(false);
  expect(IssueDeployedEventPayloadSchema.safeParse({ ...payload, number: 0 }).success).toBe(false);
});

test("models Dispatch's announcement of its fleet's build and the subject it is published on", () => {
  expect(DISPATCH_SERVER_STARTED_SUBJECT).toBe("notifications.dispatch.server.started");
  const payload = {
    commit: "0123456789abcdef0123456789abcdef01234567",
    started_at: "2026-10-19T12:00:00Z",
  };
  expect(DispatchServerStartedPayloadSchema.parse(payload)).toEqual(payload);
  // An announcement names a commit: a fleet with an unstamped build is never announced, so an
  // empty commit is no announcement.
  expect(DispatchServerStartedPayloadSchema.safeParse({ ...payload, commit: "" }).success).toBe(
    false
  );
  expect(DispatchServerStartedPayloadSchema.safeParse({ ...payload, extra: true }).success).toBe(
    false
  );
  expect(
    DispatchServerStartedPayloadSchema.safeParse({ ...payload, started_at: "noon" }).success
  ).toBe(false);
});

test("the envelope Dispatch publishes for an announcement carries a payload this schema reads", async () => {
  // The fixture is shared with the Go writer (`packages/envoy/internal/dispatch/startup`, whose
  // test requires the envelope it builds to equal it) and the Legion daemon's intake test.
  const path = new URL(
    "../../daemon/internal/intake/testdata/dispatch/server-started.json",
    import.meta.url
  );
  const envelope = JSON.parse(await Bun.file(path).text());
  expect(envelope.topic).toBe(DISPATCH_SERVER_STARTED_SUBJECT);
  expect(DispatchServerStartedPayloadSchema.parse(JSON.parse(envelope.payload))).toEqual({
    commit: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c",
    started_at: "2026-10-27T12:00:00Z",
  });
});

test("models every supported typed-block content rule", () => {
  const types: readonly BlockTypeSchema[] = [
    { attributes: {}, content: "paragraph+", name: "paragraphs", render: "host" },
    { attributes: {}, content: "block+", name: "blocks", render: "host" },
    {
      attributes: {},
      content: "paragraph+ bullet_list?",
      name: "ask",
      render: "host",
    },
  ];

  expect(types.map((type) => type.content)).toEqual([
    "paragraph+",
    "block+",
    "paragraph+ bullet_list?",
  ]);
});
test("requires a positive opened event id on an ask event payload", () => {
  expect(AskEventPayloadSchema.safeParse({ question: "Q" }).success).toBe(false);
  expect(AskEventPayloadSchema.safeParse({ opened_event_id: 0, question: "Q" }).success).toBe(
    false
  );
  expect(AskEventPayloadSchema.safeParse({ opened_event_id: 12, question: "Q" }).success).toBe(
    true
  );
});

test("keeps the replaced answer on a changed ask answer event", () => {
  const parsed = AskEventPayloadSchema.parse({
    opened_event_id: 12,
    question: "Ship it?",
    answer: { selected: ["Ship"], text: null },
    previous_answer: {
      user: "alice@example.com",
      selected: ["Hold"],
      text: "Wait for review.",
      at: "2026-10-08T00:00:00Z",
    },
  });

  expect(parsed.previous_answer).toEqual({
    user: "alice@example.com",
    selected: ["Hold"],
    text: "Wait for review.",
    at: "2026-10-08T00:00:00Z",
  });
});

test("parses an ask the close of its issue withdrew", () => {
  const parsed = AskEventPayloadSchema.parse({
    opened_event_id: 12,
    question: "Ship it?",
    resolution: {
      kind: "withdrawn",
      reason: "Withdrawn by the close of DSP-1.",
      actor: { kind: "session", id: "legion-daemon:DSP" },
      at: "2026-10-25T00:00:00Z",
    },
  });

  expect(parsed.resolution?.kind).toBe("withdrawn");
});

test("models answer changes, ask answer history, and the answers page", () => {
  const answer = {
    user: "alice@example.com",
    selected: ["Ship"],
    text: null,
    at: "2026-10-08T00:01:00Z",
  };
  const ask = {
    id: "ask-1",
    issue_key: "CORE-1",
    author: { kind: "session", id: "session-1" },
    kind: "question",
    question: "Ship it?",
    options: [{ label: "Ship" }, { label: "Hold" }],
    multiple: false,
    urgency: "med",
    anchor: null,
    state: "answered",
    answer,
    opened_event_id: 7,
    created_at: "2026-10-08T00:00:00Z",
    edited_at: null,
  } satisfies Ask;
  const input = {
    selected: ["Hold"],
    expected_edited_at: null,
    expected_answer_at: answer.at,
  } satisfies AnswerAskInput;
  const payload = {
    ...ask,
    previous_answer: { ...answer, selected: ["Hold"] },
  } satisfies AskAnsweredEventPayload;
  const read = {
    ask,
    replies: [],
    edits: [],
    answers: [answer],
    followers: [],
    claimant: "session-claimant",
    route: "role:reviewer",
    route_status: "live",
    route_holder: "session-reviewer",
  } satisfies AskRead;
  const thread = {
    replies: [],
    edits: [],
    answers: [],
    followers: [],
    claimant: null,
    route: null,
    route_status: null,
    route_holder: null,
  } satisfies AskThread;
  const page = {
    rows: [
      {
        kind: "answer",
        at: answer.at,
        ask_id: ask.id,
        ref: "/issues/CORE-1/asks/ask-1",
        question: ask.question,
        ask_kind: ask.kind,
        ask_state: ask.state,
        edited_at: null,
        owner: { issue: { key: "CORE-1", title: "Release" } },
        answer,
        current: true,
      },
    ],
    total: 1,
    limit: 50,
    offset: 0,
  } satisfies MyAnswersResponse;

  expect([
    input.expected_answer_at,
    payload.previous_answer.selected,
    read.answers,
    thread.answers,
    page.rows[0]?.current,
  ]).toEqual([answer.at, ["Hold"], [answer], [], true]);
});

test("models the Replies page and the read mark's event", () => {
  const page = {
    rows: [
      {
        thread_id: "comment-1",
        kind: "comment",
        ref: "/issues/CORE-1/comments/comment-1",
        issue: { key: "CORE-1", title: "Release" },
        subject: "Ship it on Friday?",
        reply: {
          id: "comment-2",
          kind: "reply",
          author: { kind: "session", id: "session-1" },
          first_line: "Friday works.",
          created_at: "2026-10-12T00:00:00Z",
        },
      },
      {
        thread_id: "ask-1",
        kind: "ask",
        ref: "/issues/CORE-1/asks/ask-1",
        issue: { key: "CORE-1", title: "Release" },
        subject: "Which region?",
        reply: {
          id: "ask-1",
          kind: "answer",
          author: { kind: "user", id: "bob" },
          first_line: "eu-west-1 - closest to the users",
          created_at: "2026-10-12T00:00:30Z",
        },
      },
      {
        thread_id: "comment-3",
        kind: "comment",
        ref: "/projects/CORE/documents/runbook?comment=comment-3",
        document: { project: "CORE", slug: "runbook", name: "Runbook" },
        subject: "Is step one current?",
        reply: {
          id: "comment-4",
          kind: "reply",
          author: { kind: "session", id: "session-1" },
          first_line: "Yes.",
          created_at: "2026-10-12T00:01:00Z",
        },
      },
    ],
    total: 3,
    limit: 50,
    offset: 0,
  } satisfies MyRepliesResponse;
  const read = {
    login: "alice",
    thread_id: "comment-1",
  } satisfies UserThreadStateUpdatedEventPayload;
  const event = {
    id: 1,
    issue_key: null,
    project: "CORE",
    seq: 3,
    actor: { kind: "user", id: "alice" },
    notify: false,
    created_at: "2026-10-12T00:02:00Z",
    type: "user_thread_state.updated",
    payload: read,
  } satisfies DispatchEvent;

  expect([
    page.rows.map((row) => `${row.kind}:${row.reply.kind}`),
    event.payload.thread_id,
  ]).toEqual([["comment:reply", "ask:answer", "comment:reply"], "comment-1"]);
});

test("models a snooze's state event and a document named in references_changed", () => {
  // A snooze's event reaches every signed-in feed, so it carries the writer and the field alone.
  const snoozed = {
    login: "alice",
    changed: ["snoozed"],
  } satisfies UserStateUpdatedEventPayload;
  const event = {
    id: 2,
    issue_key: null,
    project: "CORE",
    seq: 4,
    actor: { kind: "user", id: "alice" },
    notify: false,
    created_at: "2026-10-12T00:03:00Z",
    type: "user_state.updated",
    payload: snoozed,
  } satisfies DispatchEvent;
  const references_changed = [
    { kind: "issue", id: "CORE-1" },
    {
      kind: "artifact",
      id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      ref: "dispatch://CORE/artifact/runbook",
    },
  ] satisfies ChangedReference[];

  expect(DispatchEventSchema.safeParse(event)).toMatchObject({ success: true });
  // A reader that declares no references_changed still reads the comment around a document entry.
  expect(CommentEventPayloadSchema.parse({ id: "comment-1", references_changed })).toMatchObject({
    id: "comment-1",
  });
  expect([event.payload.changed, references_changed[1]?.ref]).toEqual([
    ["snoozed"],
    "dispatch://CORE/artifact/runbook",
  ]);
});

test("a truncated write names the kinds it cut beside the targets of the others", () => {
  const changes = {
    references_changed: [{ kind: "issue", id: "CORE-1" }],
    references_changed_truncated: true,
    references_changed_truncated_kinds: ["artifact"],
  } satisfies ReferenceChangesPayload;
  // A reader validating a comment's payload still sees which kinds were cut and what was named.
  expect(CommentEventPayloadSchema.parse({ id: "comment-1", ...changes })).toMatchObject(changes);
});

test("preserves block-pinned and legacy anchors in ask event payloads", () => {
  const blockPinned = AskEventPayloadSchema.parse({
    opened_event_id: 12,
    anchor: { block_id: "block-1", mark_id: "mark-1", quote: "Selected text" },
  });
  const legacy = AskEventPayloadSchema.parse({
    opened_event_id: 13,
    anchor: { mark_id: "mark-2", quote: "Older text" },
  });
  const withDocument = AskEventPayloadSchema.parse({
    opened_event_id: 14,
    anchor_artifact: { name: "Spec", primary: true, project: "CORE", slug: "spec" },
  });

  expect(blockPinned.anchor).toEqual({
    block_id: "block-1",
    mark_id: "mark-1",
    quote: "Selected text",
  });
  expect(legacy.anchor).toEqual({ mark_id: "mark-2", quote: "Older text" });
  expect(withDocument.anchor_artifact).toEqual({
    name: "Spec",
    primary: true,
    project: "CORE",
    slug: "spec",
  });
});

test("models full orphaned anchor refresh events without a notification", () => {
  const actor: Actor = { id: "alice", kind: "user" };
  const anchor: Anchor = {
    artifact_id: "artifact-1",
    block_id: "block-1",
    mark_id: "mark-1",
    orphaned: true,
    quote: "Removed text",
    version: 3,
  };
  const ask: Ask = {
    anchor,
    anchor_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec" },
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-18T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 7,
    options: [{ label: "Review the revised plan" }],
    question: "The release is ready, but the revised plan needs review. How should we proceed?",
    state: "open",
    urgency: "med",
  };
  const comment: Comment = {
    anchor,
    author: { id: "session-1", kind: "session" },
    body: "This suggestion is now stale.",
    created_at: "2026-09-18T00:00:00Z",
    deliveries: [],
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    mentions: [],
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: { accepted: null, replace_with: "Replacement" },
    ask_id: null,
    turn: null,
  };
  const events: DispatchEvent[] = [
    {
      actor,
      created_at: "2026-09-18T00:01:00Z",
      id: 8,
      issue_key: "CORE-1",
      notify: false,
      payload: ask,
      seq: 8,
      type: "ask.anchor_refreshed",
    },
    {
      actor,
      created_at: "2026-09-18T00:01:01Z",
      id: 9,
      issue_key: "CORE-1",
      notify: false,
      payload: { ...comment, artifact_name: "spec.md", artifact_slug: "spec", project_key: "CORE" },
      seq: 9,
      type: "comment.anchor_refreshed",
    },
  ];

  expect(events.map((event) => event.type)).toEqual([
    "ask.anchor_refreshed",
    "comment.anchor_refreshed",
  ]);
});

test("accepts an artifact-owned ask edit event", () => {
  const event: DispatchEvent = {
    id: 72,
    issue_key: null,
    artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
    project: "CORE",
    seq: 4,
    type: "ask.edited",
    actor: { kind: "session", id: "session-1" },
    notify: false,
    created_at: "2026-09-11T04:05:29Z",
    payload: {
      id: "ask-1",
      issue_key: null,
      artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      author: { kind: "session", id: "session-1" },
      kind: "question",
      question: "The document is ready for readers. How should we publish it?",
      options: [],
      multiple: false,
      urgency: "med",
      anchor: null,
      state: "open",
      answer: null,
      opened_event_id: 3,
      created_at: "2026-09-11T04:00:00Z",
      edited_at: "2026-09-11T04:05:29Z",
      previous: {
        question: "The document needs a publication decision. How should we proceed?",
        options: [{ label: "Publish for readers" }],
        multiple: false,
        urgency: "med",
      },
      edited_by: { kind: "session", id: "session-1" },
    },
  };

  expect(DispatchEventSchema.safeParse(event)).toMatchObject({
    data: {
      artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      issue_key: null,
      type: "ask.edited",
    },
    success: true,
  });
});

test("models an invalid ask block event", () => {
  const event: DispatchEvent = {
    id: 73,
    issue_key: "DOC-1",
    seq: 5,
    type: "block.invalid",
    actor: { kind: "session", id: "session-1" },
    notify: false,
    created_at: "2026-09-13T16:30:00Z",
    payload: {
      block_id: "ask-block",
      version: 3,
      reason: 'ask block "ask-block" has an option without a label',
      disturbed_by: { kind: "session", id: "session-1" },
    },
  };

  expect(DispatchEventSchema.safeParse(event)).toMatchObject({
    data: { type: "block.invalid" },
    success: true,
  });
});

test("rejects edit history on non-edit ask events", () => {
  expect(
    AskEventPayloadSchema.safeParse({
      opened_event_id: 7,
      question: "Which API?",
      previous: {
        question: "Which transport?",
        options: [{ label: "HTTP" }],
        multiple: false,
        urgency: "med",
      },
    }).success
  ).toBe(false);
});

test("permits the actor supplied with an agent project creation request", () => {
  const input: CreateProjectInput = {
    actor: { id: "session-1", kind: "session" },
    key: "DSP",
    name: "Dispatch",
  };

  expect(input).toEqual({
    actor: { id: "session-1", kind: "session" },
    key: "DSP",
    name: "Dispatch",
  });
});

test("models anchors pinned to a block while preserving mark selectors", () => {
  const anchor: Anchor = {
    artifact_id: "artifact-1",
    block_id: "block-1",
    mark_id: "mark-1",
    version: 2,
    quote: "selected text",
    orphaned: false,
  };
  const quoteInput: AnchorInput = {
    artifact: "spec",
    quote: "selected text",
    occurrence: 1,
  };
  const markInput: AnchorInput = { artifact: "spec", mark_id: "mark-1" };

  expect({ anchor, quoteInput, markInput }).toEqual({
    anchor: {
      artifact_id: "artifact-1",
      block_id: "block-1",
      mark_id: "mark-1",
      version: 2,
      quote: "selected text",
      orphaned: false,
    },
    quoteInput: { artifact: "spec", quote: "selected text", occurrence: 1 },
    markInput: { artifact: "spec", mark_id: "mark-1" },
  });
});

test("models comment thread lifecycle events and author edits", () => {
  const actor: Actor = { id: "alice", kind: "user" };
  const comment: Comment = {
    anchor: null,
    author: actor,
    body: "Edited discussion",
    created_at: "2026-09-10T00:00:00Z",
    edited_at: "2026-09-10T00:02:00Z",
    id: "comment-1",
    issue_key: "DSP-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    ask_id: null,
    turn: null,
    mentions: [],
    deliveries: [],
  };
  const input: EditCommentInput = { body: "Edited discussion" };
  const reopened: DispatchEvent = {
    actor,
    created_at: "2026-09-10T00:03:00Z",
    id: 43,
    issue_key: "DSP-1",
    notify: true,
    payload: { ...comment, artifact_name: "" },
    seq: 8,
    type: "comment.reopened",
  };
  const edited: DispatchEvent = {
    actor,
    created_at: "2026-09-10T00:04:00Z",
    id: 44,
    issue_key: "DSP-1",
    notify: true,
    payload: { ...comment, artifact_name: "" },
    seq: 9,
    type: "comment.edited",
  };

  expect(DispatchEventSchema.safeParse(reopened)).toMatchObject({ success: true });
  expect(DispatchEventSchema.safeParse(edited)).toMatchObject({ success: true });
  expect(input).toEqual({ body: "Edited discussion" });
});

test("models comment mention inputs and hydrated delivery records", () => {
  const input = {
    body: "Please review this.",
    mentions: [{ target: "session:session-1" }],
    delivery: "aside",
  } satisfies CreateCommentInput;
  const eventPayload = {
    id: "comment-mention-1",
    issue_key: "DSP-1",
    artifact_id: null,
    author: { id: "alice", kind: "user" },
    body: "Please review this.",
    anchor: null,
    reply_to: null,
    ask_id: null,
    turn: null,
    resolved: false,
    resolved_by: null,
    resolved_at: null,
    edited_at: null,
    suggestion: null,
    created_at: "2026-09-18T00:00:00Z",
    mentions: [{ target: "session:session-1", delivery: "aside", session_id: "session-1" }],
    deliveries: [
      {
        comment_id: "comment-mention-1",
        target: "session:session-1",
        attempt: 1,
        delivery: "aside",
        session_id: "session-1",
        envelope_id: "envelope-1",
        state: "sent",
        error: null,
        resolve_error: null,
        reply_id: null,
        created_at: "2026-09-18T00:00:01Z",
      },
    ],
    artifact_name: "spec.md",
  };

  expect(input).toEqual({
    body: "Please review this.",
    mentions: [{ target: "session:session-1" }],
    delivery: "aside",
  });
  expect(CommentEventPayloadSchema.parse(eventPayload)).toMatchObject({
    mentions: eventPayload.mentions,
    deliveries: eventPayload.deliveries,
  });
});

test("keeps the comment id, author, and message id when parsing event payloads", () => {
  const actor: Actor = { id: "alice", kind: "user" };
  const comment: Comment = {
    anchor: null,
    author: actor,
    body: "Please update this.",
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id: "comment-1",
    issue_key: "DSP-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    ask_id: null,
    turn: null,
    mentions: [],
    deliveries: [],
  };

  expect(CommentEventPayloadSchema.parse({ ...comment, artifact_name: "spec.md" })).toMatchObject({
    id: "comment-1",
    author: actor,
    created_at: "2026-09-10T00:00:00Z",
  });
  expect(
    MessageEventPayloadSchema.parse({
      id: "message-1",
      issue_key: "DSP-1",
      author: actor,
      body: "The build is green.",
      created_at: "2026-09-10T00:00:00Z",
    })
  ).toMatchObject({ id: "message-1", author: actor });
});

test("parses a message reply's in_reply_to, reply_body preview, and thread target", () => {
  const actor: Actor = { id: "bob", kind: "user" };
  expect(
    MessageEventPayloadSchema.parse({
      id: "message-2",
      issue_key: "DSP-1",
      author: actor,
      body: "Sounds good.",
      in_reply_to: "message-1",
      reply_body: "The build is green.",
      created_at: "2026-09-10T00:00:01Z",
    })
  ).toMatchObject({ in_reply_to: "message-1", reply_body: "The build is green." });
  // MessageEventPayloadSchema strips what it does not declare, so a consumer parsing an
  // event through it loses any field missing from the schema.
  expect(
    MessageEventPayloadSchema.parse({
      id: "message-3",
      issue_key: "DSP-1",
      author: actor,
      body: "Noted.",
      in_reply_to: "message-2",
      thread_target: "session:s1",
      created_at: "2026-09-10T00:00:02Z",
    }).thread_target
  ).toBe("session:s1");
  expect(
    MessageEventPayloadSchema.parse({ id: "message-1", author: actor, body: "Root message." })
      .in_reply_to
  ).toBeUndefined();
});

test("a reply names the block it answers, and an event recorded before reads no block", () => {
  const actor: Actor = { id: "bob", kind: "user" };
  const commentInput: CreateCommentInput = {
    body: "On the second paragraph.",
    reply_to: "comment-1",
    reply_to_block: "b2",
  };
  const messageInput: CreateMessageInput = {
    body: "On two.",
    in_reply_to: "message-1",
    in_reply_to_block: "b2",
  };
  const agentInput: CreateAgentMessageInput = {
    body: "On two.",
    delivery: "steer",
    in_reply_to: "message-1",
    in_reply_to_block: "b2",
  };
  const replyInput: MessageReplyInput = {
    actor: { id: "s1", kind: "session" },
    attempt: 1,
    body: "On two.",
    in_reply_to_block: "b2",
  };
  expect([
    commentInput.reply_to_block,
    messageInput.in_reply_to_block,
    agentInput.in_reply_to_block,
    replyInput.in_reply_to_block,
  ]).toEqual(["b2", "b2", "b2", "b2"]);

  const block: ReplyBlock = {
    block_id: "b2",
    node: "comment/comment-1#b2",
    ref: "dispatch://DSP-1/comment/comment-1#b-b2",
    removed: false,
    text: "Beta.",
  };
  // A direct message's block has no address, so it carries no ref.
  const directBlock: ReplyBlock = {
    block_id: "b2",
    node: "message/message-1#b2",
    removed: true,
    text: "Two.",
  };
  const reply: Message = {
    author: actor,
    body: "On two.",
    created_at: "2026-09-10T00:00:01Z",
    deliveries: [],
    id: "message-2",
    in_reply_to: "message-1",
    issue_key: null,
    reply_block: directBlock,
    target: null,
  };
  const comment: Comment = {
    anchor: null,
    ask_id: null,
    author: actor,
    body: "On the second paragraph.",
    created_at: "2026-09-10T00:00:01Z",
    deliveries: [],
    edited_at: null,
    id: "comment-2",
    issue_key: "DSP-1",
    mentions: [],
    reply_block: block,
    reply_to: "comment-1",
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
  expect(ReplyBlockSchema.parse(block)).toEqual(block);
  expect(ReplyBlockSchema.parse(directBlock)).toEqual(directBlock);
  expect(ReplyBlockSchema.safeParse({ block_id: "b2", node: "comment/c#b2" }).success).toBe(false);

  expect(CommentEventPayloadSchema.parse(comment).reply_block).toEqual(block);
  expect(MessageEventPayloadSchema.parse(reply).reply_block).toEqual(directBlock);
  expect(MessageEventPayloadSchema.parse({ ...reply, reply_block: null }).reply_block).toBeNull();

  // An event recorded before replies named blocks carries no reply_block at all.
  const before = { id: "comment-3", author: actor, body: "Older.", reply_to: "comment-1" };
  expect(CommentEventPayloadSchema.parse(before).reply_block).toBeUndefined();
  expect(
    MessageEventPayloadSchema.parse({ id: "message-3", author: actor, in_reply_to: "message-1" })
      .reply_block
  ).toBeUndefined();
});

test("accepts complete targeted-message payloads while ignoring future fields", () => {
  const payload = {
    id: "11111111-1111-4111-8111-111111111111",
    issue_key: null,
    author: { id: "alice", kind: "user" },
    body: "Can this ship?",
    target: "session:ses_target",
    in_reply_to: null,
    deliveries: [],
    created_at: "2026-09-12T00:00:00Z",
  };

  expect(DispatchTargetedMessagePayloadSchema.parse(payload)).toEqual(payload);
  expect(DispatchTargetedMessagePayloadSchema.parse({ ...payload, future_field: true })).toEqual(
    payload
  );
});

test("retains the complete targeted-comment payload including document ownership", () => {
  const payload = {
    id: "22222222-2222-4222-8222-222222222222",
    issue_key: null,
    artifact_id: "artifact-1",
    author: { id: "alice", kind: "user" },
    body: "Please review this section.",
    reply_to: null,
    ask_id: null,
    deliveries: [],
    mentions: [],
    created_at: "2026-09-12T00:00:00Z",
    artifact_name: "spec.md",
    project_key: "CORE",
    artifact_slug: "spec",
  };

  expect(DispatchTargetedCommentPayloadSchema.parse(payload)).toEqual(payload);
});

test("preserves ask edit history in the event payload", () => {
  const payload = {
    opened_event_id: 7,
    question: "Which transport should we implement?",
    options: [{ label: "REST" }, { label: "gRPC" }],
    multiple: true,
    urgency: "high",
    answer: null,
    edited_at: "2026-09-11T03:26:00Z",
    previous: {
      question: "Which implementation?",
      options: [{ label: "HTTP" }, { label: "MCP" }],
      multiple: false,
      urgency: "low",
    },
    edited_by: { kind: "session", id: "session-1" },
  };

  expect(AskEditedEventPayloadSchema.parse(payload)).toEqual(payload);
});

test("requires previous fields on ask edit events", () => {
  expect(
    AskEditedEventPayloadSchema.safeParse({
      opened_event_id: 7,
      question: "Which API?",
      options: [],
      multiple: false,
      urgency: "med",
      edited_at: "2026-09-11T03:26:00Z",
      edited_by: { kind: "session", id: "session-1" },
    }).success
  ).toBe(false);
});

test("accepts a document event with an unlinked owner", () => {
  expect(
    DispatchEventSchema.safeParse({
      id: 12,
      issue_key: null,
      artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      project: "CORE",
      seq: 4,
      type: "artifact.version",
      actor: { kind: "session", id: "session-1" },
      notify: true,
      created_at: "2026-09-11T04:05:29Z",
      payload: {},
    }).success
  ).toBe(true);
});

test("models per-block anchor reference counts", () => {
  const block: ArtifactBlock = {
    from: 12,
    id: "block-1",
    references: { asks: 1, comments: 2 },
    to: 32,
    token: "sha256:block-token",
    type: "paragraph",
  };
  expect(block.references).toEqual({ asks: 1, comments: 2 });
});

test("keeps document read tokens optional for older Dispatch servers", () => {
  const block: ArtifactBlock = {
    from: 0,
    id: "legacy-block",
    references: { asks: 0, comments: 0 },
    to: 6,
    type: "paragraph",
  };
  const text: ArtifactText = { markdown: "legacy", version: null };

  expect(block.token).toBeUndefined();
  expect(text.token).toBeUndefined();
});

test("models a comment's and a direct message's blocks", () => {
  const comment = "11111111-1111-4111-8111-111111111111";
  const commentBlocks: ItemBlocks = {
    item: {
      kind: "comment",
      id: comment,
      issue_key: "CORE-1",
      ref: `dispatch://CORE-1/comment/${comment}`,
    },
    revision: 2,
    indexed: true,
    blocks: [
      {
        id: "b1",
        type: "paragraph",
        text: "Alpha 😀.",
        ref: `dispatch://CORE-1/comment/${comment}#b-b1`,
        from: 0,
        to: "Alpha 😀.".length,
        changed_revision: 1,
        changed_at: "2026-10-28T00:00:00Z",
      },
    ],
  };
  // A direct message has no address and no revision; neither has any of its blocks.
  const directBlocks: ItemBlocks = {
    item: { kind: "message", id: "33333333-3333-4333-8333-333333333333" },
    indexed: true,
    blocks: [{ id: "b1", type: "paragraph", text: "Where?", from: 0, to: 6 }],
  };
  // An item owed an index lists no block.
  const owed: ItemBlocks = { ...commentBlocks, indexed: false, blocks: [] };

  expect(commentBlocks.blocks[0]?.to).toBe(9);
  expect(directBlocks.revision).toBeUndefined();
  expect(directBlocks.blocks[0]?.ref).toBeUndefined();
  expect(owed.blocks).toEqual([]);
});

test("a Kubernetes service subject renders as namespace/name, and anything else renders whole", () => {
  // The namespace is the half that disambiguates: two clusters' `default` service
  // accounts are the same name and a different identity, and the audit trail has to
  // tell them apart.
  expect(serviceSubjectLabel("system:serviceaccount:legion:default")).toBe("legion/default");
  expect(serviceSubjectLabel("system:serviceaccount:kube-system:default")).toBe(
    "kube-system/default"
  );

  // Anything that is not exactly that shape is shown in full rather than guessed at:
  // truncating an unknown subject is how the ambiguity got in.
  expect(serviceSubjectLabel("spiffe://cluster.local/ns/legion/sa/worker")).toBe(
    "spiffe://cluster.local/ns/legion/sa/worker"
  );
  expect(serviceSubjectLabel("system:serviceaccount:legion")).toBe("system:serviceaccount:legion");
  expect(serviceSubjectLabel("system:serviceaccount::default")).toBe(
    "system:serviceaccount::default"
  );
  expect(serviceSubjectLabel("system:serviceaccount:legion:")).toBe(
    "system:serviceaccount:legion:"
  );
  expect(serviceSubjectLabel("system:serviceaccount:legion:default:extra")).toBe(
    "system:serviceaccount:legion:default:extra"
  );
  expect(serviceSubjectLabel("kubernetes-admin")).toBe("kubernetes-admin");
});

test("a client's wait ends when its timer fires", async () => {
  await expect(sleep(0, new AbortController().signal)).resolves.toBeUndefined();
});

test("a client's wait rejects at once with the reason its request was aborted for", async () => {
  const controller = new AbortController();
  const reason = new Error("a newer read replaced this one");
  const waiting = sleep(60_000, controller.signal);
  controller.abort(reason);
  await expect(waiting).rejects.toBe(reason);
});

test("a client's wait for a request already aborted rejects at once", async () => {
  const reason = new Error("the caller gave up");
  await expect(sleep(60_000, AbortSignal.abort(reason))).rejects.toBe(reason);
});

/** What a Dispatch process that has no route for the request answers, as a client recognises it:
 *  each client has its own error class, and `retryNoRoute` knows none of them. */
class NoRouteAnswer extends Error {}

/** A wait that returns at once, recording each wait it was asked for and the signal it was given. */
function recordedWait(): {
  wait: Sleep;
  waits: number[];
  signals: (AbortSignal | null | undefined)[];
} {
  const waits: number[] = [];
  const signals: (AbortSignal | null | undefined)[] = [];
  return {
    wait: async (ms, signal) => {
      waits.push(ms);
      signals.push(signal);
    },
    waits,
    signals,
  };
}

test("a no-route answer is attempted again, and the next answer resolves the call", async () => {
  const answers: (NoRouteAnswer | string)[] = [
    new NoRouteAnswer(),
    new NoRouteAnswer(),
    "the page",
  ];
  const recorded = recordedWait();
  const { signal } = new AbortController();
  let attempts = 0;

  const answer = await retryNoRoute(
    async () => {
      attempts += 1;
      const next = answers.shift();
      if (next === undefined || next instanceof Error) throw next ?? new Error("no answer left");
      return next;
    },
    (error) => error instanceof NoRouteAnswer,
    { exhausted: () => new Error("the attempts were not exhausted"), signal, sleep: recorded.wait }
  );

  expect(answer).toBe("the page");
  expect(attempts).toBe(3);
  // Waits of 1 s and 2 s, each jittered by a quarter either way, each given the caller's signal.
  expect(recorded.waits).toHaveLength(2);
  for (const [index, base] of [1_000, 2_000].entries()) {
    expect(recorded.waits[index]).toBeGreaterThanOrEqual(base * 0.75);
    expect(recorded.waits[index]).toBeLessThanOrEqual(base * 1.25);
  }
  expect(recorded.signals).toEqual([signal, signal]);
});

test("an answer that is not a no-route is thrown on at once", async () => {
  const failure = new Error("Dispatch refused the write");
  const recorded = recordedWait();
  let attempts = 0;

  await expect(
    retryNoRoute(
      async () => {
        attempts += 1;
        throw failure;
      },
      (error) => error instanceof NoRouteAnswer,
      { exhausted: () => new Error("the attempts were not exhausted"), sleep: recorded.wait }
    )
  ).rejects.toBe(failure);
  expect(attempts).toBe(1);
  expect(recorded.waits).toEqual([]);
});

test("every attempt answered with no route throws what the caller builds", async () => {
  const rollingRelease = new Error("a new release of Dispatch is in progress");
  const recorded = recordedWait();
  let attempts = 0;

  await expect(
    retryNoRoute(
      async () => {
        attempts += 1;
        throw new NoRouteAnswer();
      },
      (error) => error instanceof NoRouteAnswer,
      { exhausted: () => rollingRelease, sleep: recorded.wait }
    )
  ).rejects.toBe(rollingRelease);
  // One attempt more than the waits between them: waits of 1 s, 2 s and 4 s, each jittered.
  expect(attempts).toBe(NO_ROUTE_RETRY_DELAYS_MS.length + 1);
  expect([...NO_ROUTE_RETRY_DELAYS_MS]).toEqual([1_000, 2_000, 4_000]);
  expect(recorded.waits).toHaveLength(NO_ROUTE_RETRY_DELAYS_MS.length);
  for (const [index, base] of NO_ROUTE_RETRY_DELAYS_MS.entries()) {
    expect(recorded.waits[index]).toBeGreaterThanOrEqual(base * 0.75);
    expect(recorded.waits[index]).toBeLessThanOrEqual(base * 1.25);
  }
});

test("a request aborted during a wait rejects at once with the abort, and is not attempted again", async () => {
  const controller = new AbortController();
  const reason = new Error("a newer read replaced this one");
  let attempts = 0;

  await expect(
    retryNoRoute(
      async () => {
        attempts += 1;
        throw new NoRouteAnswer();
      },
      (error) => error instanceof NoRouteAnswer,
      {
        exhausted: () => new Error("the attempts were not exhausted"),
        signal: controller.signal,
        // The shared wait, with the caller's abort arriving once the wait has begun.
        sleep: (ms, signal) => {
          const waiting = sleep(ms, signal);
          controller.abort(reason);
          return waiting;
        },
      }
    )
  ).rejects.toBe(reason);
  expect(attempts).toBe(1);
});

test("a call with no wait of its own waits on a timer, which an aborted request rejects at once", async () => {
  const reason = new Error("the caller gave up");
  let attempts = 0;

  await expect(
    retryNoRoute(
      async () => {
        attempts += 1;
        throw new NoRouteAnswer();
      },
      (error) => error instanceof NoRouteAnswer,
      {
        exhausted: () => new Error("the attempts were not exhausted"),
        signal: AbortSignal.abort(reason),
      }
    )
  ).rejects.toBe(reason);
  expect(attempts).toBe(1);
});

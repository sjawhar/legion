import { expect, test } from "bun:test";
import {
  type Actor,
  type Agent,
  type Anchor,
  type AnchorInput,
  type ArtifactBlock,
  type ArtifactText,
  type Ask,
  AskEditedEventPayloadSchema,
  AskEventPayloadSchema,
  type BlockTypeSchema,
  type Comment,
  CommentEventPayloadSchema,
  type CreateCommentInput,
  type CreateProjectInput,
  DELIVERY_CAPABILITIES,
  type DeliveryCapability,
  type DispatchEvent,
  DispatchEventSchema,
  DispatchTargetedCommentPayloadSchema,
  DispatchTargetedMessagePayloadSchema,
  type EditCommentInput,
  IssueEventPayloadSchema,
  MessageDeliveryEventPayloadSchema,
  type MessageDeliveryMode,
  MessageEventPayloadSchema,
  type WhoamiResponse,
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

test("models Agent activity and open-ask aggregates", () => {
  const agent = {
    capabilities: ["btw"],
    dir: "/workspaces/planner",
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
    options: [{ label: "Ship" }],
    question: "Ship it?",
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
      question: "Publish?",
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
        question: "Draft?",
        options: [{ label: "Yes" }],
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

test("a session actor carries the service subject its token verified, and whoami reports it", () => {
  const actor: Actor = {
    id: "session-1",
    kind: "session",
    service: "system:serviceaccount:legion:legion-worker",
  };
  const verified: WhoamiResponse = {
    kind: "agent",
    owner: null,
    service: "system:serviceaccount:legion:legion-worker",
  };
  const shared: WhoamiResponse = { kind: "agent", owner: null, service: null };

  expect(actor).toEqual({
    id: "session-1",
    kind: "session",
    service: "system:serviceaccount:legion:legion-worker",
  });
  expect(
    [verified, shared].map((identity) => (identity.kind === "agent" ? identity.service : undefined))
  ).toEqual(["system:serviceaccount:legion:legion-worker", null]);
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

test("parses a message reply's in_reply_to and reply_body preview", () => {
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
  expect(
    MessageEventPayloadSchema.parse({ id: "message-1", author: actor, body: "Root message." })
      .in_reply_to
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

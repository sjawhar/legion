import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { dispatchIssueSubject, type Envelope } from "@legion/contracts";
import { decode } from "@toon-format/toon";
import {
  type DispatchDelivery,
  postDeliveryReply,
  renderInbound,
  replyWith,
  senderLabel,
} from "../delivery";

const reader = "01a01111-2222-7333-4444-555555555555";
const actor = { kind: "session", id: "session-1" };

const targetedDispatchPayload = readFileSync(
  new URL("../../../contracts/fixtures/dispatch-targeted-delivery.json", import.meta.url),
  "utf8"
);

const targetedMessageID = "11111111-1111-4111-8111-111111111111";
const targetedCommentID = "22222222-2222-4222-8222-222222222222";
const targetedCommentDeliveryID = "33333333-3333-4333-8333-333333333333";

function envelope(overrides: Partial<Envelope> = {}): Envelope {
  return {
    event_id: "event-1",
    source: "agent",
    source_event_id: "agent.ses_sender.event-1",
    topic: "notifications.agent.ses_target",
    dedupe_key: "agent.ses_target.event-1",
    issued_at: 1,
    payload_summary: "message",
    trace_id: "trace-1",
    ...overrides,
  };
}

function dispatchEvent(type: string, payload: object, eventActor = actor, notify = true): string {
  return JSON.stringify(
    envelope({
      event_id: "dispatch-1",
      source: "dispatch",
      source_event_id: "1",
      topic: dispatchIssueSubject("DSP-1", ">"),
      payload_summary: "Dispatch update",
      payload: JSON.stringify({
        id: 1,
        issue_key: "DSP-1",
        seq: 7,
        type,
        actor: eventActor,
        notify,
        created_at: "2026-09-09T00:00:00Z",
        payload,
      }),
    })
  );
}

const issue = { title: "Native Dispatch", status: "in_progress", route: "role:legion-controller" };
const openAsk = {
  id: "ask-1",
  issue_key: "DSP-1",
  author: actor,
  question: "Which API?",
  options: [{ label: "JSON" }, { label: "MCP" }],
  multiple: false,
  urgency: "med",
  opened_event_id: 7,
  anchor: {
    artifact_id: "artifact-1",
    block_id: "block-1",
    mark_id: "mark-1",
    quote: "Which API?",
    orphaned: false,
  },
  state: "open",
  answer: null,
  created_at: "2026-09-09T00:00:00Z",
};
const answeredAsk = {
  ...openAsk,
  state: "answered",
  answer: {
    user: { kind: "user", id: "sami" },
    selected: ["JSON"],
    text: "Use JSON HTTP.",
    at: "2026-09-09T00:01:00Z",
  },
};
const comment = {
  id: "comment-1",
  issue_key: "DSP-1",
  author: actor,
  body: "Please update this.",
  anchor: { artifact_id: "artifact-1", mark_id: "m-1", quote: "old line", orphaned: false },
  reply_to: "comment-0",
  resolved: false,
  suggestion: null,
  created_at: "2026-09-09T00:00:00Z",
  artifact_name: "spec.md",
};

const targetedComment = {
  ...comment,
  id: targetedCommentID,
  artifact_id: "artifact-1",
  deliveries: [],
  mentions: [],
  resolved_by: null,
  resolved_at: null,
  edited_at: null,
  ask_id: null,
  project_key: "CORE",
  artifact_slug: "spec",
  suppress_route: false,
  suppressed_authors: [],
};

function targetedCommentFrame(
  payload: Record<string, unknown> = targetedComment,
  delivery: Partial<{
    readonly attempt: number;
    readonly mode: "aside" | "btw" | "steer";
    readonly comment_id: string;
    readonly target: string;
  }> = {}
): string {
  return JSON.stringify({
    event: {
      id: 8,
      issue_key: "CORE-1",
      seq: 8,
      type: "comment.created",
      actor: { kind: "user", id: "alice" },
      notify: true,
      created_at: "2026-09-12T00:00:00Z",
      payload,
    },
    delivery: {
      attempt: 1,
      mode: "aside",
      comment_id: targetedCommentDeliveryID,
      target: "session:ses_target",
      ...delivery,
    },
  });
}

describe("inbound delivery policy", () => {
  test("labels agent, human, and other envelopes by session before source", () => {
    expect(senderLabel(envelope({ source: "agent", source_session: "ses_agent" }))).toBe(
      "ses_agent"
    );
    expect(senderLabel(envelope({ source: "human", source_session: "ses_human" }))).toBe(
      "ses_human"
    );
    expect(senderLabel(envelope({ source: "github" }))).toBe("github");
    expect(senderLabel(envelope({ source: "agent" }))).toBe("agent");
    expect(senderLabel(envelope({ source: "human" }))).toBe("human");
  });

  test("offers a reply hint only for an agent envelope with a source session", () => {
    expect(replyWith(envelope({ source: "agent", source_session: "ses_agent" }))).toEqual({
      tool: "envoy_send",
      args: { session_id: "ses_agent", in_reply_to: "event-1", message: "..." },
    });
    expect(replyWith(envelope({ source: "agent" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "human", source_session: "ses_human" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "human" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "github", source_session: "ses_github" }))).toBeUndefined();
    expect(
      replyWith(envelope({ source: "dispatch", source_session: "ses_dispatch" }))
    ).toBeUndefined();
  });
});

describe("renderInbound dispatch events", () => {
  test("renders ask.answered as the answer alone: no ask row, no restated owner", () => {
    const rendered = renderInbound(dispatchEvent("ask.answered", answeredAsk), reader);
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    // The asker wrote the question and `re:` names the ask, so the frame carries what moved
    // (who answered, which question, the answer) and never the ask row again.
    expect(decoded.envoy).toEqual({
      from: "dispatch",
      at: "1970-01-01T00:00:00Z",
      id: "dispatch-1",
      dispatch: {
        owner: "DSP-1",
        type: "ask.answered",
        actor: { kind: "session", id: "session-1" },
        ask: "dispatch://DSP-1/ask/ask-1",
        question: "Which API?",
        answer: "JSON - Use JSON HTTP.",
      },
    });
    for (const restated of ["payload:", "issue_key:", "opened_event_id", "options", "anchor"]) {
      expect(rendered.content).not.toContain(restated);
    }
  });

  test("preserves an anchor document in delivered ask events", () => {
    const anchorArtifact = { name: "Spec", primary: true, project: "DSP", slug: "spec" };
    const rendered = renderInbound(
      dispatchEvent("ask.opened", { ...openAsk, anchor_artifact: anchorArtifact }),
      reader
    );
    const decoded = decode(rendered.content) as { envoy: { dispatch: Record<string, unknown> } };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "ask.opened",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: "Which API?",
      options: ["JSON", "MCP"],
      quote: "Which API?",
      document: "DSP/spec",
    });
  });

  test("still heads a retained ask.answered envelope that carries the removed action kind", () => {
    // JetStream keeps 72 h of ask.* envelopes written before the action kind was folded into
    // questions; the inbound decoder must not drop their question/answer lines over a stale kind.
    const retained = {
      ...answeredAsk,
      kind: "action",
      options: [{ label: "Done" }, { label: "Can't" }],
      answer: { ...answeredAsk.answer, selected: ["Can't"], text: "No access." },
    };
    const rendered = renderInbound(dispatchEvent("ask.answered", retained), reader);
    const decoded = decode(rendered.content) as { envoy: { dispatch: Record<string, unknown> } };

    expect(decoded.envoy.dispatch).toMatchObject({
      type: "ask.answered",
      question: "Which API?",
      answer: "Can't - No access.",
    });
  });

  test("shows the ask's ref as 're:' when an answered ask is correlated to its ask id", () => {
    const correlated = JSON.parse(dispatchEvent("ask.answered", answeredAsk)) as Record<
      string,
      unknown
    >;
    const rendered = renderInbound(JSON.stringify({ ...correlated, in_reply_to: "ask-1" }), reader);
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };
    expect(decoded.envoy.re).toBe("dispatch://DSP-1/ask/ask-1");
    expect(rendered.content).not.toContain("re: Which API?");
    // Correlated: the ask is named once, by `re:`, never again inside the record.
    expect(decoded.envoy.dispatch).not.toHaveProperty("ask");
  });

  test("an uncorrelated ask event still names its ask, so two asks never render alike", () => {
    // The producer correlates answers and replies (in_reply_to) but not openings or edits;
    // without the ref the receiving session could not tell one new ask from another.
    const first = decode(renderInbound(dispatchEvent("ask.opened", openAsk), reader).content) as {
      envoy: { re?: string; dispatch: Record<string, unknown> };
    };
    const second = decode(
      renderInbound(dispatchEvent("ask.opened", { ...openAsk, id: "ask-2" }), reader).content
    ) as { envoy: { dispatch: Record<string, unknown> } };

    expect(first.envoy.re).toBeUndefined();
    expect(first.envoy.dispatch.ask).toBe("dispatch://DSP-1/ask/ask-1");
    expect(second.envoy.dispatch.ask).toBe("dispatch://DSP-1/ask/ask-2");
    expect(first.envoy.dispatch).not.toEqual(second.envoy.dispatch);
  });

  test("does not offer a reply hint for an ask.opened event", () => {
    const decoded = decode(renderInbound(dispatchEvent("ask.opened", openAsk), reader).content) as {
      envoy: Record<string, unknown>;
    };

    expect(decoded.envoy.reply_with).toBeUndefined();
  });

  test("renders a targeted Dispatch message with its reply path", () => {
    const rendered = renderInbound(
      JSON.stringify(
        envelope({
          source: "dispatch",
          payload: targetedDispatchPayload,
        })
      ),
      reader
    );
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(decoded.envoy.dispatch).toMatchObject({
      owner: "CORE-1",
      type: "message.created",
      actor: { kind: "user", id: "alice" },
      payload: { body: "Can this ship?" },
    });
    expect(decoded.envoy.dispatch).not.toHaveProperty("issue_key");
    expect(decoded.envoy.reply_with).toEqual({
      tool: "dispatch_message",
      args: { issue: "CORE-1", in_reply_to: targetedMessageID, body: "..." },
    });
    expect(rendered.delivery).toEqual({
      resource: "message",
      id: targetedMessageID,
      attempt: 1,
      mode: "btw",
      replyPath: `/api/v1/messages/${targetedMessageID}/reply`,
      replyFields: {},
      issueKey: "CORE-1",
      body: "Can this ship?",
    });
  });

  test("decodes a targeted comment with its comment reply address and hint", () => {
    const rendered = renderInbound(
      JSON.stringify(
        envelope({
          source: "dispatch",
          payload: targetedCommentFrame(),
        })
      ),
      reader
    );
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(rendered.malformedDelivery).toBeUndefined();
    expect(rendered.delivery).toEqual({
      resource: "comment",
      id: targetedCommentDeliveryID,
      attempt: 1,
      mode: "aside",
      replyPath: `/api/v1/comments/${targetedCommentDeliveryID}/reply`,
      replyFields: { target: "session:ses_target" },
      issueKey: "CORE-1",
      body: "Please update this.",
    });
    expect(decoded.envoy.reply_with).toEqual({
      tool: "dispatch_comment",
      args: { issue: "CORE-1", reply_to: targetedCommentID, body: "..." },
    });
  });

  test("reports malformed targeted comments through their delivery-derived reply address", async () => {
    const replies: Array<{ readonly path: string; readonly body: unknown }> = [];
    const server = Bun.serve({
      port: 0,
      fetch: async (request) => {
        replies.push({ path: new URL(request.url).pathname, body: await request.json() });
        return Response.json({});
      },
    });
    const { created_at: _createdAt, ...malformedPayload } = targetedComment;

    try {
      const rendered = renderInbound(
        JSON.stringify(
          envelope({
            source: "dispatch",
            payload: targetedCommentFrame({
              ...malformedPayload,
              id: "comment-payload-1",
            }),
          })
        ),
        reader
      );
      const rejected = rendered.rejectedDelivery;
      if (rejected === undefined) throw new Error("missing recoverable rejected delivery");

      await postDeliveryReply(
        { url: `http://127.0.0.1:${server.port}`, token: "reply-token" },
        reader,
        rejected,
        { error: "Invalid Dispatch targeted delivery frame" }
      );

      expect(replies).toEqual([
        {
          path: `/api/v1/comments/${targetedCommentDeliveryID}/reply`,
          body: {
            actor: { kind: "session", id: reader },
            attempt: 1,
            target: "session:ses_target",
            error: "Invalid Dispatch targeted delivery frame",
          },
        },
      ]);
    } finally {
      server.stop(true);
    }
  });

  test("rejects resource-crossing identifiers before targeted frames can issue replies", async () => {
    const paths: string[] = [];
    const server = Bun.serve({
      port: 0,
      fetch: (request) => {
        paths.push(new URL(request.url).pathname);
        return Response.json({});
      },
    });
    const message = JSON.parse(targetedDispatchPayload) as {
      event: { payload: { id: string } };
    };
    message.event.payload.id = "../comments/comment-1";
    const frames = [
      targetedCommentFrame(targetedComment, { comment_id: "../messages/message-1" }),
      JSON.stringify(message),
    ];

    try {
      for (const payload of frames) {
        const rendered = renderInbound(
          JSON.stringify(envelope({ source: "dispatch", payload })),
          reader
        );
        if (rendered.delivery !== undefined) {
          await postDeliveryReply(
            { url: `http://127.0.0.1:${server.port}`, token: "reply-token" },
            reader,
            rendered.delivery,
            { error: "Invalid Dispatch targeted delivery frame" }
          );
        }
      }

      expect(paths).toEqual([]);
    } finally {
      server.stop(true);
    }
  });

  test("constructs encoded reply paths from the declared delivery resource", async () => {
    const paths: string[] = [];
    const server = Bun.serve({
      port: 0,
      fetch: (request) => {
        paths.push(new URL(request.url).pathname);
        return Response.json({});
      },
    });
    const deliveries: DispatchDelivery[] = [
      {
        resource: "message",
        id: "../comments/comment-1",
        attempt: 1,
        mode: "aside",
        replyPath: "/api/v1/comments/comment-1/reply",
        replyFields: {},
        issueKey: "CORE-1",
        body: "message",
      },
      {
        resource: "comment",
        id: "../messages/message-1",
        attempt: 1,
        mode: "aside",
        replyPath: "/api/v1/messages/message-1/reply",
        replyFields: { target: "session:ses_target" },
        issueKey: "CORE-1",
        body: "comment",
      },
    ];

    try {
      for (const delivery of deliveries) {
        await postDeliveryReply(
          { url: `http://127.0.0.1:${server.port}`, token: "reply-token" },
          reader,
          delivery,
          { error: "Invalid Dispatch targeted delivery frame" }
        );
      }

      expect(paths).toEqual([
        "/api/v1/messages/..%2Fcomments%2Fcomment-1/reply",
        "/api/v1/comments/..%2Fmessages%2Fmessage-1/reply",
      ]);
    } finally {
      server.stop(true);
    }
  });

  test("renders an issue-less targeted BTW with a delivery reply address", () => {
    const frame = JSON.parse(targetedDispatchPayload) as {
      event: { issue_key: string | null; payload: { issue_key: string | null; target: string } };
    };
    frame.event.issue_key = null;
    frame.event.payload.issue_key = null;
    frame.event.payload.target = `session:${reader}`;
    const rendered = renderInbound(
      JSON.stringify(
        envelope({
          source: "dispatch",
          topic: `notifications.agent.${reader}`,
          payload: JSON.stringify(frame),
        })
      ),
      reader,
      `notifications.agent.${reader}`
    );
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(rendered.delivery).toMatchObject({
      resource: "message",
      id: targetedMessageID,
      attempt: 1,
      mode: "btw",
      replyPath: `/api/v1/messages/${targetedMessageID}/reply`,
      replyFields: {},
      issueKey: null,
    });
    expect(decoded.envoy.reply_with).toBeUndefined();
  });

  test("keeps rendering a targeted Dispatch message when its payload grows", () => {
    const extendedPayload = JSON.parse(targetedDispatchPayload) as {
      event: { payload: Record<string, unknown> };
    };
    extendedPayload.event.payload.future_field = "added by a newer Dispatch";
    const unchanged = renderInbound(
      JSON.stringify(envelope({ source: "dispatch", payload: targetedDispatchPayload })),
      reader
    );
    const rendered = renderInbound(
      JSON.stringify(envelope({ source: "dispatch", payload: JSON.stringify(extendedPayload) })),
      reader
    );

    expect(rendered.malformedDelivery).toBeUndefined();
    expect(rendered.content).toBe(unchanged.content);
    expect(rendered.delivery).toEqual(unchanged.delivery);
  });

  test("renders ask.answered's free-text answer even with no selected option", () => {
    const textOnlyAnswered = {
      ...openAsk,
      state: "answered",
      answer: {
        user: { kind: "user", id: "sami" },
        selected: [],
        text: "Neither; let's do a third thing.",
        at: "2026-09-09T00:01:00Z",
      },
    };
    const rendered = renderInbound(dispatchEvent("ask.answered", textOnlyAnswered), reader);
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(decoded.envoy).toEqual({
      from: "dispatch",
      at: "1970-01-01T00:00:00Z",
      id: "dispatch-1",
      dispatch: {
        owner: "DSP-1",
        type: "ask.answered",
        actor: { kind: "session", id: "session-1" },
        ask: "dispatch://DSP-1/ask/ask-1",
        question: "Which API?",
        answer: "Neither; let's do a third thing.",
      },
    });
  });

  test("does not flag the asker's session as foreign when the answer reaches another follower", () => {
    const asker = "01a0aaaa-bbbb-7ccc-dddd-0123456789ab";
    const answeredByAnotherFollower = {
      ...answeredAsk,
      author: { kind: "session", id: asker },
    };
    const rendered = renderInbound(
      dispatchEvent("ask.answered", answeredByAnotherFollower, { kind: "user", id: "sami" }),
      reader
    );

    expect(rendered.skip).toBe(false);
    expect(rendered.content).not.toContain("note:");
  });

  test("renders ask.resolved as a low-key Dispatch update", () => {
    const resolvedAsk = {
      ...openAsk,
      resolution: {
        actor: { kind: "session", id: "session-1" },
        at: "2026-09-10T00:01:00Z",
        kind: "retracted",
        reason: "A newer question supersedes this one.",
      },
      state: "resolved",
    };

    const decoded = decode(
      renderInbound(dispatchEvent("ask.resolved", resolvedAsk), reader).content
    ) as {
      envoy: { dispatch: Record<string, unknown> };
    };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "ask.resolved",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: "Which API?",
      resolved: "retracted: A newer question supersedes this one.",
    });
  });

  test("renders ask.edited with its current question and edit history", () => {
    const decoded = decode(
      renderInbound(
        dispatchEvent("ask.edited", {
          ...openAsk,
          question: "Which transport should we implement?",
          options: [{ label: "REST" }, { label: "gRPC" }],
          multiple: true,
          urgency: "high",
          edited_at: "2026-09-11T03:26:00Z",
          previous: {
            question: "Which API?",
            options: [{ label: "JSON" }, { label: "MCP" }],
            multiple: false,
            urgency: "med",
          },
          edited_by: actor,
        }),
        reader
      ).content
    ) as { envoy: { dispatch: Record<string, unknown> } };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "ask.edited",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: "Which transport should we implement?",
      options: ["REST", "gRPC"],
      quote: "Which API?",
      previous: "Which API?",
    });
  });
  test("renders an artifact-owned ask edit by project document", () => {
    const capturedArtifactAskEdit = JSON.stringify(
      envelope({
        event_id: "dispatch-72",
        source: "dispatch",
        source_event_id: "72",
        topic: "notifications.dispatch.document.CORE.design-notes.ask.edited",
        payload_summary: "CORE/design-notes ask edited",
        payload: JSON.stringify({
          id: 72,
          issue_key: null,
          artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
          project: "CORE",
          seq: 4,
          type: "ask.edited",
          actor,
          notify: true,
          created_at: "2026-09-11T04:05:29Z",
          payload: {
            ...openAsk,
            issue_key: null,
            artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
            question: "Publish?",
            edited_at: "2026-09-11T04:05:29Z",
            previous: {
              question: "Draft?",
              options: [{ label: "Yes" }],
              multiple: false,
              urgency: "med",
            },
            edited_by: actor,
          },
        }),
      })
    );

    const decoded = decode(renderInbound(capturedArtifactAskEdit, reader).content) as {
      envoy: { dispatch: Record<string, unknown> };
    };

    expect(decoded.envoy.dispatch).toMatchObject({
      owner: "CORE / design-notes",
      type: "ask.edited",
      ask: "dispatch://CORE/artifact/design-notes/ask/ask-1",
      question: "Publish?",
      previous: "Draft?",
    });
  });

  test("skips a non-notify document frame", () => {
    const raw = JSON.stringify(
      envelope({
        source: "dispatch",
        topic: "notifications.dispatch.document.CORE.runbook-md.artifact.version",
        payload: JSON.stringify({
          id: 72,
          issue_key: null,
          artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
          project: "CORE",
          seq: 4,
          type: "artifact.version",
          actor,
          notify: false,
          created_at: "2026-09-11T04:05:29Z",
          payload: {},
        }),
      })
    );

    expect(renderInbound(raw, reader)).toMatchObject({ skip: true, content: "" });
  });

  test("renders subscription.removed as a short plain-text notice, not the generic TOON envelope", () => {
    const rendered = renderInbound(
      dispatchEvent("subscription.removed", {
        session_id: reader,
        by: { kind: "user", id: "alice" },
        topics: ["notifications.dispatch.issue.DSP-1.>"],
      }),
      reader
    );

    expect(rendered).toMatchObject({ skip: false, content: "Unsubscribed from DSP-1 by alice" });
  });

  test("falls back to 'someone' when subscription.removed carries no actor", () => {
    const rendered = renderInbound(
      dispatchEvent("subscription.removed", { session_id: reader, topics: [] }),
      reader
    );

    expect(rendered.content).toBe("Unsubscribed from DSP-1 by someone");
  });

  test("renders nothing for a subscription.removed naming a different session — this event also reaches the issue's own topic, so every other subscriber sees nothing", () => {
    const rendered = renderInbound(
      dispatchEvent("subscription.removed", {
        session_id: "ses_someone_else",
        by: { kind: "user", id: "alice" },
        topics: ["notifications.dispatch.issue.DSP-1.>"],
      }),
      reader
    );

    expect(rendered).toMatchObject({ skip: true, content: "" });
  });

  test("renders ask.follower_added and ask.follower_removed as short notices for the named session only", () => {
    const human = { kind: "user", id: "alice" };
    expect(
      renderInbound(
        dispatchEvent(
          "ask.follower_added",
          { ask_id: "ask-1", session_id: reader, by: human },
          human
        ),
        reader
      )
    ).toMatchObject({
      skip: false,
      content:
        "Now following ask ask-1 on DSP-1 (added by alice): its answer and replies reach you directly; dispatch_follow unfollow to stop.",
    });
    expect(
      renderInbound(
        dispatchEvent(
          "ask.follower_removed",
          { ask_id: "ask-1", session_id: reader, by: human },
          human
        ),
        reader
      )
    ).toMatchObject({
      skip: false,
      content: "No longer following ask ask-1 on DSP-1 (removed by alice).",
    });
    expect(
      renderInbound(
        dispatchEvent(
          "ask.follower_removed",
          { ask_id: "ask-1", session_id: "ses_someone_else", by: human },
          human
        ),
        reader
      )
    ).toMatchObject({ skip: true, content: "" });
  });

  test("renders a comment.created reply to an ask as 're: <ask ref>' with the question head under dispatch", () => {
    const askID = "ask-1";
    const raw = JSON.stringify(
      envelope({
        event_id: "dispatch-2",
        source: "dispatch",
        source_event_id: "2",
        topic: "notifications.agent.session-asker",
        payload_summary: "DSP-1 comment created",
        in_reply_to: askID,
        payload: JSON.stringify({
          id: 2,
          issue_key: "DSP-1",
          seq: 8,
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          created_at: "2026-09-09T00:00:00Z",
          payload: { ...comment, ask_id: askID, ask_question: "Which API should we ship?" },
        }),
      })
    );

    const decoded = decode(renderInbound(raw, reader).content) as {
      envoy: Record<string, unknown> & { dispatch: Record<string, unknown> };
    };

    expect(decoded.envoy.re).toBe("dispatch://DSP-1/ask/ask-1");
    expect(decoded.envoy.dispatch.question).toBe("Which API should we ship?");
    expect(decoded.envoy.reply_with).toEqual({
      tool: "dispatch_comment",
      args: { issue: "DSP-1", reply_to_ask: "ask-1", body: "..." },
    });
  });

  test("gives ordinary comment events a reply_to hint", () => {
    const rendered = renderInbound(
      dispatchEvent("comment.created", { ...comment, ask_id: null }),
      reader
    );
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(rendered.delivery).toBeUndefined();

    expect(decoded.envoy.reply_with).toEqual({
      tool: "dispatch_comment",
      args: { issue: "DSP-1", reply_to: "comment-1", body: "..." },
    });
  });

  test("gives document-owned ask comments a project document reply hint and ask ref", () => {
    const raw = JSON.stringify(
      envelope({
        event_id: "dispatch-document-comment",
        source: "dispatch",
        topic: "notifications.agent.session-asker",
        in_reply_to: "ask-1",
        payload: JSON.stringify({
          id: 3,
          issue_key: null,
          artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
          project: "CORE",
          seq: 9,
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          created_at: "2026-09-09T00:00:00Z",
          payload: {
            ...comment,
            ask_id: "ask-1",
            project_key: "CORE",
            artifact_slug: "design-notes",
          },
        }),
      })
    );

    const decoded = decode(renderInbound(raw, reader).content) as {
      envoy: Record<string, unknown>;
    };

    expect(decoded.envoy.re).toBe("dispatch://CORE/artifact/design-notes/ask/ask-1");
    expect(decoded.envoy.reply_with).toEqual({
      tool: "dispatch_comment",
      args: { project: "CORE", artifact: "design-notes", reply_to_ask: "ask-1", body: "..." },
    });
  });

  test("a human reply on a still-open ask reaches the agent with the ask's state", () => {
    const askID = "ask-1";
    const raw = JSON.stringify(
      envelope({
        event_id: "dispatch-2b",
        topic: "notifications.dispatch.issue.DSP-1.comment.created",
        source: "dispatch",
        in_reply_to: askID,
        payload: JSON.stringify({
          id: 2,
          issue_key: "DSP-1",
          seq: 8,
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          created_at: "2026-09-09T00:00:00Z",
          payload: {
            ...comment,
            ask_id: askID,
            ask_question: "Which API should we ship?",
            ask_state: "open",
          },
        }),
      })
    );

    const decoded = decode(renderInbound(raw, reader).content) as {
      envoy: { dispatch: Record<string, unknown> };
    };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "comment.created",
      actor: { kind: "user", id: "alice" },
      question: "Which API should we ship?",
      reply: "Please update this.",
      state: "open",
    });
  });

  test("an agent's progress note on an ask renders as the reply with whose turn it is", () => {
    const decoded = decode(
      renderInbound(
        dispatchEvent("comment.created", {
          ...comment,
          ask_id: "ask-1",
          ask_question: "Ship it?",
          ask_state: "open",
          ask_waiting_on: "agent",
          turn: "agent",
        }),
        reader
      ).content
    ) as { envoy: { dispatch: Record<string, unknown> } };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "comment.created",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: "Ship it?",
      reply: "Please update this.",
      state: "open",
      waiting_on: "agent",
    });
  });

  test("renders a message.created reply as 're: <message ref>', not the parent's text", () => {
    const rootID = "message-1";
    const raw = JSON.stringify(
      envelope({
        event_id: "dispatch-3",
        source: "dispatch",
        source_event_id: "3",
        topic: "notifications.agent.session-writer",
        payload_summary: "DSP-1 message created",
        in_reply_to: rootID,
        payload: JSON.stringify({
          id: 3,
          issue_key: "DSP-1",
          seq: 9,
          type: "message.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          created_at: "2026-09-09T00:00:00Z",
          payload: {
            id: "message-2",
            issue_key: "DSP-1",
            author: { kind: "user", id: "alice" },
            body: "Sounds good.",
            in_reply_to: rootID,
            reply_body: "Ship the build tonight.",
            created_at: "2026-09-09T00:00:00Z",
          },
        }),
      })
    );

    const decoded = decode(renderInbound(raw, reader).content) as {
      envoy: Record<string, unknown> & { dispatch: { payload: Record<string, unknown> } };
    };

    expect(decoded.envoy.re).toBe("dispatch://DSP-1/message/message-1");
    expect(decoded.envoy.dispatch.payload.reply_body).toBe("Ship the build tonight.");
  });

  test("types each Dispatch event's nested payload by its wire-contract schema", () => {
    const cases: Array<{ type: string; payload: object; expectedPayload: unknown }> = [
      { type: "issue.updated", payload: issue, expectedPayload: issue },
      {
        type: "artifact.created",
        payload: { artifact: { id: "artifact-1", slug: "spec-md", name: "spec.md" } },
        expectedPayload: { artifact: { id: "artifact-1", slug: "spec-md", name: "spec.md" } },
      },
      {
        type: "artifact.version",
        payload: {
          artifact_id: "artifact-1",
          name: "spec.md",
          version: {
            number: 3,
            named: true,
            summary: "Clarify transport",
            authors: [actor],
            created_at: "2026-09-09T00:00:00Z",
          },
          diff: "@@ -1 +1 @@\n-MCP\n+JSON",
        },
        expectedPayload: {
          artifact_id: "artifact-1",
          name: "spec.md",
          version: { number: 3, summary: "Clarify transport" },
          diff: "@@ -1 +1 @@\n-MCP\n+JSON",
        },
      },
      {
        type: "comment.resolved",
        payload: { ...comment, resolved: true },
        expectedPayload: {
          id: "comment-1",
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: null,
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
      },
      {
        type: "comment.reopened",
        payload: { ...comment, resolved: false },
        expectedPayload: {
          id: "comment-1",
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: null,
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
      },
      {
        type: "comment.edited",
        payload: { ...comment },
        expectedPayload: {
          id: "comment-1",
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: null,
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
      },
      {
        type: "comment.anchor_refreshed",
        payload: { ...comment, anchor: { ...comment.anchor, orphaned: true } },
        expectedPayload: {
          id: "comment-1",
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { artifact_id: "artifact-1", mark_id: "m-1", orphaned: true },
          suggestion: null,
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
      },
      {
        type: "suggestion.accepted",
        payload: { ...comment, suggestion: { replace_with: "new line", accepted: true } },
        expectedPayload: {
          id: "comment-1",
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: { replace_with: "new line" },
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
      },
      {
        type: "message.created",
        payload: {
          id: "message-1",
          issue_key: "DSP-1",
          author: actor,
          body: "The build is green.",
          created_at: "2026-09-09T00:00:00Z",
        },
        expectedPayload: { id: "message-1", author: actor, body: "The build is green." },
      },
      {
        type: "message.created",
        payload: {
          id: "message-2",
          issue_key: "DSP-1",
          author: actor,
          body: "Sounds good.",
          in_reply_to: "message-1",
          reply_body: "The build is green.",
          created_at: "2026-09-09T00:00:00Z",
        },
        expectedPayload: {
          id: "message-2",
          author: actor,
          body: "Sounds good.",
          in_reply_to: "message-1",
          reply_body: "The build is green.",
        },
      },
      {
        type: "child.status",
        payload: { child_key: "DSP-2", from: "todo", to: "in_progress" },
        expectedPayload: { child_key: "DSP-2", from: "todo", to: "in_progress" },
      },
    ];

    for (const { type, payload, expectedPayload } of cases) {
      const decoded = decode(renderInbound(dispatchEvent(type, payload), reader).content) as {
        envoy: { dispatch: Record<string, unknown> };
      };
      expect(decoded.envoy.dispatch).toMatchObject({
        owner: "DSP-1",
        type,
        actor: { kind: "session", id: "session-1" },
        payload: expectedPayload,
      });
    }
  });

  test("surfaces the comment id before the body, and the message id, in rendered dispatch payloads", () => {
    const commentContent = renderInbound(
      dispatchEvent("comment.created", { ...comment }),
      reader
    ).content;
    expect(commentContent).toContain("comment-1");
    expect(commentContent.indexOf("comment-1")).toBeLessThan(commentContent.indexOf("body:"));

    const messageContent = renderInbound(
      dispatchEvent("message.created", {
        id: "message-1",
        issue_key: "DSP-1",
        author: actor,
        body: "The build is green.",
        created_at: "2026-09-09T00:00:00Z",
      }),
      reader
    ).content;
    expect(messageContent).toContain("message-1");
  });

  test("renders an unrecognized-shape dispatch payload as its raw structured object", () => {
    const rendered = renderInbound(
      JSON.stringify(
        envelope({
          source: "dispatch",
          payload: JSON.stringify({
            secret: "now visible as data, not prose",
            unexpected: { nested: true },
          }),
        })
      ),
      reader
    );
    const decoded = decode(rendered.content) as { envoy: { dispatch: unknown } };

    expect(decoded.envoy.dispatch).toEqual({
      secret: "now visible as data, not prose",
      unexpected: { nested: true },
    });
  });

  test("falls back to the generic summary path when a dispatch envelope has no payload", () => {
    const rendered = renderInbound(
      JSON.stringify(envelope({ source: "dispatch", payload_summary: "DSP-1 ask answered" })),
      reader
    );
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(decoded.envoy.summary).toBe("DSP-1 ask answered");
    expect(decoded.envoy.dispatch).toBeUndefined();
    expect(decoded.envoy.unrecognised).toBe("payload");
  });

  test("preserves punctuation and newlines in a dispatch message body through TOON", () => {
    const body = 'First line: has "quotes", commas,\nand a second line.';
    const decoded = decode(
      renderInbound(dispatchEvent("message.created", { body }), reader).content
    ) as { envoy: { dispatch: { payload: { body: string } } } };

    expect(decoded.envoy.dispatch.payload.body).toBe(body);
  });

  test("renders a non-notifying Dispatch event on an agent subject", () => {
    const rendered = renderInbound(
      dispatchEvent("message.created", { body: "Agent subject remains visible" }, actor, false),
      reader,
      `notifications.agent.${reader}`
    );

    expect(rendered.skip).toBe(false);
    expect(decode(rendered.content)).toMatchObject({
      envoy: {
        dispatch: { type: "message.created", payload: { body: "Agent subject remains visible" } },
      },
    });
  });

  test("renders a non-notifying Dispatch event on a role subject", () => {
    const rendered = renderInbound(
      dispatchEvent("message.created", { body: "Role subject remains visible" }, actor, false),
      reader,
      "notifications.role.legion-controller"
    );

    expect(rendered.skip).toBe(false);
    expect(decode(rendered.content)).toMatchObject({
      envoy: {
        dispatch: { type: "message.created", payload: { body: "Role subject remains visible" } },
      },
    });
  });

  test("drops an event that does not request an agent wake", () => {
    expect(
      renderInbound(
        dispatchEvent("message.created", { body: "Persist without steering" }, actor, false),
        reader
      )
    ).toMatchObject({ skip: true, content: "" });
  });

  test("drops an event authored by the reader session", () => {
    expect(
      renderInbound(
        dispatchEvent("message.created", { body: "Own update" }, { kind: "session", id: reader }),
        reader
      )
    ).toMatchObject({ skip: true, content: "" });
  });
});

describe("renderInbound non-dispatch envelopes", () => {
  test("keeps GitHub messages on the existing TOON rendering path", () => {
    expect(
      renderInbound(
        JSON.stringify(
          envelope({
            event_id: "github-comment-1",
            source: "github",
            source_event_id: "github.issue_comment.42",
            topic: "notifications.github.example-org.example-repo.issue.42.comment",
            dedupe_key: "github-comment-1",
            issued_at: Date.parse("2026-09-07T04:41:12Z"),
            payload_summary: "A comment was created on issue 42.",
            payload: JSON.stringify({ author: "Reviewer", body: "Ship it.", number: 42 }),
            trace_id: "trace-github-comment-1",
          })
        ),
        reader
      ).content
    ).toBe(
      [
        "envoy:",
        "  from: github",
        '  at: "2026-09-07T04:41:12Z"',
        "  id: github-comment-1",
        "  summary: A comment was created on issue 42.",
        "  message:",
        "    author: Reviewer",
        "    body: Ship it.",
        "    number: 42",
      ].join("\n")
    );
  });

  test("renders direct agent metadata with a one-line summary and no payload", () => {
    const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "agent-message-2",
        source: "agent",
        source_session: sender,
        source_event_id: "agent.message-2",
        topic: `notifications.agent.${reader}`,
        dedupe_key: "agent-message-2",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        expires_at: Date.parse("2026-09-07T05:00:00Z"),
        payload_summary: "First paragraph.",
        trace_id: "trace-agent-message-2",
        sender: { session_id: sender, title: "Reviewer", roles: ["legion-reviewer"] },
        in_reply_to: "agent-message-1",
        supersedes: "agent-message-0",
        urgency: "high",
        expects_reply: "required",
      }),
      reader
    );

    expect(rendered.content).toBe(
      [
        "envoy:",
        "  to: you (01a0…)",
        `  from: ${sender} (Reviewer)`,
        '  at: "2026-09-07T04:41:12Z"',
        "  id: agent-message-2",
        '  by: "2026-09-07T05:00:00Z"',
        "  urgency: high",
        "  expects_reply: required",
        "  re: agent-message-1",
        "  supersedes: agent-message-0",
        "  reply_with:",
        "    tool: envoy_send",
        "    args:",
        `      session_id: ${sender}`,
        "      in_reply_to: agent-message-2",
        "      message: ...",
        "  reply_role:",
        "    tool: envoy_publish",
        "    args:",
        "      topic: notifications.role.legion-reviewer",
        "      message: ...",
        "  summary: First paragraph.",
      ].join("\n")
    );
  });

  test("omits a summary duplicated by a longer agent message", () => {
    const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
    const head = "A".repeat(159);
    const body = `${head}B long first line continues past the truncation cap.\n\nSecond paragraph.`;
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "agent-message-3",
        source: "agent",
        source_session: sender,
        source_event_id: "agent.message-3",
        topic: `notifications.agent.${reader}`,
        dedupe_key: "agent-message-3",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: `${head}…`,
        payload: body,
        trace_id: "trace-agent-message-3",
      }),
      reader
    );

    expect(rendered.content).not.toMatch(/\n {2}summary:/);
    expect(rendered.content).toContain(`  message: "${body.replace(/\n/g, "\\n")}"`);
  });

  test("keeps a GitHub headline with a distinct structured payload", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "github-comment-2",
        source: "github",
        source_event_id: "github.issue_comment.43",
        topic: "notifications.github.example-org.example-repo.issue.43.comment",
        dedupe_key: "github-comment-2",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: "A comment was created on issue 43.",
        payload: JSON.stringify({ author: "Reviewer", body: "A comment was created", number: 43 }),
        trace_id: "trace-github-comment-2",
      }),
      reader
    );

    expect(rendered.content).toContain("  summary: A comment was created on issue 43.");
    expect(rendered.content).toContain("    body: A comment was created");
  });

  test("notes a foreign session named only in a payload", () => {
    const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
    const foreign = "01a0cccc-dddd-7eee-ffff-0123456789ab";
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "agent-message-foreign-session",
        source: "agent",
        source_session: sender,
        source_event_id: "agent.message-foreign-session",
        topic: `notifications.agent.${reader}`,
        dedupe_key: "agent-message-foreign-session",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: "The reviewer shared a detailed update.",
        payload: `Ask ${foreign} to confirm the release.`,
        trace_id: "trace-agent-message-foreign-session",
      }),
      reader
    );

    expect(rendered.content).toContain(
      `  note: body names session ${foreign}; the sender is ${sender}`
    );
  });

  test("renders recognized fields without raw unknown data", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "unknown-source-1",
        source: "newkind",
        topic: "notifications.example",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: "A future source delivered this summary.",
        unknown_field: "never show this raw value",
      }),
      reader
    );

    expect(rendered.content).toContain("  unrecognised: source=newkind");
    expect(rendered.content).not.toContain("never show this raw value");
    expect(rendered.content).not.toContain("unknown_field");
  });

  test("keeps a literal unknown payload when the summary is absent", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "payload-unknown-1",
        source: "agent",
        topic: "notifications.agent.ses_target",
        payload: "unknown",
      }),
      reader
    );

    expect(rendered.content).toContain("  summary: unknown");
    expect(rendered.content).toContain("  message: unknown");
  });

  test("keeps recognized fields when a JSON envelope omits its source", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "missing-source-1",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: "A sender was not provided.",
      }),
      reader
    );

    expect(rendered.content).toBe(
      [
        "envoy:",
        "  from: unknown",
        '  at: "2026-09-07T04:41:12Z"',
        "  id: missing-source-1",
        "  summary: A sender was not provided.",
        "  unrecognised: source",
      ].join("\n")
    );
  });

  test("keeps valid fields when a known field has the wrong type", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "invalid-timestamp-1",
        source: "agent",
        issued_at: "not-a-timestamp",
        payload_summary: "The timestamp was malformed.",
      }),
      reader
    );

    expect(rendered.content).toBe(
      [
        "envoy:",
        "  from: agent",
        "  at: unknown",
        "  id: invalid-timestamp-1",
        "  summary: The timestamp was malformed.",
        "  unrecognised: issued_at",
      ].join("\n")
    );
  });

  test("keeps valid sender fields when sender.roles is malformed", () => {
    const sender = "01a00000-0000-7000-0000-000000000001";
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "invalid-sender-1",
        source: "agent",
        source_session: sender,
        topic: "notifications.agent.ses_target",
        payload_summary: "The sender role was malformed.",
        sender: { title: "Reviewer", roles: "reviewer" },
      }),
      reader
    );

    expect(rendered.content).toContain(`  from: ${sender} (Reviewer)`);
    expect(rendered.content).toContain("  unrecognised: sender.roles");
  });

  test("hides a non-JSON frame and identifies its delivery subject", () => {
    const rendered = renderInbound("not json: secret raw bytes", reader, "notifications.example");

    expect(rendered).toEqual({
      skip: false,
      content: "envoy:\n  topic: notifications.example\n  unrecognised: payload was not JSON",
    });
    expect(rendered.content).not.toContain("secret raw bytes");
  });

  test("notes a foreign session named in the body", () => {
    const foreign = "01a0ffff-aaaa-7bbb-cccc-0123456789ab";
    const sender = "01a00000-0000-7000-0000-000000000001";
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "foreign-session-1",
        source: "agent",
        source_session: sender,
        topic: "notifications.agent.elsewhere",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: `Please contact ${foreign} before proceeding.`,
      }),
      reader
    );

    expect(rendered.content).toContain(
      `note: body names session ${foreign}; the sender is ${sender}`
    );
  });
});

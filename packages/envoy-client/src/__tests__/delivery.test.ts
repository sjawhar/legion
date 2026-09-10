import { describe, expect, test } from "bun:test";
import { dispatchIssueSubject, type Envelope } from "@legion/contracts";
import { decode } from "@toon-format/toon";
import { renderInbound, replyWith, senderLabel } from "../delivery";

const reader = "01a01111-2222-7333-4444-555555555555";
const actor = { kind: "session", id: "session-1" };

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
  anchor: null,
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
    expect(replyWith(envelope({ source: "agent", source_session: "ses_agent" }))).toBe(
      'envoy_send(session_id="ses_agent", message="...")'
    );
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
  test("renders ask.answered as TOON carrying the full inbound envelope contract", () => {
    const rendered = renderInbound(dispatchEvent("ask.answered", answeredAsk), reader);
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(decoded.envoy).toEqual({
      from: "dispatch",
      at: "1970-01-01T00:00:00Z",
      id: "dispatch-1",
      dispatch: {
        issue_key: "DSP-1",
        type: "ask.answered",
        actor: { kind: "session", id: "session-1" },
        payload: {
          opened_event_id: 7,
          question: "Which API?",
          options: [{ label: "JSON" }, { label: "MCP" }],
          answer: { selected: ["JSON"], text: "Use JSON HTTP." },
        },
      },
    });
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
        issue_key: "DSP-1",
        type: "ask.answered",
        actor: { kind: "session", id: "session-1" },
        payload: {
          opened_event_id: 7,
          question: "Which API?",
          options: [{ label: "JSON" }, { label: "MCP" }],
          answer: { selected: [], text: "Neither; let's do a third thing." },
        },
      },
    });
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
      issue_key: "DSP-1",
      type: "ask.resolved",
      actor: { kind: "session", id: "session-1" },
      payload: {
        opened_event_id: 7,
        question: "Which API?",
        options: [{ label: "JSON" }, { label: "MCP" }],
        answer: null,
        resolution: {
          actor: { kind: "session", id: "session-1" },
          at: "2026-09-10T00:01:00Z",
          kind: "retracted",
          reason: "A newer question supersedes this one.",
        },
      },
    });
  });

  test("renders a comment.created reply to an ask as 're: <question>', not the raw ask id", () => {
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
      envoy: Record<string, unknown>;
    };

    expect(decoded.envoy.re).toBe("Which API should we ship?");
  });

  test("types each Dispatch event's nested payload by its wire-contract schema", () => {
    const cases: Array<{ type: string; payload: object; expectedPayload: unknown }> = [
      { type: "issue.updated", payload: issue, expectedPayload: issue },
      {
        type: "artifact.created",
        payload: { artifact: { id: "artifact-1", name: "spec.md" } },
        expectedPayload: { artifact: { name: "spec.md" } },
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
          name: "spec.md",
          version: { number: 3, summary: "Clarify transport" },
          diff: "@@ -1 +1 @@\n-MCP\n+JSON",
        },
      },
      {
        type: "comment.resolved",
        payload: { ...comment, resolved: true },
        expectedPayload: {
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: null,
        },
      },
      {
        type: "suggestion.accepted",
        payload: { ...comment, suggestion: { replace_with: "new line", accepted: true } },
        expectedPayload: {
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: { replace_with: "new line" },
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
        expectedPayload: { body: "The build is green." },
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
      expect(decoded.envoy.dispatch).toEqual({
        issue_key: "DSP-1",
        type,
        actor: { kind: "session", id: "session-1" },
        payload: expectedPayload,
      });
    }
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
        `  reply_with: "envoy_send(session_id=\\"${sender}\\", message=\\"...\\")"`,
        '  reply_role: "envoy_publish(topic=\\"notifications.role.legion-reviewer\\", message=\\"...\\")"',
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

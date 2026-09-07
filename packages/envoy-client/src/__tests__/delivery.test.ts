import { describe, expect, test } from "bun:test";
import type { Envelope } from "@legion/contracts";
import { isOwnDispatchEcho, renderInbound, replyWith, senderLabel } from "../delivery";

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

describe("inbound delivery policy", () => {
  test("labels agent, human, and other envelopes by session before source", () => {
    expect(senderLabel(envelope({ source: "agent", source_session: "ses_agent" }))).toBe(
      "ses_agent"
    );
    expect(senderLabel(envelope({ source: "agent" }))).toBe("agent");
    expect(senderLabel(envelope({ source: "human", source_session: "ses_human" }))).toBe(
      "ses_human"
    );
    expect(senderLabel(envelope({ source: "human" }))).toBe("human");
    expect(senderLabel(envelope({ source: "github" }))).toBe("github");
  });

  test("offers a reply hint only for an agent envelope with a source session", () => {
    expect(replyWith(envelope({ source: "agent", source_session: "ses_agent" }))).toBe(
      'envoy_send(session_id="ses_agent", message="...")'
    );
    expect(replyWith(envelope({ source: "agent" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "human", source_session: "ses_human" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "human" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "github", source_session: "ses_github" }))).toBeUndefined();
  });

  test("recognizes only a GitHub dispatch echo for the matching session", () => {
    const matching = envelope({
      source: "github",
      payload: JSON.stringify({ dispatch_session: "ses_reader" }),
    });

    expect(isOwnDispatchEcho(matching, "ses_reader")).toBe(true);
    expect(isOwnDispatchEcho(matching, "ses_other")).toBe(false);
    expect(
      isOwnDispatchEcho(
        envelope({ source: "github", payload: JSON.stringify({ kind: "comment" }) }),
        "ses_reader"
      )
    ).toBe(false);
    expect(
      isOwnDispatchEcho(
        envelope({ source: "agent", payload: JSON.stringify({ dispatch_session: "ses_reader" }) }),
        "ses_reader"
      )
    ).toBe(false);
  });
});

describe("renderInbound", () => {
  const reader = "01a01111-2222-7333-4444-555555555555";

  test("renders a GitHub comment summary and structured payload once", async () => {
    const rendered = await renderInbound(
      JSON.stringify({
        event_id: "github-comment-1",
        source: "github",
        source_event_id: "github.issue_comment.42",
        topic: "notifications.github.example-org.example-repo.issue.42.comment",
        dedupe_key: "github-comment-1",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: "A comment was created on issue 42.",
        payload: JSON.stringify({ author: "Reviewer", body: "Ship it.", number: 42 }),
        trace_id: "trace-github-comment-1",
      }),
      reader
    );

    expect(rendered).toMatchObject({
      skip: false,
      content: [
        "envoy:",
        "  from: github",
        '  at: "2026-09-07T04:41:12Z"',
        "  id: github-comment-1",
        "  summary: A comment was created on issue 42.",
        "  message:",
        "    author: Reviewer",
        "    body: Ship it.",
        "    number: 42",
      ].join("\n"),
      envelope: expect.any(Object),
    });
    expect(rendered.content.match(/A comment was created on issue 42\./g)).toHaveLength(1);
  });

  test("renders direct agent metadata with a one-line summary and body once each", async () => {
    const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
    const summary = "First paragraph.";
    const body = "First paragraph.\n\nSecond paragraph.\n\nThird paragraph.";
    const rendered = await renderInbound(
      JSON.stringify({
        event_id: "agent-message-2",
        source: "agent",
        source_session: sender,
        source_event_id: "agent.message-2",
        topic: `notifications.agent.${reader}`,
        dedupe_key: "agent-message-2",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        expires_at: Date.parse("2026-09-07T05:00:00Z"),
        payload_summary: summary,
        payload: body,
        trace_id: "trace-agent-message-2",
        sender: { session_id: sender, title: "Reviewer", roles: ["legion-reviewer"] },
        in_reply_to: "agent-message-1",
        supersedes: "agent-message-0",
        urgency: "high",
        expects_reply: "required",
      }),
      reader
    );

    expect(rendered).toMatchObject({
      skip: false,
      content: [
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
        '  message: "First paragraph.\\n\\nSecond paragraph.\\n\\nThird paragraph."',
      ].join("\n"),
      envelope: expect.any(Object),
    });
    expect(rendered.content.match(/\n {2}summary:/g)).toHaveLength(1);
    expect(rendered.content.match(/\n {2}message:/g)).toHaveLength(1);
  });

  test("notes a foreign session named only in a payload", async () => {
    const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
    const foreign = "01a0cccc-dddd-7eee-ffff-0123456789ab";
    const rendered = await renderInbound(
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

  test("renders recognized fields without raw unknown data", async () => {
    const rendered = await renderInbound(
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

    expect(rendered).toMatchObject({
      skip: false,
      content: [
        "envoy:",
        "  from: newkind",
        '  at: "2026-09-07T04:41:12Z"',
        "  id: unknown-source-1",
        "  summary: A future source delivered this summary.",
        "  unrecognised: source=newkind",
      ].join("\n"),
      envelope: expect.any(Object),
    });
    expect(rendered.content).not.toContain("never show this raw value");
    expect(rendered.content).not.toContain("unknown_field");
  });

  test("keeps a literal unknown payload when the summary is absent", async () => {
    const rendered = await renderInbound(
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

  test("keeps recognized fields when a JSON envelope omits its source", async () => {
    const rendered = await renderInbound(
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

  test("keeps valid fields when a known field has the wrong type", async () => {
    const rendered = await renderInbound(
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

  test("keeps valid sender fields when sender.roles is malformed", async () => {
    const sender = "01a00000-0000-7000-0000-000000000001";
    const rendered = await renderInbound(
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

  test("hides a non-JSON frame and identifies its delivery subject", async () => {
    const rendered = await renderInbound(
      "not json: secret raw bytes",
      reader,
      "notifications.example"
    );

    expect(rendered).toEqual({
      skip: false,
      content: "envoy:\n  topic: notifications.example\n  unrecognised: payload was not JSON",
    });
    expect(rendered.content).not.toContain("secret raw bytes");
  });

  test("notes a foreign session named in the body", async () => {
    const foreign = "01a0ffff-aaaa-7bbb-cccc-0123456789ab";
    const sender = "01a00000-0000-7000-0000-000000000001";
    const rendered = await renderInbound(
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

  test("skips the reader's own GitHub dispatch echo", async () => {
    const rendered = await renderInbound(
      JSON.stringify({
        event_id: "dispatch-echo-1",
        source: "github",
        topic: `notifications.agent.${reader}`,
        payload_summary: "The reader opened this thread.",
        payload: JSON.stringify({ dispatch_session: reader }),
      }),
      reader
    );

    expect(rendered).toMatchObject({ skip: true });
  });
});

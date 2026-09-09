import { describe, expect, test } from "bun:test";
import type { Envelope } from "@legion/contracts";
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

function dispatchEvent(type: string, payload: object, eventActor = actor): string {
  return JSON.stringify(
    envelope({
      event_id: "dispatch-1",
      source: "dispatch",
      source_event_id: "1",
      topic: "notifications.dispatch.issue.DSP-1.>",
      payload_summary: "Dispatch update",
      payload: JSON.stringify({
        id: 1,
        issue_key: "DSP-1",
        seq: 7,
        type,
        actor: eventActor,
        notify: true,
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
  custom: true,
  urgency: "med",
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
  anchor: { artifact_id: "artifact-1", quote: "old line", from: 0, to: 8, orphaned: false },
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
  test("renders issue lifecycle event blocks", () => {
    for (const type of ["issue.created", "issue.updated", "issue.closed"]) {
      expect(renderInbound(dispatchEvent(type, issue), reader).content).toBe(
        [
          `dispatch DSP-1 · ${type} · by session session-1`,
          "Title: Native Dispatch",
          "Status: in_progress",
          "Route: role:legion-controller",
        ].join("\n")
      );
    }
  });

  test("renders artifact creation", () => {
    expect(
      renderInbound(
        dispatchEvent("artifact.created", { id: "artifact-1", name: "spec.md" }),
        reader
      ).content
    ).toBe(
      ["dispatch DSP-1 · artifact.created · by session session-1", "Artifact: spec.md"].join("\n")
    );
  });

  test("renders named artifact versions with their diff", () => {
    expect(
      renderInbound(
        dispatchEvent("artifact.version", {
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
        }),
        reader
      ).content
    ).toBe(
      [
        "dispatch DSP-1 · artifact.version · by session session-1",
        "Artifact: spec.md",
        "Version: 3",
        "Summary: Clarify transport",
        "Diff:",
        "@@ -1 +1 @@",
        "-MCP",
        "+JSON",
      ].join("\n")
    );
  });

  test("renders opened and answered asks", () => {
    expect(renderInbound(dispatchEvent("ask.opened", openAsk), reader).content).toBe(
      [
        "dispatch DSP-1 · ask.opened · by session session-1",
        "Question: Which API?",
        "Options: JSON, MCP",
      ].join("\n")
    );
    expect(renderInbound(dispatchEvent("ask.answered", answeredAsk), reader).content).toBe(
      [
        "dispatch DSP-1 · ask.answered · by session session-1",
        "Question: Which API?",
        "Selected: JSON",
        "Text: Use JSON HTTP.",
      ].join("\n")
    );
  });

  test("renders created and resolved comments with their anchor and reply chain", () => {
    for (const eventComment of [comment, { ...comment, resolved: true }]) {
      const type = eventComment.resolved ? "comment.resolved" : "comment.created";
      expect(renderInbound(dispatchEvent(type, eventComment), reader).content).toBe(
        [
          `dispatch DSP-1 · ${type} · by session session-1`,
          "Artifact: spec.md",
          "> old line",
          "Reply chain: comment-0",
          "Body: Please update this.",
        ].join("\n")
      );
    }
  });

  test("renders accepted and rejected suggestions as replacements", () => {
    for (const type of ["suggestion.accepted", "suggestion.rejected"]) {
      expect(
        renderInbound(
          dispatchEvent(type, {
            ...comment,
            suggestion: { replace_with: "new line", accepted: type === "suggestion.accepted" },
          }),
          reader
        ).content
      ).toBe(
        [
          `dispatch DSP-1 · ${type} · by session session-1`,
          "Artifact: spec.md",
          "> old line",
          "old line → new line",
        ].join("\n")
      );
    }
  });

  test("renders messages", () => {
    expect(
      renderInbound(
        dispatchEvent("message.created", {
          id: "message-1",
          issue_key: "DSP-1",
          author: actor,
          body: "The build is green.",
          created_at: "2026-09-09T00:00:00Z",
        }),
        reader
      ).content
    ).toBe(
      ["dispatch DSP-1 · message.created · by session session-1", "Body: The build is green."].join(
        "\n"
      )
    );
  });

  test("renders child status transitions", () => {
    expect(
      renderInbound(
        dispatchEvent("child.status", { child_key: "DSP-2", from: "todo", to: "in_progress" }),
        reader
      ).content
    ).toBe(
      [
        "dispatch DSP-1 · child.status · by session session-1",
        "Child: DSP-2 todo → in_progress",
      ].join("\n")
    );
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

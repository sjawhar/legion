import { beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import * as path from "node:path";
import type { SearchAsk, Suggestions, WriteAdvice, WriteSuggestion } from "@legion/contracts";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool, resetAdviceMemory } from "../dispatch-execute";

type AdviceFixture = WriteAdvice;

const config = { enabled: true, url: "http://dispatch.test", token: "secret", error: null };
const issueSuffix =
  "(not subscribed to DSP-42; envoy_subscribe notifications.dispatch.issue.DSP-42.> for every event on it)";

function response(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
  });
}

function repoExec(): ExecFn {
  return async (file, args) => {
    if (file === "jj" && args.join(" ") === "git remote list") {
      return { stdout: "origin https://github.com/owner/repo.git\n" };
    }
    throw new Error(`unexpected command: ${file} ${args.join(" ")}`);
  };
}

function withAdvice<T extends Record<string, unknown>>(
  body: T,
  advice: AdviceFixture | undefined
): T & { advice?: AdviceFixture } {
  return advice === undefined ? body : { ...body, advice };
}

async function executeWrite(tool: string, args: Record<string, unknown>, advice?: AdviceFixture) {
  const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const target = new URL(String(url));
    const method = init?.method ?? "GET";
    const body = init?.body === undefined ? {} : JSON.parse(String(init.body));

    if (target.pathname === "/api/v1/issues" && method === "POST") {
      return response(
        withAdvice(
          {
            key: "DSP-42",
            title: "Created issue",
            project: "DSP",
            components: {
              mode: "inherit",
              ids: [],
              unknown: [],
              reason: null,
              inherited_from: null,
            },
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/projects/DSP/architecture-sources" && method === "GET") {
      // No source configured is the list's own answer, not a refusal.
      return new Response("[]", { headers: { "Content-Type": "application/json" } });
    }
    if (target.pathname === "/api/v1/issues/DSP-42/artifacts" && method === "POST") {
      return response(
        withAdvice(
          {
            artifact: {
              id: "artifact-42",
              issue_key: "DSP-42",
              project: "DSP",
              name: String(body.name),
              slug: "spec",
              primary: body.name === "spec.md",
            },
            version: { number: 1 },
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/issues/DSP-42/messages" && method === "POST") {
      return response(withAdvice({ id: "message-1", issue_key: "DSP-42" }, advice));
    }
    if (target.pathname === "/api/v1/issues/DSP-42/comments" && method === "POST") {
      return response(
        withAdvice(
          {
            id: "comment-1",
            issue_key: "DSP-42",
            ask_id: body.ask_id ?? null,
            reply_to: null,
            turn: body.ask_id === undefined ? null : "human",
            ask_waiting_on: body.ask_id === undefined ? undefined : "human",
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/projects/DSP/artifacts" && method === "POST") {
      return response(
        withAdvice(
          {
            artifact: {
              id: "artifact-42",
              issue_key: null,
              project: "DSP",
              name: String(body.name),
              slug: "spec",
              primary: body.name === "spec.md",
            },
            version: { number: 1 },
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/projects/DSP/artifacts" && method === "GET")
      return response([]);
    if (target.pathname === "/api/v1/projects/DSP/artifacts/spec" && method === "GET") {
      return response({
        id: "artifact-42",
        issue_key: null,
        project: "DSP",
        ref_key: "DSP/spec",
        slug: "spec",
        name: "spec.md",
        kind: "doc",
        primary: true,
        created_by: { kind: "session", id: "session-1" },
        created_at: "2026-09-20T00:00:00Z",
        versions: [],
      });
    }
    if (target.pathname === "/api/v1/artifacts/artifact-42/comments" && method === "POST") {
      return response(
        withAdvice(
          {
            id: "comment-1",
            issue_key: null,
            ask_id: body.ask_id ?? null,
            reply_to: null,
            turn: body.ask_id === undefined ? null : "human",
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/issues/DSP-42/asks" && method === "POST") {
      return response(
        withAdvice(
          {
            id: "ask-new",
            issue_key: "DSP-42",
            question: body.question,
            urgency: body.urgency ?? "med",
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/issues/DSP-42" && method === "GET") {
      return response({
        key: "DSP-42",
        title: "Original title",
        status: "in_progress",
        labels: [],
        route: null,
        parent: null,
        external_links: [],
        primary_artifact_id: "artifact-42",
        artifacts: [
          {
            id: "artifact-42",
            issue_key: "DSP-42",
            project: "DSP",
            name: "spec.md",
            slug: "spec",
            primary: true,
          },
        ],
      });
    }
    if (target.pathname === "/api/v1/artifacts/artifact-42/edits" && method === "POST") {
      return response(withAdvice({ applied: 1, version: null, token: "sha256:edited" }, advice));
    }
    if (target.pathname === "/api/v1/issues/DSP-42" && method === "PATCH") {
      return response(
        withAdvice(
          {
            key: "DSP-42",
            title: body.title ?? "Original title",
            status: body.status ?? "in_progress",
            labels: [],
            route: null,
            parent: null,
            external_links: [],
          },
          advice
        )
      );
    }
    throw new Error(`unexpected request: ${method} ${target.pathname}`);
  };

  return executeDispatchTool({
    tool,
    args,
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-1",
    config,
    env: {},
    exec: repoExec(),
    fetchImpl: fetchImpl as typeof fetch,
    // A session the listener records with no topics: every write names its owner.
    subscriptions: async () => [],
  });
}

function advice(overrides: Partial<AdviceFixture> = {}): AdviceFixture {
  return {
    issue_status: "in_progress",
    session_writes_since_human: 0,
    your_open_asks: [],
    ...overrides,
  };
}

beforeEach(() => resetAdviceMemory());

describe("Dispatch write advice", () => {
  const zeroBlocks =
    "This spec holds no ask blocks. A person does not watch your chat. They see that you need them only through an open ask or a spec that waits for their approval. If the design needs a person's choice, add an ask block. If it does not, request approval with `dispatch request-approval`.";

  test.each([
    ["dispatch_issue", { project: "DSP", title: "Created issue", spec: "# Spec\n" }],
    ["dispatch_artifact", { issue: "DSP-42", name: "spec.md", content: "# Spec\n" }],
  ])("%s renders the decision-block pointer for a primary spec", async (tool, args) => {
    const rawAdvice = advice({ decision_blocks: 0 });
    const result = await executeWrite(tool, args, rawAdvice);

    expect(result.text).toEndWith(zeroBlocks);
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("renders only decision-block advice for a project document", async () => {
    const rawAdvice: WriteAdvice = { decision_blocks: 0 };
    const result = await executeWrite(
      "dispatch_artifact",
      { project: "DSP", name: "spec.md", content: "# Spec\n" },
      rawAdvice
    );

    expect(result.text).toEndWith(zeroBlocks);
    expect(result.text).not.toContain("messages on DSP");
    expect(result.text).not.toContain("still in triage");
    expect(result.text).not.toContain("still have an open ask");
    expect(result.details.advice).toEqual(rawAdvice);
  });
  test.each([
    [
      "a populated issue spec",
      "dispatch_issue",
      { project: "DSP", title: "Created issue", spec: "# Spec\n" },
      advice({ decision_blocks: 1 }),
    ],
    [
      "a non-primary artifact",
      "dispatch_artifact",
      { issue: "DSP-42", name: "notes.md", content: "# Notes\n" },
      advice({ decision_blocks: 0 }),
    ],
  ])("does not render decision-block advice for %s", async (_case, tool, args, rawAdvice) => {
    const result = await executeWrite(tool, args, rawAdvice);

    expect(result.text).not.toContain("holds no ask blocks");
    expect(result.details.advice).toEqual(rawAdvice);
  });

  // A spec whose questions are text says so before the count, which alone reads as a spec that
  // needs no decision (LEGION-416); a non-primary document gets the same line, and no count.
  test.each([
    [
      "a primary spec",
      "dispatch_issue",
      { project: "DSP", title: "Created issue", spec: "# Spec\n" },
      true,
    ],
    [
      "a non-primary artifact",
      "dispatch_artifact",
      { issue: "DSP-42", name: "notes.md", content: "# Notes\n" },
      false,
    ],
  ])("names typed-block openings stored as text in %s", async (_case, tool, args, primary) => {
    const rawAdvice = advice({
      decision_blocks: 0,
      unparsed_openers: {
        count: 4,
        examples: ['Intro 27::::ask{urgency="med"}', '…unmeasured. 57::::ask{urgency="low"}'],
      },
    });
    const result = await executeWrite(tool, args, rawAdvice);

    const openers =
      '4 typed-block openings in this document are text, not blocks: "Intro 27::::ask{urgency=\\"med\\"}", "…unmeasured. 57::::ask{urgency=\\"low\\"}". An opening like `:::ask{…}` makes a block only as a line of its own, so as text it asks nobody. Mentioning the syntax on purpose? Put it in code. See the `dispatch-writing-specs` skill, "Questions".';
    expect(result.text).toContain(openers);
    if (primary) {
      expect(result.text).toEndWith(`${openers}\n${zeroBlocks}`);
    } else {
      expect(result.text).toEndWith(openers);
    }
    expect(result.details.advice).toEqual(rawAdvice);
  });
  test("handles decision-only advice on a project-document ask reply", async () => {
    const ask = "01234567-0000-4000-8000-000000000042";
    const rawAdvice: WriteAdvice = { decision_blocks: 0 };
    const result = await executeWrite(
      "dispatch_comment",
      {
        project: "DSP",
        artifact: "spec",
        body: "Use JSON.",
        reply_to_ask: ask,
      },
      rawAdvice
    );

    expect(result.text).toContain(`Replied on ask ${ask}`);
    expect(result.text).not.toContain("messages on DSP");
    expect(result.text).not.toContain("still in triage");
    expect(result.text).not.toContain("still have an open ask");
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test.each([
    ["dispatch_message", { issue: "DSP-42", body: "Progress" }],
    ["dispatch_comment", { issue: "DSP-42", body: "Progress" }],
    ["dispatch_ask", { issue: "DSP-42", question: "What should we do?" }],
  ])("%s renders the cadence pointer after three session writes", async (tool, args) => {
    const rawAdvice = advice({ session_writes_since_human: 3 });
    const result = await executeWrite(tool, args, rawAdvice);

    expect(result.text).toEndWith(
      'Posted. You\'ve sent 3 messages on DSP-42 with no human response. Progress ledger or scratchpad? If so, stop. See the `dispatch` skill, "Structure over stream".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("escalates the cadence wording at six writes", async () => {
    const rawAdvice = advice({ session_writes_since_human: 6 });
    const result = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "Progress" },
      rawAdvice
    );

    expect(result.text).toEndWith(
      'Posted. You\'ve sent 6 messages on DSP-42 with no human response. Stop posting here until a human replies. See the `dispatch` skill, "Structure over stream".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("renders the triage pointer once per issue per process", async () => {
    const rawAdvice = advice({ issue_status: "triage" });
    const first = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "First" },
      rawAdvice
    );
    const second = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "Second" },
      rawAdvice
    );
    const line =
      'DSP-42 is still in triage — nobody can see its development status. See the `dispatch` skill, "Issue status is yours to move".';

    expect(first.text).toEndWith(line);
    expect(second.text).toBe(
      `Posted message message-1 (dispatch://DSP-42/message/message-1) ${issueSuffix}`
    );
    expect(first.details.advice).toEqual(rawAdvice);
    expect(second.details.advice).toEqual(rawAdvice);
  });

  test("drops the triage pointer when Legion owns the issue's status", async () => {
    const rawAdvice = advice({ issue_status: "triage", legion_managed_status: true });
    const result = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "First" },
      rawAdvice
    );

    expect(result.text).toBe(
      `Posted message message-1 (dispatch://DSP-42/message/message-1) ${issueSuffix}`
    );
    expect(result.details.advice).toEqual(rawAdvice);
    // The memo stays unmarked, so the line is still available if the issue leaves Legion's hands.
    const later = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "Second" },
      advice({ issue_status: "triage" })
    );
    expect(later.text).toContain("is still in triage");
  });

  // The absent case is the rest of this file's fixtures, which carry no field and print the line.
  test("renders the triage pointer when legion_managed_status is false", async () => {
    const rawAdvice = advice({ issue_status: "triage", legion_managed_status: false });
    const result = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "First" },
      rawAdvice
    );

    expect(result.text).toEndWith(
      'DSP-42 is still in triage — nobody can see its development status. See the `dispatch` skill, "Issue status is yours to move".'
    );
  });

  test("caps open-ask pointers at two and truncates questions to 80 characters", async () => {
    const rawAdvice = advice({
      your_open_asks: [
        { id: "ask-1", question: "A".repeat(81) },
        { id: "ask-2", question: "Second question" },
        { id: "ask-3", question: "Third question" },
      ],
    });
    const result = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "Update" },
      rawAdvice
    );

    expect(result.text).toBe(
      [
        `Posted message message-1 (dispatch://DSP-42/message/message-1) ${issueSuffix}`,
        `You still have an open ask on DSP-42: "${"A".repeat(80)}" (ask-1). Still needed? See the \`dispatch\` skill, "Close what you opened".`,
        'You still have an open ask on DSP-42: "Second question" (ask-2). Still needed? See the `dispatch` skill, "Close what you opened".',
      ].join("\n")
    );
    expect(result.text).not.toContain("ask-3");
    expect(result.details.advice).toEqual(rawAdvice);
  });
  test.each([
    ["dispatch_comment", { issue: "DSP-42", body: "Update" }],
    [
      "dispatch_doc_edit",
      {
        issue: "DSP-42",
        artifact: "spec",
        ops: [{ op: "replace", find: "draft", with: "final" }],
      },
    ],
    ["dispatch_issue_update", { issue: "DSP-42", title: "Renamed" }],
  ])("%s renders an open-ask pointer", async (tool, args) => {
    const rawAdvice = advice({
      your_open_asks: [{ id: "ask-1", question: "Choose the format?" }],
    });
    const result = await executeWrite(tool, args, rawAdvice);

    expect(result.text).toEndWith(
      'You still have an open ask on DSP-42: "Choose the format?" (ask-1). Still needed? See the `dispatch` skill, "Close what you opened".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("a reply to another ask still renders the caller's open ask", async () => {
    const replyTarget = "01234567-0000-4000-8000-000000000042";
    const rawAdvice = advice({
      your_open_asks: [
        { id: "aaaaaaaa-0000-4000-8000-000000000042", question: "Choose the format?" },
      ],
    });
    const result = await executeWrite(
      "dispatch_comment",
      { issue: "DSP-42", body: "JSON.", reply_to_ask: replyTarget },
      rawAdvice
    );

    expect(result.text).toEndWith(
      'You still have an open ask on DSP-42: "Choose the format?" (aaaaaaaa-0000-4000-8000-000000000042). Still needed? See the `dispatch` skill, "Close what you opened".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("an ask reply is exempt from cadence and its own open-ask pointer", async () => {
    const ownAsk = "01234567-0000-4000-8000-000000000042";
    const rawAdvice = advice({
      session_writes_since_human: 3,
      your_open_asks: [{ id: ownAsk, question: "Choose the format?" }],
    });
    const result = await executeWrite(
      "dispatch_comment",
      { issue: "DSP-42", body: "JSON.", reply_to_ask: ownAsk },
      rawAdvice
    );

    expect(result.text).toBe(
      `Replied on ask ${ownAsk} (comment comment-1; ask now waiting on human). ` +
        "You follow this ask: its answer and replies reach you directly. " +
        "For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>"
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("a status-setting issue update is exempt from the triage pointer", async () => {
    const rawAdvice = advice({ issue_status: "triage" });
    const result = await executeWrite(
      "dispatch_issue_update",
      { issue: "DSP-42", status: "testing" },
      rawAdvice
    );

    expect(result.text).toBe(`DSP-42: status in_progress -> testing ${issueSuffix}`);
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("a non-status issue update renders the triage pointer", async () => {
    const rawAdvice = advice({ issue_status: "triage" });
    const result = await executeWrite(
      "dispatch_issue_update",
      { issue: "DSP-42", title: "Renamed" },
      rawAdvice
    );

    expect(result.text).toEndWith(
      'DSP-42 is still in triage — nobody can see its development status. See the `dispatch` skill, "Issue status is yours to move".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test.each([
    // An issue or an ask from a server that sends no advice gets the not-made form: that server
    // ran no related-item check the reader can rely on.
    [
      "dispatch_issue",
      { project: "DSP", title: "Created issue" },
      `Created DSP-42: Created issue → http://dispatch.test/issues/DSP-42 ${issueSuffix}\n` +
        "No related-item check was made: the server sent no check result. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
      { issue: "DSP-42" },
    ],
    [
      "dispatch_artifact",
      { issue: "DSP-42", name: "spec.md", content: "# Spec\n" },
      `Uploaded spec.md as version 1 (artifact slug spec; dispatch://DSP-42/artifact/spec) ${issueSuffix}`,
      { issue: "DSP-42", artifact: "artifact-42", version: 1 },
    ],
    [
      "dispatch_message",
      { issue: "DSP-42", body: "Progress" },
      `Posted message message-1 (dispatch://DSP-42/message/message-1) ${issueSuffix}`,
      { issue: "DSP-42", message: "message-1" },
    ],

    [
      "dispatch_comment",
      { issue: "DSP-42", body: "Progress" },
      `Posted comment comment-1 ${issueSuffix}`,
      { issue: "DSP-42", comment: "comment-1" },
    ],
    [
      "dispatch_ask",
      { issue: "DSP-42", question: "What should we do?" },
      "Asked ask-new on DSP-42 (urgency med): What should we do?\n" +
        "No related-item check was made: the server sent no check result. Search before you rely on this being new: dispatch search --project DSP --query 'What should'\n" +
        "You follow this ask: its answer and replies reach you directly. " +
        "For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>",
      { issue: "DSP-42", ask: "ask-new", follows: { ask: "ask-new" } },
    ],
    [
      "dispatch_doc_edit",
      {
        issue: "DSP-42",
        artifact: "spec",
        ops: [{ op: "replace", find: "draft", with: "final" }],
      },
      `Applied 1 ops (no new version) ${issueSuffix}\nDocument token: sha256:edited`,
      { issue: "DSP-42", applied: 1, token: "sha256:edited" },
    ],
    [
      "dispatch_issue_update",
      { issue: "DSP-42", title: "Renamed" },
      `DSP-42: title "Renamed" ${issueSuffix}`,
      { issue: "DSP-42", status: "in_progress", external_links: [] },
    ],
  ])("%s renders its result when advice is absent", async (tool, args, text, details) => {
    const result = await executeWrite(tool, args);

    expect(result.text).toBe(text);
    expect(result.details).toEqual(details);
    expect(result.details).not.toHaveProperty("advice");
  });
});

const createdLine = `Created DSP-42: Created issue → http://dispatch.test/issues/DSP-42 ${issueSuffix}`;
const askedLine = "Asked ask-new on DSP-42 (urgency med): Should the billing service retry?";
const followsLine =
  "You follow this ask: its answer and replies reach you directly. " +
  "For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>";

function issueItem(key: string, title: string, status = "backlog"): WriteSuggestion {
  return {
    kind: "issue",
    owner: { kind: "issue", key, title, status },
    id: key,
    snippet: title,
    href: `/issues/${key}`,
  };
}

function askItem(key: string, id: string, ask?: SearchAsk): WriteSuggestion {
  return {
    kind: "ask",
    owner: { kind: "issue", key, title: "Billing retries", status: "in_progress" },
    id,
    snippet: "Should billing retry?",
    href: `/issues/${key}#ask-${id}`,
    ...(ask === undefined ? {} : { ask }),
  };
}

async function createIssue(suggestions: Suggestions | undefined, extra: Partial<WriteAdvice> = {}) {
  return executeWrite(
    "dispatch_issue",
    { project: "DSP", title: "Created issue" },
    advice({ ...(suggestions === undefined ? {} : { suggestions }), ...extra })
  );
}

describe("the related-item check", () => {
  test("dispatch_issue lists what it found, and how it searched, right under the created line", async () => {
    const suggestions: Suggestions = {
      outcome: "found",
      search: "meaning_and_keywords",
      related: [issueItem("DSP-7", "Existing issue", "triage")],
    };
    const result = await createIssue(suggestions);

    expect(result.text).toBe(
      [
        createdLine,
        "Related items (searched by meaning and keywords):",
        "- DSP-7 [triage] Existing issue → http://dispatch.test/issues/DSP-7",
      ].join("\n")
    );
    expect(result.details.advice).toEqual(advice({ suggestions }));
  });

  test("a keywords-only search names why in its header", async () => {
    const result = await createIssue({
      outcome: "found",
      search: "keywords_only",
      search_note: "meaning search is off on this server",
      related: [issueItem("DSP-7", "Existing issue")],
    });

    expect(result.text.split("\n")[1]).toBe(
      "Related items (searched by keywords only: meaning search is off on this server):"
    );
  });

  test.each([
    [
      { search: "meaning_and_keywords" } as const,
      "Related-item check searched (meaning and keywords) and found nothing.",
    ],
    [
      { search: "keywords_only", search_note: "meaning search is off on this server" } as const,
      "Related-item check searched (keywords only: meaning search is off on this server) and found nothing.",
    ],
  ])("a search that found nothing says so: %p", async (mode, line) => {
    const result = await createIssue({ outcome: "none", related: [], ...mode });

    expect(result.text).toBe([createdLine, line].join("\n"));
  });

  test("a check not made says why and names the search to run, quoting the server's words for a shell", async () => {
    const result = await createIssue({
      outcome: "not_made",
      related: [],
      missing: "the search behind it did not answer within 5s",
      search_query: "carrier's webhook retries",
    });

    expect(result.text).toBe(
      [
        createdLine,
        "No related-item check was made: the search behind it did not answer within 5s. Search before you rely on this being new: dispatch search --project DSP --query 'carrier'\\''s webhook retries'",
      ].join("\n")
    );
  });

  test.each([
    [
      "no check result",
      undefined,
      "No related-item check was made: the server sent no check result. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
    ],
    [
      "a missing reason",
      { related: [], missing: "the search behind it did not answer within 300ms" },
      "No related-item check was made: the search behind it did not answer within 300ms. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
    ],
    [
      "items and no outcome",
      { related: [issueItem("DSP-7", "Existing issue")] },
      "Related items:\n- DSP-7 [backlog] Existing issue → http://dispatch.test/issues/DSP-7",
    ],
    [
      "no items and no outcome",
      { related: [] },
      "No related-item check was made: the server did not say whether it searched. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
    ],
  ] as const)("an older server's %s reads as one of the forms", async (_shape, suggestions, form) => {
    const result = await createIssue(suggestions as Suggestions | undefined);

    expect(result.text).toBe([createdLine, form].join("\n"));
  });

  test("an ask item says where the ask stands", async () => {
    const answer = (selected: string[], text: string | null) => ({
      user: "alice",
      selected,
      text,
      at: "2026-09-12T13:20:00Z",
    });
    const resolution = {
      kind: "retracted" as const,
      reason: "Superseded.",
      actor: { kind: "user" as const, id: "bob" },
      at: "2026-09-13T08:00:00Z",
    };
    const result = await createIssue({
      outcome: "found",
      search: "meaning_and_keywords",
      related: [
        askItem("DSP-1", "a1", { state: "open" }),
        askItem("DSP-2", "a2", {
          state: "answered",
          answer: answer(["Exponential", "Capped"], "Back off.\nCap at an hour."),
        }),
        askItem("DSP-3", "a3", { state: "answered", answer: answer([], "Never retry.") }),
        askItem("DSP-4", "a4", { state: "resolved", resolution }),
        askItem("DSP-5", "a5", {
          state: "resolved",
          resolution: { ...resolution, kind: "resolved" },
          answer: answer(["Linear"], null),
        }),
        askItem("DSP-6", "a6"),
      ],
    });

    expect(result.text).toBe(
      [
        createdLine,
        "Related items (searched by meaning and keywords):",
        "- DSP-1 [in_progress] Billing retries → http://dispatch.test/issues/DSP-1#ask-a1",
        "  Ask: open",
        "- DSP-2 [in_progress] Billing retries → http://dispatch.test/issues/DSP-2#ask-a2",
        '  Ask: answered by alice at 2026-09-12T13:20:00Z, chose "Exponential", "Capped"',
        "  Answer text: Back off.",
        "    Cap at an hour.",
        "- DSP-3 [in_progress] Billing retries → http://dispatch.test/issues/DSP-3#ask-a3",
        "  Ask: answered by alice at 2026-09-12T13:20:00Z, chose no option",
        "  Answer text: Never retry.",
        "- DSP-4 [in_progress] Billing retries → http://dispatch.test/issues/DSP-4#ask-a4",
        "  Ask: closed with no answer: retracted",
        "- DSP-5 [in_progress] Billing retries → http://dispatch.test/issues/DSP-5#ask-a5",
        '  Ask: closed after an answer: resolved; last answered by alice at 2026-09-12T13:20:00Z, chose "Linear"',
        "  Answer text: none",
        // An older server sends no state; the item reads as it did.
        "- DSP-6 [in_progress] Billing retries → http://dispatch.test/issues/DSP-6#ask-a6",
      ].join("\n")
    );
  });

  test("a forced create names the titles it went past after the form", async () => {
    const result = await createIssue({
      outcome: "not_made",
      related: [],
      missing: "the search behind it failed",
      title_matches: [
        {
          key: "DSP-3",
          title: "Created issue",
          status: "todo",
          snippet: "Created issue",
          shared_terms: 2,
          href: "/issues/DSP-3",
        },
      ],
    });

    expect(result.text).toBe(
      [
        createdLine,
        "No related-item check was made: the search behind it failed. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
        "Title overlaps you forced past: DSP-3 [todo] Created issue → http://dispatch.test/issues/DSP-3",
      ].join("\n")
    );
  });

  test("a link that does not parse prints as the server sent it", async () => {
    const result = await createIssue({
      outcome: "found",
      search: "meaning_and_keywords",
      related: [{ ...issueItem("DSP-7", "Existing issue"), href: "http://[" }],
    });

    expect(result.text.split("\n")[2]).toBe("- DSP-7 [backlog] Existing issue → http://[");
  });

  test("advice that cannot be shown leaves the created line and the form, and says so", async () => {
    const result = await createIssue(
      { outcome: "none", search: "meaning_and_keywords", related: [] },
      // A malformed field makes the advice renderer throw.
      { unparsed_openers: { count: 2, examples: null as unknown as string[] } }
    );
    const lines = result.text.split("\n");

    expect(lines.slice(0, 2)).toEqual([
      createdLine,
      "Related-item check searched (meaning and keywords) and found nothing.",
    ]);
    expect(lines[2]).toStartWith("Other advice could not be shown: ");
    expect(lines).toHaveLength(3);
  });

  test("dispatch_ask prints the form between the asked line and the follow line, then a past decision", async () => {
    const decision: WriteSuggestion = {
      ...askItem("DSP-9", "ask-old", {
        state: "answered",
        answer: { user: "alice", selected: ["REST"], text: null, at: "2026-09-12T13:20:00Z" },
      }),
      answered_by: "alice",
      answered_at: "2026-09-12T13:20:00Z",
    };
    const result = await executeWrite(
      "dispatch_ask",
      { issue: "DSP-42", question: "Should the billing service retry?" },
      advice({
        suggestions: {
          outcome: "found",
          search: "meaning_and_keywords",
          related: [decision],
          decision,
        },
      })
    );

    expect(result.text).toBe(
      [
        askedLine,
        "Related items (searched by meaning and keywords):",
        "- DSP-9 [in_progress] Billing retries → http://dispatch.test/issues/DSP-9#ask-ask-old",
        '  Ask: answered by alice at 2026-09-12T13:20:00Z, chose "REST"',
        "  Answer text: none",
        "A past decision may already answer this: DSP-9 [in_progress] Billing retries, answered by alice on 2026-09-12T13:20:00Z → http://dispatch.test/issues/DSP-9#ask-ask-old",
        followsLine,
      ].join("\n")
    );
  });

  test("dispatch_ask still ends with the cadence line", async () => {
    const result = await executeWrite(
      "dispatch_ask",
      { issue: "DSP-42", question: "Should the billing service retry?" },
      advice({
        session_writes_since_human: 3,
        suggestions: { related: [], missing: "the search behind it did not answer within 300ms" },
      })
    );
    const lines = result.text.split("\n");

    expect(lines.slice(0, 3)).toEqual([
      askedLine,
      "No related-item check was made: the search behind it did not answer within 300ms. Search before you rely on this being new: dispatch search --project DSP --query 'Should billing service'",
      followsLine,
    ]);
    expect(lines.at(-1)).toStartWith("Posted. You've sent 3 messages on DSP-42");
  });
});

describe("dispatch_doc_edit's decision block checks", () => {
  const askBlock =
    ':::ask{urgency="med"}\nShould webhook deliveries back off exponentially?\n:::\n';
  const editArgs = {
    issue: "DSP-42",
    artifact: "spec",
    ops: [{ op: "insert", after: "end", markdown: askBlock }],
  };
  const appliedLine = `Applied 1 ops (no new version) ${issueSuffix}`;

  test("prints each new block's check under its id, after the applied line", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      advice({
        decision_blocks_added: 2,
        decision_block_checks: [
          {
            block_id: "ask-block-1",
            question: "Should webhook deliveries back off exponentially?",
            suggestions: {
              outcome: "found",
              search: "keywords_only",
              search_note: "meaning search is off on this server",
              related: [issueItem("DSP-7", "Webhook retries")],
            },
          },
          {
            block_id: "ask-block-2",
            question: "Which queue holds them?",
            suggestions: {
              outcome: "not_made",
              related: [],
              missing: "more than five new decision blocks in one edit",
            },
          },
        ],
      })
    );

    expect(result.text).toBe(
      [
        appliedLine,
        "Decision block ask-block-1:",
        "Related items (searched by keywords only: meaning search is off on this server):",
        "- DSP-7 [backlog] Webhook retries → http://dispatch.test/issues/DSP-7",
        "Decision block ask-block-2:",
        "No related-item check was made: more than five new decision blocks in one edit. Search before you rely on this being new: dispatch search --project DSP --query 'Which queue holds'",
        "Document token: sha256:edited",
      ].join("\n")
    );
  });

  test("an older server that only counts the new blocks gets one not-made line, searched by the block's question", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      advice({ decision_blocks_added: 1 })
    );

    expect(result.text).toBe(
      [
        appliedLine,
        "1 new decision block:",
        "No related-item check was made: the server sent no check result. Search before you rely on this being new: dispatch search --project DSP --query 'Should webhook deliveries'",
        "Document token: sha256:edited",
      ].join("\n")
    );
  });

  test("an older server's count with no block text in the edit searches by the issue's title", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      { issue: "DSP-42", artifact: "spec", ops: [{ op: "replace", find: "draft", with: "final" }] },
      advice({ decision_blocks_added: 1 })
    );

    expect(result.text.split("\n")[2]).toEndWith("--query 'Original title'");
  });

  test("an edit that added no block prints no check", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      advice({ decision_blocks_added: 0 })
    );

    expect(result.text).toBe([appliedLine, "Document token: sha256:edited"].join("\n"));
  });
});

describe("dispatch_related", () => {
  async function related(args: Record<string, unknown>, answer: Suggestions) {
    const requests: Array<{ method: string; path: string; body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      requests.push({
        method: init?.method ?? "GET",
        path: target.pathname,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      return response(answer);
    };
    const result = await executeDispatchTool({
      tool: "dispatch_related",
      args,
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: repoExec(),
      fetchImpl: fetchImpl as typeof fetch,
    });
    return { result, requests };
  }

  test("posts the title, text and switches to the project's related route and prints the form", async () => {
    const answer: Suggestions = {
      outcome: "found",
      search: "keywords_only",
      search_note: "the caller asked for keywords only",
      related: [issueItem("DSP-7", "Webhook retries")],
    };
    const { result, requests } = await related(
      {
        project: "DSP",
        title: "Retry failed webhook deliveries",
        text: "Back off exponentially.",
        exclude_issue: "DSP-9",
        keywords_only: true,
      },
      answer
    );

    expect(requests).toEqual([
      {
        method: "POST",
        path: "/api/v1/projects/DSP/related",
        body: {
          title: "Retry failed webhook deliveries",
          text: "Back off exponentially.",
          exclude_issue: "DSP-9",
          keywords_only: true,
        },
      },
    ]);
    expect(result.text).toBe(
      [
        "Related items (searched by keywords only: the caller asked for keywords only):",
        "- DSP-7 [backlog] Webhook retries → http://dispatch.test/issues/DSP-7",
      ].join("\n")
    );
    expect(result.details).toEqual({ project: "DSP", suggestions: answer });
  });

  test("a check not made names the project and the text's words to search by", async () => {
    const { result, requests } = await related(
      { project: "DSP", text: "the a of" },
      { outcome: "not_made", related: [], missing: "the text holds no word to search for" }
    );

    expect(requests.map(({ body }) => body)).toEqual([{ text: "the a of" }]);
    expect(result.text).toBe(
      "No related-item check was made: the text holds no word to search for. Search before you rely on this being new: dispatch search --project DSP with two or three of its words as --query"
    );
  });

  test("prints that it found nothing", async () => {
    const { result } = await related(
      { project: "DSP", title: "Quantum teleportation" },
      { outcome: "none", search: "meaning_and_keywords", related: [] }
    );

    expect(result.text).toBe(
      "Related-item check searched (meaning and keywords) and found nothing."
    );
  });
});

// A notice that cites a skill section sends the agent to read it, so each cited section must be a
// `## ` heading of that skill's SKILL.md, read as dispatch-tools.test.ts reads the Dispatch skill's.
test("every skill section a notice cites is a heading of that skill", () => {
  const repoRoot = path.resolve(import.meta.dir, "../../../..");
  const source = readFileSync(path.resolve(import.meta.dir, "../dispatch-execute.ts"), "utf8");
  const citations = [...source.matchAll(/the \\?`([a-z0-9-]+)\\?` skill, "([^"]+)"/gu)].map(
    ([, skill, section]) => ({ skill, section })
  );
  expect(citations.length).toBeGreaterThan(0);
  for (const { skill, section } of citations) {
    const headings = readFileSync(path.join(repoRoot, "skills", skill, "SKILL.md"), "utf8")
      .split("\n")
      .flatMap((line) => {
        const match = line.match(/^## (.+)$/u);
        return match === null ? [] : [match[1]];
      });
    expect(headings, `${skill} "${section}"`).toContain(section);
  }
});

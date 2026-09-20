import { beforeEach, describe, expect, test } from "bun:test";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool, resetAdviceMemory } from "../dispatch-execute";

interface AdviceFixture {
  readonly issue_status?: string;
  readonly session_writes_since_human?: number;
  readonly your_open_asks?: Array<{ readonly id: string; readonly question: string }>;
  readonly decision_blocks?: number;
}

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

async function executeWrite(
  tool: string,
  args: Record<string, unknown>,
  advice?: AdviceFixture
) {
  const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const target = new URL(String(url));
    const method = init?.method ?? "GET";
    const body = init?.body === undefined ? {} : JSON.parse(String(init.body));

    if (target.pathname === "/api/v1/issues" && method === "POST") {
      return response(withAdvice({ key: "DSP-42", title: "Created issue" }, advice));
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
      return response(withAdvice({ applied: 1, version: null }, advice));
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
  test.each([
    ["dispatch_issue", { project: "DSP", title: "Created issue", spec: "# Spec\n" }],
    ["dispatch_artifact", { issue: "DSP-42", name: "spec.md", content: "# Spec\n" }],
  ])("%s renders the decision-block pointer for a primary spec", async (tool, args) => {
    const rawAdvice = advice({ decision_blocks: 0 });
    const result = await executeWrite(tool, args, rawAdvice);

    expect(result.text).toEndWith(
      'No decision blocks in this spec — nothing here reaches a human\'s inbox. Want human feedback? See the `dispatch` skill, "Decision blocks".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("renders decision-block advice on a project document that carries only that fact", async () => {
    const rawAdvice = { decision_blocks: 0 };
    const result = await executeWrite(
      "dispatch_artifact",
      { project: "DSP", name: "spec.md", content: "# Spec\n" },
      rawAdvice
    );

    expect(result.text).toEndWith(
      'No decision blocks in this spec — nothing here reaches a human\'s inbox. Want human feedback? See the `dispatch` skill, "Decision blocks".'
    );
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

    expect(result.text).not.toContain("No decision blocks in this spec");
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
      "You've sent 3 messages on DSP-42 with no human response. Progress ledger or scratchpad? If so, stop. See the `dispatch` skill, \"Structure over stream\"."
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
      "You've sent 6 messages on DSP-42 with no human response. Stop posting here until a human replies. See the `dispatch` skill, \"Structure over stream\"."
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
    expect(second.text).toBe(`Posted message message-1 (dispatch://DSP-42/message/message-1) ${issueSuffix}`);
    expect(first.details.advice).toEqual(rawAdvice);
    expect(second.details.advice).toEqual(rawAdvice);
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
    [
      "dispatch_issue",
      { project: "DSP", title: "Created issue" },
      `Created DSP-42: Created issue ${issueSuffix}`,
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
      `Applied 1 ops (no new version) ${issueSuffix}`,
      { issue: "DSP-42", applied: 1 },
    ],
    [
      "dispatch_issue_update",
      { issue: "DSP-42", title: "Renamed" },
      `DSP-42: title "Renamed" ${issueSuffix}`,
      { issue: "DSP-42", status: "in_progress", external_links: [] },
    ],
  ])("%s preserves the existing result when advice is absent", async (tool, args, text, details) => {
    const result = await executeWrite(tool, args);

    expect(result.text).toBe(text);
    expect(result.details).toEqual(details);
    expect(result.details).not.toHaveProperty("advice");
  });
});

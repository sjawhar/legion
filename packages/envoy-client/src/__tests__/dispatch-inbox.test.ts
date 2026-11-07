import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

// `dispatch inbox` against a stub Dispatch: the four routes the Inbox page reads, answered as the
// server answers them, so the test pins the lines the command prints rather than the renderer's
// own idea of them.

const makeDirectory = temporaryDirectories("dispatch-inbox");

const VIEWER = "alice@theorem.dev";
const SNOOZED_ID = "33333333-3333-4333-8333-333333333333";

/** A row of `GET /api/v1/inbox`, with the fields the summary reads; the thread and the rest of an
 *  ask ride along in the real answer and are not read here. */
function inboxRow(row: Record<string, unknown>): Record<string, unknown> {
  return {
    author: { kind: "session", id: "ses-9" },
    kind: "question",
    options: [],
    multiple: false,
    urgency: "med",
    anchor: null,
    state: "open",
    answer: null,
    opened_event_id: 1,
    created_at: "2026-11-01T00:00:00Z",
    edited_at: null,
    priority: null,
    snoozed_until: null,
    thread: { replies: [], edits: [], answers: [], followers: [] },
    ...row,
  };
}

/** The rows the stub lists, in the server's order: two the person must answer, one an agent owes,
 *  one the person deferred, and one on a project document, which the renderer names by its slug. */
const ROWS = [
  inboxRow({
    id: "11111111-1111-4111-8111-111111111111",
    issue_key: "DSP-1",
    issue: { key: "DSP-1", title: "Native workspace", assignee: VIEWER },
    priority: 1,
    question: "Which store do we keep?",
    waiting_on: "human",
  }),
  inboxRow({
    id: "22222222-2222-4222-8222-222222222222",
    issue_key: "DSP-2",
    issue: { key: "DSP-2", title: "The second one", assignee: VIEWER },
    priority: 2,
    kind: "approval",
    question: `Approve spec.md (version 3)? ${"x".repeat(90)}`,
    waiting_on: "human",
  }),
  inboxRow({
    id: SNOOZED_ID,
    issue_key: "DSP-3",
    issue: { key: "DSP-3", title: "The deferred one", assignee: VIEWER },
    priority: 3,
    question: "Does the export still need its nightly run?",
    waiting_on: "human",
    snoozed_until: "2099-01-01T00:00:00Z",
  }),
  inboxRow({
    id: "44444444-4444-4444-8444-444444444444",
    issue_key: "DSP-4",
    issue: { key: "DSP-4", title: "The agent's turn", assignee: VIEWER },
    priority: 0,
    question: "Running the migration now.",
    waiting_on: "agent",
  }),
  inboxRow({
    id: "55555555-5555-4555-8555-555555555555",
    issue_key: null,
    document: { project: "CORE", slug: "design-notes", name: "design.md" },
    question: "Is this still the plan?",
    waiting_on: "agent",
  }),
];

/** The secrets broker's pending list as Dispatch relays it: one secret request with both session
 *  ids and its page on the broker, and one machine login with neither. */
const PENDING = [
  {
    record_id: "rec-1",
    kind: "agent_secret",
    identifiers: ["github/token", "aws/role"],
    requested_at: "2026-11-02T09:00:00Z",
    session: { request: "ses-7", enrollment: "ses-8" },
    approve_url: "https://broker.theorem.dev/approve/rec-1",
  },
  {
    record_id: "rec-2",
    kind: "launcher_credential",
    identifiers: ["runner-12"],
    requested_at: "2026-11-02T10:00:00Z",
  },
];

/** A time `ms` before `now`, as the server writes one. */
function ago(now: number, ms: number): string {
  return new Date(now - ms).toISOString();
}

/** The rows of `GET /api/v1/me/replies?kind=reply,agent`, newest reply first: a comment thread
 *  on an issue someone answered, and an agent session's reply to a message the person sent it.
 *  Built when the stub answers, so their ages are read off the clock the command reads, and
 *  each offset sits between two steps so neither age falls to the step below as the test runs. */
function threadRows(now: number): Record<string, unknown>[] {
  return [
    {
      kind: "comment",
      thread_id: "66666666-6666-4666-8666-666666666666",
      ref: "/issues/DSP-6/comments/66666666-6666-4666-8666-666666666666",
      subject: "The staging box lost its disk again",
      issue: { key: "DSP-6", title: "Staging keeps falling over" },
      priority: 2,
      project: "DSP",
      read_through: ago(now, 30 * 60_000),
      unread: true,
      unread_count: 2,
      reply: {
        id: "99999999-9999-4999-8999-999999999999",
        kind: "reply",
        author: { kind: "user", id: "bob@theorem.dev" },
        first_line: "I swapped the disk this morning.",
        created_at: ago(now, 5 * 60_000 + 30_000),
      },
    },
    {
      kind: "agent",
      thread_id: "77777777-7777-4777-8777-777777777777",
      ref: "/agents/ses-5/live",
      subject: "Can you take the migration next?",
      session: { id: "ses-5", name: "legion-implementer" },
      unread_reply_ids: ["88888888-8888-4888-8888-888888888888"],
      unread: true,
      unread_count: 1,
      reply: {
        id: "88888888-8888-4888-8888-888888888888",
        kind: "reply",
        author: { kind: "session", id: "ses-5" },
        first_line: "Taking it now.",
        created_at: ago(now, 95 * 60_000),
      },
    },
  ];
}

/** `GET /api/v1/me/replies`'s answer around `rows`, unpaged as the command asks for it. */
function repliesBody(rows: Record<string, unknown>[], now: number): Record<string, unknown> {
  return { rows, total: rows.length, limit: 0, offset: 0, as_of: new Date(now).toISOString() };
}

/** How the stub answers one read: the rows it lists by default (`LISTED`), or a body and status
 *  of its own, a literal `null` body among them. */
type Answer = typeof LISTED | { readonly status: number; readonly body: unknown };

const LISTED = "listed" as const;
let whoamiAnswer: Answer = LISTED;
let inboxAnswer: Answer = LISTED;
let pendingAnswer: Answer = LISTED;
let repliesAnswer: Answer = LISTED;
const requests: string[] = [];

function refusal(code: string, error: string, status: number): Answer {
  return { status, body: { code, error } };
}

/** The refusal every one of these routes gives a credential that holds no `person` scope. */
const SCOPE_REFUSED = refusal(
  "OAUTH_SCOPE_REQUIRED",
  "this request's credential does not hold the person scope, which this route requires",
  403
);

const server = Bun.serve({
  port: 0,
  fetch(request) {
    const { pathname, search } = new URL(request.url);
    requests.push(`${request.method} ${pathname}${search}`);
    const answered = (answer: Answer, listed: unknown): Response =>
      answer === LISTED
        ? Response.json(listed)
        : Response.json(answer.body, { status: answer.status });
    if (pathname === "/api/v1/whoami") {
      return answered(whoamiAnswer, {
        kind: "agent",
        owner: VIEWER,
        service: null,
        scopes: ["person"],
      });
    }
    if (pathname === "/api/v1/inbox") return answered(inboxAnswer, ROWS);
    if (pathname === "/api/v1/credential-requests") {
      return answered(pendingAnswer, { pending: PENDING });
    }
    if (pathname === "/api/v1/me/replies") {
      const now = Date.now();
      return answered(repliesAnswer, repliesBody(threadRows(now), now));
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  whoamiAnswer = LISTED;
  inboxAnswer = LISTED;
  pendingAnswer = LISTED;
  repliesAnswer = LISTED;
  stateDir = makeDirectory();
  resetAdviceMemory();
  forgetKeyedLayerMemory();
});

async function runInbox(): Promise<{ code: number; stdout: string; stderr: string }> {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(
    ["inbox"],
    {
      HOME: stateDir,
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: "test-token",
      DISPATCH_STATE_DIR: stateDir,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: (text) => {
        stderr += text;
      },
      readText: () => "",
      cwd: stateDir,
    }
  );
  return { code, stdout, stderr };
}

describe("dispatch inbox", () => {
  test("prints the counts, the headings in order and each row's fields", async () => {
    const { code, stdout, stderr } = await runInbox();

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout.trimEnd().split("\n")).toEqual([
      // Needs you: the two asks waiting on the person, and the two credential requests.
      "Needs you 4",
      "- DSP-1 · P1 · question · Which store do we keep? · dispatch://DSP-1/ask/11111111-1111-4111-8111-111111111111",
      `- DSP-2 · P2 · approval · Approve spec.md (version 3)? ${"x".repeat(51)}… · dispatch://DSP-2/ask/22222222-2222-4222-8222-222222222222`,
      "",
      "Waiting on agents 2",
      "- DSP-4 · P0 · question · Running the migration now. · dispatch://DSP-4/ask/44444444-4444-4444-8444-444444444444",
      "- design-notes · P- · question · Is this still the plan? · dispatch://CORE/artifact/design-notes/ask/55555555-5555-4555-8555-555555555555",
      "",
      "Later 1",
      `- DSP-3 · P3 · question · Does the export still need its nightly run? · dispatch://DSP-3/ask/${SNOOZED_ID}`,
      "",
      "Unread threads 2",
      "- DSP-6 · comment · The staging box lost its disk again · user bob@theorem.dev · 5m · /issues/DSP-6/comments/66666666-6666-4666-8666-666666666666",
      "- legion-implementer · agent · Can you take the migration next? · session ses-5 · 1h · /agents/ses-5/live",
      "",
      "Credential requests 2",
      "- agent_secret · github/token, aws/role · 2026-11-02T09:00:00Z · session request=ses-7, enrollment=ses-8 · https://broker.theorem.dev/approve/rec-1",
      "- launcher_credential · runner-12 · 2026-11-02T10:00:00Z",
    ]);
    // The caller's own credential, on the four routes the Inbox page reads and no other.
    expect(requests.sort()).toEqual([
      "GET /api/v1/credential-requests?approver=me",
      "GET /api/v1/inbox?assignee=me",
      "GET /api/v1/me/replies?kind=reply%2Cagent",
      "GET /api/v1/whoami",
    ]);
  });

  test("a failed credential-request read warns on stderr, counts none and prints the rest", async () => {
    pendingAnswer = refusal("BROKER_UNAVAILABLE", "the secrets broker did not answer", 502);

    const { code, stdout, stderr } = await runInbox();

    expect(code).toBe(0);
    expect(stderr).toBe(
      "dispatch: warning: credential requests could not be read: BROKER_UNAVAILABLE the secrets broker did not answer\n"
    );
    expect(stdout).toContain("Needs you 2\n");
    expect(stdout.trimEnd().endsWith("Credential requests 0")).toBe(true);
    // The asks are the greater part of the Inbox, and one failed read does not cost them.
    expect(stdout).toContain("Waiting on agents 2\n");
    expect(stdout).toContain("Later 1\n");
  });

  test("a null credential-requests body counts 0 and lists nothing", async () => {
    // A Dispatch with no secrets broker configured, which is an ordinary deployment.
    pendingAnswer = { status: 200, body: null };

    const { code, stdout, stderr } = await runInbox();

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("Needs you 2\n");
    expect(stdout.trimEnd().endsWith("Credential requests 0")).toBe(true);
  });

  // The two reads a scope can refuse. GET /api/v1/whoami needs none — it answers every caller
  // its own scopes — so a refusal from it is one the server cannot give and no test fakes.
  test("a scope refusal from either scoped read exits 1 naming the code and the scope", async () => {
    for (const refuse of [
      () => {
        inboxAnswer = SCOPE_REFUSED;
      },
      () => {
        pendingAnswer = SCOPE_REFUSED;
      },
    ]) {
      whoamiAnswer = LISTED;
      inboxAnswer = LISTED;
      pendingAnswer = LISTED;
      refuse();

      const { code, stdout } = await runInbox();

      expect(code).toBe(1);
      expect(stdout).toContain("OAUTH_SCOPE_REQUIRED");
      expect(stdout).toContain("does not hold the person scope");
      expect(stdout).not.toContain("Needs you");
    }
  });

  test("lists a Reply row by its owner and an Agent row by its session, with replier and age", async () => {
    const now = Date.now();
    repliesAnswer = {
      status: 200,
      body: repliesBody(
        [
          {
            kind: "message",
            thread_id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
            ref: "/projects/CORE/documents/design-notes",
            subject: "Is the nightly export still ours?",
            document: { project: "CORE", slug: "design-notes", name: "design.md" },
            read_through: ago(now, 3 * 24 * 60 * 60_000),
            unread: true,
            unread_count: 1,
            reply: {
              id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
              kind: "reply",
              author: { kind: "user", id: "bob@theorem.dev" },
              first_line: "It is, until the new one runs.",
              created_at: ago(now, 2 * 24 * 60 * 60_000 + 60 * 60_000),
            },
          },
          {
            kind: "agent",
            thread_id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
            ref: "/agents/ses-5/live",
            subject: "Can you take the migration next?",
            session: { id: "ses-5", name: "legion-implementer" },
            unread_reply_ids: ["dddddddd-dddd-4ddd-8ddd-dddddddddddd"],
            unread: true,
            unread_count: 1,
            reply: {
              id: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
              kind: "reply",
              author: { kind: "session", id: "ses-5" },
              first_line: "Taking it now.",
              created_at: ago(now, 45_000),
            },
          },
        ],
        now
      ),
    };

    const { code, stdout, stderr } = await runInbox();

    expect(stderr).toBe("");
    expect(code).toBe(0);
    // A thread on a document is named by its slug, an agent thread by the session's name, and a
    // reply written in the last minute reads `<1m`.
    expect(stdout).toContain(
      [
        "Unread threads 2",
        "- design-notes · message · Is the nightly export still ours? · user bob@theorem.dev · 2d · /projects/CORE/documents/design-notes",
        "- legion-implementer · agent · Can you take the migration next? · session ses-5 · <1m · /agents/ses-5/live",
      ].join("\n")
    );
  });

  test("counts the unread threads in the call's details", async () => {
    const { code } = await runInbox();

    expect(code).toBe(0);
    // The counts are not printed; the command leaves them in the session's ledger line.
    const ledger = readFileSync(
      join(stateDir, "sessions", "ses-1", "results.jsonl"),
      "utf-8"
    ).trim();
    const entry = JSON.parse(ledger) as { details?: Record<string, unknown> };
    expect(entry.details).toMatchObject({ asks: 5, unread_threads: 2, credential_requests: 2 });
  });

  test("a failed replies read warns on stderr, counts no threads and prints the rest", async () => {
    repliesAnswer = refusal("INTERNAL", "the thread list could not be read", 502);

    const { code, stdout, stderr } = await runInbox();

    expect(code).toBe(0);
    expect(stderr).toBe(
      "dispatch: warning: unread threads could not be read: INTERNAL the thread list could not be read\n"
    );
    expect(stdout).toContain("Unread threads 0\n");
    // The asks and the credential requests are read apart from the threads, and still print.
    expect(stdout).toContain("Needs you 4\n");
    expect(stdout).toContain("Later 1\n");
    expect(
      stdout.trimEnd().endsWith("- launcher_credential · runner-12 · 2026-11-02T10:00:00Z")
    ).toBe(true);
  });

  test("a scope refusal from the replies read exits 1 naming the code and the scope", async () => {
    repliesAnswer = SCOPE_REFUSED;

    const { code, stdout } = await runInbox();

    expect(code).toBe(1);
    expect(stdout).toContain("OAUTH_SCOPE_REQUIRED");
    expect(stdout).toContain("does not hold the person scope");
    expect(stdout).not.toContain("Needs you");
  });
});

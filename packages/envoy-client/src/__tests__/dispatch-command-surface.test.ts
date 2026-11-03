import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import { ASK_QUESTION_MAX, dispatchToolSpecs } from "@legion/contracts";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool } from "../dispatch-execute";
import { DISPATCH_WRITE_RETRY, type DispatchWriteRetry } from "../dispatch-http";
import { fakeSearchResponse } from "./fake-search-response";

/**
 * The surface every Dispatch command presents: for each command `dispatchToolSpecs` names, what
 * one valid call sends and prints, and what each refusal `argumentProblems` and
 * `ownerConflictProblems` can give it prints. The record is the golden file
 * `fixtures/command-surface.json`, so that the record taken before `executeDispatchTool` is split
 * into per-group handler modules and the record taken after it can be compared byte for byte.
 *
 * Run it with `UPDATE_COMMAND_SURFACE=1 bun test src/__tests__/dispatch-command-surface.test.ts`
 * to rewrite the golden file; any other run asserts the file byte for byte.
 *
 * The command list is read from `dispatchToolSpecs` at run time and its valid call is each spec's
 * own `example`, so a command added to the contract is driven here rather than silently dropped:
 * the byte comparison then fails until the golden file is rewritten.
 *
 * What is normalised, and why:
 * - Every run uses the same fixed session, host, cwd, repository and issue keys, so nothing in a
 *   request or a printed line comes from the machine it ran on. The machine's own name is one of
 *   those: `machineId()` reads `ENVOY_MACHINE_ID` and falls back to the kernel hostname, which a
 *   write's `origin` carries, so this test sets that variable and puts back what it found.
 * - A request's path keeps its query parameters sorted by name, because `URLSearchParams` order
 *   follows the order the caller happened to append in.
 * - `Idempotency-Key` is dropped from the recorded headers: `DispatchClient` mints a fresh UUID
 *   for every write.
 * - In a request body, any UUID becomes `<id>` and any ISO-8601 timestamp `<time>`, and object
 *   keys are sorted, so neither a generated id nor the wall clock nor a map's iteration order can
 *   reach the file. Arrays in a body keep their order, which is part of the surface.
 * - The lists this test writes (the runs, a request's headers) are sorted; the requests of one run
 *   stay in the order they were sent, which is part of the surface.
 * - The fake server answers with fixed rows, and the rows of any list whose rendering prints an
 *   age (open asks, answers) are empty, so no printed line is dated against the wall clock.
 */

const GOLDEN = path.resolve(import.meta.dir, "fixtures/command-surface.json");
const UPDATE = process.env.UPDATE_COMMAND_SURFACE === "1";

// machineId() falls back to the kernel hostname, which would write this box's name into the
// record; the variable it honours is set here so the record is the same on every machine.
const MACHINE = "test-machine";
const MACHINE_BEFORE = process.env.ENVOY_MACHINE_ID;
beforeAll(() => {
  process.env.ENVOY_MACHINE_ID = MACHINE;
});
afterAll(() => {
  if (MACHINE_BEFORE === undefined) delete process.env.ENVOY_MACHINE_ID;
  else process.env.ENVOY_MACHINE_ID = MACHINE_BEFORE;
});

const SESSION = "session-1";
const ISSUE = "DSP-1";
const UUID = "01234567-0000-4000-8000-000000000001";
const OTHER_UUID = "01234567-0000-4000-8000-000000000002";
const ARTIFACT_ID = "01234567-0000-4000-8000-00000000000a";
const AT = "2026-01-02T03:04:05Z";

const config = { url: "http://dispatch.test", token: "secret" };

const exec: ExecFn = async (file, args) => {
  if (file === "jj" && args.join(" ") === "git remote list") {
    return { stdout: "origin https://github.com/owner/repo.git\n" };
  }
  throw new Error(`unexpected command: ${file} ${args.join(" ")}`);
};

/** The same fake answer the executor's own tests use: JSON with a 200 unless `status` says. */
function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** A write's attempts without their pauses, as dispatch-execute.test.ts runs them. */
function writeRetry(): DispatchWriteRetry {
  let clock = 0;
  return {
    ...DISPATCH_WRITE_RETRY,
    pause: async (ms) => {
      clock += ms;
    },
    now: () => clock,
  };
}

const actor = { kind: "session", id: SESSION };

function issueBody(key = ISSUE) {
  return {
    key,
    project: key.split("-")[0],
    number: Number(key.split("-")[1]),
    title: "Native workspace",
    status: "todo",
    priority: 1,
    rank: "0|hzzzzz:",
    labels: ["infra"],
    parent: null,
    blocked_by: [],
    assignee: "person@example.com",
    claim: null,
    components: { mode: "explicit", ids: [], unknown: [], reason: null, inherited_from: null },
    external_links: [],
    route: null,
    repo: "owner/repo",
    resolved_repo: "owner/repo",
    created_by: actor,
    created_at: AT,
    updated_at: AT,
    closed_at: null,
    primary_artifact_id: ARTIFACT_ID,
    last_seq: 0,
    route_status: null,
    route_holder: null,
    progress: { tasks: null, children: null },
    artifacts: [artifactBody("spec.md")],
    open_asks: [],
    children: [],
    referenced_by_count: 0,
  };
}

function versionBody() {
  return {
    number: 1,
    named: false,
    summary: null,
    authors: [actor],
    created_at: AT,
    size: 12,
  };
}

function askBody() {
  return {
    id: UUID,
    issue_key: ISSUE,
    artifact_id: null,
    author: actor,
    question: "Which way should the release go?",
    options: [],
    multiple: false,
    urgency: "med",
    anchor: null,
    state: "open",
    answer: null,
    kind: "question",
    waiting_on: "human",
    opened_event_id: 1,
    created_at: AT,
    updated_at: AT,
  };
}

/** `GET /api/v1/asks/{id}`: the ask with its thread, as `AskRead` carries it. */
function askReadBody() {
  return {
    ask: askBody(),
    replies: [],
    edits: [],
    answers: [],
    followers: [],
    claimant: null,
    route: null,
    route_status: null,
    route_holder: null,
  };
}

function commentBody() {
  return {
    id: UUID,
    issue_key: ISSUE,
    artifact_id: null,
    author: actor,
    body: "Looks good.",
    anchor: null,
    state: "open",
    reply_to: null,
    created_at: AT,
    updated_at: AT,
  };
}

function messageBody() {
  return {
    id: UUID,
    issue_key: ISSUE,
    author: actor,
    body: "Implementation started.",
    to: [],
    created_at: AT,
    deliveries: [],
  };
}

function artifactBody(name = "design.md", kind = "doc") {
  return {
    id: ARTIFACT_ID,
    issue_key: ISSUE,
    project: "DSP",
    slug: name.replace(/\./g, "-"),
    ref_key: `${ISSUE}/${name.replace(/\./g, "-")}`,
    name,
    kind,
    primary: name === "spec.md",
    created_by: actor,
    created_at: AT,
    versions: [versionBody()],
  };
}

function handoffBody() {
  return {
    id: UUID,
    issue_key: ISSUE,
    phase: "implement",
    version: 1,
    body: '{"schemaVersion":1,"phase":"implement","issue":"DSP-1"}',
    author: actor,
    created_at: AT,
  };
}

function architectureSource() {
  return {
    project: "CORE",
    repo: "acme/infra",
    branch: "main",
    enabled: true,
    created_by: { kind: "user", login: "sjawhar" },
    created_at: AT,
    last_sync_at: AT,
    last_commit: "abc1234",
    last_error: null,
    components: 3,
  };
}

/** The fake Dispatch: one fixed answer per route every command here reaches. */
function answerFor(method: string, pathname: string): Response {
  const route = `${method} ${pathname}`;
  const segments = pathname.split("/").filter((segment) => segment !== "");
  const rest = segments.slice(4).join("/");
  switch (route) {
    case "GET /api/v1/issues/resolve":
      return response({ key: ISSUE });
    case "POST /api/v1/issues":
      return response({ ...issueBody("DSP-216"), created: true }, 201);
    case "GET /api/v1/issues":
      return response({ issues: [issueBody()], total: 1, limit: 250, offset: 250 });
    case "GET /api/v1/projects":
      return response([{ key: "DSP", name: "Dispatch", created_at: AT }]);
    case "POST /api/v1/projects":
      return response({ key: "CORE", name: "Core", created_at: AT }, 201);
    case "GET /api/v1/search":
      return response(fakeSearchResponse([]));
    case "GET /api/v1/whoami":
      return response({ kind: "agent", owner: "person@example.com", session: SESSION });
    case "GET /api/v1/agents":
      return response([]);
    case "GET /api/v1/references":
      return response({ node: { kind: "issue", id: ISSUE }, edges: [] });
    case "GET /api/v1/asks/open":
      return response({
        session_id: SESSION,
        as_of: AT,
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      });
  }
  if (segments[2] === "agents" && segments.length >= 4) {
    if (rest === "open-asks") {
      return response({
        session_id: SESSION,
        as_of: AT,
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      });
    }
    if (rest === "answers") {
      return response({ session_id: SESSION, since: AT, total: 0, items: [] });
    }
  }
  if (segments[2] === "issues" && segments.length >= 4) {
    if (rest === "" && (method === "GET" || method === "PATCH")) {
      return response(issueBody(segments[3]));
    }
    if (rest === "claim") {
      return response({
        ...issueBody(segments[3]),
        claim: { actor, at: AT },
        legion_managed_status: false,
      });
    }
    if (rest === "events") return response([]);
    if (rest === "asks" && method === "POST") return response(askBody(), 201);
    if (rest === "asks") return response([]);
    if (rest === "comments" && method === "POST") return response(commentBody(), 201);
    if (rest === "comments") return response([]);
    if (rest === "messages" && method === "POST") return response(messageBody(), 201);
    if (rest === "artifacts" && method === "POST") {
      return response({ artifact: artifactBody(), version: versionBody() }, 201);
    }
    if (rest === "artifacts") return response([artifactBody("spec.md")]);
    if (rest === "references") return response({ members: [], truncated: false });
    if (rest === "handoffs") return response([]);
    if (segments[4] === "handoffs" && method === "POST") return response(handoffBody(), 201);
    if (segments[4] === "handoffs") return response(handoffBody());
  }
  if (segments[2] === "artifacts" && segments.length >= 4) {
    if (rest === "" && method === "GET") return response(artifactBody("spec.md"));
    if (rest === "text") return response({ markdown: "# Spec\n", version: 1 });
    if (rest === "blocks") return response([]);
    if (rest === "edits") return response({ applied: 1, version: versionBody(), changed: true });
    if (rest === "suggestions") {
      return response({ ...commentBody(), artifact_id: ARTIFACT_ID }, 201);
    }
    if (rest === "approval-requests") {
      return response(
        {
          ask: askBody(),
          artifact_id: ARTIFACT_ID,
          version: 1,
          approval: { state: "awaiting", latest_version: 1, version: 1 },
        },
        201
      );
    }
    if (rest === "comments" || rest === "asks" || rest === "events") return response([]);
    if (rest === "references") return response({ artifact_id: ARTIFACT_ID, items: [] });
  }
  if (segments[2] === "asks" && segments.length >= 4) {
    if (rest === "") return response(method === "GET" ? askReadBody() : askBody());
    if (rest === "resolve") {
      return response({
        ...askBody(),
        state: "resolved",
        resolution: { kind: "retracted", reason: "A newer question supersedes this one.", actor },
      });
    }
    if (rest.startsWith("followers")) return new Response(null, { status: 204 });
  }
  if (segments[2] === "comments" && segments.length >= 4) {
    if (rest === "" || rest === "resolve") return response(commentBody());
  }
  if (segments[2] === "projects" && segments.length >= 4) {
    if (rest === "related") {
      return response({ outcome: "none", search: "meaning_and_keywords", related: [] });
    }
    if (rest === "architecture-sources") return response([architectureSource()]);
    if (rest.startsWith("architecture-sources/")) return response(architectureSource());
    if (rest === "artifacts") return response([]);
  }
  throw new Error(`unexpected request: ${route}`);
}

interface RecordedRequest {
  readonly method: string;
  readonly path: string;
  readonly headers: Record<string, string>;
  readonly body: unknown;
}

interface RecordedRun {
  readonly tool: string;
  readonly call: string;
  readonly args: unknown;
  readonly requests: readonly RecordedRequest[];
  readonly outcome: "result" | "error";
  readonly text: readonly string[];
}

const UUID_PATTERN = /[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/g;
const TIME_PATTERN = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})/g;

/** A recorded value with its ids, times and key order fixed. */
function normalize(value: unknown): unknown {
  if (typeof value === "string") {
    return value.replace(UUID_PATTERN, "<id>").replace(TIME_PATTERN, "<time>");
  }
  if (Array.isArray(value)) return value.map(normalize);
  if (typeof value === "object" && value !== null) {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>)
        .sort(([left], [right]) => (left < right ? -1 : left > right ? 1 : 0))
        .map(([key, entry]) => [key, normalize(entry)])
    );
  }
  return value;
}

function recordRequest(url: string, init: RequestInit | undefined): RecordedRequest {
  const target = new URL(url);
  const query = [...target.searchParams.entries()].sort(([left], [right]) =>
    left < right ? -1 : left > right ? 1 : 0
  );
  const search = query.map(([key, value]) => `${key}=${value}`).join("&");
  const headers = Object.entries((init?.headers ?? {}) as Record<string, string>)
    .filter(([key]) => key.toLowerCase() !== "idempotency-key")
    .sort(([left], [right]) => (left < right ? -1 : left > right ? 1 : 0));
  const raw = init?.body;
  const text = typeof raw === "string" ? raw : raw === undefined ? undefined : "<stream>";
  let body: unknown;
  if (text === undefined) {
    body = null;
  } else {
    try {
      body = normalize(JSON.parse(text));
    } catch {
      body = normalize(text);
    }
  }
  return {
    method: init?.method ?? "GET",
    path: search === "" ? target.pathname : `${target.pathname}?${search}`,
    headers: Object.fromEntries(headers),
    body,
  };
}

async function runCommand(tool: string, call: string, args: Record<string, unknown>) {
  const requests: RecordedRequest[] = [];
  const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const target = new URL(String(url));
    requests.push(recordRequest(String(url), init));
    return answerFor(init?.method ?? "GET", target.pathname);
  };
  const run = await executeDispatchTool({
    tool,
    args,
    cwd: "/workspace",
    host: "omp",
    sessionId: SESSION,
    sessionTitle: "Record the command surface",
    config,
    env: {},
    exec,
    fetchImpl: fetchImpl as typeof fetch,
    writeRetry: writeRetry(),
  }).then(
    (result) => ({ outcome: "result" as const, text: result.text }),
    (error: Error) => ({ outcome: "error" as const, text: error.message })
  );
  const recorded: RecordedRun = {
    tool,
    call,
    args: normalize(args),
    requests,
    outcome: run.outcome,
    text: run.text.split("\n"),
  };
  return recorded;
}

interface RefusalCase {
  readonly tool: string;
  readonly call: string;
  readonly args: Record<string, unknown>;
}

const documentItemRef = `dispatch://${ISSUE}/artifact/spec/ask/${UUID}`;
const versionRef = `dispatch://${ISSUE}/artifact/spec@v2`;
const askRef = `dispatch://${ISSUE}/ask/${UUID}`;
const commentRef = `dispatch://${ISSUE}/comment/${UUID}`;

/**
 * One call for each refusal `argumentProblems` and `ownerConflictProblems` can give a command.
 * Each is written whole, so the call that makes a problem is readable beside the problem it makes.
 */
const refusals: readonly RefusalCase[] = [
  {
    tool: "dispatch_ask",
    call: "question over the cap once the ref is appended",
    args: { issue: ISSUE, question: "q".repeat(ASK_QUESTION_MAX), ref: commentRef },
  },
  {
    tool: "dispatch_ask",
    call: "artifact with no anchor",
    args: { issue: ISSUE, question: "Which way should the release go?", artifact: "spec" },
  },
  {
    tool: "dispatch_ask",
    call: "artifact beside another anchor artifact",
    args: {
      issue: ISSUE,
      question: "Which way should the release go?",
      artifact: "spec",
      anchor: { artifact: "plan.md", quote: "old wording" },
    },
  },
  {
    tool: "dispatch_edit_ask",
    call: "ask named through its document",
    args: { ask: documentItemRef, question: "Which way should the release go?" },
  },
  {
    tool: "dispatch_edit_ask",
    call: "ask that is not an ask reference",
    args: { ask: commentRef, question: "Which way should the release go?" },
  },
  {
    tool: "dispatch_resolve_ask",
    call: "ask that is not an ask reference",
    args: { ask: commentRef, kind: "retracted", reason: "A newer question supersedes this one." },
  },
  {
    tool: "dispatch_follow",
    call: "ask that is not an ask reference",
    args: { ask: commentRef, action: "follow" },
  },
  {
    tool: "dispatch_resolve_comment",
    call: "comment that is not a comment reference",
    args: { comment: askRef },
  },
  {
    tool: "dispatch_resolve_comment",
    call: "comment named at a document version",
    args: { comment: `dispatch://DSP/artifact/plan-md@v2/comment/${UUID}` },
  },
  {
    tool: "dispatch_comment",
    call: "quote with no artifact",
    args: { issue: ISSUE, body: "Looks good.", quote: "old wording" },
  },
  {
    tool: "dispatch_comment",
    call: "reply_to beside reply_to_ask",
    args: { issue: ISSUE, body: "Looks good.", reply_to: UUID, reply_to_ask: OTHER_UUID },
  },
  {
    tool: "dispatch_comment",
    call: "reply_to naming a block of an ask",
    args: { issue: ISSUE, body: "Looks good.", reply_to: `${askRef}#b-p-1` },
  },
  {
    tool: "dispatch_comment",
    call: "reply_to_ask naming a block of a comment",
    args: { issue: ISSUE, body: "Looks good.", reply_to_ask: `${commentRef}#b-p-1` },
  },
  {
    tool: "dispatch_comment",
    call: "artifact with no quote",
    args: { issue: ISSUE, body: "Looks good.", artifact: "spec" },
  },
  {
    tool: "dispatch_comment",
    call: "issue beside a ref on another issue",
    args: { issue: "DSP-2", ref: `dispatch://${ISSUE}`, body: "Looks good." },
  },
  {
    tool: "dispatch_comment",
    call: "version ref on a live document",
    args: { ref: versionRef, body: "Looks good.", quote: "old wording" },
  },
  {
    tool: "dispatch_message",
    call: "in_reply_to that is no message id",
    args: { issue: ISSUE, body: "Implementation started.", in_reply_to: "not-a-message" },
  },
  {
    tool: "dispatch_message",
    call: "in_reply_to naming a block of an ask",
    args: { issue: ISSUE, body: "Implementation started.", in_reply_to: `${askRef}#b-p-1` },
  },
  {
    tool: "dispatch_read",
    call: "message that is no message id",
    args: { message: "not-a-message" },
  },
  {
    tool: "dispatch_read",
    call: "events beside all_events",
    args: { issue: ISSUE, events: 5, all_events: true },
  },
  {
    tool: "dispatch_read",
    call: "events on a read that lists none",
    args: { ref: askRef, events: 5 },
  },
  {
    tool: "dispatch_read",
    call: "artifact beside a ref that names no document",
    args: { ref: askRef, artifact: "spec" },
  },
  {
    tool: "dispatch_read",
    call: "version ref on a summary read",
    args: { ref: versionRef },
  },
  {
    tool: "dispatch_read",
    call: "project beside an issue ref",
    args: { ref: `dispatch://${ISSUE}`, project: "CORE" },
  },
  {
    tool: "dispatch_suggest",
    call: "version ref on a live document",
    args: { ref: versionRef, quote: "old wording", replace_with: "new wording" },
  },
  {
    tool: "dispatch_doc_edit",
    call: "version ref on a live document",
    args: { ref: versionRef, ops: [{ op: "delete_column", block: "table-123", index: 1 }] },
  },
  {
    tool: "dispatch_doc_edit",
    call: "artifact beside a ref naming another document",
    args: {
      ref: `dispatch://${ISSUE}/artifact/spec`,
      artifact: "plan.md",
      ops: [{ op: "delete_column", block: "table-123", index: 1 }],
    },
  },
  {
    tool: "dispatch_doc_read",
    call: "blocks beside a version",
    args: { issue: ISSUE, blocks: true, version: 2 },
  },
  {
    tool: "dispatch_doc_read",
    call: "version beside a ref naming another version",
    args: { ref: versionRef, version: 3 },
  },
  {
    tool: "dispatch_issues",
    call: "all beside limit and offset",
    args: { project: "PROJ", all: true, limit: 50, offset: 100 },
  },
];

function compare(left: RecordedRun, right: RecordedRun): number {
  const key = (run: RecordedRun) => `${run.tool}\u0000${run.call}`;
  return key(left) < key(right) ? -1 : key(left) > key(right) ? 1 : 0;
}

describe("the Dispatch command surface", () => {
  test("records every command and every refusal it can give", async () => {
    const runs: RecordedRun[] = [];
    for (const spec of dispatchToolSpecs) {
      runs.push(await runCommand(spec.name, "valid", { ...spec.example }));
    }
    for (const refusal of refusals) {
      runs.push(await runCommand(refusal.tool, `refusal: ${refusal.call}`, refusal.args));
    }
    runs.sort(compare);
    const record = {
      commands: dispatchToolSpecs.length,
      refusals: refusals.length,
      runs,
    };
    const text = `${JSON.stringify(record, null, 2)}\n`;
    if (UPDATE) {
      writeFileSync(GOLDEN, text);
      return;
    }
    expect(text).toBe(readFileSync(GOLDEN, "utf8"));
  });

  test("drives every command the contract names", () => {
    const golden = JSON.parse(readFileSync(GOLDEN, "utf8")) as {
      commands: number;
      runs: RecordedRun[];
    };
    const driven = golden.runs
      .filter((run) => run.call === "valid")
      .map((run) => run.tool)
      .sort();
    expect(driven).toEqual([...dispatchToolSpecs.map((spec) => spec.name)].sort());
    expect(golden.commands).toBe(dispatchToolSpecs.length);
  });

  test("holds one refusal run for each refusal a command can give", () => {
    const golden = JSON.parse(readFileSync(GOLDEN, "utf8")) as {
      refusals: number;
      runs: RecordedRun[];
    };
    const refused = golden.runs.filter((run) => run.call.startsWith("refusal: "));
    expect(refused).toHaveLength(refusals.length);
    expect(golden.refusals).toBe(refusals.length);
    // Every refusal run is refused before any request leaves the process, with one problem: the
    // one its case names, never a schema problem beside it.
    for (const run of refused) {
      expect(run.outcome, run.call).toBe("error");
      expect(run.requests, run.call).toEqual([]);
      expect(run.text[0], run.call).toEndWith("1 problem");
    }
  });
});

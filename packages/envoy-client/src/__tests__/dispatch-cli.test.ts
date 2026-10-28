import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { ArtifactBlock } from "@legion/contracts";
import { DISPATCH_BODY_MAX, PROJECT_KEY_RULE } from "@legion/contracts";
import { cliRefusals, fitOutput, runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { forgetShownPictures } from "../dispatch-picture-tools";

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const PNG = Buffer.from("89504e470d0a1a0a0000000d49484452", "hex");
const requests: Recorded[] = [];
let triage = false;
/** The length of the reply body the long message thread carries. */
let replyLength = 1;
const LONG_THREAD = "11111111-1111-4111-8111-111111111111";
const human = { kind: "human", id: "h" };

/** The Envoy listener's answer for session ses-1's subscriptions. */
let interests: () => Response = () => Response.json({ error: "unset" }, { status: 500 });

const issue = {
  key: "DSP-1",
  title: "Native workspace",
  status: "in_progress",
  labels: [],
  route: null,
  parent: null,
  external_links: [],
  artifacts: [],
};

/** DSP-2's spec, whose blocks `dispatch doc-read --blocks` lists. "é" is two bytes in UTF-8, so
 *  every block after the heading starts at a byte offset past its index in the string. */
const intro = `${"x".repeat(59)}😀 then more words past the cut.`;
const spec = [
  "# Café plan",
  "",
  intro,
  "",
  "- First item",
  "- Second item",
  "",
  "| # | Line | |",
  "| --- | --- | --- |",
  "| 1 | Backfill | extra |",
  "",
].join("\n");

/** Where `text` first stands in `spec`, in UTF-8 bytes, as Dispatch's /blocks route reports it. */
function byteRange(text: string): { from: number; to: number } {
  const at = spec.indexOf(text);
  if (at < 0) throw new Error(`the spec holds no ${JSON.stringify(text)}`);
  const from = Buffer.byteLength(spec.slice(0, at));
  return { from, to: from + Buffer.byteLength(text) };
}

const unanchored = { comments: 0, asks: 0 };
const specBlocks: ArtifactBlock[] = [
  {
    id: "h-1",
    type: "heading",
    ...byteRange("# Café plan"),
    token: "sha256:h",
    references: unanchored,
  },
  {
    id: "p-1",
    type: "paragraph",
    ...byteRange(intro),
    token: "sha256:p",
    references: { comments: 1, asks: 0 },
  },
  {
    id: "l-1",
    type: "bullet_list",
    ...byteRange("- First item\n- Second item"),
    token: "sha256:l",
    references: unanchored,
  },
  {
    id: "i-1",
    type: "list_item",
    ...byteRange("- First item"),
    token: "sha256:i1",
    references: unanchored,
  },
  {
    id: "ip-1",
    type: "paragraph",
    ...byteRange("First item"),
    token: "sha256:ip1",
    references: unanchored,
  },
  {
    id: "i-2",
    type: "list_item",
    ...byteRange("- Second item"),
    token: "sha256:i2",
    references: unanchored,
  },
  {
    id: "t-1",
    type: "table",
    ...byteRange("| # | Line | |\n| --- | --- | --- |\n| 1 | Backfill | extra |"),
    token: "sha256:t",
    references: { comments: 0, asks: 1 },
    table: {
      rows: [
        {
          id: "r-0",
          index: 0,
          cells: [
            { id: "c-0-0", row: 0, column: 0, header: "#", text: "#" },
            { id: "c-0-1", row: 0, column: 1, header: "Line", text: "Line" },
            { id: "c-0-2", row: 0, column: 2, header: "", text: "" },
          ],
        },
        {
          id: "",
          index: 1,
          cells: [
            { id: "c-1-0", row: 1, column: 0, header: "#", text: "1" },
            { id: "", row: 1, column: 1, header: "Line", text: "Backfill" },
            { id: "c-1-2", row: 1, column: 2, header: "", text: "extra" },
          ],
        },
      ],
    },
  },
];

const blocksIssue = {
  ...issue,
  key: "DSP-2",
  primary_artifact_id: "doc-2",
  open_asks: [],
  artifacts: [
    { id: "doc-2", slug: "spec", name: "spec.md", kind: "doc", primary: true, versions: [] },
    {
      id: "pic-2",
      slug: "chart-png",
      name: "chart.png",
      kind: "image",
      primary: false,
      versions: [{ number: 1, mime: "image/png", size: PNG.length }],
    },
  ],
};

/** DSP-3 as a Dispatch server from before the repository fields answers it: no repo or
 *  resolved_repo at all. */
const olderServerIssue = {
  key: "DSP-3",
  title: "Read from an older server",
  status: "todo",
  priority: null,
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  route: null,
  open_asks: [],
  last_seq: 0,
  external_links: [],
  labels: [],
};

/** How the DSP-2 routes answer: a Dispatch older than the table listing omits `table`, and a
 *  document a browser edits between the CLI's two reads of its text answers the second with other
 *  text and another token. */
let blocksServer = { tableRows: true, editedBetweenReads: false };
let textReads = 0;
/** DSP-4's event log: the issue reads `last_seq` `last`, and the events route serves seq 1 to
 *  `served` (past `last` once events were written after the issue was read), each a message, by
 *  `after` and `limit` as Dispatch pages them. `queries` records each events request's query. */
let eventLog = { last: 40, served: 40, queries: [] as string[] };

const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname, search, searchParams } = new URL(request.url);
    const text = await request.text();
    const body = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, body });
    if (pathname === "/api/v1/issues/DSP-2" && request.method === "GET") {
      return Response.json(blocksIssue);
    }
    if (pathname === "/api/v1/artifacts/doc-2/text") {
      textReads++;
      const edited = blocksServer.editedBetweenReads && textReads > 1;
      return Response.json({
        markdown: edited ? `${spec}\nTyped in the browser.\n` : spec,
        version: 3,
        token: edited ? "sha256:edited" : "sha256:doc",
      });
    }
    if (pathname === "/api/v1/artifacts/doc-2/blocks") {
      return Response.json(
        blocksServer.tableRows ? specBlocks : specBlocks.map(({ table: _, ...block }) => block)
      );
    }
    if (pathname === "/api/v1/issues/DSP-1/asks" && request.method === "POST") {
      return Response.json({
        id: "ask-a",
        issue_key: "DSP-1",
        question: body.question,
        urgency: "med",
        advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
      });
    }
    if (pathname === "/api/v1/issues/DSP-1/messages" && request.method === "POST") {
      return Response.json({
        id: "message-1",
        issue_key: "DSP-1",
        advice: {
          issue_status: triage ? "triage" : "in_progress",
          session_writes_since_human: 0,
          your_open_asks: [],
        },
      });
    }
    if (pathname === "/api/v1/issues/DSP-3" && request.method === "GET") {
      return Response.json(olderServerIssue);
    }
    if (pathname === "/api/v1/issues/DSP-3/events") {
      return Response.json([]);
    }
    if (pathname === "/api/v1/issues/DSP-4" && request.method === "GET") {
      return Response.json({
        ...olderServerIssue,
        key: "DSP-4",
        title: "A long history",
        last_seq: eventLog.last,
        repo: null,
        resolved_repo: null,
      });
    }
    if (pathname === "/api/v1/issues/DSP-4/events") {
      eventLog.queries.push(search);
      const after = Number(searchParams.get("after"));
      const through = Math.min(after + Number(searchParams.get("limit")), eventLog.served);
      const seqs = Array.from(
        { length: Math.max(0, through - after) },
        (_, index) => after + index + 1
      );
      return Response.json(
        seqs.map((seq) => ({
          id: `event-${seq}`,
          issue_key: "DSP-4",
          seq,
          type: "message.created",
          actor: human,
          payload: { body: `Event ${seq}` },
          created_at: "2026-10-25T00:00:00Z",
        }))
      );
    }
    if (
      pathname === "/api/v1/issues/DSP-3/references" ||
      pathname === "/api/v1/issues/DSP-4/references" ||
      pathname === "/api/v1/references"
    ) {
      return Response.json({ node: { kind: "issue", id: "DSP-3" }, edges: [], members: [] });
    }
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "GET") {
      return Response.json(issue);
    }
    if (pathname === "/api/v1/projects" && request.method === "POST") {
      if (body.key === "TAKEN") {
        return Response.json(
          { code: "PROJECT_EXISTS", error: "a project with this key already exists" },
          { status: 409 }
        );
      }
      return Response.json(
        { key: body.key, name: body.name, open_asks: 0, created_at: "2026-10-11T00:00:00Z" },
        { status: 201 }
      );
    }
    if (pathname === "/api/v1/projects" && request.method === "GET") {
      return Response.json([
        { key: "CORE", name: "Core", open_asks: 0, created_at: "2026-09-01T00:00:00Z" },
        { key: "SMOKE684", name: "Smoke", open_asks: 0, created_at: "2026-10-11T00:00:00Z" },
      ]);
    }
    if (pathname === "/api/v1/agents/ses-pic/artifacts/shot-png") {
      return Response.json({
        id: "pic-1",
        issue_key: null,
        project: "",
        session_id: "ses-pic",
        slug: "shot-png",
        name: "shot.png",
        kind: "image",
        versions: [{ number: 1, mime: "image/png", size: PNG.length }],
      });
    }
    if (pathname === "/api/v1/artifacts/pic-1/versions/1") {
      return new Response(PNG, { headers: { "Content-Type": "image/png" } });
    }
    if (pathname === `/api/v1/messages/${LONG_THREAD}`) {
      return Response.json({
        message: {
          id: LONG_THREAD,
          issue_key: null,
          author: human,
          body: "![shot.png](dispatch://agent/ses-pic/artifact/shot-png@v1)",
          target: null,
          in_reply_to: null,
          deliveries: [],
          created_at: "2026-10-08T00:00:00Z",
        },
        replies: [
          {
            id: "22222222-2222-4222-8222-222222222222",
            issue_key: null,
            author: human,
            body: "x".repeat(replyLength),
            target: null,
            in_reply_to: LONG_THREAD,
            deliveries: [],
            created_at: "2026-10-08T00:01:00Z",
          },
        ],
      });
    }
    if (pathname === "/v1/interests/ses-1") return interests();
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  triage = false;
  blocksServer = { tableRows: true, editedBetweenReads: false };
  textReads = 0;
  eventLog = { last: 40, served: 40, queries: [] };
  interests = () => Response.json({ error: "unset" }, { status: 500 });
  stateDir = mkdtempSync(join(tmpdir(), "dispatch-cli-"));
  // Each run is its own process in production: nothing the library remembers carries over.
  resetAdviceMemory();
});

function environment(
  extra: Record<string, string | undefined>
): Record<string, string | undefined> {
  return {
    HOME: stateDir,
    DISPATCH_URL: `http://127.0.0.1:${server.port}`,
    DISPATCH_TOKEN: "test-token",
    DISPATCH_STATE_DIR: stateDir,
    ...extra,
  };
}

async function run(
  argv: readonly string[],
  extra: Record<string, string | undefined>,
  stdin = ""
): Promise<{ code: number; stdout: string; stderr: string }> {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(argv, environment(extra), {
    stdout: (text) => {
      stdout += text;
    },
    stderr: (text) => {
      stderr += text;
    },
    readText: (path) => (path === "-" ? stdin : readFileSync(path, "utf-8")),
    cwd: stateDir,
  });
  return { code, stdout, stderr };
}

/** A fresh process for the same session: the in-memory memories are gone, the state dir stays. */
function nextProcess(sessionId: string): void {
  resetAdviceMemory();
  forgetShownPictures(sessionId);
}

function ledger(sessionId: string): { tool: string; error?: string }[] {
  const path = join(stateDir, "sessions", sessionId, "results.jsonl");
  return readFileSync(path, "utf-8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line));
}

const omp = { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-1" };

describe("the dispatch CLI", () => {
  test("refuses without DISPATCH_HOST, naming it, and sends nothing", async () => {
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "x"], {
      DISPATCH_SESSION_ID: "ses-1",
    });
    expect(code).toBe(2);
    expect(stdout).toContain("DISPATCH_HOST");
    expect(requests).toEqual([]);
  });

  test("Claude Code's own session id wins over an inherited DISPATCH_SESSION_ID", async () => {
    const { code } = await run(["message", "--issue", "DSP-1", "--body", "hello"], {
      DISPATCH_HOST: "claude",
      DISPATCH_SESSION_ID: "a",
      CLAUDE_CODE_SESSION_ID: "b",
    });
    expect(code).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/messages");
    expect(posted?.body).toMatchObject({ actor: { id: "b" } });
  });

  test("an empty session id counts as unset", async () => {
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "x"], {
      DISPATCH_HOST: "opencode",
      DISPATCH_SESSION_ID: "",
    });
    expect(code).toBe(2);
    expect(stdout).toContain("DISPATCH_SESSION_ID");
    expect(requests).toEqual([]);
  });

  test("an unknown flag is refused in command syntax, with an example", async () => {
    const { code, stdout } = await run(["message", "--issue", "X", "--isue", "Y"], omp);
    expect(code).toBe(1);
    expect(stdout.split("\n")[0]).toBe("dispatch message was not called: 1 problem");
    expect(stdout).toContain("- Example: dispatch message");
    expect(requests).toEqual([]);
  });

  test("--dry-run prints the arguments and sends nothing", async () => {
    const { code, stdout } = await run(
      ["message", "--dry-run", "--issue", "DSP-1", "--body-file", "-"],
      omp,
      "line one\nline two\n"
    );
    expect(code).toBe(0);
    expect(JSON.parse(stdout)).toEqual({ issue: "DSP-1", body: "line one\nline two\n" });
    expect(requests).toEqual([]);
  });

  // --reason is the close's note: a reopen (or any update but a close) that carries one is
  // refused by the argument check, in a dry run as in the call, naming that rule, before any
  // request; never by a closed issue's refusal of the note it would have posted.
  test("--reason without --status done is refused by the argument check, dry run included", async () => {
    const argv = ["issue-update", "--issue", "DSP-1", "--status", "in_progress", "--reason", "x"];
    const rule =
      "- reason goes only with status done: it is the note posted on the issue as it closes, and status in_progress closes nothing. Drop reason, or set status done to close the issue.";
    for (const call of [[...argv, "--dry-run"], argv]) {
      const { code, stdout } = await run(call, omp);
      expect(code, call.join(" ")).toBe(1);
      expect(stdout.split("\n").slice(0, 2)).toEqual([
        "dispatch issue-update was not called: 1 problem",
        rule,
      ]);
    }
    expect(requests).toEqual([]);
  });

  test("--dry-run refuses a body over the cap the call would refuse", async () => {
    const { code, stdout } = await run(
      ["comment", "--dry-run", "--issue", "DSP-1", "--body-file", "-"],
      omp,
      "x".repeat(DISPATCH_BODY_MAX + 1)
    );
    expect(code).toBe(1);
    expect(stdout).toContain(
      `- --body is 1 characters over the ${DISPATCH_BODY_MAX}-character limit (${DISPATCH_BODY_MAX + 1}/${DISPATCH_BODY_MAX})`
    );
    expect(requests).toEqual([]);
  });

  // A dry run runs the call's own pre-send check, so a call it refuses is refused alike, before
  // any request: the owner resolved from LEGION_ISSUE or left missing, a ref naming another
  // owner, a picture file that is not there.
  test("--dry-run refuses exactly what the call refuses before sending", async () => {
    const cases: { argv: string[]; env?: Record<string, string> }[] = [
      { argv: ["issue-update", "--status", "done"], env: { LEGION_ISSUE: "DSP-1" } },
      { argv: ["issue-update", "--status", "done"] },
      { argv: ["comment", "--issue", "DSP-2", "--ref", "dispatch://DSP-1", "--body", "x"] },
      { argv: ["message", "--issue", "DSP-1", "--body", "x", "--image", "missing.png"] },
    ];
    for (const { argv, env } of cases) {
      const call = await run(argv, { ...omp, ...env });
      const dry = await run([...argv, "--dry-run"], { ...omp, ...env });
      expect(call.code, argv.join(" ")).toBe(1);
      expect(call.stdout, argv.join(" ")).toStartWith("dispatch ");
      expect(dry, argv.join(" ")).toEqual(call);
    }
    expect(requests).toEqual([]);
  });

  // What the call fills before its check (the issue from LEGION_ISSUE, the document a ref names,
  // a dashboard URL on this server as its dispatch:// form) passes a dry run too, which prints
  // the caller's own arguments.
  test("--dry-run passes what the call resolves, printing the caller's arguments", async () => {
    const dashboard = `http://127.0.0.1:${server.port}/issues/DSP-1/spec`;
    const cases: { argv: string[]; env?: Record<string, string>; args: object }[] = [
      { argv: ["message", "--body", "hi"], env: { LEGION_ISSUE: "DSP-1" }, args: { body: "hi" } },
      ...["dispatch://DSP-1/spec", dashboard].map((ref) => ({
        argv: ["comment", "--ref", ref, "--quote", "old", "--body", "why"],
        args: { ref, quote: "old", body: "why" },
      })),
    ];
    for (const { argv, env, args } of cases) {
      const { code, stdout } = await run([...argv, "--dry-run"], { ...omp, ...env });
      expect(code, `${argv.join(" ")}\n${stdout}`).toBe(0);
      expect(JSON.parse(stdout)).toEqual(args);
    }
    expect(requests).toEqual([]);
  });

  // A dry run reads nothing from Dispatch, so it checks and prints with Dispatch off too, and
  // prints the JSON alone.
  test("--dry-run checks and prints with Dispatch off", async () => {
    const off = { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined };
    const passed = await run(["message", "--issue", "DSP-1", "--body", "hi", "--dry-run"], off);
    expect(passed.code).toBe(0);
    expect(JSON.parse(passed.stdout)).toEqual({ issue: "DSP-1", body: "hi" });
    const refused = await run(
      ["issue-update", "--issue", "DSP-1", "--status", "in_progress", "--reason", "x", "--dry-run"],
      off
    );
    expect(refused.code).toBe(1);
    expect(refused.stdout).toStartWith(
      "dispatch issue-update was not called: 1 problem\n- reason goes only with status done"
    );
    expect(requests).toEqual([]);
  });

  test("the follow notice is printed once per ask across processes", async () => {
    const ask = ["ask", "--issue", "DSP-1", "--question", "Which way?", "--option", "A"];
    const first = await run(ask, omp);
    nextProcess("ses-1");
    const second = await run(ask, omp);
    expect(first.code).toBe(0);
    expect(second.code).toBe(0);
    expect(first.stdout).toContain("Following ask ask-a");
    expect(second.stdout).not.toContain("Following ask ask-a");
  });

  test("the triage line is printed once per issue across processes", async () => {
    triage = true;
    const message = ["message", "--issue", "DSP-1", "--body", "Deliverable landed."];
    const first = await run(message, omp);
    nextProcess("ses-1");
    const second = await run(message, omp);
    expect(first.stdout).toContain("DSP-1 is still in triage");
    expect(second.stdout).not.toContain("is still in triage");
  });

  test("a read from a Dispatch server older than the repository fields answers, warning on stderr", async () => {
    const { code, stdout, stderr } = await run(["read", "--issue", "DSP-3"], omp);
    expect(code).toBe(0);
    expect(stdout).toContain(
      "Repository: unknown (this Dispatch server predates repository fields; upgrade it)\n"
    );
    expect(stderr).toBe(
      "dispatch: warning: Dispatch issue is missing resolved_repo: this Dispatch server predates repository fields, so DSP-3's repository is unknown; upgrade the server\n"
    );
  });

  // An issue read lists the issue's last 10 events unless it is asked for more: --events N lists
  // the last N and --all-events every one, read 200 at a time, the most one events request returns.
  describe("read --events and --all-events", () => {
    const listed = (stdout: string) => stdout.split("\n").filter((line) => /^- #\d+ /.test(line));

    test("--events 25 on an issue whose last_seq is 40 asks for the events after 15 through 40 and prints 25", async () => {
      const { code, stdout } = await run(["read", "--issue", "DSP-4", "--events", "25"], omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=15&limit=25"]);
      const events = listed(stdout);
      expect(events).toHaveLength(25);
      expect(events[0]).toStartWith("- #16 message.created · human h · ");
      expect(events.at(-1)).toStartWith("- #40 message.created · human h · ");
    });

    test("without either flag the read lists the last 10, as before", async () => {
      const { code, stdout } = await run(["read", "--issue", "DSP-4"], omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=30&limit=10"]);
      expect(listed(stdout).map((line) => line.split(" ")[1])).toEqual(
        Array.from({ length: 10 }, (_, index) => `#${31 + index}`)
      );
    });

    test("--all-events and an --events count over one page read every page up to last_seq", async () => {
      eventLog = { last: 450, served: 450, queries: [] };
      const all = await run(["read", "--issue", "DSP-4", "--all-events"], omp);
      expect(all.code, all.stdout).toBe(0);
      expect(eventLog.queries).toEqual([
        "?after=0&limit=200",
        "?after=200&limit=200",
        "?after=400&limit=50",
      ]);
      expect(listed(all.stdout).map((line) => line.split(" ")[1])).toEqual(
        Array.from({ length: 450 }, (_, index) => `#${index + 1}`)
      );

      eventLog.queries.length = 0;
      const last = await run(["read", "--issue", "DSP-4", "--events", "250"], omp);
      expect(last.code, last.stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=200&limit=200", "?after=400&limit=50"]);
      const events = listed(last.stdout);
      expect(events).toHaveLength(250);
      expect(events[0]).toStartWith("- #201 ");
      expect(events.at(-1)).toStartWith("- #450 ");
    });

    test("an --events count past the issue's history lists every event through the captured last_seq", async () => {
      const { code, stdout } = await run(["read", "--issue", "DSP-4", "--events", "500"], omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=0&limit=40"]);
      expect(listed(stdout)).toHaveLength(40);
    });

    test("events written after the issue was read are left to the next read", async () => {
      eventLog.served = 45;
      const last = await run(["read", "--issue", "DSP-4", "--events", "25"], omp);
      expect(last.code, last.stdout).toBe(0);
      expect(listed(last.stdout).at(-1)).toStartWith("- #40 ");
      const all = await run(["read", "--issue", "DSP-4", "--all-events"], omp);
      expect(all.code, all.stdout).toBe(0);
      expect(listed(all.stdout)).toHaveLength(40);
      expect(listed(all.stdout).at(-1)).toStartWith("- #40 ");
      const oversized = await run(["read", "--issue", "DSP-4", "--events", "500"], omp);
      expect(oversized.code, oversized.stdout).toBe(0);
      expect(eventLog.queries).toEqual([
        "?after=15&limit=25",
        "?after=0&limit=40",
        "?after=0&limit=40",
      ]);
      expect(listed(oversized.stdout)).toHaveLength(40);
      expect(listed(oversized.stdout).at(-1)).toStartWith("- #40 ");
    });

    test("--all-events on an issue with no events sends no events request", async () => {
      eventLog = { last: 0, served: 0, queries: [] };
      const { code, stdout } = await run(["read", "--issue", "DSP-4", "--all-events"], omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual([]);
      expect(stdout).toContain("\nEvents:\n- none\n");
    });

    test("--no-all-events beside --events keeps the count", async () => {
      const argv = ["read", "--issue", "DSP-4", "--events", "25", "--no-all-events"];
      const { code, stdout } = await run(argv, omp);
      expect(code, stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=15&limit=25"]);
    });

    test("the issue's ref and its log read the same count", async () => {
      for (const argv of [
        ["--ref", "dispatch://DSP-4", "--events", "25"],
        ["--ref", "dispatch://DSP-4/log", "--events", "25"],
      ]) {
        eventLog.queries.length = 0;
        const { code, stdout } = await run(["read", ...argv], omp);
        expect(code, stdout).toBe(0);
        expect(eventLog.queries, argv.join(" ")).toEqual(["?after=15&limit=25"]);
        expect(listed(stdout), argv.join(" ")).toHaveLength(25);
      }
      eventLog.queries.length = 0;
      const log = await run(["read", "--ref", "dispatch://DSP-4/log", "--all-events"], omp);
      expect(log.code, log.stdout).toBe(0);
      expect(eventLog.queries).toEqual(["?after=0&limit=40"]);
      expect(listed(log.stdout)).toHaveLength(40);
    });

    test("--help names both flags", async () => {
      const { code, stdout } = await run(["read", "--help"], omp);
      expect(code).toBe(0);
      expect(stdout).toContain("\n  --events <n>\n");
      expect(stdout).toContain("\n  --all-events\n");
    });

    // A read that lists no events would drop the count without a word, so it is refused before any
    // request, naming what the read prints instead.
    test("two counts, a count that is not a positive integer, or one the read would drop is refused before any request", async () => {
      const id = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
      const dropped = (flag: string, read: string) =>
        `${flag} lists an issue's events, which dispatch read prints only for an issue or its dispatch://KEY/log, not for ${read}: drop ${flag}`;
      const cases: [string[], string][] = [
        [
          ["--issue", "DSP-4", "--events", "5", "--all-events"],
          "--events and --all-events both say how many events the read lists: name one, --events N for the last N or --all-events for every one",
        ],
        [["--issue", "DSP-4", "--events", "0"], "--events: Too small: expected number to be >=1"],
        [["--issue", "DSP-4", "--events", "2.5"], "--events must be an integer, not 2.5"],
        [["--message", id, "--events", "5"], dropped("--events", "a message's conversation")],
        [
          ["--issue", "DSP-4", "--artifact", "spec", "--all-events"],
          dropped("--all-events", "a document summary"),
        ],
        [
          ["--project", "CORE", "--artifact", "runbook", "--events", "5"],
          dropped("--events", "a document summary"),
        ],
        [
          ["--ref", "dispatch://DSP-4/spec", "--events", "5"],
          dropped("--events", "a document summary"),
        ],
        [
          ["--ref", "dispatch://CORE/artifact/runbook", "--all-events"],
          dropped("--all-events", "a document summary"),
        ],
        [
          ["--ref", "dispatch://DSP-4/children", "--events", "5"],
          dropped("--events", "the issue's children"),
        ],
        [["--ref", `dispatch://DSP-4/ask/${id}`, "--events", "5"], dropped("--events", "an ask")],
        [
          ["--ref", `dispatch://CORE/artifact/runbook/ask/${id}`, "--events", "5"],
          dropped("--events", "an ask"),
        ],
        [
          ["--ref", `dispatch://CORE/artifact/runbook/comment/${id}`, "--all-events"],
          dropped("--all-events", "a comment"),
        ],
        [
          ["--ref", `dispatch://DSP-4/comment/${id}`, "--all-events"],
          dropped("--all-events", "a comment"),
        ],
        [
          ["--ref", `dispatch://DSP-4/message/${id}`, "--events", "5"],
          dropped("--events", "a message's conversation"),
        ],
      ];
      for (const [flags, problem] of cases) {
        const { code, stdout } = await run(["read", ...flags], omp);
        expect(code, flags.join(" ")).toBe(1);
        expect(stdout, flags.join(" ")).toContain(`\n- ${problem}\n`);
      }
      expect(requests).toEqual([]);
    });

    // The log lists events, so beside an --artifact the log ref cannot use, only the artifact is
    // refused: dropping it, as the refusal says, reads the log with the count.
    test("an --artifact beside dispatch://KEY/log is refused alone, keeping the count", async () => {
      const { code, stdout } = await run(
        ["read", "--ref", "dispatch://DSP-4/log", "--artifact", "spec", "--events", "5"],
        omp
      );
      expect(code).toBe(1);
      expect(stdout).toContain("drop --artifact to read the ref");
      expect(stdout).not.toContain("drop --events");
      expect(requests).toEqual([]);
    });
  });

  test("a picture is written to a file the agent opens", async () => {
    const { code, stdout } = await run(
      ["doc-read", "--ref", "dispatch://agent/ses-pic/artifact/shot-png@v1"],
      { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-2" }
    );
    expect(code).toBe(0);
    const line = stdout.split("\n").find((candidate) => candidate.startsWith("- picture: "));
    const path = line?.slice("- picture: ".length).split(" (")[0] ?? "";
    expect(existsSync(path)).toBe(true);
    expect(readFileSync(path).subarray(0, 4).toString("hex")).toBe("89504e47");
  });

  test("every call that reaches Dispatch appends one ledger line naming its tool", async () => {
    await run(["message", "--issue", "DSP-1", "--body", "Deliverable landed."], omp);
    await run(["read", "--message", "missing"], omp);
    expect(ledger("ses-1").map((entry) => entry.tool)).toEqual([
      "dispatch_message",
      "dispatch_read",
    ]);
    expect(ledger("ses-1")[1]?.error).toBeDefined();
  });

  test("Claude Code's whole output stays under its read-back limit, picture lines kept, and names the full result", async () => {
    const limit = 25_000;
    const read = ["read", "--message", LONG_THREAD];
    // The result's text without the reply body, measured on a host that never shortens output.
    replyLength = 1;
    const short = await run(read, { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-long-1" });
    const textOf = (stdout: string): string => stdout.slice(0, stdout.indexOf("\n- picture: "));
    const overhead = textOf(short.stdout).length - 1;
    // The text alone is 10 characters under the limit; the picture line carries the output past it.
    replyLength = limit - overhead - 10;
    const full = await run(read, { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-long-2" });
    const text = textOf(full.stdout);
    expect(text.length).toBe(limit - 10);
    expect(full.stdout.length).toBeGreaterThan(limit);

    const claude = await run(read, {
      DISPATCH_HOST: "claude",
      CLAUDE_CODE_SESSION_ID: "ses-long-3",
    });
    expect(claude.code).toBe(0);
    // print() ends the output with one newline.
    expect(claude.stdout.length).toBeLessThanOrEqual(limit + 1);
    const pictureLine = /\n(- picture: [^\n]*)/.exec(claude.stdout)?.[1];
    expect(pictureLine).toBeDefined();
    // The file holds the whole result, its picture lines included.
    const marker = /\(the full result, (\d+) characters: (\S+)\)/.exec(claude.stdout);
    const written = `${text}\n${pictureLine}`;
    expect(marker?.[1]).toBe(String(written.length));
    expect(readFileSync(marker?.[2] ?? "", "utf-8")).toBe(written);
  });

  test("a state write that fails after a cut result still leaves Claude Code's output under the limit", async () => {
    const limit = 25_000;
    const read = ["read", "--message", LONG_THREAD];
    replyLength = limit;
    const dir = join(stateDir, "sessions", "ses-long-4");
    mkdirSync(dir, { recursive: true });
    // The ledger cannot be written, so the CLI adds its own diagnostic line after the result.
    mkdirSync(join(dir, "results.jsonl"));
    const claude = await run(read, {
      DISPATCH_HOST: "claude",
      CLAUDE_CODE_SESSION_ID: "ses-long-4",
    });
    expect(claude.code).toBe(0);
    expect(claude.stdout).toContain("dispatch: Dispatch took the call, but this session's state");
    expect(claude.stdout).toContain("(the full result, ");
    expect(claude.stdout.length).toBeLessThanOrEqual(limit + 1);
  });
  test("a corrupted state.json is refused with exit 2, naming the file, and sends nothing", async () => {
    const dir = join(stateDir, "sessions", "ses-1");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "state.json"), "{not json");
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "x"], omp);
    expect(code).toBe(2);
    expect(stdout).toStartWith("dispatch: ");
    expect(stdout).toContain(join(dir, "state.json"));
    expect(requests).toEqual([]);
  });

  test("a ledger the call cannot be recorded in still reports the call, which Dispatch took", async () => {
    mkdirSync(join(stateDir, "sessions", "ses-1", "results.jsonl"), { recursive: true });
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "x"], omp);
    expect(code).toBe(0);
    expect(requests.map((request) => request.path)).toContain("/api/v1/issues/DSP-1/messages");
    expect(stdout).toContain("message-1");
    expect(stdout).toContain("dispatch: Dispatch took the call, but this session's state");
  });

  test("a picture that cannot be saved still prints the result text, naming the failure", async () => {
    const dir = join(stateDir, "sessions", "ses-2");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "pictures"), "a file where the directory goes");
    const { code, stdout } = await run(
      ["doc-read", "--ref", "dispatch://agent/ses-pic/artifact/shot-png@v1"],
      { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-2" }
    );
    expect(code).toBe(0);
    expect(stdout).toContain("Picture shot.png");
    expect(stdout).not.toContain("- picture: ");
    expect(stdout).toContain("dispatch: Dispatch took the call, but this session's state");
  });

  describe("doc-read --blocks", () => {
    const listing = [
      "Blocks of spec.md, in document order, each indented under the block holding it: id, type, (token; anchored comments and asks), opening words. A table lists its rows by the index delete_row takes and its cells by the row and the column delete_column takes.",
      "h-1 heading (sha256:h; 0 comments, 0 asks): # Café plan",
      // The 60th character is an emoji, two UTF-16 units: a cut by code unit would split it.
      `p-1 paragraph (sha256:p; 1 comment, 0 asks): ${"x".repeat(59)}😀…`,
      "l-1 bullet_list (sha256:l; 0 comments, 0 asks): - First item - Second item",
      "  i-1 list_item (sha256:i1; 0 comments, 0 asks): - First item",
      "    ip-1 paragraph (sha256:ip1; 0 comments, 0 asks): First item",
      "  i-2 list_item (sha256:i2; 0 comments, 0 asks): - Second item",
      "t-1 table (sha256:t; 0 comments, 1 ask): | # | Line | | | --- | --- | --- | | 1 | Backfill | extra |",
    ];
    const reads = [
      "/api/v1/issues/DSP-2",
      "/api/v1/artifacts/doc-2/text",
      "/api/v1/artifacts/doc-2/blocks",
      "/api/v1/artifacts/doc-2/text",
    ];

    test("lists every block's id, type, token, anchors and opening words, and each table's rows and cells", async () => {
      const { code, stdout } = await run(["doc-read", "--issue", "DSP-2", "--blocks"], omp);
      expect(code).toBe(0);
      expect(stdout).toBe(
        [
          ...listing,
          "  row 0, id r-0",
          '    row 0 column 0, id c-0-0, under "#": #',
          '    row 0 column 1, id c-0-1, under "Line": Line',
          "    row 0 column 2, id c-0-2: (empty)",
          "  row 1, no id yet",
          '    row 1 column 0, id c-1-0, under "#": 1',
          '    row 1 column 1, no id yet, under "Line": Backfill',
          "    row 1 column 2, id c-1-2: extra",
          "",
          "Document token: sha256:doc",
          "",
        ].join("\n")
      );
      expect(requests.map((request) => request.path)).toEqual(reads);
    });

    test("says the Dispatch lists no rows when its table entry carries none, and reads no markdown for them", async () => {
      blocksServer.tableRows = false;
      const { code, stdout } = await run(["doc-read", "--issue", "DSP-2", "--blocks"], omp);
      expect(code).toBe(0);
      expect(stdout).toBe(
        [
          ...listing,
          "  This Dispatch does not report table rows yet, so no row or column index is listed.",
          "",
          "Document token: sha256:doc",
          "",
        ].join("\n")
      );
      expect(requests.map((request) => request.path)).toEqual(reads);
    });

    test("refuses an uploaded picture, naming it, and reads none of its bytes", async () => {
      const { code, stdout } = await run(
        ["doc-read", "--issue", "DSP-2", "--artifact", "chart.png", "--blocks"],
        omp
      );
      expect(code).toBe(1);
      expect(stdout).toBe(
        "chart.png is an uploaded image, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks.\n"
      );
      expect(requests.map((request) => request.path)).toEqual(["/api/v1/issues/DSP-2"]);
    });

    test("refuses a conversation's upload, naming it, as it refuses an issue's", async () => {
      const { code, stdout } = await run(
        ["doc-read", "--ref", "dispatch://agent/ses-pic/artifact/shot-png@v1", "--blocks"],
        omp
      );
      expect(code).toBe(1);
      expect(stdout).toBe(
        "shot.png is an uploaded image, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks.\n"
      );
      expect(requests.map((request) => request.path)).toEqual([
        "/api/v1/agents/ses-pic/artifacts/shot-png",
      ]);
    });

    test("refuses a version, since only the live document's blocks are what an edit addresses", async () => {
      const { code, stdout } = await run(
        ["doc-read", "--issue", "DSP-2", "--blocks", "--version", "2"],
        omp
      );
      expect(code).toBe(1);
      expect(stdout).toContain(
        "- blocks lists the live document, the one dispatch doc-edit changes; name no version"
      );
      expect(requests).toEqual([]);
    });

    test("refuses a document that changed between its reads, listing nothing", async () => {
      blocksServer.editedBetweenReads = true;
      const { code, stdout } = await run(["doc-read", "--issue", "DSP-2", "--blocks"], omp);
      expect(code).toBe(1);
      expect(stdout).toBe(
        "spec.md changed while dispatch doc-read --blocks read it, so the opening words could name the wrong text; nothing is listed. Run it again.\n"
      );
    });
  });

  test("dispatch project prints the created project, and a taken key exits 1 naming PROJECT_EXISTS", async () => {
    const created = await run(["project", "--key", "SMOKE684", "--name", "Smoke"], omp);
    expect(created.code).toBe(0);
    expect(created.stdout).toContain("Created project SMOKE684 (Smoke)");
    expect(requests).toEqual([
      {
        method: "POST",
        path: "/api/v1/projects",
        body: expect.objectContaining({
          key: "SMOKE684",
          name: "Smoke",
          actor: expect.objectContaining({ kind: "session", id: "ses-1" }),
        }),
      },
    ]);

    const taken = await run(["project", "--key", "TAKEN", "--name", "Smoke"], omp);
    expect(taken.code).toBe(1);
    expect(taken.stdout).toContain("PROJECT_EXISTS: a project with this key already exists");
  });

  test("dispatch search refuses a project that is not a key in the server's wording, sending nothing", async () => {
    const { code, stdout } = await run(["search", "--query", "ok", "--project", "lower"], omp);
    expect(code).toBe(1);
    expect(stdout).toContain(`project is not a project key: ${PROJECT_KEY_RULE}`);
    expect(requests).toEqual([]);
  });

  test("dispatch projects prints each project's key and name, keys first", async () => {
    const { code, stdout } = await run(["projects"], omp);
    expect(code).toBe(0);
    expect(stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
    expect(requests).toEqual([{ method: "GET", path: "/api/v1/projects", body: undefined }]);
  });
});

describe("the dispatch CLI's envoy.json", () => {
  /** No Dispatch in the environment: the command reads it from a file. */
  const fromFile = { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined };

  /** Writes an envoy.json naming the test server into `dir`, returning its path. */
  function serverConfig(dir: string): string {
    mkdirSync(dir, { recursive: true });
    const file = join(dir, "envoy.json");
    writeFileSync(
      file,
      JSON.stringify({
        dispatch: {
          enabled: true,
          serverUrl: `http://127.0.0.1:${server.port}`,
          token: "test-token",
        },
      })
    );
    return file;
  }

  /** A directory whose envoy.json is not JSON, so a command that reads it exits 2. */
  function brokenConfigDirectory(): string {
    const dir = mkdtempSync(join(tmpdir(), "dispatch-cli-broken-"));
    writeFileSync(join(dir, "envoy.json"), "{not json");
    return dir;
  }

  // Each plugin sets DISPATCH_HOST and ENVOY_CONFIG_FILE together in its session's shell: that pair
  // is the session's own hand-off, read over whatever harness variables the shell also inherited.
  test("with DISPATCH_HOST set, reads the file ENVOY_CONFIG_FILE names, whatever the harness variables name", async () => {
    const handed = serverConfig(join(stateDir, "handed"));
    // An omp session on the default profile started from a named profile's shell: omp leaves the
    // parent's OMP_PROFILE and PI_CODING_AGENT_DIR in its shell, and its plugin read the neutral file.
    const nestedDefault = await run(["projects"], {
      ...fromFile,
      OMP_PROFILE: "other",
      PI_CODING_AGENT_DIR: brokenConfigDirectory(),
      ENVOY_CONFIG_FILE: handed,
    });
    expect(nestedDefault.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
    expect(nestedDefault.code).toBe(0);
    // A named Claude Code profile whose shell's rc file exported another CLAUDE_CONFIG_DIR over
    // the profile's: the hook handed the profile's file.
    const namedClaude = await run(["projects"], {
      ...fromFile,
      DISPATCH_HOST: "claude",
      CLAUDE_CODE_SESSION_ID: "ses-1",
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
      ENVOY_CONFIG_FILE: handed,
    });
    expect(namedClaude.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
    // OpenCode likewise.
    const opencode = await run(["projects"], {
      ...fromFile,
      DISPATCH_HOST: "opencode",
      OPENCODE_CONFIG_DIR: brokenConfigDirectory(),
      ENVOY_CONFIG_FILE: handed,
    });
    expect(opencode.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
  });

  test("with DISPATCH_HOST set and no ENVOY_CONFIG_FILE, each host takes its own harness's directory and no other's, else the neutral file", async () => {
    const claude = { DISPATCH_HOST: "claude", CLAUDE_CODE_SESSION_ID: "ses-1" };
    const opencode = { DISPATCH_HOST: "opencode", DISPATCH_SESSION_ID: "ses-1" };
    const profile = join(stateDir, "profile");
    serverConfig(profile);
    const ompRun = await run(["projects"], {
      ...fromFile,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: profile,
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
      OPENCODE_CONFIG_DIR: brokenConfigDirectory(),
    });
    expect(ompRun.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");

    const claudeDirectory = join(stateDir, "claude-own");
    serverConfig(claudeDirectory);
    const claudeRun = await run(["projects"], {
      ...fromFile,
      ...claude,
      CLAUDE_CONFIG_DIR: claudeDirectory,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: brokenConfigDirectory(),
    });
    expect(claudeRun.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
    // An empty ENVOY_CONFIG_FILE is no hand-off: the host's own rule, not an inherited profile's.
    const emptyHanded = await run(["projects"], {
      ...fromFile,
      ...claude,
      CLAUDE_CONFIG_DIR: claudeDirectory,
      ENVOY_CONFIG_FILE: "",
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: brokenConfigDirectory(),
    });
    expect(emptyHanded.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");

    const own = join(stateDir, "opencode");
    serverConfig(own);
    const ownRun = await run(["projects"], {
      ...fromFile,
      ...opencode,
      OPENCODE_CONFIG_DIR: own,
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
    });
    expect(ownRun.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");

    serverConfig(join(stateDir, ".config", "legion"));
    const neutral = await run(["projects"], {
      ...fromFile,
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
      OPENCODE_CONFIG_DIR: brokenConfigDirectory(),
    });
    expect(neutral.stdout).toBe("CORE Core\nSMOKE684 Smoke\n");
  });

  // An omp session started from a Claude Code Bash inherits CLAUDE_CODE_CHILD_SESSION=1, and its
  // plugin sets DISPATCH_HOST=omp: the command acts as the omp session on omp's file.
  test("DISPATCH_HOST wins over CLAUDE_CODE_CHILD_SESSION=1", async () => {
    const profile = join(stateDir, "profile");
    serverConfig(profile);
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "hello"], {
      ...fromFile,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: profile,
      CLAUDE_CODE_CHILD_SESSION: "1",
      CLAUDE_CODE_SESSION_ID: "claude-ses",
      CLAUDE_CONFIG_DIR: brokenConfigDirectory(),
    });
    expect(stdout).not.toStartWith("dispatch: ");
    expect(code).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/messages");
    expect(posted?.body).toMatchObject({ actor: { id: "ses-1" } });
  });

  // Claude Code sets CLAUDE_CODE_CHILD_SESSION=1 in every Bash it runs, and a subagent's Bash can
  // get none of what the bridge's SessionStart hook writes: no DISPATCH_HOST, and whatever
  // ENVOY_CONFIG_FILE an outer omp session exported.
  test("a Claude Code shell with no DISPATCH_HOST is host claude, reading CLAUDE_CONFIG_DIR's file ahead of an inherited ENVOY_CONFIG_FILE", async () => {
    const claude = join(stateDir, "claude");
    serverConfig(claude);
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "hello"], {
      DISPATCH_URL: undefined,
      DISPATCH_TOKEN: undefined,
      CLAUDE_CODE_CHILD_SESSION: "1",
      CLAUDE_CODE_SESSION_ID: "subagent-1",
      CLAUDE_CONFIG_DIR: claude,
      ENVOY_CONFIG_FILE: join(brokenConfigDirectory(), "envoy.json"),
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: brokenConfigDirectory(),
    });
    expect(stdout).not.toStartWith("dispatch: ");
    expect(code).toBe(0);
    const posted = requests.find((request) => request.path === "/api/v1/issues/DSP-1/messages");
    expect(posted?.body).toMatchObject({ actor: { id: "subagent-1" } });
  });

  test("with neither DISPATCH_HOST nor CLAUDE_CODE_CHILD_SESSION=1 it refuses naming DISPATCH_HOST, whatever else Claude Code left", async () => {
    const claude = join(stateDir, "claude");
    serverConfig(claude);
    for (const child of [{}, { CLAUDE_CODE_CHILD_SESSION: "0" }]) {
      const { code, stdout } = await run(["projects"], {
        DISPATCH_URL: undefined,
        DISPATCH_TOKEN: undefined,
        CLAUDE_CODE_SESSION_ID: "subagent-1",
        CLAUDE_CONFIG_DIR: claude,
        ...child,
      });
      expect(stdout).toStartWith("dispatch: DISPATCH_HOST is not set.");
      // An agent in a Python cell reads this first. With a made-up session id, the CLI finds no
      // AskGate record, so the write posts with no review.
      expect(stdout).toContain("From an agent's session, send the command as a bash call.");
      expect(stdout).toContain("set DISPATCH_SESSION_ID to the id of your own session");
      expect(code).toBe(2);
    }
    expect(requests).toEqual([]);
  });

  test("a file location that cannot be worked out is refused naming it, exit 2, sending nothing", async () => {
    for (const [variables, refusal] of [
      [{ XDG_CONFIG_HOME: "rel" }, "XDG_CONFIG_HOME is rel"],
      [{ ENVOY_CONFIG_FILE: "envoy.json" }, "ENVOY_CONFIG_FILE is envoy.json"],
    ] as const) {
      const { code, stdout } = await run(["projects"], { ...fromFile, ...variables });
      expect(stdout).toBe(`dispatch: ${refusal}, which is not an absolute path\n`);
      expect(code).toBe(2);
    }
    expect(requests).toEqual([]);
  });

  test("a missing file prints the notice and exits 2, sending nothing", async () => {
    const { code, stdout } = await run(["projects"], fromFile);
    expect(stdout).toBe(
      `dispatch: no envoy.json at ${join(stateDir, ".config", "legion", "envoy.json")}: Dispatch is off\n`
    );
    expect(code).toBe(2);
    expect(requests).toEqual([]);
  });

  test("a missing neutral file beside the OpenCode-era file names the move and exits 2, running and sending nothing", async () => {
    const marker = join(stateDir, "old-token-ran");
    const old = join(stateDir, ".config", "opencode", "envoy.json");
    mkdirSync(join(stateDir, ".config", "opencode"), { recursive: true });
    writeFileSync(
      old,
      JSON.stringify({
        dispatch: {
          enabled: true,
          serverUrl: `http://127.0.0.1:${server.port}`,
          token: `!touch ${marker}; echo old-token`,
        },
      })
    );

    const { code, stdout } = await run(["projects"], fromFile);

    expect(stdout).toBe(
      `dispatch: no envoy.json at ${join(stateDir, ".config", "legion", "envoy.json")}: Dispatch is off; ${old} is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/\n`
    );
    expect(code).toBe(2);
    expect(existsSync(marker)).toBe(false);
    expect(requests).toEqual([]);
  });

  test("a checkout's .opencode/envoy.json is named on every call, and its server is never used", async () => {
    serverConfig(join(stateDir, ".config", "legion"));
    const old = join(stateDir, ".opencode", "envoy.json");
    mkdirSync(join(stateDir, ".opencode"), { recursive: true });
    writeFileSync(
      old,
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: "http://127.0.0.1:1", token: "checkout-token" },
      })
    );

    const { code, stdout } = await run(["projects"], fromFile);

    expect(stdout).toBe(
      `dispatch: ${old} is not read: mkdir -p ${join(stateDir, ".envoy")} && mv ${old} ${join(stateDir, ".envoy", "envoy.json")}\nCORE Core\nSMOKE684 Smoke\n`
    );
    expect(code).toBe(0);
  });

  test("a profile's missing file tells dispatch where the old file belongs without running its token", async () => {
    const profile = join(stateDir, "profile");
    mkdirSync(profile);
    const marker = join(stateDir, "old-token-ran");
    const old = join(stateDir, ".config", "opencode", "envoy.json");
    mkdirSync(join(stateDir, ".config", "opencode"), { recursive: true });
    writeFileSync(
      old,
      JSON.stringify({
        dispatch: {
          enabled: true,
          serverUrl: `http://127.0.0.1:${server.port}`,
          token: `!touch ${marker}; echo old-token`,
        },
      })
    );

    const { code, stdout } = await run(["projects"], {
      ...fromFile,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: profile,
    });

    const chosen = join(profile, "envoy.json");
    expect(stdout).toBe(
      `dispatch: no envoy.json at ${chosen}: Dispatch is off; ${old} is not read for this profile; copy or link it to ${chosen}, or write that file\n`
    );
    expect(code).toBe(2);
    expect(existsSync(marker)).toBe(false);
    expect(requests).toEqual([]);
  });

  test("a file that turns Dispatch off says so and exits 2, sending nothing", async () => {
    const dir = join(stateDir, "off");
    mkdirSync(dir, { recursive: true });
    const file = join(dir, "envoy.json");
    writeFileSync(file, JSON.stringify({ dispatch: { enabled: false } }));
    const { code, stdout } = await run(["projects"], { ...fromFile, ENVOY_CONFIG_FILE: file });
    expect(stdout).toBe(`dispatch: ${file} sets dispatch.enabled to false: Dispatch is off\n`);
    expect(code).toBe(2);
    expect(requests).toEqual([]);
  });

  test("a file that names no Dispatch server, and no DISPATCH_URL, names the file and exits 2, sending nothing", async () => {
    const dir = join(stateDir, "envoy-only");
    mkdirSync(dir, { recursive: true });
    const file = join(dir, "envoy.json");
    for (const contents of [
      { envoy: { enabled: true, natsUrls: ["nats://nats.internal.example:4222"] } },
      { dispatch: { token: "file-token" } },
    ]) {
      writeFileSync(file, JSON.stringify(contents));
      const { code, stdout } = await run(["projects"], { ...fromFile, ENVOY_CONFIG_FILE: file });
      expect(stdout).toBe(
        `dispatch: ${file} names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off\n`
      );
      expect(code).toBe(2);
    }
    expect(requests).toEqual([]);
  });
});

describe("the dispatch CLI's subscription note", () => {
  const note =
    "(not subscribed to DSP-1; envoy_subscribe notifications.dispatch.issue.DSP-1.> for every event on it)";
  const withListener = { ...omp, ENVOY_URL: `http://127.0.0.1:${server.port}` };
  const post = () => run(["message", "--issue", "DSP-1", "--body", "hello"], withListener);
  const listed = (topics: readonly string[]) => () =>
    Response.json({ session_id: "ses-1", machine_id: "m", dir: "/", topics });

  test("a write names its issue only when the listener says none of the session's topics covers it", async () => {
    for (const [label, answer, noted] of [
      ["its own topic alone", listed(["notifications.agent.ses-1"]), true],
      [
        "the issue",
        listed(["notifications.agent.ses-1", "notifications.dispatch.issue.DSP-1.>"]),
        false,
      ],
      ["every Dispatch event", listed(["notifications.dispatch.>"]), false],
      // The listener holds no record of the session: it is subscribed to nothing.
      ["no record", () => Response.json({ error: "not found" }, { status: 404 }), true],
      // The listener cannot answer: the write says nothing about subscriptions.
      ["a listener error", () => Response.json({ error: "down" }, { status: 500 }), false],
    ] as const) {
      interests = answer;
      requests.length = 0;
      const { code, stdout } = await post();
      expect(code).toBe(0);
      expect([label, stdout.includes(note), stdout.includes("not subscribed")]).toEqual([
        label,
        noted,
        noted,
      ]);
      expect(requests.some((request) => request.path === "/v1/interests/ses-1")).toBe(true);
    }
  });

  test("with Envoy off, a write says nothing about subscriptions and asks no listener", async () => {
    const { code, stdout } = await run(["message", "--issue", "DSP-1", "--body", "hello"], omp);
    expect(code).toBe(0);
    expect(stdout).not.toContain("not subscribed");
    expect(requests.some((request) => request.path.startsWith("/v1/interests/"))).toBe(false);
  });

  test("an ask names the subscribe line, in its result and its follow notice, only when no subscription covers the issue, reading the listener once", async () => {
    const ask = ["ask", "--issue", "DSP-1", "--question", "Which way?", "--option", "A"];
    const line = "For every event on DSP-1: envoy_subscribe notifications.dispatch.issue.DSP-1.>";
    for (const [label, answer, named] of [
      ["its own topic alone", listed(["notifications.agent.ses-1"]), true],
      ["the issue", listed(["notifications.dispatch.issue.DSP-1.>"]), false],
      ["a listener error", () => Response.json({ error: "down" }, { status: 500 }), false],
    ] as const) {
      interests = answer;
      requests.length = 0;
      stateDir = mkdtempSync(join(tmpdir(), "dispatch-cli-"));
      nextProcess("ses-1");
      const { code, stdout } = await run(ask, withListener);
      expect(code).toBe(0);
      expect(stdout).toContain("Following ask ask-a");
      // Both the result's line and the notice's, or neither.
      expect([label, stdout.split(line).length - 1]).toEqual([label, named ? 2 : 0]);
      // One read, shared by the result and the notice, and no retry after a 5xx.
      expect(requests.filter((request) => request.path === "/v1/interests/ses-1")).toHaveLength(1);
    }
  });

  test("a listener read that throws leaves the landed write reported as written", async () => {
    // The listener's fetch throws a value no transport wraps: the read is the only thing it breaks.
    const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).includes("/v1/interests/")) throw "listener exploded";
      return fetch(input, init);
    }) as typeof fetch;
    let stdout = "";
    const code = await runDispatchCli(
      ["message", "--issue", "DSP-1", "--body", "hello"],
      environment(withListener),
      {
        stdout: (text) => {
          stdout += text;
        },
        stderr: () => {},
        readText: () => "",
        cwd: stateDir,
        fetchImpl,
      }
    );
    expect(code).toBe(0);
    expect(stdout).toStartWith("Posted message message-1");
    expect(stdout).not.toContain("not subscribed");
    expect(requests.some((request) => request.path === "/api/v1/issues/DSP-1/messages")).toBe(true);
  });

  test("a read asks no listener", async () => {
    interests = listed([]);
    const { code } = await run(["projects"], withListener);
    expect(code).toBe(0);
    expect(requests.some((request) => request.path.startsWith("/v1/interests/"))).toBe(false);
  });
});

// A reader of a transcript, the skill-scenario scorer among them, finds a refusal the CLI printed
// even when a pipe hid its exit: each path below is the real CLI's, and its first line is the one
// refusal cliRefusals reads. The CLI's other `dispatch:` lines refuse nothing.
describe("cliRefusals", () => {
  const fromFile = { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined };
  /** The one refusal `stdout` holds: its first line, and the command it names. */
  const refusal = (stdout: string, command: string | null) => [
    { line: stdout.split("\n")[0], command },
  ];

  test("an argument check's refusal names its command, in the call and in its dry run (exit 1)", async () => {
    for (const argv of [
      ["message", "--issue", "DSP-1", "--isue", "X"],
      ["issue-update", "--status", "done"],
      ["issue-update", "--status", "done", "--dry-run"],
    ]) {
      const { code, stdout } = await run(argv, omp);
      expect(code, argv.join(" ")).toBe(1);
      expect(cliRefusals(stdout), argv.join(" ")).toEqual(refusal(stdout, argv[0] ?? ""));
    }
    expect(requests).toEqual([]);
  });

  test("each usage refusal is a refusal of no command (exit 2)", async () => {
    const off = join(stateDir, "off.json");
    writeFileSync(off, JSON.stringify({ dispatch: { enabled: false } }));
    const cases: [string, readonly string[], Record<string, string | undefined>][] = [
      ["unknown command", ["frobnicate"], omp],
      ["no host", ["projects"], { DISPATCH_SESSION_ID: "ses-1" }],
      ["another host", ["projects"], { DISPATCH_HOST: "vim", DISPATCH_SESSION_ID: "ses-1" }],
      ["no omp session", ["projects"], { DISPATCH_HOST: "omp" }],
      ["no Claude Code session", ["projects"], { DISPATCH_HOST: "claude" }],
      ["a session id of two segments", ["projects"], { ...omp, DISPATCH_SESSION_ID: "a/b" }],
      ["a relative ENVOY_CONFIG_FILE", ["projects"], { ...fromFile, ENVOY_CONFIG_FILE: "e.json" }],
      ["a relative XDG_CONFIG_HOME", ["projects"], { ...fromFile, XDG_CONFIG_HOME: "rel" }],
      ["no envoy.json", ["projects"], fromFile],
      ["a file that turns Dispatch off", ["projects"], { ...fromFile, ENVOY_CONFIG_FILE: off }],
      ["DISPATCH_URL with no token", ["projects"], { ...omp, DISPATCH_TOKEN: undefined }],
    ];
    for (const [name, argv, env] of cases) {
      const { code, stdout } = await run(argv, env);
      expect(code, name).toBe(2);
      expect(stdout, name).toStartWith("dispatch: ");
      expect(cliRefusals(stdout), name).toEqual(refusal(stdout, null));
    }
    expect(requests).toEqual([]);
  });

  test("a session state the CLI cannot read is a refusal (exit 2)", async () => {
    const corrupt = join(stateDir, "sessions", "ses-1");
    mkdirSync(corrupt, { recursive: true });
    writeFileSync(join(corrupt, "state.json"), "{not json");
    const unreadable = await run(["projects"], omp);
    expect(unreadable.code).toBe(2);
    expect(cliRefusals(unreadable.stdout)).toEqual(refusal(unreadable.stdout, null));

    stateDir = mkdtempSync(join(tmpdir(), "dispatch-cli-"));
    writeFileSync(join(stateDir, "sessions"), "a file where the directory goes");
    const unlisted = await run(["projects"], omp);
    expect(unlisted.code).toBe(2);
    expect(unlisted.stdout).toStartWith("dispatch: ENOTDIR: ");
    expect(cliRefusals(unlisted.stdout)).toEqual(refusal(unlisted.stdout, null));
    expect(requests).toEqual([]);
  });

  test("a warning, the note that the state was not written, and a moved file's notice refuse nothing", async () => {
    const warned = await run(["read", "--issue", "DSP-3"], omp);
    expect(warned.code).toBe(0);
    expect(warned.stderr).toStartWith("dispatch: warning: ");
    expect(cliRefusals(`${warned.stderr}${warned.stdout}`)).toEqual([]);

    stateDir = mkdtempSync(join(tmpdir(), "dispatch-cli-"));
    mkdirSync(join(stateDir, "sessions", "ses-1", "results.jsonl"), { recursive: true });
    const unrecorded = await run(["message", "--issue", "DSP-1", "--body", "x"], omp);
    expect(unrecorded.code).toBe(0);
    expect(unrecorded.stdout).toContain("\ndispatch: Dispatch took the call, but this session's");
    expect(cliRefusals(unrecorded.stdout)).toEqual([]);

    stateDir = mkdtempSync(join(tmpdir(), "dispatch-cli-"));
    mkdirSync(join(stateDir, ".config", "legion"), { recursive: true });
    writeFileSync(
      join(stateDir, ".config", "legion", "envoy.json"),
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: `http://127.0.0.1:${server.port}`, token: "t" },
      })
    );
    mkdirSync(join(stateDir, ".opencode"), { recursive: true });
    writeFileSync(join(stateDir, ".opencode", "envoy.json"), "{}");
    const moved = await run(["projects"], fromFile);
    expect(moved.code).toBe(0);
    expect(moved.stdout).toStartWith("dispatch: ");
    expect(cliRefusals(moved.stdout)).toEqual([]);
  });

  // A refusal the CLI prints before it reaches Dispatch is logged too, whenever the call knows its
  // session: a reader of the ledger (pi-envoy's run-end check, the skill-scenario rig) sees a call
  // whose output a pipe, `$(…)` or another program swallowed. The host and the command need not be
  // known for the session to be: a Python `subprocess.run(["dispatch", …])` inherits the session id
  // without `DISPATCH_HOST`.
  test("an early refusal is logged when the call knows its session, with no request", async () => {
    const cases: [string, readonly string[], Record<string, string | undefined>, string][] = [
      ["an unknown flag", ["message", "--issue", "X", "--isue", "Y"], omp, "dispatch_message"],
      ["an unknown command", ["frobnicate"], omp, "dispatch"],
      // The CLI checks the command's name before its flags, so a help call of an unknown command
      // is refused, and a dry run with a bad argument is refused before it is a dry run. The
      // skill-scenario scorer matches each ledger line to its command on this.
      ["an unknown command's help", ["frobnicate", "--help"], omp, "dispatch"],
      [
        "a dry run's bad argument",
        ["message", "--issue", "X", "--isue", "Y", "--dry-run"],
        omp,
        "dispatch_message",
      ],
      ["no host", ["projects"], { DISPATCH_SESSION_ID: "ses-1" }, "dispatch_projects"],
      [
        "another host",
        ["projects"],
        { DISPATCH_HOST: "vim", DISPATCH_SESSION_ID: "ses-1" },
        "dispatch_projects",
      ],
      [
        "Dispatch turned off",
        ["projects"],
        { ...omp, DISPATCH_URL: undefined, DISPATCH_TOKEN: undefined },
        "dispatch_projects",
      ],
    ];
    for (const [name, argv, env, tool] of cases) {
      stateDir = mkdtempSync(join(tmpdir(), "dispatch-cli-"));
      const { code, stdout } = await run(argv, env);
      expect(code, name).toBeGreaterThan(0);
      const logged = ledger("ses-1");
      expect(
        logged.map((line) => line.tool),
        name
      ).toEqual([tool]);
      expect(logged[0]?.error, name).toBe(stdout.trimEnd());
      expect(cliRefusals(`${logged[0]?.error}\n`), name).toHaveLength(1);
    }
    expect(requests).toEqual([]);
  });

  // Catches: a crash, or an empty ledger file, when no session id is set at all, and a help or dry
  // run logged as a refusal.
  test("a call with no session logs nothing, and help and a dry run log no refusal", async () => {
    const { code } = await run(["projects"], { DISPATCH_HOST: "omp" });
    expect(code).toBe(2);
    expect(existsSync(join(stateDir, "sessions"))).toBe(false);

    stateDir = mkdtempSync(join(tmpdir(), "dispatch-cli-"));
    await run(["doc-edit", "--help"], omp);
    await run(["message", "--issue", "DSP-1", "--body", "hi", "--dry-run"], omp);
    expect(existsSync(join(stateDir, "sessions", "ses-1", "results.jsonl"))).toBe(false);
  });

  test("help, a dry run's arguments, and a refusal quoted inside a line refuse nothing", async () => {
    const help = await run(["doc-edit", "--help"], omp);
    const dry = await run(["message", "--issue", "DSP-1", "--body", "hi", "--dry-run"], omp);
    for (const { code, stdout } of [help, dry]) {
      expect(code).toBe(0);
      expect(cliRefusals(stdout)).toEqual([]);
    }
    expect(cliRefusals("It said: dispatch message was not called: 1 problem")).toEqual([]);
  });
});

describe("fitOutput", () => {
  const write = (text: string): string => `/state/out/${text.length}.md`;

  test("leaves an output under the limit as it is", () => {
    expect(fitOutput("text", ["- picture: a"], ["note"], 100, write)).toBe(
      "text\n- picture: a\nnote"
    );
  });

  test("cuts the result text first, keeping every line after it and naming the full result", () => {
    const out = fitOutput("x".repeat(200), ["- picture: a"], ["follow notice", "note"], 120, write);
    expect(out.length).toBeLessThanOrEqual(120);
    expect(out).toContain("(the full result, 213 characters: /state/out/213.md)");
    expect(out).toEndWith("\n- picture: a\nfollow notice\nnote");
  });

  test("when the picture lines and notice alone pass the limit, drops picture lines and names them", () => {
    const pictures = Array.from(
      { length: 10 },
      (_, index) => `- picture: /p/${index} (${"y".repeat(30)})`
    );
    const out = fitOutput("x".repeat(50), pictures, ["follow notice", "note"], 200, write);
    expect(out.length).toBeLessThanOrEqual(200);
    expect(out).toEndWith("\nfollow notice\nnote");
    expect(out).toMatch(/\n\(\d+ more picture lines? left out: see the full result\)\n/);
    expect(out).toContain("(the full result, ");
  });

  test("when the kept lines alone pass the limit, the output is cut to it and names the file first", () => {
    const out = fitOutput("x".repeat(50), ["- picture: a"], ["n".repeat(500)], 200, write);
    expect(out.length).toBe(200);
    expect(out).toStartWith("(the full result, 63 characters: /state/out/63.md)");
  });

  test("an unbounded limit returns every line as given", () => {
    expect(fitOutput("text", ["- picture: a"], ["note"], Number.POSITIVE_INFINITY, write)).toBe(
      "text\n- picture: a\nnote"
    );
  });
});

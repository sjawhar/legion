import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Actor } from "../../../packages/contracts/src/dispatch-api";
import {
  applyCodes,
  type CensusAsk,
  type CensusReport,
  excludeSessionAsks,
  filterAsksInWindow,
  parseCodes,
  summarizeApprovalRounds,
  summarizeAsks,
  summarizeSessions,
} from "../ask-census.ts";
import {
  approvalAsk,
  blockAsk,
  edited,
  handedBack,
  opened,
  recordedAt,
} from "./approval-events.ts";

const script = join(import.meta.dir, "..", "ask-census.ts");
const servers: Array<{ stop: (force?: boolean) => void }> = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

const from = recordedAt(0);
const to = recordedAt(60);
const EXCLUDED_SESSION = "session-old-plugin";
const excluded = new Set([EXCLUDED_SESSION]);
const CODES_CSV = "# ask-id,code\nquestion-b,design\n";

function sessionAuthor(id: string, machine: string, title: string): Actor {
  return { kind: "session", id, origin: { machine, session_title: title } };
}

function questionAsk(id: string, createdAt: string, author: Actor): CensusAsk {
  return {
    id,
    created_at: createdAt,
    kind: "question",
    block_id: null,
    question: `Which way for ${id}?`,
    author,
  };
}

const reader = sessionAuthor("session-reader", "devbox", "Implement a change");

/** TEST-1 holds the approval round: the request, the decision block of the document it names, and
 * one session's standalone question. */
const issueOneAsks: CensusAsk[] = [
  approvalAsk(2, 2),
  blockAsk("q1"),
  questionAsk("question-a", recordedAt(5), sessionAuthor("session-writer", "agent-box", "Spec")),
];
/** The request's two arrivals with no human turn: the round the census flags. */
const issueOneEvents = [opened(1, 1), edited(2, 2, 1, { version: 1 }), handedBack(3, 2)];
/** TEST-2 carries no approval ask: a coded question, an excluded session's, and one opened before
 * the window. */
const issueTwoAsks: CensusAsk[] = [
  questionAsk("question-b", recordedAt(7), reader),
  questionAsk("question-old", recordedAt(8), sessionAuthor(EXCLUDED_SESSION, "devbox", "Old")),
  questionAsk("question-before", "2026-10-01T09:30:00.000Z", reader),
];

/** A stub Dispatch serving the fixture above, as the URL a run reads it at. */
function stubDispatch(): string {
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      if (url.pathname === "/api/v1/issues") {
        return Response.json([{ key: "TEST-1" }, { key: "TEST-2" }]);
      }
      if (url.pathname === "/api/v1/issues/TEST-1/asks") return Response.json(issueOneAsks);
      if (url.pathname === "/api/v1/issues/TEST-2/asks") return Response.json(issueTwoAsks);
      if (url.pathname === "/api/v1/issues/TEST-1/events") {
        return Response.json(url.searchParams.get("after") === "0" ? issueOneEvents : []);
      }
      return new Response("not found", { status: 404 });
    },
  });
  servers.push(server);
  return `http://127.0.0.1:${server.port}`;
}

async function spawnCensus(
  args: readonly string[],
  environment: Record<string, string> = {}
): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  // DISPATCH_TOKEN_FILE outranks DISPATCH_TOKEN, so a pane's own token file would reach the stub.
  const { DISPATCH_TOKEN_FILE: _paneTokenFile, ...inherited } = process.env;
  const census = Bun.spawn(["bun", script, ...args], {
    env: { ...inherited, ...environment },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(census.stdout).text(),
    new Response(census.stderr).text(),
    census.exited,
  ]);
  return { stdout, stderr, exitCode };
}

/** One run over the fixture above, with the codes of `CODES_CSV`. */
async function census(flags: readonly string[]): Promise<{
  stdout: string;
  stderr: string;
  exitCode: number;
}> {
  const url = stubDispatch();
  const codesPath = join(await mkdtemp(join(tmpdir(), "ask-census-json-")), "codes.csv");
  await writeFile(codesPath, CODES_CSV);
  return spawnCensus(
    [
      "--from",
      from,
      "--to",
      to,
      "--project",
      "TEST",
      "--exclude-session",
      EXCLUDED_SESSION,
      "--codes",
      codesPath,
      ...flags,
    ],
    { DISPATCH_URL: url, DISPATCH_TOKEN: "token" }
  );
}

/** Every printed table row, cell by cell: the index column, then the row's values in the order
 * `console.table` takes from the object, which is the order `JSON.stringify` writes them in. */
function tableRows(text: string): string[][] {
  return text
    .split("\n")
    .filter((line) => line.startsWith("│"))
    .map((line) =>
      line
        .slice(1, -1)
        .split("│")
        .map((cell) => cell.trim())
    );
}

function printedRow(index: number, row: object): string[] {
  return [String(index), ...Object.values(row).map(String)];
}

describe("ask census --json", () => {
  test("carries the numbers the helpers count for the same asks, issues, and events", async () => {
    const windowOne = filterAsksInWindow(issueOneAsks, from, to);
    const windowTwo = filterAsksInWindow(issueTwoAsks, from, to);
    const countedOne = excludeSessionAsks(windowOne, excluded);
    const countedTwo = excludeSessionAsks(windowTwo, excluded);
    const counted = [...countedOne, ...countedTwo];
    const codes = parseCodes(CODES_CSV);
    const sessions = summarizeSessions([...windowOne, ...windowTwo], excluded);
    // The fixture counts something in every table, so no assertion below passes on an empty run.
    expect(summarizeAsks(counted)).toEqual({
      asks: 4,
      decisionBlocks: 1,
      standaloneQuestions: 2,
      approvalRequests: 1,
    });

    const json = await census(["--json"]);
    expect(json.stderr).toBe("");
    expect(json.exitCode).toBe(0);
    const report = JSON.parse(json.stdout) as CensusReport;

    expect(report.totals).toEqual(summarizeAsks(counted));
    expect(report.issues).toEqual([
      { issue: "TEST-1", ...summarizeAsks(countedOne) },
      { issue: "TEST-2", ...summarizeAsks(countedTwo) },
    ]);
    expect(report.approvalRounds).toEqual(
      summarizeApprovalRounds(issueOneEvents, issueOneAsks).map((round) => ({
        issue: "TEST-1",
        ...round,
      }))
    );
    expect(report.standaloneQuestions).toEqual([
      { issue: "TEST-1", ask: "question-a", code: "", question: "Which way for question-a?" },
      {
        issue: "TEST-2",
        ask: "question-b",
        code: "design",
        question: "Which way for question-b?",
      },
    ]);
    expect(report.codingTotals).toEqual(applyCodes(counted, codes));
    expect(report.droppedAsks).toBe(sessions.dropped);
    expect(report.sessions).toEqual(sessions.sessions);

    // The same run without --json prints those very numbers, so a count that moves in one mode
    // and not the other fails here.
    const text = await census([]);
    expect(text.exitCode).toBe(0);
    const printed = tableRows(text.stdout);
    expect(printed).toContainEqual(printedRow(0, report.totals));
    expect(printed).toContainEqual(printedRow(0, report.codingTotals));
    for (const [index, row] of report.issues.entries()) {
      expect(printed).toContainEqual(printedRow(index, row));
    }
    for (const [index, row] of report.approvalRounds.entries()) {
      expect(printed).toContainEqual(printedRow(index, row));
    }
    for (const [index, row] of report.standaloneQuestions.entries()) {
      expect(printed).toContainEqual(printedRow(index, row));
    }
    for (const [index, row] of report.sessions.entries()) {
      expect(printed).toContainEqual(printedRow(index, row));
    }
    expect(text.stdout).toContain(`Excluded asks from old-plugin sessions: ${report.droppedAsks}`);
  });

  test("prints one JSON object on stdout and nothing else", async () => {
    const json = await census(["--json"]);

    expect(json.stderr).toBe("");
    expect(json.exitCode).toBe(0);
    expect(json.stdout.split("\n").filter((line) => line !== "")).toHaveLength(1);
    const report: unknown = JSON.parse(json.stdout);
    expect(Object.keys(report as object)).toEqual([
      "totals",
      "issues",
      "approvalRounds",
      "standaloneQuestions",
      "codingTotals",
      "droppedAsks",
      "sessions",
    ]);
  });

  test("refuses a value for --json, and keeps every other flag's refusal", async () => {
    const refusals: Array<[readonly string[], string]> = [
      [[], "--from and --to are required"],
      [["--from", to, "--to", from, "--project", "TEST"], "--from must be before --to"],
      [["--from", from, "--to", to], "--project is required: name each project to read"],
      [["--from", from, "--to", to, "--project", ""], "--project requires a project key"],
      [
        ["--from", from, "--to", to, "--project", "TEST", "--exclude-session", ""],
        "--exclude-session requires a session id",
      ],
      [
        ["--from", from, "--to", to, "--project", "TEST", "--codes", ""],
        "--codes requires a file path",
      ],
      [
        ["--from", from, "--to", to, "--project", "TEST", "--json=true"],
        "Option '--json' does not take an argument",
      ],
      [
        ["--from", from, "--to", to, "--project", "TEST", "--json", "true"],
        "Unexpected argument 'true'",
      ],
    ];

    for (const [args, refusal] of refusals) {
      const refused = await spawnCensus(args);
      expect(refused.stderr).toContain(refusal);
      expect(refused.exitCode).toBe(1);
      expect(refused.stdout).toBe("");
    }
  });
});

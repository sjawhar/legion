import { describe, expect, spyOn, test } from "bun:test";
import { join } from "node:path";
import {
  DISPATCH_TOOL_DEADLINE_MS,
  DispatchClient,
} from "../../../packages/envoy-client/src/dispatch-http.ts";
import { runJq } from "../../e2e/lib/run-jq.ts";
import {
  applyCodes,
  censusIssues,
  excludeSessionAsks,
  fetchIssueEvents,
  filterAsksInWindow,
  ISSUE_CONCURRENCY,
  parseCodes,
  summarizeApprovalRounds,
  summarizeAsks,
  summarizeSessions,
} from "../ask-census.ts";
import {
  type ApprovalHistoryEvent,
  ARTIFACT,
  approvalAsk,
  approved,
  blockAnswered,
  blockAsk,
  edited,
  followed,
  handedBack,
  opened,
  recordedAt,
  recordedRounds,
  reply,
} from "./approval-events.ts";

// One request since #1671, when an approval request began following its document's versions:
// opened at version 1, moved to version 2 by a new version, reworded and handed back at 2,
// answered in its thread by the human, handed back at 2 again, and approved.
const since1671: ApprovalHistoryEvent[] = [
  opened(1, 1),
  edited(2, 2, 1, { version: 1 }),
  edited(3, 2, 1, { version: 2 }, "Proposes an hourly export."),
  handedBack(4, 2, "Proposes an hourly export."),
  reply(5, "human"),
  handedBack(6, 2, "Proposes an hourly export."),
  ...approved(7, 2),
];

/** The approval requests `drive_gated_spec` in the stage 4b proof reads from the same events. */
function stage4bRequests(events: readonly unknown[], artifactId: string): number {
  const library = join(import.meta.dir, "..", "..", "e2e", "lib");
  const program = 'include "design-gate-approval-requests"; approval_requests($artifact) | length';
  return Number(
    runJq(["-L", library, "--arg", "artifact", artifactId, program], JSON.stringify(events))
  );
}

const from = "2026-10-01T00:00:00.000Z";
const to = "2026-10-02T00:00:00.000Z";
const human = { kind: "user", id: "alice" } as const;

const asks = [
  {
    id: "before-window",
    created_at: "2026-09-30T23:59:59.999Z",
    kind: "question",
    block_id: null,
    author: human,
  },
  {
    id: "decision-block",
    created_at: "2026-10-01T01:00:00.000Z",
    kind: "question",
    block_id: "block-1",
    author: {
      kind: "session",
      id: "session-a",
      origin: { machine: "agent-box", session_title: "Write a spec" },
    },
  },
  {
    id: "standalone-question",
    created_at: "2026-10-01T02:00:00.000Z",
    kind: "question",
    block_id: null,
    author: {
      kind: "session",
      id: "session-b",
      origin: { machine: "devbox", session_title: "Implement a change" },
    },
  },
  {
    id: "approval-request",
    created_at: "2026-10-01T03:00:00.000Z",
    kind: "approval",
    block_id: null,
    author: human,
  },
  {
    id: "at-window-end",
    created_at: "2026-10-02T00:00:00.000Z",
    kind: "question",
    block_id: null,
    author: human,
  },
] as const;

describe("ask census", () => {
  test("keeps the half-open window and separates blocks, standalone questions, and approvals", () => {
    const inWindow = filterAsksInWindow(asks, from, to);

    expect(inWindow.map((ask) => ask.id)).toEqual([
      "decision-block",
      "standalone-question",
      "approval-request",
    ]);
    expect(summarizeAsks(inWindow)).toEqual({
      asks: 3,
      decisionBlocks: 1,
      standaloneQuestions: 1,
      approvalRequests: 1,
    });
  });

  test("reproduces the live census's approval rounds from the recorded events", () => {
    const recordedAsks = recordedRounds.issues.flatMap((issue) => issue.asks);
    expect(summarizeAsks(recordedAsks)).toEqual({
      asks: 12,
      decisionBlocks: 3,
      standaloneQuestions: 0,
      approvalRequests: 9,
    });
    // Each request made again opened a new row before #1671, so every arrival is an ask.opened. A
    // human's thread reply is a turn; a session's is not. One turn each of LEGION-464's and
    // AGENTC-418's is a human's reply on a decision block in the spec; LEGION-462's block was
    // answered before its first request, so it is no turn.
    expect(
      recordedRounds.issues.flatMap((issue) =>
        summarizeApprovalRounds(issue.events, issue.asks).map((round) => ({
          issue: issue.key,
          ...round,
        }))
      )
    ).toEqual([
      {
        issue: "LEGION-464",
        artifactId: "83feb774-59a9-4a3d-bf6b-ce583ac5aebf",
        inboxRows: 4,
        arrivals: 4,
        humanTurns: 2,
        exceedsHumanTurnBudget: true,
      },
      {
        issue: "AGENTC-418",
        artifactId: "4bd9cfed-2e6e-4d79-83a4-ef99173d088d",
        inboxRows: 3,
        arrivals: 3,
        humanTurns: 4,
        exceedsHumanTurnBudget: false,
      },
      {
        issue: "LEGION-462",
        artifactId: "9ca2a4e4-515e-4dd9-a1a3-2f68b296d9bf",
        inboxRows: 2,
        arrivals: 2,
        humanTurns: 2,
        exceedsHumanTurnBudget: false,
      },
    ]);
  });

  test("counts the opening request and each hand-back since #1671, but no rewording", () => {
    // The move and the new summary are ask.edited and never reach the human; the reworded
    // hand-back arrives once, as its ask.handed_back.
    expect(summarizeApprovalRounds(since1671, [])).toEqual([
      {
        artifactId: ARTIFACT,
        inboxRows: 1,
        arrivals: 3,
        humanTurns: 2,
        exceedsHumanTurnBudget: false,
      },
    ]);
  });

  test("counts the arrivals the stage 4b proof reads as approval requests, before and since #1671", () => {
    const histories = [...recordedRounds.issues, { asks: [], events: since1671 }];
    const counted = histories.flatMap(({ asks, events }) =>
      summarizeApprovalRounds(events, asks).map((round) => ({
        census: round.arrivals,
        stage4b: stage4bRequests(events, round.artifactId),
      }))
    );
    expect(counted).toEqual([
      { census: 4, stage4b: 4 },
      { census: 3, stage4b: 3 },
      { census: 2, stage4b: 2 },
      { census: 3, stage4b: 3 },
    ]);
  });

  test("reads every event page before counting approval rounds", async () => {
    const events: ApprovalHistoryEvent[] = Array.from({ length: 201 }, (_, index) =>
      followed(index + 1)
    );
    events[0] = opened(1, 1);
    events[200] = reply(201, "human");
    const requests: string[] = [];
    const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(String(input));
      requests.push(`${url.pathname}${url.search}`);
      expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer token");
      const after = Number(url.searchParams.get("after"));
      return Response.json(events.filter((event) => event.seq > after).slice(0, 200));
    };

    const fetched = await fetchIssueEvents(
      new DispatchClient("https://dispatch.example", "token", fetchImpl as typeof fetch),
      "LEGION-470"
    );

    expect(requests).toEqual([
      "/api/v1/issues/LEGION-470/events?after=0&limit=200",
      "/api/v1/issues/LEGION-470/events?after=200&limit=200",
    ]);
    expect(summarizeApprovalRounds(fetched, [])).toEqual([
      {
        artifactId: ARTIFACT,
        inboxRows: 1,
        arrivals: 1,
        humanTurns: 1,
        exceedsHumanTurnBudget: false,
      },
    ]);
  });

  test("totals supplied judgment codes and excludes known old-plugin sessions", () => {
    const inWindow = filterAsksInWindow(asks, from, to);

    expect(applyCodes(inWindow, { "standalone-question": "to-do" })).toEqual({
      design: 0,
      "to-do": 1,
      "may-I-proceed": 0,
      operations: 0,
      uncoded: 0,
    });
    expect(parseCodes("# ask-id,code\nstandalone-question,to-do\n")).toEqual({
      "standalone-question": "to-do",
    });
    expect(excludeSessionAsks(inWindow, new Set(["session-a"])).map((ask) => ask.id)).toEqual([
      "standalone-question",
      "approval-request",
    ]);
    expect(summarizeSessions(inWindow, new Set(["session-a"]))).toEqual({
      dropped: 1,
      sessions: [
        {
          sessionId: "session-b",
          machine: "devbox",
          title: "Implement a change",
          firstAsk: "2026-10-01T02:00:00.000Z",
          lastAsk: "2026-10-01T02:00:00.000Z",
          asks: 1,
        },
      ],
    });
  });

  test("flags two Inbox arrivals with no human turn, before and since #1671", () => {
    // Before #1671 a request made again opened a new row: LEGION-464's request at version 2 was
    // retracted when the document moved on to version 3, and the agent opened another at version
    // 3, with no human turn between them.
    // Since #1671 the same two arrivals are the opening request and one hand-back of the same row.
    const recorded = recordedRounds.issues.find((issue) => issue.key === "LEGION-464");
    expect(recorded?.events.slice(0, 3).map((event) => event.type)).toEqual([
      "ask.opened",
      "ask.resolved",
      "ask.opened",
    ]);
    const before1671 = summarizeApprovalRounds(
      recorded?.events.slice(0, 3) ?? [],
      recorded?.asks ?? []
    );
    const following = summarizeApprovalRounds(
      [opened(1, 1), edited(2, 2, 1, { version: 1 }), handedBack(3, 2)],
      []
    );

    expect(before1671[0]?.arrivals).toBe(2);
    expect(before1671[0]?.exceedsHumanTurnBudget).toBe(true);
    expect(following[0]?.arrivals).toBe(2);
    expect(following[0]?.exceedsHumanTurnBudget).toBe(true);
  });

  test("a hand-back after the human answers a decision block in the requested document is within budget", () => {
    const events = [
      opened(1, 1),
      edited(2, 2, 1, { version: 1 }),
      blockAnswered(3, "q1"),
      edited(4, 4, 1, { version: 2 }),
      handedBack(5, 4),
      edited(6, 5, 4, { version: 4 }),
      blockAnswered(7, "q2"),
      edited(8, 7, 4, { version: 5 }),
      handedBack(9, 7),
    ];
    expect(summarizeApprovalRounds(events, [blockAsk("q1"), blockAsk("q2")])).toEqual([
      {
        artifactId: ARTIFACT,
        inboxRows: 1,
        arrivals: 3,
        humanTurns: 2,
        exceedsHumanTurnBudget: false,
      },
    ]);
  });

  test("a reply on a decision block is a turn once the round begins, and only in the requested document", () => {
    const notes = blockAsk("notes", "artifact-2");
    const events = [
      blockAnswered(1, "q0"),
      opened(2, 1),
      reply(3, "human", notes),
      edited(4, 2, 1, { version: 1 }),
      reply(5, "human", blockAsk("q1")),
      handedBack(6, 2),
      edited(7, 3, 2, { version: 2 }),
      handedBack(8, 3),
    ];

    // The reply on q1 is the one turn: q0 was answered before the first request, and the notes
    // block lives in another document.
    expect(summarizeApprovalRounds(events, [blockAsk("q0"), blockAsk("q1"), notes])).toEqual([
      {
        artifactId: ARTIFACT,
        inboxRows: 1,
        arrivals: 3,
        humanTurns: 1,
        exceedsHumanTurnBudget: true,
      },
    ]);
  });

  test("reads every issue when the whole run outlasts one client's deadline", async () => {
    // Each client's deadline runs 200 times faster: 60 s becomes 300 ms. No read is slow, an issue's
    // two take a fifth of a deadline, and six waves of ISSUE_CONCURRENCY issues take longer than one.
    // The clock is real: the deadline is AbortSignal.timeout, which fake timers do not move, and a
    // clock the stub moves as it serves runs ahead of the clients still reading its answers.
    const deadline = DISPATCH_TOOL_DEADLINE_MS / 200;
    const latency = deadline / 10;
    const timeoutAfter = AbortSignal.timeout.bind(AbortSignal);
    const timeout = spyOn(AbortSignal, "timeout").mockImplementation((milliseconds: number) =>
      timeoutAfter(milliseconds / 200)
    );
    const read: string[] = [];
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      async fetch(request) {
        await Bun.sleep(latency);
        const { pathname } = new URL(request.url);
        read.push(pathname);
        if (pathname.endsWith("/asks")) return Response.json([approvalAsk(1, 1)]);
        if (pathname.endsWith("/events")) return Response.json([]);
        return new Response("not found", { status: 404 });
      },
    });
    const issues = Array.from({ length: 6 * ISSUE_CONCURRENCY }, (_, index) => `TEST-${index + 1}`);

    try {
      const started = performance.now();
      const census = await censusIssues(
        { url: `http://127.0.0.1:${server.port}`, token: "token" },
        issues,
        {
          from: recordedAt(0),
          to: recordedAt(60),
          projects: ["TEST"],
          excludedSessionIds: new Set(),
        }
      );

      expect(performance.now() - started).toBeGreaterThan(deadline);
      expect(census.map((issue) => issue.issueRow?.issue)).toEqual(issues);
      expect(read).toHaveLength(2 * issues.length);
    } finally {
      server.stop(true);
      timeout.mockRestore();
    }
  });
});

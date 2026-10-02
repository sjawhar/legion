import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import {
  applyCodes,
  excludeSessionAsks,
  fetchIssueEvents,
  filterAsksInWindow,
  parseCodes,
  summarizeApprovalRounds,
  summarizeAsks,
  summarizeSessions,
} from "../ask-census.ts";
import {
  type ApprovalHistoryEvent,
  ARTIFACT,
  approved,
  edited,
  followed,
  handedBack,
  opened,
  recordedRounds,
  reply,
} from "./approval-events.ts";

// One request after F1: opened at version 1, moved to version 2 by a new version, reworded and
// handed back at 2, answered in its thread by the human, handed back at 2 again, and approved.
const afterF1: ApprovalHistoryEvent[] = [
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
  const run = Bun.spawnSync(
    [
      "jq",
      "-L",
      join(import.meta.dir, "..", "..", "e2e", "lib"),
      "--arg",
      "artifact",
      artifactId,
      'include "design-gate-approval-requests"; approval_requests($artifact) | length',
    ],
    { stdin: new TextEncoder().encode(JSON.stringify(events)) }
  );
  if (run.exitCode !== 0) throw new Error(`jq exited ${run.exitCode}: ${run.stderr.toString()}`);
  return Number(run.stdout.toString());
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
    const approvalAsks = recordedRounds.issues.flatMap((issue) => issue.asks);
    expect(summarizeAsks(approvalAsks)).toEqual({
      asks: 9,
      decisionBlocks: 0,
      standaloneQuestions: 0,
      approvalRequests: 9,
    });
    // Each request made again opened a new row before F1, so every arrival is an ask.opened. A
    // human's thread reply is a turn; a session's is not.
    expect(
      recordedRounds.issues.flatMap((issue) =>
        summarizeApprovalRounds(issue.events).map((round) => ({ issue: issue.key, ...round }))
      )
    ).toEqual([
      {
        issue: "LEGION-464",
        artifactId: "83feb774-59a9-4a3d-bf6b-ce583ac5aebf",
        inboxRows: 4,
        arrivals: 4,
        humanTurns: 1,
        exceedsHumanTurnBudget: true,
      },
      {
        issue: "AGENTC-418",
        artifactId: "4bd9cfed-2e6e-4d79-83a4-ef99173d088d",
        inboxRows: 3,
        arrivals: 3,
        humanTurns: 3,
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

  test("counts the opening request and each hand-back after F1, but no rewording", () => {
    // The move and the new summary are ask.edited and never reach the human; the reworded
    // hand-back arrives once, as its ask.handed_back.
    expect(summarizeApprovalRounds(afterF1)).toEqual([
      {
        artifactId: ARTIFACT,
        inboxRows: 1,
        arrivals: 3,
        humanTurns: 2,
        exceedsHumanTurnBudget: false,
      },
    ]);
  });

  test("counts the arrivals the stage 4b proof reads as approval requests, before and after F1", () => {
    const histories = [...recordedRounds.issues.map((issue) => issue.events), afterF1];
    const counted = histories.flatMap((events) =>
      summarizeApprovalRounds(events).map((round) => ({
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
      { url: "https://dispatch.example", token: "token" },
      "LEGION-470",
      fetchImpl as typeof fetch
    );

    expect(requests).toEqual([
      "/api/v1/issues/LEGION-470/events?limit=200&after=0",
      "/api/v1/issues/LEGION-470/events?limit=200&after=200",
    ]);
    expect(summarizeApprovalRounds(fetched)).toEqual([
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
});

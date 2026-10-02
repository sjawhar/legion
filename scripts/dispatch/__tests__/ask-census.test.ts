import { describe, expect, test } from "bun:test";
import fixtureJson from "../__fixtures__/ask-census.json";
import {
  applyCodes,
  type CensusAsk,
  type CensusEvent,
  excludeSessionAsks,
  fetchIssueEvents,
  filterAsksInWindow,
  parseCodes,
  summarizeApprovalRounds,
  summarizeAsks,
  summarizeSessions,
} from "../ask-census.ts";

// The snapshots are the server's JSON, whose literal fields a JSON import widens to `string`.
const fixture = fixtureJson as unknown as {
  readonly issues: ReadonlyArray<{
    readonly key: string;
    readonly asks: ReadonlyArray<Pick<CensusAsk, "id" | "created_at" | "kind" | "block_id">>;
    readonly events: readonly CensusEvent[];
  }>;
};

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

  test("reproduces the closed legacy approval snapshots", () => {
    const approvalAsks = fixture.issues.flatMap((issue) => issue.asks);
    expect(summarizeAsks(approvalAsks)).toEqual({
      asks: 9,
      decisionBlocks: 0,
      standaloneQuestions: 0,
      approvalRequests: 9,
    });
    expect(
      fixture.issues.flatMap((issue) =>
        summarizeApprovalRounds(issue.events).map((round) => ({
          issue: issue.key,
          artifactId: round.artifactId,
          handbacks: round.handbacks,
          humanTurns: round.humanTurns,
          exceedsHumanTurnBudget: round.exceedsHumanTurnBudget,
        }))
      )
    ).toEqual([
      {
        issue: "LEGION-464",
        artifactId: "artifact-464",
        handbacks: 4,
        humanTurns: 1,
        exceedsHumanTurnBudget: true,
      },
      {
        issue: "AGENTC-418",
        artifactId: "artifact-418",
        handbacks: 3,
        humanTurns: 0,
        exceedsHumanTurnBudget: true,
      },
      {
        issue: "LEGION-462",
        artifactId: "artifact-462",
        handbacks: 2,
        humanTurns: 0,
        exceedsHumanTurnBudget: true,
      },
    ]);
  });

  test("counts the opening request and each hand-back, separately from human turns", () => {
    expect(
      summarizeApprovalRounds([
        {
          id: 1,
          type: "ask.opened",
          actor: { kind: "session", id: "session-a" },
          payload: {
            id: "approval-1",
            kind: "approval",
            approval: { artifact_id: "artifact-1", version: 1, requested_version: 1 },
          },
        },
        {
          id: 2,
          type: "ask.edited",
          actor: { kind: "session", id: "session-a" },
          payload: {
            id: "approval-1",
            kind: "approval",
            approval: { artifact_id: "artifact-1", version: 2, requested_version: 1 },
          },
        },
        {
          id: 3,
          type: "ask.edited",
          actor: { kind: "session", id: "session-a" },
          payload: {
            id: "approval-1",
            kind: "approval",
            approval: { artifact_id: "artifact-1", version: 2, requested_version: 2 },
          },
        },
        {
          id: 4,
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          payload: { ask_id: "approval-1" },
        },
        {
          id: 5,
          type: "ask.answered",
          actor: { kind: "user", id: "alice" },
          payload: {
            id: "approval-1",
            kind: "approval",
            approval: { artifact_id: "artifact-1", version: 2, requested_version: 2 },
          },
        },
        {
          id: 6,
          type: "artifact.approved",
          actor: { kind: "user", id: "alice" },
          payload: { ask_id: "approval-1" },
        },
      ])
    ).toEqual([
      {
        artifactId: "artifact-1",
        inboxRows: 1,
        handbacks: 2,
        humanTurns: 2,
        exceedsHumanTurnBudget: false,
      },
    ]);
  });

  test("reads every event page before counting approval rounds", async () => {
    const events: Array<CensusEvent & { readonly seq: number }> = Array.from(
      { length: 201 },
      (_, index) => ({
        id: index + 1,
        seq: index + 1,
        type: "issue.updated",
        payload: {},
      })
    );
    events[0] = {
      id: 1,
      seq: 1,
      type: "ask.opened",
      actor: { kind: "session", id: "session-a" },
      payload: {
        id: "approval-1",
        kind: "approval",
        approval: { artifact_id: "artifact-1", version: 1, requested_version: 1 },
      },
    };
    events[200] = {
      id: 201,
      seq: 201,
      type: "comment.created",
      actor: { kind: "user", id: "alice" },
      payload: { ask_id: "approval-1" },
    };
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
        artifactId: "artifact-1",
        inboxRows: 1,
        handbacks: 1,
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

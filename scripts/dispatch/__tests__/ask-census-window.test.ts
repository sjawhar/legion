import { afterEach, describe, expect, test } from "bun:test";
import { join } from "node:path";
import { summarizeApprovalRounds } from "../ask-census.ts";

const script = join(import.meta.dir, "..", "ask-census.ts");
const servers: Array<{ stop: (force?: boolean) => void }> = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

const approval = (version: number, requestedVersion: number) => ({
  id: "approval-1",
  kind: "approval",
  approval: { artifact_id: "artifact-1", version, requested_version: requestedVersion },
});

describe("ask census after F1", () => {
  test("counts in-window hand-backs of an approval request opened before the window", async () => {
    // After F1 one approval row follows the document for its whole life, so a request opened
    // before the window and handed back twice inside it, with no human turn, is the loop the
    // census exists to flag, even though no ask was opened in the window.
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch(request) {
        const url = new URL(request.url);
        if (url.pathname === "/api/v1/issues") return Response.json([{ key: "TEST-1" }]);
        if (url.pathname === "/api/v1/issues/TEST-1/asks") {
          return Response.json([
            { ...approval(3, 3), created_at: "2026-10-01T09:00:00Z", block_id: null },
          ]);
        }
        if (url.pathname === "/api/v1/issues/TEST-1/events") {
          if (url.searchParams.get("after") !== "0") return Response.json([]);
          const actor = { kind: "session", id: "session-a" };
          return Response.json([
            {
              id: 1,
              seq: 1,
              type: "ask.opened",
              actor,
              created_at: "2026-10-01T09:00:00Z",
              payload: approval(1, 1),
            },
            {
              id: 2,
              seq: 2,
              type: "ask.edited",
              actor,
              created_at: "2026-10-01T11:00:00Z",
              payload: approval(2, 1),
            },
            {
              id: 3,
              seq: 3,
              type: "ask.edited",
              actor,
              created_at: "2026-10-01T11:01:00Z",
              payload: approval(2, 2),
            },
            {
              id: 4,
              seq: 4,
              type: "ask.edited",
              actor,
              created_at: "2026-10-01T11:02:00Z",
              payload: approval(3, 2),
            },
            {
              id: 5,
              seq: 5,
              type: "ask.edited",
              actor,
              created_at: "2026-10-01T11:03:00Z",
              payload: approval(3, 3),
            },
          ]);
        }
        return new Response("not found", { status: 404 });
      },
    });
    servers.push(server);

    const census = Bun.spawn(
      [
        "bun",
        script,
        "--from",
        "2026-10-01T10:00:00Z",
        "--to",
        "2026-10-01T12:00:00Z",
        "--project",
        "TEST",
      ],
      {
        env: {
          ...process.env,
          DISPATCH_URL: `http://127.0.0.1:${server.port}`,
          DISPATCH_TOKEN: "token",
        },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(census.stdout).text(),
      new Response(census.stderr).text(),
      census.exited,
    ]);

    expect(stderr).toBe("");
    expect(exitCode).toBe(0);
    const rounds = stdout.slice(
      stdout.indexOf("Approval rounds"),
      stdout.indexOf("Standalone question asks")
    );
    expect(rounds).toContain("TEST-1");
    expect(stdout).toContain("Approval rounds above the human-turn budget");
  });

  test("flags two Inbox arrivals with no human turn, as it does before F1", () => {
    // Before F1 every approval ask.opened is a hand-back, so two requests with no human turn
    // between them exceed the budget. After F1 the same two arrivals are the opening request and
    // one catch-up edit, and the opening request is an Inbox arrival too.
    const actor = { kind: "session", id: "session-a" };
    const beforeF1 = summarizeApprovalRounds([
      {
        id: 1,
        type: "ask.opened",
        actor,
        payload: { id: "a", kind: "approval", approval: { artifact_id: "doc", version: 1 } },
      },
      {
        id: 2,
        type: "ask.opened",
        actor,
        payload: { id: "b", kind: "approval", approval: { artifact_id: "doc", version: 2 } },
      },
    ]);
    const afterF1 = summarizeApprovalRounds([
      {
        id: 1,
        type: "ask.opened",
        actor,
        payload: {
          id: "a",
          kind: "approval",
          approval: { artifact_id: "doc", version: 1, requested_version: 1 },
        },
      },
      {
        id: 2,
        type: "ask.edited",
        actor,
        payload: {
          id: "a",
          kind: "approval",
          approval: { artifact_id: "doc", version: 2, requested_version: 1 },
        },
      },
      {
        id: 3,
        type: "ask.edited",
        actor,
        payload: {
          id: "a",
          kind: "approval",
          approval: { artifact_id: "doc", version: 2, requested_version: 2 },
        },
      },
    ]);

    expect(beforeF1[0]?.exceedsHumanTurnBudget).toBe(true);
    expect(afterF1[0]?.handbacks).toBe(beforeF1[0]?.handbacks);
    expect(afterF1[0]?.exceedsHumanTurnBudget).toBe(true);
  });
});

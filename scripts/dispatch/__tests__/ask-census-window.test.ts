import { afterEach, describe, expect, test } from "bun:test";
import { join } from "node:path";
import { summarizeApprovalRounds } from "../ask-census.ts";
import {
  approvalAsk,
  edited,
  handedBack,
  opened,
  recordedAt,
  recordedRounds,
} from "./approval-events.ts";

const script = join(import.meta.dir, "..", "ask-census.ts");
const servers: Array<{ stop: (force?: boolean) => void }> = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

describe("ask census after F1", () => {
  test("counts in-window hand-backs of an approval request opened before the window", async () => {
    // After F1 one approval row follows the document for its whole life, so a request opened
    // before the window and handed back twice inside it, with no human turn, is the loop the
    // census exists to flag, even though no ask was opened in the window.
    const events = [
      opened(1, 1),
      edited(2, 2, 1, { version: 1 }),
      handedBack(3, 2),
      edited(4, 3, 2, { version: 2 }),
      edited(5, 3, 2, { version: 3 }, "Proposes an hourly export."),
      handedBack(6, 3, "Proposes an hourly export."),
    ];
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch(request) {
        const url = new URL(request.url);
        if (url.pathname === "/api/v1/issues") return Response.json([{ key: "TEST-1" }]);
        if (url.pathname === "/api/v1/issues/TEST-1/asks") {
          return Response.json([approvalAsk(3, 3, "Proposes an hourly export.")]);
        }
        if (url.pathname === "/api/v1/issues/TEST-1/events") {
          return Response.json(url.searchParams.get("after") === "0" ? events : []);
        }
        return new Response("not found", { status: 404 });
      },
    });
    servers.push(server);

    const census = Bun.spawn(
      ["bun", script, "--from", recordedAt(2), "--to", recordedAt(7), "--project", "TEST"],
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
    const flagged = stdout.slice(stdout.indexOf("Approval rounds above the human-turn budget"));
    // No ask opened in the window, two arrivals (the hand-backs), no human turn.
    expect(flagged).toMatch(/│ TEST-1\s*│ artifact-1\s*│ 0\s*│ 2\s*│ 0\s*│ true\s*│/);
  });

  test("flags two Inbox arrivals with no human turn, as it does before F1", () => {
    // Before F1 a request made again opened a new row: LEGION-464's request at version 2 was
    // retracted when the document moved on to version 3, and the agent opened another at version
    // 3, with no human turn between them.
    // After F1 the same two arrivals are the opening request and one hand-back of the same row.
    const recorded = recordedRounds.issues.find((issue) => issue.key === "LEGION-464")?.events;
    expect(recorded?.slice(0, 3).map((event) => event.type)).toEqual([
      "ask.opened",
      "ask.resolved",
      "ask.opened",
    ]);
    const beforeF1 = summarizeApprovalRounds(recorded?.slice(0, 3) ?? []);
    const afterF1 = summarizeApprovalRounds([
      opened(1, 1),
      edited(2, 2, 1, { version: 1 }),
      handedBack(3, 2),
    ]);

    expect(beforeF1[0]?.arrivals).toBe(2);
    expect(beforeF1[0]?.exceedsHumanTurnBudget).toBe(true);
    expect(afterF1[0]?.arrivals).toBe(2);
    expect(afterF1[0]?.exceedsHumanTurnBudget).toBe(true);
  });
});

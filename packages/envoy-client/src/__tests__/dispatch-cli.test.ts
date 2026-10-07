import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runDispatchCli } from "../dispatch-cli";
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

const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname } = new URL(request.url);
    const text = await request.text();
    const body = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, body });
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
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "GET") {
      return Response.json(issue);
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
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  triage = false;
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
): Promise<{ code: number; stdout: string }> {
  let stdout = "";
  const code = await runDispatchCli(argv, environment(extra), {
    stdout: (text) => {
      stdout += text;
    },
    readText: (path) => (path === "-" ? stdin : readFileSync(path, "utf-8")),
    cwd: stateDir,
  });
  return { code, stdout };
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
});

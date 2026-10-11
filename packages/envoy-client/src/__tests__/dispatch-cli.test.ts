import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fitOutput, runDispatchCli } from "../dispatch-cli";
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

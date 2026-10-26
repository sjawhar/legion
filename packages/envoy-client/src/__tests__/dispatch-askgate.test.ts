import { afterAll, afterEach, beforeEach, describe, expect, test } from "bun:test";
import { createHash, randomBytes } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { mintMarker } from "../askgate-marker";
import { cliRefusals, runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { pruneSessions } from "../dispatch-session-state";

// AskGate's enforcement at the posting CLI. A live Oh My Pi process whose gate is bound leaves an
// owner file in the session's state directory (`askgate/<pid>-<start>.json`) that names it and
// its marker store; the gate mints one single-use marker for each literal call it lets run. The
// CLI consumes a marker before it posts a gated write, and refuses one with no marker, whatever
// form ran it. These tests write the record and the markers by hand, in the format the gate
// writes, so they hold the CLI to that format and to nothing of the gate's.

interface Recorded {
  readonly method: string;
  readonly path: string;
  /** The session's ledger as it stood when the request arrived. */
  readonly ledger: string;
  /** What a JSON request posted, or an upload's file as text. */
  readonly sent?: unknown;
}

const posts: Recorded[] = [];
const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname } = new URL(request.url);
    const ledgerFile = join(stateDir, "sessions", "ses-1", "results.jsonl");
    const ledgerText = existsSync(ledgerFile) ? readFileSync(ledgerFile, "utf-8") : "";
    const record = (sent?: unknown) =>
      posts.push({ method: request.method, path: pathname, ledger: ledgerText, sent });
    if (request.method === "POST" && pathname === ARTIFACTS) {
      const file = (await request.formData()).get("file");
      record(file instanceof Blob ? await file.text() : undefined);
      return Response.json({
        artifact: { id: "artifact-1", issue_key: "DSP-1", name: "brief.md", slug: "brief-md" },
        version: { number: 1 },
      });
    }
    if (request.method === "POST" && (pathname === MESSAGES || pathname === COMMENTS)) {
      record(await request.json());
      return Response.json({
        id: "message-1",
        issue_key: "DSP-1",
        advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
      });
    }
    record();
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});
afterAll(() => server.stop(true));

const MESSAGES = "/api/v1/issues/DSP-1/messages";
const COMMENTS = "/api/v1/issues/DSP-1/comments";
const ARTIFACTS = "/api/v1/issues/DSP-1/artifacts";
const postsTo = (route: string) => posts.filter((p) => p.method === "POST" && p.path === route);
const messagePosts = () => postsTo(MESSAGES);

let stateDir = "";
beforeEach(() => {
  posts.length = 0;
  stateDir = mkdtempSync(join(tmpdir(), "dispatch-askgate-"));
  resetAdviceMemory();
});
afterEach(() => rmSync(stateDir, { recursive: true, force: true }));

const sha = (value: unknown) => createHash("sha256").update(JSON.stringify(value)).digest("hex");

/** Field 22 of /proc/<pid>/stat: the process's start time, in clock ticks since boot. */
function startOf(pid: number): string {
  const stat = readFileSync(`/proc/${pid}/stat`, "utf-8");
  return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19] ?? "";
}

const store = () => join(stateDir, "askgate", String(process.pid));
const markers = () => join(store(), "markers");

/** The owner file of `owner` in session `id`'s record, as the gate names it. */
function ownerFile(id: string, owner: { pid: number; start: string | null }): string {
  const name = owner.start === null ? String(owner.pid) : `${owner.pid}-${owner.start}`;
  return join(stateDir, "sessions", id, "askgate", `${name}.json`);
}

/** The record a live gate writes for session `id`: this test process stands in for Oh My Pi. */
function writeRecord(
  id = "ses-1",
  owner: { pid: number; start: string | null; store: string } = {
    pid: process.pid,
    start: startOf(process.pid),
    store: store(),
  },
  mode?: "warn" | "block"
): void {
  const file = ownerFile(id, owner);
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, JSON.stringify({ v: 1, ...owner }));
  mkdirSync(join(owner.store, "markers"), { recursive: true });
  if (mode !== undefined) writeFileSync(join(owner.store, "mode"), mode);
}

/** The sha256 of a file's bytes, as a marker binds each file it names. */
const fileSha = (file: string) => createHash("sha256").update(readFileSync(file)).digest("hex");

/**
 * One marker as the gate mints it: keyed by the argv's digest, bound to the arguments' digest and
 * to each file the argv names, with the digest of its bytes as the gate read them.
 */
function mint(
  argv: readonly string[],
  args: Record<string, unknown> | null,
  createdAt = Date.now(),
  files: ReadonlyArray<{ flag: string; operand: string; sha256: string }> = []
): string {
  const file = join(markers(), `${sha(argv)}.${randomBytes(16).toString("hex")}`);
  writeFileSync(
    file,
    JSON.stringify({
      v: 2,
      argv: sha(argv),
      args: args === null ? null : sha(args),
      files,
      toolCallId: "call-1",
      outcome: "verdict",
      createdAt,
    }),
    { mode: 0o600 }
  );
  return file;
}

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
  extra: Record<string, string | undefined> = omp,
  stdin = "",
  fetchImpl?: typeof fetch
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
    readText: (path) => (path === "-" ? stdin : readFileSync(resolve(stateDir, path), "utf-8")),
    cwd: stateDir,
    ...(fetchImpl === undefined ? {} : { fetchImpl }),
  });
  return { code, stdout, stderr };
}

/** The arguments the gate binds a reviewed marker to: what the call's own dry run prints. */
async function dryArgs(argv: readonly string[], stdin = ""): Promise<Record<string, unknown>> {
  const [command, ...rest] = argv;
  const { code, stdout } = await run([command ?? "", "--dry-run", ...rest], omp, stdin);
  expect(code).toBe(0);
  return JSON.parse(stdout);
}

function ledger(id = "ses-1"): Array<Record<string, unknown>> {
  return readFileSync(join(stateDir, "sessions", id, "results.jsonl"), "utf-8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line));
}

const omp = { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-1" };
const MESSAGE = ["message", "--issue", "DSP-1", "--body", "Deliverable landed."];
const REFUSAL = "dispatch message was not called: AskGate is on for this session";

describe("AskGate at the dispatch CLI", () => {
  test("a gated write with a live record and no marker is refused before any request, and logged", async () => {
    writeRecord();
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(1);
    expect(stdout).toStartWith(REFUSAL);
    expect(stdout).toContain("carries no AskGate marker");
    expect(cliRefusals(stdout)).toEqual([
      { line: stdout.split("\n")[0] ?? "", command: "message" },
    ]);
    expect(posts).toHaveLength(0);
    expect(ledger()).toMatchObject([
      {
        tool: "dispatch_message",
        error: expect.stringContaining(REFUSAL),
        askgate: { mode: "block" },
      },
    ]);
  });

  test("a matching marker lets the write post once, and is consumed", async () => {
    writeRecord();
    const marker = mint(MESSAGE, await dryArgs(MESSAGE));
    const { code } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
    expect(existsSync(marker)).toBe(false);
  });

  test("a marker is single use: the same call again is refused", async () => {
    writeRecord();
    mint(MESSAGE, await dryArgs(MESSAGE));
    expect((await run(MESSAGE)).code).toBe(0);
    const again = await run(MESSAGE);
    expect(again.code).toBe(1);
    expect(again.stdout).toStartWith(REFUSAL);
    expect(messagePosts()).toHaveLength(1);
  });

  test("a marker bound to other arguments is refused and left for the call it was minted for", async () => {
    writeRecord();
    const argv = ["message", "--issue", "DSP-1", "--body-file", "-"];
    const marker = mint(argv, await dryArgs(argv, "the reviewed text"));
    const { code, stdout } = await run(argv, omp, "other text");
    expect(code).toBe(1);
    expect(stdout).toStartWith(REFUSAL);
    expect(stdout).toContain("arguments differ from the ones AskGate reviewed");
    expect(messagePosts()).toHaveLength(0);
    expect(existsSync(marker)).toBe(true);
  });

  test("a marker for another argument vector is no marker for this one", async () => {
    writeRecord();
    mint(["message", "--issue", "DSP-1", "--body", "other"], null);
    expect((await run(MESSAGE)).code).toBe(1);
    expect(messagePosts()).toHaveLength(0);
  });

  test("an argv-only marker, minted for a call no review stands behind, takes any arguments", async () => {
    writeRecord();
    const argv = ["message", "--issue", "DSP-1", "--body-file", "-"];
    const marker = mint(argv, null);
    expect((await run(argv, omp, "whatever text")).code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
    expect(existsSync(marker)).toBe(false);
  });

  // The review of the marker design at cf3f1e021, C1, and its attack, B1 and B2: the gate reviews
  // each file a write posts in place, and its marker binds the digest of the bytes it read. The CLI
  // reads each file the argv names once before it posts, and a file that changed since the review
  // is refused with what to do, since its call is literal already.
  describe("a file the write posts", () => {
    const CHANGED = "a file this write posts changed after AskGate reviewed it";
    /** The marker the gate mints for `argv` once it reviewed `file`, which `flag` names. */
    const reviewed = async (argv: readonly string[], flag: string, file: string) =>
      mint(argv, await dryArgs(argv), Date.now(), [
        { flag, operand: file, sha256: fileSha(resolve(stateDir, file)) },
      ]);
    const note = () => join(stateDir, "note.md");
    const comment = () => ["comment", "--issue", "DSP-1", "--body-file", note()];
    const upload = (file: string) => [
      "artifact",
      "--issue",
      "DSP-1",
      "--name",
      "brief.md",
      "--path",
      file,
    ];

    test("a --body-file that AskGate reviewed posts once", async () => {
      writeRecord();
      writeFileSync(note(), "The reviewed text.\n");
      const marker = await reviewed(comment(), "--body-file", note());
      expect((await run(comment())).code).toBe(0);
      expect(postsTo(COMMENTS).map((p) => p.sent)).toMatchObject([
        { body: "The reviewed text.\n" },
      ]);
      expect(existsSync(marker)).toBe(false);
    });

    test("a --body-file changed after the review is refused with what to do, and nothing posts", async () => {
      writeRecord();
      writeFileSync(note(), "The reviewed text.\n");
      const marker = await reviewed(comment(), "--body-file", note());
      writeFileSync(note(), "Text an edit of the same message wrote.\n");
      const { code, stdout } = await run(comment());
      expect(code).toBe(1);
      expect(stdout).toStartWith("dispatch comment was not called: AskGate is on for this session");
      expect(stdout).toContain(`${CHANGED} (\`--body-file ${note()}\`)`);
      expect(stdout).toContain("Send the call again in a later message.");
      expect(stdout).not.toContain("literal words");
      expect(posts).toHaveLength(0);
      expect(existsSync(marker)).toBe(true);
      expect(ledger()).toMatchObject([
        {
          tool: "dispatch_comment",
          askgate: { mode: "block", why: expect.stringContaining(CHANGED) },
        },
      ]);
    });

    test("a markdown upload by a relative --path that AskGate reviewed posts its bytes once", async () => {
      writeRecord();
      writeFileSync(join(stateDir, "brief.md"), "# Brief\n");
      await reviewed(upload("brief.md"), "--path", "brief.md");
      expect((await run(upload("brief.md"))).code).toBe(0);
      expect(postsTo(ARTIFACTS).map((p) => p.sent)).toEqual(["# Brief\n"]);
    });

    test("an upload by --path changed after the review is refused, and nothing posts", async () => {
      writeRecord();
      const brief = join(stateDir, "brief.md");
      writeFileSync(brief, "# Brief\n");
      await reviewed(upload(brief), "--path", brief);
      writeFileSync(brief, "# Brief, edited in the same message\n");
      const { code, stdout } = await run(upload(brief));
      expect(code).toBe(1);
      expect(stdout).toContain(`${CHANGED} (\`--path ${brief}\`)`);
      expect(stdout).toContain("Send the call again in a later message.");
      expect(posts).toHaveLength(0);
    });

    test("an upload by --path sends the bytes the check read, though the file changes before the send", async () => {
      writeRecord();
      const brief = join(stateDir, "brief.md");
      writeFileSync(brief, "# Brief\n");
      await reviewed(upload(brief), "--path", brief);
      // The file changes after the check, as the request goes out: a fresh read would send it.
      const changing = (async (url: string | URL | Request, init?: RequestInit) => {
        writeFileSync(brief, "# Brief, changed after the check\n");
        return fetch(url, init);
      }) as typeof fetch;
      expect((await run(upload(brief), omp, "", changing)).code).toBe(0);
      expect(postsTo(ARTIFACTS).map((p) => p.sent)).toEqual(["# Brief\n"]);
    });

    test("in warn mode a changed file posts with a warning and is logged first", async () => {
      writeRecord("ses-1", undefined, "warn");
      writeFileSync(note(), "The reviewed text.\n");
      await reviewed(comment(), "--body-file", note());
      writeFileSync(note(), "Text an edit of the same message wrote.\n");
      const { code, stdout } = await run(comment());
      expect(code).toBe(0);
      expect(stdout).toContain(
        `dispatch: warning: AskGate is on for this session in warn mode, and ${CHANGED}`
      );
      expect(stdout).toContain(
        "Send a write in a later message than the call that changes its file."
      );
      expect(postsTo(COMMENTS)).toHaveLength(1);
      expect(ledger()[0]).toMatchObject({
        tool: "dispatch_comment",
        askgate: { mode: "warn", why: expect.stringContaining(CHANGED) },
      });
    });
  });

  // The attack on the marker design at cf3f1e021, N1: the gate mints a marker when it prepares the
  // calls of a message, and a call can wait there behind an exclusive call of the same message for
  // up to that call's limit (an eval cell's is 3,600 s). The age limit bounds only a marker its call
  // left behind, since the gate removes each marker when its call ends.
  test("a marker that waited an hour behind another call of its message still posts", async () => {
    writeRecord();
    const marker = mint(MESSAGE, await dryArgs(MESSAGE), Date.now() - 61 * 60_000);
    expect((await run(MESSAGE)).code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
    expect(existsSync(marker)).toBe(false);
  });

  test("a marker older than the longest tool limit is refused and removed", async () => {
    writeRecord();
    const marker = mint(MESSAGE, null, Date.now() - 121 * 60_000);
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(1);
    expect(stdout).toContain("expired");
    expect(existsSync(marker)).toBe(false);
    expect(messagePosts()).toHaveLength(0);
  });

  test("the gate's next mint keeps a marker an hour old, and sweeps one older than the age limit", () => {
    writeRecord();
    const waiting = mint(MESSAGE, null);
    const left = mint(["message", "--issue", "DSP-1", "--body", "left behind"], null);
    const hour = (Date.now() - 61 * 60_000) / 1000;
    const old = (Date.now() - 121 * 60_000) / 1000;
    utimesSync(waiting, hour, hour);
    utimesSync(left, old, old);
    mintMarker(
      store(),
      { argv: MESSAGE, args: null, files: [], toolCallId: "call-2", outcome: "verdict" },
      Date.now()
    );
    expect(existsSync(waiting)).toBe(true);
    expect(existsSync(left)).toBe(false);
  });

  // The attack on the marker design at cf3f1e021, N4: the ledger line that tells pi-envoy a write
  // went out unreviewed was written after the post, so a ledger that failed then hid the write.
  // In warn mode the CLI now writes that line on its own before it sends, and the result's line
  // after it carries no `askgate` field, so the report counts the write once.
  test("in warn mode a write with no marker is logged first, then posts and warns", async () => {
    writeRecord("ses-1", undefined, "warn");
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
    const [line] = messagePosts()[0]?.ledger.split("\n") ?? [];
    expect(JSON.parse(line ?? "")).toMatchObject({
      tool: "dispatch_message",
      askgate: { mode: "warn", why: "this call carries no AskGate marker" },
    });
    expect(stdout).toContain("dispatch: warning: AskGate is on for this session in warn mode");
    expect(cliRefusals(stdout)).toEqual([]);
    const [before, after, ...rest] = ledger();
    expect(rest).toEqual([]);
    expect(before).not.toHaveProperty("details");
    expect(after).toMatchObject({ tool: "dispatch_message", details: expect.any(Object) });
    expect(after).not.toHaveProperty("askgate");
  });

  test("in warn mode a write with no marker whose ledger line cannot be written does not post", async () => {
    writeRecord("ses-1", undefined, "warn");
    mkdirSync(join(stateDir, "sessions", "ses-1", "results.jsonl"));
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(1);
    expect(stdout).toStartWith("dispatch message was not called:");
    expect(stdout).toContain("could not be written");
    expect(messagePosts()).toHaveLength(0);
  });

  test("with no record the write posts and nothing is required", async () => {
    const { code } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
  });

  test("a record of a dead process, of a reused pid, of another session or of another host is inert", async () => {
    const child = Bun.spawn(["true"]);
    await child.exited;
    const dead = { pid: child.pid, start: "1", store: store() };
    const reused = { pid: process.pid, start: "1", store: store() };
    for (const owner of [dead, reused]) {
      writeRecord("ses-1", owner);
      expect((await run(MESSAGE)).code).toBe(0);
    }
    writeRecord("ses-other");
    expect((await run(MESSAGE)).code).toBe(0);
    // Claude Code's session: host claude, its own session id, which no Oh My Pi record names.
    writeRecord("ses-1");
    expect(
      (
        await run(MESSAGE, {
          DISPATCH_HOST: "claude",
          CLAUDE_CODE_SESSION_ID: "cc-1",
          DISPATCH_SESSION_ID: "ses-1",
        })
      ).code
    ).toBe(0);
    expect(messagePosts()).toHaveLength(4);
  });

  test("help, a dry run and every ungated command need no marker under a live record", async () => {
    writeFileSync(join(stateDir, "shot.png"), "image bytes");
    writeRecord();
    expect((await run(["message", "--help"])).code).toBe(0);
    expect((await run(["--help"])).code).toBe(0);
    expect((await run([...MESSAGE, "--dry-run"])).code).toBe(0);
    for (const argv of [
      ["search", "--query", "landed"],
      ["issue-update", "--issue", "DSP-1", "--status", "in_progress"],
      ["issue", "--project", "DSP", "--title", "No spec"],
      ["artifact", "--issue", "DSP-1", "--name", "shot.png", "--path", "shot.png"],
    ]) {
      posts.length = 0;
      const { stdout } = await run(argv);
      expect(stdout).not.toContain("AskGate");
      expect(posts.length).toBeGreaterThan(0);
    }
  });

  test("an issue with a spec and a markdown upload are gated", async () => {
    writeRecord();
    for (const [argv, command] of [
      [["issue", "--project", "DSP", "--title", "t", "--spec", "# Spec"], "issue"],
      [["artifact", "--issue", "DSP-1", "--name", "brief.md", "--content", "# Brief"], "artifact"],
      [["artifact", "--issue", "DSP-1", "--name", "notes", "--path", "notes.markdown"], "artifact"],
    ] as const) {
      const { code, stdout } = await run(argv);
      expect(code).toBe(1);
      expect(stdout).toStartWith(`dispatch ${command} was not called: AskGate is on`);
    }
    expect(posts).toHaveLength(0);
  });

  test("a record that cannot be read, or that names a missing store, is a refusal naming the cause", async () => {
    const self = { pid: process.pid, start: startOf(process.pid) };
    const file = ownerFile("ses-1", self);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, "{not json");
    const broken = await run(MESSAGE);
    expect(broken.code).toBe(1);
    expect(broken.stdout).toStartWith("dispatch message was not called: AskGate's record");
    expect(broken.stdout).toContain(file);
    writeFileSync(file, JSON.stringify({ v: 1, ...self, store: join(stateDir, "gone") }));
    const missing = await run(MESSAGE);
    expect(missing.code).toBe(1);
    expect(missing.stdout).toContain(join(stateDir, "gone"));
    expect(posts).toHaveLength(0);
  });

  test("pruning keeps an idle session whose record is live, and removes it once the process is gone", () => {
    writeRecord();
    const dir = join(stateDir, "sessions", "ses-1");
    const old = (Date.now() - 15 * 86_400_000) / 1000;
    utimesSync(dir, old, old);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(dir)).toBe(true);
    rmSync(ownerFile("ses-1", { pid: process.pid, start: startOf(process.pid) }));
    writeRecord("ses-1", { pid: process.pid, start: "1", store: store() });
    utimesSync(dir, old, old);
    writeFileSync(join(stateDir, ".pruned"), "");
    utimesSync(join(stateDir, ".pruned"), old, old);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(dir)).toBe(false);
  });
});

describe("AskGate at the dispatch entry, across processes", () => {
  const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
  test("one marker and two concurrent calls of its argv: exactly one posts", async () => {
    writeRecord();
    mint(MESSAGE, null);
    const spawnOne = () =>
      Bun.spawn([process.execPath, ENTRY, ...MESSAGE], {
        env: {
          PATH: process.env.PATH,
          ...(environment(omp) as Record<string, string>),
        },
        stdout: "pipe",
        stderr: "pipe",
      });
    const children = [spawnOne(), spawnOne()];
    const results = await Promise.all(
      children.map(async (child) => ({
        code: await child.exited,
        stdout: await new Response(child.stdout).text(),
      }))
    );
    expect(results.map((r) => r.code).sort()).toEqual([0, 1]);
    expect(results.find((r) => r.code === 1)?.stdout).toStartWith(REFUSAL);
    expect(messagePosts()).toHaveLength(1);
    expect(readdirSync(markers())).toEqual([]);
  });

  // Every form AskGate's gate does not read, run as an agent would run it: each reaches the CLI as
  // the session, finds the live record and no marker, and is refused before any request. With no
  // record the same form posts: the check is inert outside a gated Oh My Pi session.
  test("a gated write from xargs, python3 -c, node -e, bun -e, an alias or a script is refused; with no record it posts", async () => {
    const bin = join(stateDir, "bin");
    mkdirSync(bin);
    writeFileSync(
      join(bin, "dispatch"),
      `#!/bin/sh\nexec "${process.execPath}" "${ENTRY}" "$@"\n`,
      { mode: 0o755 }
    );
    const script = join(stateDir, "post.sh");
    writeFileSync(script, "dispatch message --issue DSP-1 --body 'from a script'\n");
    const argv = '"message", "--issue", "DSP-1", "--body", "hi"';
    const forms: Array<[string, string[]]> = [
      ["xargs", ["bash", "-c", "printf '%s\\n' message --issue DSP-1 --body hi | xargs dispatch"]],
      ["python3 -c", ["python3", "-c", `import subprocess; subprocess.run(["dispatch", ${argv}])`]],
      [
        "node -e",
        [
          "node",
          "-e",
          `require("node:child_process").spawnSync("dispatch", [${argv}], { stdio: "inherit" })`,
        ],
      ],
      [
        "bun -e",
        [process.execPath, "-e", `Bun.spawnSync(["dispatch", ${argv}], { stdout: "inherit" })`],
      ],
      [
        "an alias",
        [
          "bash",
          "-c",
          "shopt -s expand_aliases\nalias d=dispatch\nd message --issue DSP-1 --body hi",
        ],
      ],
      ["a script", ["bash", script]],
    ];
    const runForm = async (command: string[]) => {
      const child = Bun.spawn(command, {
        cwd: stateDir,
        env: {
          ...(environment(omp) as Record<string, string>),
          PATH: `${bin}:${process.env.PATH ?? ""}`,
        },
        stdout: "pipe",
        stderr: "pipe",
      });
      await child.exited;
      return new Response(child.stdout).text();
    };
    writeRecord();
    for (const [form, command] of forms) {
      const stdout = await runForm(command);
      expect({ form, refused: stdout.includes(REFUSAL) }).toEqual({ form, refused: true });
    }
    expect(messagePosts()).toHaveLength(0);
    rmSync(join(stateDir, "sessions", "ses-1", "askgate"), { recursive: true });
    for (const [, command] of forms) await runForm(command);
    expect(messagePosts()).toHaveLength(forms.length);
  }, 60_000);
});

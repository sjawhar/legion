import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { mkdir, readFile, symlink } from "node:fs/promises";
import * as path from "node:path";
import {
  type Block,
  type Cleanup,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { z } from "zod";

// Acceptance 1 of LEGION-630, and LEGION-631's deletion of the tool_call hook, on the real Oh My
// Pi: no Legion role is refused a tool, and nothing a pane runs is refused. A root architect runs
// `jj log` and reads `/proc/self/cgroup`; it, a reviewer and a merger each write, read and `edit`
// a scratch file in the issue workspace, then run `jj undo --help` — the command the deleted
// operation-log rule refused — and every result reaches the model as a success, the last one
// jj's own usage text. Only the real binary shows that its own tools (bash, write, read, the
// hashline edit) run unrefused under the extension.
// LEGION_TEST_OMP names the binary: the fork pin in the repository's .omp-pin, which CI's
// pi-legion job installs (.github/actions/install-omp) before its `bun test` runs this file; on
// the devbox, `mise where <pin>`/bin/omp. A run without one skips, except on GitHub Actions,
// where a skip would hide the only run of the check on the host that ships it (GITHUB_ACTIONS,
// not CI: agent harnesses on the devbox export CI=true).
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";
/** The daemon's golden registration answer (`packages/daemon/internal/api`), so a field the daemon
 * adds to it reaches the stub below. */
const registered: Record<string, unknown> = JSON.parse(
  readFileSync(
    path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
    "utf8"
  )
);

/** Every pane runs in this tree; a root architect's own issue is the tree. */
const TREE = "TOOLS-1";

/** The roles the deleted gate refused tools to (LEGION-630). */
type Role = "architect" | "reviewer" | "merger";

/** Which pane runs. */
interface PaneOptions {
  readonly role: Role;
  /** True makes the pane the tree's root (`LEGION_TREE === LEGION_ISSUE`): a root architect. False
   * puts it on a child issue: a phase worker's pane. */
  readonly root: boolean;
}

/**
 * One scripted reply: the blocks, or a function of the Messages request it answers, so a reply can
 * be built from the previous turn's tool result (`toolResultsIn(request).at(-1)`).
 */
type Reply = readonly Block[] | ((request: Request) => readonly Block[]);

/** A `tool_result` block as the host sent it back to the gateway. */
interface ToolResult {
  readonly tool_use_id: string;
  readonly is_error: boolean;
  /** The result's string content, or its text blocks joined. */
  readonly text: string;
}

interface Pane {
  /** Every `tool_result` the host sent back to the gateway, in conversation order. */
  readonly toolResults: () => ToolResult[];
  /** The pane's issue workspace. */
  readonly workspace: string;
}

/**
 * Whether a Messages request is a side turn rather than a turn. The host sends one as an
 * ordinary Messages request over a snapshot of the conversation whose last message is the `<btw>`
 * block around the question, which pi-envoy adds for `ctx.runEphemeralTurn` — so the stand-in
 * answers it distinctly and nothing about it reaches the transcript.
 */
function isSelfCheck(request: Request): boolean {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null).includes("<btw>");
}

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** The conversation of a Messages request, as far as tool results go: each message's role and
 * content blocks. */
const Messages = z.array(
  z.looseObject({ role: z.string(), content: z.union([z.string(), z.array(z.unknown())]) })
);

/** A `tool_result` block of a user message, whose content is a string or text blocks. */
const ToolResultBlock = z.looseObject({
  type: z.literal("tool_result"),
  tool_use_id: z.string(),
  is_error: z.boolean().optional(),
  content: z
    .union([z.string(), z.array(z.looseObject({ type: z.string(), text: z.string().optional() }))])
    .optional(),
});

/** The `tool_result` blocks of a Messages request's user messages, in conversation order. */
function toolResultsIn(request: Request): ToolResult[] {
  return Messages.parse(request.body.messages ?? []).flatMap((message) => {
    if (message.role !== "user" || typeof message.content === "string") return [];
    return message.content.flatMap((block) => {
      const parsed = ToolResultBlock.safeParse(block);
      if (!parsed.success) return [];
      const { tool_use_id, is_error, content } = parsed.data;
      const text =
        typeof content === "string"
          ? content
          : (content ?? [])
              .filter((part) => part.type === "text")
              .map((part) => part.text ?? "")
              .join("");
      return [{ tool_use_id, is_error: is_error === true, text }];
    });
  });
}

/** Runs `jj` with a test identity, so it commits in CI too, failing loudly on a non-zero exit. */
async function runJj(jj: string, args: readonly string[]): Promise<void> {
  const child = Bun.spawn([jj, ...args], {
    env: { ...process.env, JJ_USER: "Legion Test", JJ_EMAIL: "legion-test@example.invalid" },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stderr, code] = await Promise.all([new Response(child.stderr).text(), child.exited]);
  if (code !== 0) throw new Error(`jj ${args.join(" ")} exited ${code}:\n${stderr}`);
}

/**
 * Runs one Legion pane on the real Oh My Pi until its run settles: the Legion and Envoy
 * extensions from this checkout, booted against a stand-in for the daemon's claim routes and the
 * Envoy listener (no NATS: the Envoy extension then skips inbound delivery, and the role claim is
 * two listener calls), with a stand-in model gateway that answers the pane's turns from
 * `replies`. The pane's workspace is a jj repository with one described commit, as an issue
 * workspace is. The daemon's assignment arrives as the RPC `prompt`.
 */
async function runPane(
  binary: string,
  replies: readonly Reply[],
  options: PaneOptions
): Promise<Pane> {
  const { role } = options;
  const issue = options.root ? TREE : "TOOLS-2";
  const claimToken = `legion-tools-${issue.toLowerCase()}-${role}`;
  const { root, home, workspace, sessions } = await ompRoot(binary, "legion-role-tools-", cleanup);
  const state = path.join(root, "state");
  const bin = path.join(root, "bin");
  await mkdir(bin, { recursive: true });
  await mkdir(state, { recursive: true, mode: 0o700 });

  // The pane's PATH is the harness's fixed one plus `bin`, and CI's jj lives in ~/.local/bin
  // (.github/actions/install-jj): the jj this test process finds is linked into `bin`, so the
  // pane's `jj log` runs the one that made its workspace.
  const jj = Bun.which("jj");
  if (jj === null) throw new Error("jj is not on PATH");
  await symlink(jj, path.join(bin, "jj"));
  await runJj(jj, ["git", "init", workspace]);
  await runJj(jj, ["-R", workspace, "describe", "-m", "role tools scratch"]);

  let answered = 0;
  let selfChecks = 0;
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const request: Request = { path: url.pathname, body };
      // The self-check is not a turn: it consumes no scripted reply, and the conversation's
      // next turn is answered as if it had never happened — which is what the host's snapshot
      // makes true.
      if (isSelfCheck(request)) {
        selfChecks += 1;
        return new Response(
          messageStream([{ type: "text", text: "PROCEEDING" }], `btw_${selfChecks}`),
          { headers: { "content-type": "text/event-stream" } }
        );
      }
      const reply = replies[answered];
      answered += 1;
      if (reply === undefined) {
        return Response.json(
          {
            type: "error",
            error: { type: "invalid_request_error", message: "no reply scripted" },
          },
          { status: 400 }
        );
      }
      const blocks = typeof reply === "function" ? reply(request) : reply;
      return new Response(messageStream(blocks, `msg_${answered}`), {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken,
        tree: TREE,
        issue,
        role,
        generation: 1,
        secret: "tools-secret",
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // The Envoy listener: registration, the role claim, and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "tools-machine",
      dir: workspace,
      topics: [],
    });
  });
  await writeStandinProfile(home, base);

  // The run has settled at the RPC stream's terminal agent_end.
  const settled = Promise.withResolvers<void>();
  const rpc = spawnRpc(
    binary,
    {
      extensions: [
        // The Envoy entry is the sibling plugin's: a Legion pane loads both, and the Legion entry
        // refuses to run without it.
        path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
        path.join(import.meta.dir, "legion.ts"),
      ],
      home,
      workspace,
      sessions,
      bin,
      env: {
        ENVOY_URL: base,
        LEGION_DAEMON_URL: base,
        LEGION_ROLE: role,
        LEGION_TREE: TREE,
        LEGION_ISSUE: issue,
        LEGION_GENERATION: "1",
        LEGION_BOOT_TOKEN: "tools-boot",
        LEGION_STATE_DIR: state,
        LEGION_WORKSPACE: workspace,
        // The git identity the daemon puts on every pane (runtime.GitIdentity), so the pane's jj
        // snapshots the working copy as a named author instead of warning on each command.
        JJ_USER: "Legion Test",
        JJ_EMAIL: "legion-test@example.invalid",
      },
      onFrame: (frame) => {
        if (
          "type" in frame &&
          frame.type === "agent_end" &&
          !("isTerminal" in frame && frame.isTerminal === false)
        ) {
          settled.resolve();
        }
      },
    },
    cleanup
  );
  rpc.send({ type: "prompt", message: `Check the ${role} pane's tools on ${issue}.` });
  await Promise.race([settled.promise, rpc.closed]);
  await rpc.end();

  const turns = (): Request[] =>
    requests.filter(
      (request) => request.path === "/anthropic/v1/messages" && !isSelfCheck(request)
    );
  return {
    toolResults: () => {
      // Every turn carries the whole conversation so far, so the first sighting of each id is
      // its place in the conversation.
      const seen = new Set<string>();
      const results: ToolResult[] = [];
      for (const turn of turns()) {
        for (const result of toolResultsIn(turn)) {
          if (seen.has(result.tool_use_id)) continue;
          seen.add(result.tool_use_id);
          results.push(result);
        }
      }
      return results;
    },
    workspace,
  };
}

/** A tool call with the one-line intent every Oh My Pi tool requires. */
function call(name: string, input: Record<string, unknown>): Block {
  return { type: "tool_use", name, input: { i: "Checking the pane's tools", ...input } };
}

/** The turns every pane runs, one tool call each: write, read and edit the scratch file, then the
 * `jj undo --help` control, then the settling reply. Paths are relative to the workspace, the
 * pane's cwd. */
const SCRATCH_TURNS: readonly Reply[] = [
  [call("write", { path: "scratch.txt", content: "first line\n" })],
  [call("read", { path: "scratch.txt" })],
  (request) => {
    const read = toolResultsIn(request).at(-1);
    if (read === undefined) throw new Error("the read turn carried no tool result");
    // The hashline edit anchors on the read's own header line, `[scratch.txt#XXXX]`: the file
    // and the 4-hex snapshot tag the host computed, which the model copies rather than invents.
    const header = read.text.split("\n")[0] ?? "";
    return [call("edit", { input: `${header}\nPUT 1.=1:\n+edited line` })];
  },
  [call("bash", { command: "jj undo --help" })],
  // The phase-stall follow-up (src/phase-stall.ts) sends a phase worker whose run settles
  // without a handoff one more turn, unless its last reply starts WAITING: so the settling reply
  // does, and the run ends on the scripted turns alone.
  [{ type: "text", text: "WAITING: done." }],
];

/** The words any refusal carries. */
const REFUSALS = ["block", "refused"];

/**
 * Asserts the pane's `count` tool results: every one is a success that names no refusal, the
 * control (the last) is jj's own usage text for `jj undo`, and the edit landed on disk. Returns
 * the results for the case's own assertions.
 */
async function expectEverySucceeded(pane: Pane, count: number): Promise<ToolResult[]> {
  const results = pane.toolResults();
  expect(results).toHaveLength(count);
  for (const result of results) {
    expect(result.is_error).toBe(false);
    for (const refusal of REFUSALS) expect(result.text).not.toContain(refusal);
  }
  expect(results.at(-1)?.text).toContain("Usage: jj undo");
  expect(await readFile(path.join(pane.workspace, "scratch.txt"), "utf8")).toBe("edited line\n");
  return results;
}

test.skipIf(omp === undefined && !onActions)(
  "a root architect runs jj log, reads /proc/self/cgroup, and writes, reads and edits a file; jj undo --help runs",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await runPane(
      omp,
      [
        [call("bash", { command: "jj log" })],
        [call("bash", { command: "cat /proc/self/cgroup" })],
        ...SCRATCH_TURNS,
      ],
      { role: "architect", root: true }
    );
    const [jjLog, cgroup] = await expectEverySucceeded(pane, 6);
    // The commit the test described, so the log is the workspace's and not an error's text.
    expect(jjLog?.text).toContain("role tools scratch");
    // A cgroup line: `hierarchy:controllers:path`, under either cgroup version.
    expect(cgroup?.text).toMatch(/^\d+:.*:/m);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a reviewer writes, reads and edits a file; jj undo --help runs",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await runPane(omp, SCRATCH_TURNS, { role: "reviewer", root: false });
    await expectEverySucceeded(pane, 4);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a merger writes, reads and edits a file; jj undo --help runs",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await runPane(omp, SCRATCH_TURNS, { role: "merger", root: false });
    await expectEverySucceeded(pane, 4);
  },
  120_000
);

import { afterEach, expect, test } from "bun:test";
import { readFile, symlink } from "node:fs/promises";
import * as path from "node:path";
import {
  type Block,
  type Cleanup,
  type LegionPane,
  type Reply,
  runLegionPane,
  type ToolResult,
  toolResultsIn,
} from "@legion/pi-shared/test/omp-harness";

// Acceptance 1 of LEGION-630 on the real Oh My Pi: no Legion role is refused a tool. A root
// architect runs `jj log` and reads `/proc/self/cgroup`; it, a reviewer and a merger each write,
// read and `edit` a scratch file in the issue workspace, and every result reaches the model as a
// success. The one rule the tool_call hook keeps (extensions/legion.ts, LEGION-45) is the control
// that proves the hook is live in each pane: `jj undo` is refused with the reason that every
// Legion issue workspace shares the jj operation log. Only the real binary shows how the host
// turns a hook's `block` into the tool result the model sees, and that its own tools (bash,
// write, read, the hashline edit) run unrefused under the extension.
// LEGION_TEST_OMP names the binary: the fork pin in the repository's .omp-pin, which CI's
// pi-legion job installs (.github/actions/install-omp) before its `bun test` runs this file; on
// the devbox, `mise where <pin>`/bin/omp. A run without one skips, except on GitHub Actions,
// where a skip would hide the only run of the check on the host that ships it (GITHUB_ACTIONS,
// not CI: agent harnesses on the devbox export CI=true).
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

/** Every pane runs in this tree; a root architect's own issue is the tree. */
const TREE = "TOOLS-1";

/** The roles the deleted gate refused tools to (LEGION-630). */
type Role = "architect" | "reviewer" | "merger";

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

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
 * Runs one pane of `role` on the real Oh My Pi (`runLegionPane`). `root` makes the pane the
 * tree's root (`LEGION_TREE === LEGION_ISSUE`): a root architect. Otherwise it is on a child
 * issue: a phase worker's pane. The pane's workspace is a jj repository with one described commit,
 * as an issue workspace is.
 */
async function toolsPane(
  binary: string,
  pane: { readonly role: Role; readonly root: boolean },
  replies: readonly Reply[]
): Promise<LegionPane> {
  const { role, root } = pane;
  const issue = root ? TREE : "TOOLS-2";
  return runLegionPane(
    binary,
    replies,
    {
      name: "tools",
      role,
      tree: TREE,
      issue,
      prompt: `Check the ${role} pane's tools on ${issue}.`,
      env: {
        // The git identity the daemon puts on every pane (runtime.GitIdentity), so the pane's jj
        // snapshots the working copy as a named author instead of warning on each command.
        JJ_USER: "Legion Test",
        JJ_EMAIL: "legion-test@example.invalid",
      },
      prepare: async ({ workspace, bin }) => {
        // The pane's PATH is the harness's fixed one plus `bin`, and CI's jj lives in ~/.local/bin
        // (.github/actions/install-jj): the jj this test process finds is linked beside the
        // stand-in `legion`, so the pane's `jj log` runs the one that made its workspace.
        const jj = Bun.which("jj");
        if (jj === null) throw new Error("jj is not on PATH");
        await symlink(jj, path.join(bin, "jj"));
        await runJj(jj, ["git", "init", workspace]);
        await runJj(jj, ["-R", workspace, "describe", "-m", "role tools scratch"]);
      },
    },
    cleanup
  );
}

/** A tool call with the one-line intent every Oh My Pi tool requires. */
function call(name: string, input: Record<string, unknown>): Block {
  return { type: "tool_use", name, input: { i: "Checking the pane's tools", ...input } };
}

/** The turns every pane runs, one tool call each: write, read and edit the scratch file, then the
 * `jj undo` control, then the settling reply. Paths are relative to the workspace, the pane's cwd. */
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
  [call("bash", { command: 'jj -R "$LEGION_WORKSPACE" undo' })],
  // The phase-stall follow-up (src/phase-stall.ts) sends a phase worker whose run settles
  // without a handoff one more turn, unless its last reply starts WAITING: so the settling reply
  // does, and the run ends on the scripted turns alone.
  [{ type: "text", text: "WAITING: done." }],
];

/** The words any refusal carries. */
const REFUSALS = ["block", "refused"];

/**
 * Asserts the pane's `count` tool results: every one before the control is a success that names
 * no refusal, the control (the last) is the operation-log refusal the model saw, the edit landed
 * on disk, and no handoff ran. Returns the results for the case's own assertions.
 */
async function expectOnlyControlRefused(pane: LegionPane, count: number): Promise<ToolResult[]> {
  const results = pane.toolResults();
  expect(results).toHaveLength(count);
  for (const result of results.slice(0, -1)) {
    expect(result.is_error).toBe(false);
    for (const refusal of REFUSALS) expect(result.text).not.toContain(refusal);
  }
  const control = results.at(-1);
  expect(control?.is_error).toBe(true);
  expect(control?.text).toContain("every Legion issue workspace shares");
  expect(await readFile(path.join(pane.workspace, "scratch.txt"), "utf8")).toBe("edited line\n");
  expect((await pane.legionLog()).filter((line) => line.startsWith("handoff"))).toEqual([]);
  return results;
}

test.skipIf(omp === undefined && !onActions)(
  "a root architect runs jj log, reads /proc/self/cgroup, and writes, reads and edits a file; only jj undo is refused",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await toolsPane(omp, { role: "architect", root: true }, [
      [call("bash", { command: "jj log" })],
      [call("bash", { command: "cat /proc/self/cgroup" })],
      ...SCRATCH_TURNS,
    ]);
    const [jjLog, cgroup] = await expectOnlyControlRefused(pane, 6);
    // The commit the test described, so the log is the workspace's and not an error's text.
    expect(jjLog?.text).toContain("role tools scratch");
    // A cgroup line: `hierarchy:controllers:path`, under either cgroup version.
    expect(cgroup?.text).toMatch(/^\d+:.*:/m);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a reviewer writes, reads and edits a file; only jj undo is refused",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await toolsPane(omp, { role: "reviewer", root: false }, SCRATCH_TURNS);
    await expectOnlyControlRefused(pane, 4);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a merger writes, reads and edits a file; only jj undo is refused",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await toolsPane(omp, { role: "merger", root: false }, SCRATCH_TURNS);
    await expectOnlyControlRefused(pane, 4);
  },
  120_000
);

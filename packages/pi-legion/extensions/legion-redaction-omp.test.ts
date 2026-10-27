import { afterEach, expect, test } from "bun:test";
import { readdir, readFile } from "node:fs/promises";
import * as path from "node:path";
import {
  type Block,
  type Cleanup,
  type LegionPane,
  type LegionPaneOptions,
  messageStream,
  type Reply,
  runLegionPane,
} from "@legion/pi-shared/test/omp-harness";

// The credential incident's path on the real Oh My Pi: a Legion pane whose process environment
// holds provider keys (the shim exports `--provider-env-dir` into it), and an agent that reads that
// environment with its own tools — `read /proc/self/environ:raw`, as the leaking subagents did, and
// `env` in bash. Oh My Pi writes every tool result into the session's transcript and sends it to the
// model; the Legion entry's tool_result hook (src/secret-redaction.ts) must have replaced each
// credential first. Only the real binary shows which of the host's writes the hook reaches: the
// transcript's text and details, and the artifact a large result spills its full text to.
// LEGION_TEST_OMP names the binary, as in legion-role-tools-omp.test.ts.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

// Shaped like the leaked keys; never real.
const PROVIDER_KEY = `sk-proj-${"Zx8Cv6Bn4Mq2Wr0Ty".repeat(8)}`;
// A credential under a name no rule knows, which only the fail-closed rule catches.
const LICENCE = "WGT-9f8e7d6c5b4a39281706f5e4d3c2b1a0";

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** A tool call with the one-line intent every Oh My Pi tool requires. */
function call(name: string, input: Record<string, unknown>): Block {
  return { type: "tool_use", name, input: { i: "Checking the pane's environment", ...input } };
}

/** Every file under `directory`, recursively, with its text. */
async function filesUnder(directory: string): Promise<{ file: string; text: string }[]> {
  const files: { file: string; text: string }[] = [];
  for (const entry of await readdir(directory, { recursive: true, withFileTypes: true })) {
    if (!entry.isFile()) continue;
    const file = path.join(entry.parentPath, entry.name);
    files.push({ file, text: await readFile(file, "utf8") });
  }
  return files;
}

/** Runs a reviewer's pane with both credentials in its environment, and returns it with the
 * directory its transcripts and their artifacts are written under. */
async function leakPane(
  binary: string,
  replies: readonly Reply[],
  extra: Pick<LegionPaneOptions, "config" | "answer"> = {}
) {
  let sessions = "";
  const pane: LegionPane = await runLegionPane(
    binary,
    replies,
    {
      name: "redact",
      role: "reviewer",
      tree: "REDACT-1",
      issue: "REDACT-2",
      prompt: "Review REDACT-2.",
      env: { OPENAI_API_KEY: PROVIDER_KEY, WIDGET_LICENCE: LICENCE },
      ...extra,
      prepare: async ({ workspace }) => {
        // ompRoot's layout: the workspace and the sessions directory are siblings.
        sessions = path.join(path.dirname(workspace), "sessions");
      },
    },
    cleanup
  );
  return { pane, sessions };
}

/** Asserts that nothing the pane wrote under `sessions`, and nothing it sent the model, holds
 * either credential. Returns the transcript files for the case's own assertions. */
async function expectNoCredential(pane: LegionPane, sessions: string) {
  const files = await filesUnder(sessions);
  expect(files.length).toBeGreaterThan(0);
  for (const { file, text } of files) {
    expect({ file, holdsKey: text.includes(PROVIDER_KEY) }).toEqual({ file, holdsKey: false });
    expect({ file, holdsLicence: text.includes(LICENCE) }).toEqual({ file, holdsLicence: false });
  }
  for (const request of pane.turns()) {
    const body = JSON.stringify(request.body);
    expect(body).not.toContain(PROVIDER_KEY);
    expect(body).not.toContain(LICENCE);
  }
  return files;
}

test.skipIf(omp === undefined && !onActions)(
  "a pane that reads its own environment with read and with bash writes no credential to its transcript or the model",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const { pane, sessions } = await leakPane(omp, [
      [call("read", { path: "/proc/self/environ:raw" })],
      [call("bash", { command: "env" })],
      // The phase-stall follow-up sends a phase worker whose run settles without a handoff one more
      // turn, unless its last reply starts WAITING.
      [{ type: "text", text: "WAITING: done." }],
    ]);
    const [environ, env] = pane.toolResults();
    // Each read did run: the key's name is there, its value replaced in place.
    expect(environ?.text).toContain("OPENAI_API_KEY=[redacted by Legion: $OPENAI_API_KEY]");
    expect(env?.text).toContain("OPENAI_API_KEY=[redacted by Legion: $OPENAI_API_KEY]");
    expect(env?.text).toContain("WIDGET_LICENCE=[redacted by Legion: $WIDGET_LICENCE]");
    // Legion's own variables stay readable for whoever debugs from the transcript.
    expect(env?.text).toContain("LEGION_ROLE=reviewer");
    expect(env?.text).toContain("LEGION_ISSUE=REDACT-2");
    await expectNoCredential(pane, sessions);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a result too large to keep inline spills to an artifact that holds no credential either",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    // About 200 KB, past the host's 50 KB spill threshold, every line carrying the key.
    const { pane, sessions } = await leakPane(omp, [
      [
        call("bash", {
          command: 'for i in $(seq 1 1200); do printf "%s %s\\n" "$i" "$OPENAI_API_KEY"; done',
        }),
      ],
      [{ type: "text", text: "WAITING: done." }],
    ]);
    const files = await expectNoCredential(pane, sessions);
    // The spill happened, so the artifact is among the files checked above.
    const artifacts = files.filter(({ text }) =>
      text.includes("1200 [redacted by Legion: $OPENAI_API_KEY]")
    );
    expect(artifacts.length).toBeGreaterThan(0);
  },
  120_000
);

test.skipIf(omp === undefined && !onActions)(
  "a task subagent that reads the environment, as the leaking ones did, writes no credential to its own transcript",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    // The pane's own turns carry the `legion` tool and take the scripted replies; the subagent's
    // turns lack it and take `subagentReplies`; a side request (the host compressing each skill's
    // routing hint for the subagent's prompt) carries no tool and is answered with a word. With async
    // jobs off the task runs inline, so the pane's second turn sees the subagent's result.
    const subagentReplies: Block[][] = [
      [call("read", { path: "/proc/self/environ:raw" })],
      [{ type: "text", text: "Read the environment." }],
    ];
    let subagentTurns = 0;
    const answer = (url: URL, body: Record<string, unknown>): Response | undefined => {
      if (url.pathname !== "/anthropic/v1/messages") return undefined;
      const tools = Array.isArray(body.tools) ? (body.tools as { name?: unknown }[]) : [];
      if (tools.some((tool) => tool.name === "legion")) return undefined;
      const blocks =
        tools.length === 0
          ? [{ type: "text" as const, text: "ok" }]
          : (subagentReplies[subagentTurns++] ?? [{ type: "text" as const, text: "Done." }]);
      return new Response(messageStream(blocks, `sub_${subagentTurns}`), {
        headers: { "content-type": "text/event-stream" },
      });
    };
    const { pane, sessions } = await leakPane(
      omp,
      [
        [
          call("task", {
            context: "Find the Dispatch endpoint this pane is configured with.",
            tasks: [{ name: "EnvReader", agent: "task", task: "Read this process's environment." }],
          }),
        ],
        [{ type: "text", text: "WAITING: done." }],
      ],
      { config: ["async:", "  enabled: false"], answer }
    );
    const files = await expectNoCredential(pane, sessions);
    // The subagent's own transcript is among the files checked, and its read ran and was redacted.
    const subagent = files.find(({ file }) => path.basename(file) === "EnvReader.jsonl");
    expect(subagent?.text).toContain("OPENAI_API_KEY=[redacted by Legion: $OPENAI_API_KEY]");
  },
  120_000
);

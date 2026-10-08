import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// merger_self_posted, taken from stage3-4b13b-acceptance.sh by name and run against a merger's
// session written as Oh My Pi writes it: the 4b.13b acceptance's soft check that the merger never
// posts or publishes READY itself (the daemon posts the packet the merger completes with). Oh My
// Pi gives the model three ways to call a tool, and each must count: the tool itself, a write to
// its xd:// device, and eval code that calls tool.<name>(...). A message is a `dispatch message`
// command the model runs through bash; a publish is the envoy_publish tool.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage3-4b13b-acceptance.sh"));
const dir = mkdtempSync(join(tmpdir(), "merger-self-posted-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

type Call = Record<string, unknown>;
const ready = "READY #502 at 7cc9f81c: every required check green, every thread resolved.";
const note = "Merge gate verified; completing with the packet.";
const assistant = (...calls: Call[]) => ({
  type: "message",
  message: {
    role: "assistant",
    stopReason: "toolUse",
    content: [{ type: "text", text: "…" }, ...calls],
  },
});
const result = (text: string) => ({
  type: "message",
  message: { role: "toolResult", content: [{ type: "text", text }] },
});
const tool = (name: string, args: Record<string, unknown>): Call => ({
  type: "toolCall",
  id: `toolu_${name}`,
  name,
  arguments: args,
});
const device = (name: string, args: Record<string, unknown>): Call =>
  tool("write", { path: `xd://${name}`, content: JSON.stringify(args), i: "Post" });
const evalCall = (code: string): Call => tool("eval", { language: "js", title: "post", code });
// The three surfaces of one bash call running COMMAND.
const bashTool = (command: string): Call => tool("bash", { command });
const bashDevice = (command: string): Call => device("bash", { command });
const bashEval = (command: string): Call =>
  evalCall(`await tool.bash({ command: ${JSON.stringify(command)} });`);
const surfaces = { tool: bashTool, device: bashDevice, eval: bashEval } as const;
const message = (body: string) =>
  `dispatch message --issue LEGSMOKE-1 --body ${JSON.stringify(body)}`;

let sessions = 0;
// selfPosts runs merger_self_posted over a session of ENTRIES and returns what it found.
function selfPosts(...entries: unknown[]): unknown[] {
  const file = join(dir, `session-${++sessions}.jsonl`);
  writeFileSync(file, `${entries.map((e) => JSON.stringify(e)).join("\n")}\n`);
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
script_lib=${JSON.stringify(import.meta.dir)}
claim_session_file() { printf '%s\\n' "$SESSION"; }
${fn("merger_self_posted")}
merger_self_posted LEGSMOKE-1
`,
    ],
    { env: { ...process.env, SESSION: file } }
  );
  if (ran.exitCode !== 0)
    throw new Error(`merger_self_posted exited ${ran.exitCode}: ${ran.stderr}`);
  return JSON.parse(ran.stdout.toString());
}

describe("merger_self_posted", () => {
  for (const [surface, call] of Object.entries(surfaces)) {
    test(`a READY posted with dispatch message through bash's ${surface} counts`, () => {
      expect(selfPosts(assistant(call(message(ready))))).toHaveLength(1);
    });
  }

  test("a READY body read from a here-document counts", () => {
    const command = `dispatch message --issue LEGSMOKE-1 --body-file - <<'EOF'\n${ready}\nEOF`;
    expect(selfPosts(assistant(bashTool(command)))).toHaveLength(1);
  });

  test("a dispatch message after an assignment or a separator counts", () => {
    expect(selfPosts(assistant(bashTool(`X=1 ${message(ready)}`)))).toHaveLength(1);
    expect(selfPosts(assistant(bashTool(`cd /tmp && ${message(ready)}`)))).toHaveLength(1);
  });

  test("any publish counts, through the tool, its device, or eval", () => {
    const args = { topic: "notifications.role.merge-queue", body: note };
    expect(selfPosts(assistant(tool("envoy_publish", args)))).toHaveLength(1);
    expect(selfPosts(assistant(device("envoy_publish", args)))).toHaveLength(1);
    expect(
      selfPosts(assistant(evalCall(`await tool.envoy_publish(${JSON.stringify(args)});`)))
    ).toHaveLength(1);
  });

  test("a message that is not READY is no self-post, however it is made", () => {
    for (const call of Object.values(surfaces)) {
      expect(selfPosts(assistant(call(message(note))))).toHaveLength(0);
    }
  });

  test("a command that only reads Dispatch, a READY in it included, is no self-post", () => {
    expect(
      selfPosts(assistant(bashTool("dispatch read --issue LEGSMOKE-1 | grep READY")))
    ).toHaveLength(0);
    expect(selfPosts(assistant(bashTool(`echo ${JSON.stringify(message(ready))}`)))).toHaveLength(
      0
    );
  });

  test("a write is a device call only to the tool's own device path", () => {
    const write = (path: string): Call =>
      tool("write", { path, content: JSON.stringify({ command: message(ready) }), i: "Post" });
    expect(selfPosts(assistant(write(" xd://bash ")))).toHaveLength(1);
    expect(selfPosts(assistant(write("xd://bashes")))).toHaveLength(0);
    expect(selfPosts(assistant(write("notes/xd://bash.md")))).toHaveLength(0);
  });

  test("the completion that carries the packet, and a tool result quoting the command, are no self-post", () => {
    expect(
      selfPosts(
        assistant(tool("legion", { op: "handoff_complete", ready: true, summary: ready })),
        result(`Publish with tool.envoy_publish(...) or post with ${message("READY")}.`)
      )
    ).toHaveLength(0);
  });
});

import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// merger_self_posted, taken from stage3-4b13b-acceptance.sh by name and run against a merger's
// session written as Oh My Pi writes it: the 4b.13b acceptance's soft check that the merger never
// posts or publishes READY itself (the daemon posts the packet the merger completes with). Oh My
// Pi gives the model three ways to call dispatch_message and envoy_publish, and each must count:
// the tool itself, a write to its xd:// device, and eval code that calls tool.<name>(...).
const script = readFileSync(join(import.meta.dir, "..", "stage3-4b13b-acceptance.sh"), "utf8");
const fn = (name: string) => {
  const found = new RegExp(`^${name}\\(\\) \\{(?:.*\\}$|[\\s\\S]*?\\n\\}$)`, "m").exec(script);
  if (found === null) throw new Error(`stage3-4b13b-acceptance.sh defines no ${name}()`);
  return found[0];
};
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
  test("a READY posted through the dispatch_message tool counts", () => {
    expect(
      selfPosts(assistant(tool("dispatch_message", { issue: "LEGSMOKE-1", body: ready })))
    ).toHaveLength(1);
  });

  test("a READY posted through a write to xd://dispatch_message counts", () => {
    expect(
      selfPosts(assistant(device("dispatch_message", { issue: "LEGSMOKE-1", body: ready })))
    ).toHaveLength(1);
  });

  test("a READY posted through eval calling tool.dispatch_message counts", () => {
    const code = `const body = \`${ready}\`;\nawait tool.dispatch_message({ issue: "LEGSMOKE-1", body });`;
    expect(selfPosts(assistant(evalCall(code)))).toHaveLength(1);
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
    const args = { issue: "LEGSMOKE-1", body: note };
    expect(selfPosts(assistant(tool("dispatch_message", args)))).toHaveLength(0);
    expect(selfPosts(assistant(device("dispatch_message", args)))).toHaveLength(0);
    expect(
      selfPosts(assistant(evalCall(`await tool.dispatch_message(${JSON.stringify(args)});`)))
    ).toHaveLength(0);
  });

  test("eval that only reads Dispatch, a READY in its code included, is no self-post", () => {
    const code = `const t = await tool.dispatch_read({ issue: "LEGSMOKE-1" });\nt.text.includes("READY");`;
    expect(selfPosts(assistant(evalCall(code)))).toHaveLength(0);
  });

  test("the completion that carries the packet, and a tool result quoting the tools, are no self-post", () => {
    expect(
      selfPosts(
        assistant(tool("legion", { op: "handoff_complete", ready: true, summary: ready })),
        result(
          'Publish with tool.envoy_publish(...) or post with tool.dispatch_message({ body: "READY" }).'
        )
      )
    ).toHaveLength(0);
  });
});

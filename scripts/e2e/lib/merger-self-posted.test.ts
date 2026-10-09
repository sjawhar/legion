import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  assistant,
  bashSurfaces,
  bashTool,
  type Call,
  deviceCall,
  evalCall,
  jsonl,
  toolCall,
  toolResult,
} from "./omp-session-fixtures";
import { scriptFunctions } from "./script-functions";

// merger_self_posted, taken from stage3-4b13b-acceptance.sh by name and run against a merger's
// session written as Oh My Pi writes it (omp-session-fixtures.ts): the 4b.13b acceptance's soft
// check that the merger never posts or publishes READY itself (the daemon posts the packet the
// merger completes with). Each of the four ways Oh My Pi gives the model to call a tool must count:
// the tool itself, a write to its xd:// device, eval code that calls tool.<name>(...), and eval code
// that calls the generic tool.write(...) naming that device. A message is a `dispatch message`
// command the model runs through bash; a publish is the envoy_publish tool.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage3-4b13b-acceptance.sh"));
const dir = mkdtempSync(join(tmpdir(), "merger-self-posted-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const ready = "READY #502 at 7cc9f81c: every required check green, every thread resolved.";
const note = "Merge gate verified; completing with the packet.";
const message = (body: string) =>
  `dispatch message --issue LEGSMOKE-1 --body ${JSON.stringify(body)}`;

let sessions = 0;
// selfPosts runs merger_self_posted over a session of ENTRIES and returns what it found.
function selfPosts(...entries: unknown[]): unknown[] {
  const file = join(dir, `session-${++sessions}.jsonl`);
  writeFileSync(file, jsonl(entries));
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
  for (const [surface, call] of Object.entries(bashSurfaces)) {
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

  test("any publish counts, through the tool, its device, or either eval form", () => {
    const args = { topic: "notifications.role.merge-queue", body: note };
    expect(selfPosts(assistant(toolCall("envoy_publish", args)))).toHaveLength(1);
    expect(selfPosts(assistant(deviceCall("envoy_publish", args)))).toHaveLength(1);
    expect(
      selfPosts(assistant(evalCall(`await tool.envoy_publish(${JSON.stringify(args)});`)))
    ).toHaveLength(1);
    expect(
      selfPosts(
        assistant(
          evalCall(
            `await tool.write({ path: "xd://envoy_publish", content: ${JSON.stringify(JSON.stringify(args))} });`
          )
        )
      )
    ).toHaveLength(1);
  });

  test("a message that is not READY is no self-post, however it is made", () => {
    for (const call of Object.values(bashSurfaces)) {
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
      toolCall("write", { path, content: JSON.stringify({ command: message(ready) }), i: "Post" });
    expect(selfPosts(assistant(write(" xd://bash ")))).toHaveLength(1);
    expect(selfPosts(assistant(write("xd://bashes")))).toHaveLength(0);
    expect(selfPosts(assistant(write("notes/xd://bash.md")))).toHaveLength(0);
  });

  // Each probe below carries a READY `dispatch message` string literal, so the downstream
  // runs_dispatch and body checks would take it: only calls("bash") rejecting the call keeps it out.
  const readyCommand = JSON.stringify(message(ready));

  test("a READY posted through eval calling tool.write with content built before path counts", () => {
    const code = `r = await tool.write({"content": json.dumps({"command": ${readyCommand}}), "path": "xd://bash"})\nr`;
    expect(selfPosts(assistant(evalCall(code)))).toHaveLength(1);
  });

  test("eval that writes to another device while a comment mentions xd://bash is no self-post", () => {
    const code = `// writing xd://another_device, not xd://bash here\nawait tool.write({ path: "xd://another_device", content: ${readyCommand} });`;
    expect(selfPosts(assistant(evalCall(code)))).toHaveLength(0);
  });

  test("a block comment naming the device inside another device's write is no self-post", () => {
    const code = `await tool.write({ path: "xd://another_device", /* xd://bash */ content: ${readyCommand} });`;
    expect(selfPosts(assistant(evalCall(code)))).toHaveLength(0);
  });

  test("a file write whose content mentions the device as prose is no self-post", () => {
    const code = `await tool.write({ path: "./notes.md", content: ${readyCommand} + " See xd://bash for the device." });`;
    expect(selfPosts(assistant(evalCall(code)))).toHaveLength(0);
  });

  test("a Python kwargs file write, then a print naming the device, is no self-post", () => {
    const code = `tool.write(path="./notes.md", content=${readyCommand})\nprint("the device is xd://bash")`;
    expect(selfPosts(assistant(evalCall(code)))).toHaveLength(0);
  });

  test("a write to a device with bash's name as a prefix is no self-post", () => {
    const code = `await tool.write({ path: "xd://bash.md", content: ${readyCommand} });`;
    expect(selfPosts(assistant(evalCall(code)))).toHaveLength(0);
  });

  test("the completion that carries the packet, and a tool result quoting the command, are no self-post", () => {
    expect(
      selfPosts(
        assistant(toolCall("legion", { op: "handoff_complete", ready: true, summary: ready })),
        toolResult(`Publish with tool.envoy_publish(...) or post with ${message("READY")}.`)
      )
    ).toHaveLength(0);
  });
});

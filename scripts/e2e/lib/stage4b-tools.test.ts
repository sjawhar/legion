import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { runJq } from "./run-jq";
import { scriptFunctions } from "./script-functions";

// tool_ran and tool_result_said, taken from stage4b-sandbox-tree.sh by name and run over a session
// a real implementer pod wrote (testdata/stage4b-tools-session.jsonl): the proof a tool ran and
// answered, where assistant_said only proves the model said so. Each test pins one rule of
// lib/stage4b-tools.jq: a result is the tool's by the call it answers (a write to xd://lsp records
// toolName write), an error result never counts, a tool that never ran has no result, and a result
// whose call the session no longer holds is no tool's.
const root = join(import.meta.dir, "..", "..", "..");
const lib = import.meta.dir;
const fn = scriptFunctions(join(root, "scripts", "e2e", "stage4b-sandbox-tree.sh"));
const fixture = join(lib, "testdata", "stage4b-tools-session.jsonl");
const session = readFileSync(fixture, "utf8");

interface Block {
  type: string;
  text?: string;
  id?: string;
  name?: string;
  arguments?: Record<string, unknown>;
}
interface Message {
  role: string;
  toolName?: string;
  toolCallId?: string;
  isError?: boolean;
  content: Block[];
}
// messages are the fixture's messages, read the way the library reads them, so each negative below
// can first show the session does hold the text or the tool name the helper refuses.
const messages = session
  .split("\n")
  .filter((line) => line !== "")
  .map((line) => JSON.parse(line) as { type: string; message?: Message })
  .flatMap((entry) => (entry.type === "message" && entry.message ? [entry.message] : []));
const results = messages.filter((m) => m.role === "toolResult");
const calls = messages
  .filter((m) => m.role === "assistant")
  .flatMap((m) => m.content.filter((c) => c.type === "toolCall"));
const resultText = (m: Message) =>
  m.content
    .filter((c) => c.type === "text")
    .map((c) => c.text ?? "")
    .join("\n");
// errorsCarrying is the isError of each result whose text carries TEXT: [true] when the session
// holds the text in one result, an error.
const errorsCarrying = (text: string) =>
  results.filter((r) => resultText(r).includes(text)).map((r) => r.isError);

// helper runs one of the script's session helpers as the script calls it, for the planner's claim
// on LEGSMOKE-1 with the fixture as that claim's session, and returns its exit code: 0 is yes and 1
// is no; any other exit is jq or the harness failing, and throws with its stderr.
function helper(name: string, ...args: string[]): number {
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(root)}
claim_session_text() { cat -- "$SESSION"; }
${fn(name)}
"$@"`,
      "stage4b-tools",
      name,
      "LEGSMOKE-1",
      "planner",
      ...args,
    ],
    { env: { ...process.env, SESSION: fixture } }
  );
  if (ran.exitCode !== 0 && ran.exitCode !== 1)
    throw new Error(`${name} exited ${ran.exitCode}: ${ran.stderr}`);
  return ran.exitCode;
}
const said = (tool: string, text: string) => helper("tool_result_said", tool, text);
const ran = (tool: string) => helper("tool_ran", tool);
// jqOver evaluates PROGRAM from the library over a session's text, as the script's jq does.
const jqOver = (text: string, program: string): unknown =>
  JSON.parse(runJq(["-R", "-s", "-L", lib, "-c", `include "stage4b-tools"; ${program}`], text));

describe("tool_result_said: a tool's non-error result carries the text", () => {
  test("eval returned 42, by name", () => {
    expect(said("eval", "42")).toBe(0);
  });

  test("the bash that ran without error returned the bot's login", () => {
    expect(said("bash", "legion-implementer[bot]")).toBe(0);
  });

  test("web_search, read, edit and task each returned, by name", () => {
    expect(said("web_search", "https://github.com/")).toBe(0);
    expect(said("read", "Symbol-aware code intelligence")).toBe(0);
    expect(said("edit", "edited rehearsal-578")).toBe(0);
    expect(said("task", "legion-implementer[bot]")).toBe(0);
  });

  test("a write to xd://lsp is lsp's result, by its call's path, and a write's too", () => {
    expect(said("lsp", "Language servers: gopls")).toBe(0);
    expect(said("lsp", "func liveDetail")).toBe(0);
    expect(said("write", "Language servers")).toBe(0);
  });
});

describe("tool_result_said: an error result never counts", () => {
  test("the Go version came back only from the bash that exited 1", () => {
    expect(errorsCarrying("go version go1.26.8")).toEqual([true]);
    expect(said("bash", "go version go1.26.8")).toBe(1);
  });

  test("the browser's refusal is in the session, in an eval that errored", () => {
    expect(errorsCarrying("Shared browser daemon unavailable")).toEqual([true]);
    expect(said("eval", "Shared browser daemon unavailable")).toBe(1);
  });
});

describe("a tool that never ran", () => {
  test("notebook has no call and no result, so it neither ran nor said what others did", () => {
    expect(calls.some((c) => c.name === "notebook")).toBe(false);
    expect(results.some((r) => r.toolName === "notebook")).toBe(false);
    expect(ran("notebook")).toBe(1);
    expect(said("notebook", "42")).toBe(1);
  });
});

describe("a text the model said that no tool returned", () => {
  // saidByModel is what assistant_said reads of the session: each assistant turn's reply text and
  // each tool call's arguments as jq's tostring writes them. unreturned is a word of it, a
  // capitalised word of six letters or more so it is a word and not a fragment of an escape, that
  // no tool result carries.
  const saidByModel = messages
    .filter((m) => m.role === "assistant")
    .flatMap((m) =>
      m.content.map((c) =>
        c.type === "text"
          ? (c.text ?? "")
          : c.type === "toolCall"
            ? JSON.stringify(c.arguments)
            : ""
      )
    )
    .join("\n");
  const resultTexts = results.map(resultText);
  const unreturned = saidByModel
    .match(/\b[A-Z][a-z]{5,}\b/g)
    ?.find((word) => !resultTexts.some((text) => text.includes(word)));

  test("assistant_said accepts it and tool_result_said refuses it", () => {
    expect(unreturned).toBeDefined();
    const word = unreturned ?? "";
    expect(helper("assistant_said", word)).toBe(0);
    expect(said("eval", word)).toBe(1);
  });
});

describe("tool_ran", () => {
  test("eval ran: two of its three results are no error", () => {
    expect(ran("eval")).toBe(0);
  });
});

describe("the library", () => {
  test("tool_results pairs each result with its call: lsp's are the two xd://lsp writes", () => {
    // The read of xd://lsp, the device's documentation, is no call of lsp.
    const lspCalls = calls
      .filter((c) => c.name === "write" && c.arguments?.path === "xd://lsp")
      .map((c) => c.id);
    expect(lspCalls).toHaveLength(2);
    expect(calls.filter((c) => c.arguments?.path === "xd://lsp")).toHaveLength(3);
    expect(jqOver(session, '[tool_results("lsp")[] | .isError]')).toEqual([false, false]);
    expect(jqOver(session, '[tool_results("lsp")[] | .toolCallId]')).toEqual(lspCalls);
    expect(jqOver(session, '[tool_results("write")[] | .isError]')).toEqual([false, false, false]);
  });

  test("tool_result_texts keeps the non-error texts alone", () => {
    expect(jqOver(session, 'tool_result_texts("eval")')).toEqual(["42", "42"]);
  });

  test("a result whose call the session no longer holds is no tool's", () => {
    const truncated = `${session
      .split("\n")
      .filter((line) => line !== "" && !line.includes('"role":"assistant"'))
      .join("\n")}\n`;
    expect(truncated).toContain('"toolName":"eval"');
    expect(jqOver(truncated, 'tool_results("eval")')).toEqual([]);
    expect(jqOver(truncated, 'tool_ran("eval")')).toBe(false);
  });

  test("a result without isError is no error, and its image block contributes no text", () => {
    const text = [
      {
        type: "message",
        message: {
          role: "assistant",
          content: [
            { type: "toolCall", id: "toolu_1", name: "read", arguments: { path: "a.png" } },
          ],
        },
      },
      {
        type: "message",
        message: {
          role: "toolResult",
          toolName: "read",
          toolCallId: "toolu_1",
          content: [
            { type: "image", data: "iVBORw0KGgo=", mimeType: "image/png" },
            { type: "text", text: "a screenshot" },
          ],
        },
      },
    ]
      .map((line) => `${JSON.stringify(line)}\n`)
      .join("");
    expect(jqOver(text, 'tool_results("read")')).toEqual([
      { toolCallId: "toolu_1", isError: false, text: "a screenshot" },
    ]);
    expect(jqOver(text, 'tool_ran("read")')).toBe(true);
  });
});

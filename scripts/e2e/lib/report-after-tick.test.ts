import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// report_after_tick, taken from stage4b-sandbox-tree.sh by name and run against controller sessions
// written as Oh My Pi writes them: the daily-report checkpoint passes only when the controller's
// call that posted its report came on a turn a tick started while the controller was idle. The
// report is a `dispatch message` command the controller runs through bash, and Oh My Pi gives the
// model three ways to call bash, each of which must count: the bash tool itself, a write to its
// xd://bash device, and eval code that calls tool.bash(...).
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const dir = mkdtempSync(join(tmpdir(), "report-after-tick-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const report = "LEGSMOKE-472";
const body = "Legion daily report for 2026-10-06 (UTC). Running: LEGSMOKE-469. Slots: 0 free.";
type Entry = Record<string, unknown>;
type Call = Record<string, unknown>;
let ids = 0;
const callId = () => `toolu_${++ids}`;

const start: Entry = {
  type: "message",
  message: {
    role: "user",
    content: [{ type: "text", text: "Legion controller start: follow the start procedure now." }],
  },
};
const assistant = (stopReason: "toolUse" | "stop" | "error", ...calls: Call[]): Entry => ({
  type: "message",
  message: { role: "assistant", stopReason, content: [{ type: "text", text: "…" }, ...calls] },
});
const result = (text: string): Entry => ({
  type: "message",
  message: { role: "toolResult", content: [{ type: "text", text }] },
});
// tick is pi-envoy's delivery of the daemon's tick, idle or steered into a running turn alike.
const tick: Entry = {
  type: "custom_message",
  customType: "envoy-message",
  content: "envoy:\n  from: agent\n  summary: tick on LEGSMOKE\n  message:\n    kind: tick",
  display: true,
};
const bash: Call = {
  type: "toolCall",
  id: callId(),
  name: "bash",
  arguments: { command: "legion state --json" },
};
// The three surfaces of one bash call running `dispatch message` on ISSUE.
const command = (issue: string) =>
  `dispatch message --issue ${issue} --body-file - <<'EOF'\n${body}\nEOF`;
const viaTool = (issue: string): Call => ({
  type: "toolCall",
  id: callId(),
  name: "bash",
  arguments: { command: command(issue) },
});
const viaDevice = (issue: string): Call => ({
  type: "toolCall",
  id: callId(),
  name: "write",
  arguments: { path: "xd://bash", content: JSON.stringify({ command: command(issue) }), i: "Post" },
});
const viaEval = (issue: string): Call => ({
  type: "toolCall",
  id: callId(),
  name: "eval",
  arguments: {
    language: "js",
    title: "post daily report",
    code: `const r = await tool.bash({ command: ${JSON.stringify(command(issue))} });\nr;`,
  },
});
const surfaces = { tool: viaTool, device: viaDevice, eval: viaEval } as const;
const posted = result(`Posted message 2adbdb49 (dispatch://${report}/message/2adbdb49)`);

// onTickTurn: the start turn has a tick steered into it and ends; the next tick finds the
// controller idle and starts the turn that posts the report with CALL.
const onTickTurn = (call: Call, startTurn: Entry[] = []): Entry[] => [
  start,
  assistant("toolUse", bash),
  result("{}"),
  tick,
  ...startTurn,
  assistant("stop"),
  tick,
  assistant("toolUse", bash),
  result("{}"),
  assistant("toolUse", call),
  posted,
  assistant("stop"),
];
// inStartTurn: the report is posted with CALL in the start turn, after a tick was steered into it.
const inStartTurn = (call: Call): Entry[] => [
  start,
  assistant("toolUse", bash),
  result("{}"),
  tick,
  assistant("toolUse", call),
  posted,
  assistant("stop"),
  tick,
  assistant("stop"),
];

let sessions = 0;
function run(entries: Entry[]) {
  const agent = join(dir, `agent-${++sessions}`);
  const sessionDir = join(agent, "sessions", "-tmp-controller-state-controller-");
  mkdirSync(sessionDir, { recursive: true });
  writeFileSync(
    join(sessionDir, "2026-10-06T18-39-06-031Z_session.jsonl"),
    `${entries.map((e) => JSON.stringify(e)).join("\n")}\n`
  );
  const ran = Bun.spawnSync([
    "bash",
    "-c",
    `set -Eeuo pipefail
root=${JSON.stringify(join(import.meta.dir, "..", "..", ".."))}
profile_agent=${JSON.stringify(agent)} project=LEGSMOKE report=${report}
${fn("report_after_tick")}
report_after_tick
`,
  ]);
  return {
    code: ran.exitCode,
    stdout: ran.stdout.toString().trim(),
    stderr: ran.stderr.toString().trim(),
  };
}

const notOnTick =
  "the controller's first report message was not posted on a turn a tick started while it was idle";

describe("report_after_tick", () => {
  for (const [surface, call] of Object.entries(surfaces)) {
    test(`a report posted through ${surface} on the turn an idle tick started passes`, () => {
      const { code, stdout, stderr } = run(onTickTurn(call(report)));
      expect(stderr).toBe("");
      expect(stdout).toBe("");
      expect(code).toBe(0);
    });

    test(`a report posted through ${surface} in the start turn, a tick steered into it, fails`, () => {
      const { code, stdout } = run(inStartTurn(call(report)));
      expect(stdout).toBe(notOnTick);
      expect(code).toBe(1);
    });
  }

  test("a start turn that ends in an unretried error is no idle turn for the next tick", () => {
    const entries = onTickTurn(viaEval(report));
    // entries[4] is the start turn's last message, its stop.
    entries[4] = assistant("error");
    const { code, stdout } = run(entries);
    expect(stdout).toBe(notOnTick);
    expect(code).toBe(1);
  });

  test("a skill file that quotes the command in the start turn is not the report's call", () => {
    const skill = result(`Post the report with ${command(report)}.`);
    const { code, stdout } = run(
      onTickTurn(viaDevice(report), [assistant("toolUse", bash), skill])
    );
    expect(stdout).toBe("");
    expect(code).toBe(0);
  });

  test("a message the start turn posts on another issue is not the report's call", () => {
    for (const call of Object.values(surfaces)) {
      const { code, stdout } = run(
        onTickTurn(viaEval(report), [assistant("toolUse", call("LEGSMOKE-469")), posted])
      );
      expect(stdout).toBe("");
      expect(code).toBe(0);
    }
  });

  test("a session with no call posting on the report issue fails naming what it looked for", () => {
    const { code, stdout } = run(onTickTurn(viaEval("LEGSMOKE-469")));
    expect(stdout).toBe(
      `no session of the controller holds a call running dispatch message --issue ${report}: the bash tool, a write to xd://bash, or eval code whose string literal runs it`
    );
    expect(code).toBe(1);
  });
});

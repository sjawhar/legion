import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  assistant,
  bashDevice,
  bashEval,
  bashEvalWrite,
  bashTool,
  type Call,
  type Entry,
  evalCall,
  jsonl,
  toolResult,
  turnEnd,
} from "./omp-session-fixtures";
import { scriptFunctions } from "./script-functions";

// report_after_tick, taken from stage4b-sandbox-tree.sh by name and run against controller sessions
// written as Oh My Pi writes them (omp-session-fixtures.ts): the daily-report checkpoint passes
// only when the controller's call that posted its report came on a turn a tick started while the
// controller was idle. The report is a `dispatch message` command the controller runs through
// bash, and each of the four ways Oh My Pi gives the model to call bash must count: the bash tool,
// a write to xd://bash, eval code calling tool.bash(...), and eval code calling the generic
// tool.write(...) naming xd://bash.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const dir = mkdtempSync(join(tmpdir(), "report-after-tick-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const report = "LEGSMOKE-472";
const body = "Legion daily report for 2026-10-06 (UTC). Running: LEGSMOKE-469. Slots: 0 free.";

const start: Entry = {
  type: "message",
  message: {
    role: "user",
    content: [{ type: "text", text: "Legion controller start: follow the start procedure now." }],
  },
};
// tick is pi-envoy's delivery of the daemon's tick, idle or steered into a running turn alike.
const tick: Entry = {
  type: "custom_message",
  customType: "envoy-message",
  content: "envoy:\n  from: agent\n  summary: tick on LEGSMOKE\n  message:\n    kind: tick",
  display: true,
};
const bash = bashTool("legion state --json");
// The four surfaces of one bash call running `dispatch message` on ISSUE.
const command = (issue: string) =>
  `dispatch message --issue ${issue} --body-file - <<'EOF'\n${body}\nEOF`;
const viaTool = (issue: string): Call => bashTool(command(issue));
const viaDevice = (issue: string): Call => bashDevice(command(issue));
const viaEval = (issue: string): Call => bashEval(command(issue));
const viaEvalWrite = (issue: string): Call => bashEvalWrite(command(issue));
const surfaces = {
  tool: viaTool,
  device: viaDevice,
  eval: viaEval,
  evalWrite: viaEvalWrite,
} as const;
const posted = toolResult(`Posted message 2adbdb49 (dispatch://${report}/message/2adbdb49)`);

// onTickTurn: the start turn has a tick steered into it and ends; the next tick finds the
// controller idle and starts the turn that posts the report with CALL.
const onTickTurn = (call: Call, startTurn: Entry[] = []): Entry[] => [
  start,
  assistant(bash),
  toolResult("{}"),
  tick,
  ...startTurn,
  turnEnd("stop"),
  tick,
  assistant(bash),
  toolResult("{}"),
  assistant(call),
  posted,
  turnEnd("stop"),
];
// inStartTurn: the report is posted with CALL in the start turn, after a tick was steered into it.
const inStartTurn = (call: Call): Entry[] => [
  start,
  assistant(bash),
  toolResult("{}"),
  tick,
  assistant(call),
  posted,
  turnEnd("stop"),
  tick,
  turnEnd("stop"),
];

let sessions = 0;
function run(entries: Entry[]) {
  const agent = join(dir, `agent-${++sessions}`);
  const sessionDir = join(agent, "sessions", "-tmp-controller-state-controller-");
  mkdirSync(sessionDir, { recursive: true });
  writeFileSync(join(sessionDir, "2026-10-06T18-39-06-031Z_session.jsonl"), jsonl(entries));
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
    entries[4] = turnEnd("error");
    const { code, stdout } = run(entries);
    expect(stdout).toBe(notOnTick);
    expect(code).toBe(1);
  });

  test("a skill file that quotes the command in the start turn is not the report's call", () => {
    const skill = toolResult(`Post the report with ${command(report)}.`);
    const { code, stdout } = run(onTickTurn(viaDevice(report), [assistant(bash), skill]));
    expect(stdout).toBe("");
    expect(code).toBe(0);
  });

  test("a message the start turn posts on another issue is not the report's call", () => {
    for (const call of Object.values(surfaces)) {
      const { code, stdout } = run(
        onTickTurn(viaEval(report), [assistant(call("LEGSMOKE-469")), posted])
      );
      expect(stdout).toBe("");
      expect(code).toBe(0);
    }
  });

  // The report's command as a string literal of eval code, so the downstream runs_dispatch and
  // --issue checks would take a probe below: only calls("bash") deciding the call decides it.
  const reportCommand = JSON.stringify(command(report));
  const missing = `no session of the controller holds a call running dispatch message --issue ${report}: the bash tool, a write to xd://bash, or eval code calling tool.bash or tool.write whose string literal runs it`;
  const pyEval = (code: string): Call => ({
    ...evalCall(code),
    arguments: { language: "py", code },
  });

  test("a report posted through eval calling tool.write with content built before path (JS) passes", () => {
    const code = `const args = { command: ${reportCommand} };\nconst r = await tool.write({ content: JSON.stringify(args), path: "xd://bash", i: "Run" });\nr;`;
    const { code: exitCode, stdout, stderr } = run(onTickTurn(evalCall(code)));
    expect(stderr).toBe("");
    expect(stdout).toBe("");
    expect(exitCode).toBe(0);
  });

  test("a report posted through eval calling tool.write with content built before path (Python) passes", () => {
    const code = `r = await tool.write({"content": json.dumps({"command": ${reportCommand}}), "path": "xd://bash", "i": "Run"})\nr`;
    const { code: exitCode, stdout, stderr } = run(onTickTurn(pyEval(code)));
    expect(stderr).toBe("");
    expect(stdout).toBe("");
    expect(exitCode).toBe(0);
  });

  test("a block comment naming the device inside another device's write is not the report's call", () => {
    const code = `await tool.write({ path: "xd://another_device", /* xd://bash */ content: ${reportCommand} });`;
    const { code: exitCode, stdout } = run(onTickTurn(evalCall(code)));
    expect(stdout).toBe(missing);
    expect(exitCode).toBe(1);
  });

  test("a file write whose content mentions the device as prose is not the report's call", () => {
    const code = `await tool.write({ path: "./notes.md", content: ${reportCommand} + " See xd://bash, the device" });`;
    const { code: exitCode, stdout } = run(onTickTurn(evalCall(code)));
    expect(stdout).toBe(missing);
    expect(exitCode).toBe(1);
  });

  test("a Python kwargs file write, then a print naming the device, is not the report's call", () => {
    const code = `tool.write(path="./notes.md", content=${reportCommand})\nprint("the device is xd://bash")`;
    const { code: exitCode, stdout } = run(onTickTurn(pyEval(code)));
    expect(stdout).toBe(missing);
    expect(exitCode).toBe(1);
  });

  test("a write to a device with bash's name as a prefix is not the report's call", () => {
    const code = `await tool.write({ path: "xd://bash.md", content: ${reportCommand} });`;
    const { code: exitCode, stdout } = run(onTickTurn(evalCall(code)));
    expect(stdout).toBe(missing);
    expect(exitCode).toBe(1);
  });

  test("eval code that writes to another device while a comment mentions xd://bash is not the report's call", () => {
    const code = `// writing another_device, not xd://bash here\nawait tool.write({ path: "xd://another_device", content: ${reportCommand} });`;
    const { code: exitCode, stdout } = run(onTickTurn(evalCall(code)));
    expect(stdout).toBe(missing);
    expect(exitCode).toBe(1);
  });

  test("a session with no call posting on the report issue fails naming what it looked for", () => {
    const { code, stdout } = run(onTickTurn(viaEval("LEGSMOKE-469")));
    expect(stdout).toBe(missing);
    expect(code).toBe(1);
  });
});

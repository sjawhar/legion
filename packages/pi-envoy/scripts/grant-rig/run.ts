#!/usr/bin/env bun
/**
 * Drives a headless phase worker (`omp --mode rpc`) through 30+ shell commands against the
 * stand-in daemon (`daemon-standin.ts`) and checks, per shell command, how the one-time
 * credential (`LEGION_GRANT`) reached the shell:
 *
 *   A. transcript — the model-visible `arguments.command` of every bash tool call holds no
 *      `export LEGION_GRANT=` line, and `arguments.env.LEGION_GRANT` carries the grant;
 *   B. stand-in log — exactly one `/legion/v1/grants` mint per bash call, and every
 *      `/git-credential`, `/gh-token`, `/phase/complete` redemption answered 200 with a minted id;
 *   C. `seen-grants.log` — the grant each `record-grant` command actually ran under equals the
 *      grant minted for that command;
 *   D. tool results — `which gh` resolves to the worker shim, `gh --version` prints a version,
 *      the `legion …` commands print `exit=0` and never `Unable to redeem`;
 *   E. OMP log — one `extension instance loaded` per session instance, and exactly one
 *      `legion tool_call hook` line per bash tool call.
 *
 * The same `analyze` subcommand scores a transcript produced by the interactive (tmux) leg, so
 * both legs share one counting script. See README.md for the layout `setup.sh` creates.
 *
 * Subcommands:
 *   bun run.ts prompt  [--short]
 *   bun run.ts drive   --rig <dir> --port <n> --omp <binary> [--profile l12rig] [--short]
 *                      [--label <name>] [--no-secrets]
 *   bun run.ts analyze --rig <dir> --transcript <file> --standin-log <file> --omp-log <file>
 *                      [--label <name>]
 */
import { randomUUID } from "node:crypto";
import { appendFile, mkdir, readdir, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { parseArgs } from "node:util";

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
/** One credential line as the unfixed hook wrote it (single-quoted), or as a model imitation
 * might (double-quoted, bare, or a literal placeholder). */
const GRANT_TEXT_LINE = /^export LEGION_GRANT=(?:'([^']*)'|"([^"]*)"|(\S*))/gm;
const RUN_DEADLINE_MS = 20 * 60_000;
const EXIT_GRACE_MS = 15_000;

interface Step {
  readonly n: number;
  readonly kind: "bash" | "task";
  readonly command?: string;
}

function recordStep(n: number): Step {
  return { n, kind: "bash", command: `record-grant; echo step-${n}` };
}

/** The full headless leg: 31 bash calls and 3 `task` spawns. `short` is the terminal leg: 8
 * bash calls and 1 spawn. Neither prompt names the credential variable or the word `export`.
 * The credential step writes to stdout on purpose: a `> file` redirection trips the profile's
 * bash interceptor ("use the write tool"), and the stand-in's token is a placeholder anyway. */
function buildSteps(short: boolean): Step[] {
  const credential = (n: number): Step => ({
    n,
    kind: "bash",
    command: `printf 'protocol=https\\nhost=github.com\\n' | legion credential get; echo exit=$?`,
  });
  const handoff = (n: number): Step => ({
    n,
    kind: "bash",
    command: "legion handoff complete --summary 'rig run complete'; echo exit=$?",
  });
  if (short) {
    return [
      recordStep(1),
      recordStep(2),
      recordStep(3),
      recordStep(4),
      { n: 5, kind: "task" },
      recordStep(6),
      recordStep(7),
      credential(8),
      handoff(9),
    ];
  }
  const steps: Step[] = [];
  for (let n = 1; n <= 5; n++) steps.push(recordStep(n));
  steps.push({ n: 6, kind: "task" });
  for (let n = 7; n <= 15; n++) steps.push(recordStep(n));
  steps.push({ n: 16, kind: "task" });
  steps.push({ n: 17, kind: "bash", command: "which gh" });
  steps.push({ n: 18, kind: "bash", command: "gh --version" });
  for (let n = 19; n <= 25; n++) steps.push(recordStep(n));
  steps.push({ n: 26, kind: "task" });
  for (let n = 27; n <= 29; n++) steps.push(recordStep(n));
  steps.push(credential(30));
  steps.push({ n: 31, kind: "bash", command: "legion gh -- --version; echo exit=$?" });
  steps.push(credential(32));
  steps.push(recordStep(33));
  steps.push(handoff(34));
  return steps;
}

function buildPrompt(steps: Step[]): string {
  const lines = steps.map((step) =>
    step.kind === "task" ? `${step.n}. TASK` : `${step.n}. ${step.command}`
  );
  return [
    'You are a test worker. Run the following numbered commands, one per bash tool call, in order, each copied exactly as written with nothing added before or after it. Where a step says TASK, use the task tool with agent scout and the instruction "Reply with the single word ok.", wait for it, then continue. Use no other tools. After the last step reply with the single word done.',
    "",
    ...lines,
  ].join("\n");
}

// ---------------------------------------------------------------------------------------------
// RPC driving
// ---------------------------------------------------------------------------------------------

interface RpcChunkSequence {
  readonly chunkId: string;
  readonly count: number;
  readonly chunks: Buffer[];
}

/** Reassembles protocol v2 `rpc_chunk` lines into one logical frame; anything else parses as
 * is. Validation is the daemon's job (`worker-rpc.ts`) — a rig only needs the payload. */
function createFrameReader(onFrame: (frame: Record<string, unknown>) => void): {
  readonly push: (chunk: Uint8Array) => void;
} {
  const decoder = new TextDecoder();
  let buffered = "";
  let pending: RpcChunkSequence | undefined;
  const handleLine = (line: string): void => {
    if (line.trim().length === 0) return;
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      return;
    }
    if (typeof parsed !== "object" || parsed === null) return;
    const frame = parsed as Record<string, unknown>;
    if (frame.type !== "rpc_chunk") {
      onFrame(frame);
      return;
    }
    const { chunkId, index, count, data } = frame;
    if (typeof chunkId !== "string" || typeof index !== "number" || typeof count !== "number") {
      return;
    }
    if (!pending || pending.chunkId !== chunkId) pending = { chunkId, count, chunks: [] };
    pending.chunks[index] = Buffer.from(String(data), "base64");
    if (pending.chunks.filter((chunk) => chunk !== undefined).length < count) return;
    const whole = Buffer.concat(pending.chunks).toString("utf8");
    pending = undefined;
    try {
      const reassembled = JSON.parse(whole);
      if (typeof reassembled === "object" && reassembled !== null) {
        onFrame(reassembled as Record<string, unknown>);
      }
    } catch {
      // A rig tolerates a malformed oversized frame: the transcript file is the source of truth.
    }
  };
  return {
    push(chunk) {
      buffered += decoder.decode(chunk, { stream: true });
      let newline = buffered.indexOf("\n");
      while (newline !== -1) {
        handleLine(buffered.slice(0, newline));
        buffered = buffered.slice(newline + 1);
        newline = buffered.indexOf("\n");
      }
    },
  };
}

interface WorkerLaunch {
  readonly rig: string;
  readonly port: number;
  readonly omp: string;
  readonly profile: string;
  readonly useSecrets: boolean;
}

/** The environment the Legion daemon gives a phase-worker pane, pointed at the scratch state
 * directory and the stand-in daemon. Every `LEGION_*` and `DISPATCH_*` value inherited from the
 * shell this rig runs in (itself possibly a Legion pane) is dropped first. */
function workerEnvironment(launch: WorkerLaunch): Record<string, string> {
  const env: Record<string, string> = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (value === undefined) continue;
    if (key.startsWith("LEGION_") || key.startsWith("DISPATCH_")) continue;
    if (key === "OMP_SESSION_ID" || key === "TMUX" || key === "TMUX_PANE") continue;
    env[key] = value;
  }
  const home = os.homedir();
  const agentDir = path.join(home, ".omp", "profiles", launch.profile, "agent");
  Object.assign(env, {
    OMP_PROFILE: launch.profile,
    PI_PROFILE: launch.profile,
    PI_CODING_AGENT_DIR: agentDir,
    PI_NOTIFICATIONS: "off",
    PI_NO_TITLE: "1",
    LEGION_ROLE: "implementer",
    LEGION_TREE: "RIG-1",
    LEGION_ISSUE: "RIG-1",
    LEGION_GENERATION: "1",
    LEGION_PROJECT: "l12rig",
    LEGION_BOOT_TOKEN_FILE: path.join(launch.rig, "state", "secrets", "boot"),
    LEGION_DAEMON_URL: `http://127.0.0.1:${launch.port}`,
    LEGION_STATE_DIR: path.join(launch.rig, "state"),
    LEGION_WORKSPACE: path.join(launch.rig, "ws"),
    PATH: `${path.join(launch.rig, "state", "bin")}${path.delimiter}${env.PATH ?? ""}`,
  });
  return env;
}

/** The headless leg runs `omp --mode rpc`; the terminal leg runs the interactive `omp`. */
function launchArgv(launch: WorkerLaunch, mode: "rpc" | "tui"): string[] {
  const omp = mode === "rpc" ? [launch.omp, "--mode", "rpc"] : [launch.omp];
  return launch.useSecrets
    ? ["secrets", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY", "--", ...omp]
    : omp;
}

interface DriveResult {
  readonly runDir: string;
  readonly pid: number;
  readonly sessionFile: string | undefined;
  readonly exitCode: number | null;
  readonly endedBy: "agent_end" | "deadline" | "exit";
}

async function drive(launch: WorkerLaunch, prompt: string, label: string): Promise<DriveResult> {
  const stamp = new Date().toISOString().replaceAll(/[:.]/g, "-");
  const runDir = path.join(launch.rig, "runs", `${label}-${stamp}`);
  await mkdir(runDir, { recursive: true });
  const eventsFile = path.join(runDir, "events.jsonl");
  const stderrFile = path.join(runDir, "stderr.log");
  await writeFile(path.join(runDir, "prompt.txt"), `${prompt}\n`);

  const child = Bun.spawn(launchArgv(launch, "rpc"), {
    cwd: path.join(launch.rig, "ws"),
    env: workerEnvironment(launch),
    stdin: "pipe",
    stdout: "pipe",
    stderr: Bun.file(stderrFile),
  });
  const pid = child.pid;
  console.log(`[rig] omp pid ${pid}; events -> ${eventsFile}`);

  const write = (frame: Record<string, unknown>): void => {
    child.stdin.write(`${JSON.stringify(frame)}\n`);
    child.stdin.flush();
  };

  const negotiateId = randomUUID();
  const promptId = randomUUID();
  const stateId = randomUUID();
  const agentEnd = Promise.withResolvers<void>();
  const stateAnswer = Promise.withResolvers<Record<string, unknown> | undefined>();
  let sessionFile: string | undefined;
  let bashCalls = 0;

  const reader = createFrameReader((frame) => {
    void appendFile(eventsFile, `${JSON.stringify(frame)}\n`);
    switch (frame.type) {
      case "ready":
        write({ id: negotiateId, type: "negotiate_protocol", protocolVersion: 2 });
        return;
      case "response":
        if (frame.id === negotiateId) {
          if (frame.success !== true) {
            console.error("[rig] protocol negotiation failed", frame);
            agentEnd.resolve();
            return;
          }
          write({ id: promptId, type: "prompt", message: prompt });
          return;
        }
        if (frame.id === promptId && frame.success !== true) {
          console.error("[rig] prompt rejected", frame);
          agentEnd.resolve();
          return;
        }
        if (frame.id === stateId) {
          const data = typeof frame.data === "object" && frame.data !== null ? frame.data : {};
          const file = (data as Record<string, unknown>).sessionFile;
          if (typeof file === "string") sessionFile = file;
          stateAnswer.resolve(data as Record<string, unknown>);
        }
        return;
      case "tool_execution_start":
        if (frame.toolName === "bash") {
          bashCalls++;
          console.log(`[rig] bash call ${bashCalls}`);
        }
        return;
      case "extension_ui_request": {
        // A worker pane runs its tools unprompted; answer any confirm the same way and cancel
        // everything else so the run never blocks on a dialog.
        const id = frame.id;
        if (typeof id !== "string") return;
        if (frame.method === "confirm")
          write({ type: "extension_ui_response", id, confirmed: true });
        else if (frame.method === "select" || frame.method === "input" || frame.method === "editor")
          write({ type: "extension_ui_response", id, cancelled: true });
        return;
      }
      case "agent_end":
        agentEnd.resolve();
        return;
      default:
        return;
    }
  });

  const pump = (async () => {
    for await (const chunk of child.stdout as ReadableStream<Uint8Array>) reader.push(chunk);
  })();

  let endedBy: DriveResult["endedBy"] = "agent_end";
  const deadline = Bun.sleep(RUN_DEADLINE_MS).then(() => "deadline" as const);
  const exited = child.exited.then(() => "exit" as const);
  const outcome = await Promise.race([
    agentEnd.promise.then(() => "agent_end" as const),
    deadline,
    exited,
  ]);
  endedBy = outcome;
  if (outcome === "deadline") console.error("[rig] run deadline reached; stopping the worker");

  if (outcome !== "exit") {
    write({ id: stateId, type: "get_state" });
    await Promise.race([stateAnswer.promise, Bun.sleep(5_000)]);
    // No shutdown command exists on OMP's stdio RPC: closing stdin ends the process.
    await child.stdin.end();
    const closed = await Promise.race([
      child.exited,
      Bun.sleep(EXIT_GRACE_MS).then(() => undefined),
    ]);
    if (closed === undefined) {
      console.error("[rig] worker did not exit after stdin closed; killing it");
      child.kill();
      await child.exited;
    }
  }
  await pump.catch(() => undefined);
  return { runDir, pid, sessionFile, exitCode: child.exitCode, endedBy };
}

// ---------------------------------------------------------------------------------------------
// Analysis
// ---------------------------------------------------------------------------------------------

interface BashCall {
  readonly id: string;
  readonly command: string;
  readonly envGrant: string | undefined;
  readonly textIds: string[];
  result?: { readonly isError: boolean; readonly text: string };
}

interface StandinLine {
  readonly at: string;
  readonly path: string;
  readonly status: number;
  readonly grantId?: string;
  readonly mintedGrantId?: string;
}

interface HookLine {
  readonly instance: string;
  readonly toolCallId: string;
  readonly toolName: string;
}

async function readJsonLines(file: string): Promise<Record<string, unknown>[]> {
  const text = await Bun.file(file).text();
  const lines: Record<string, unknown>[] = [];
  for (const line of text.split("\n")) {
    if (line.trim().length === 0) continue;
    try {
      const parsed = JSON.parse(line);
      if (typeof parsed === "object" && parsed !== null) lines.push(parsed);
    } catch {
      // A transcript can hold a partial trailing line while the session is still writing.
    }
  }
  return lines;
}

function textIdsIn(command: string): string[] {
  const ids: string[] = [];
  for (const match of command.matchAll(GRANT_TEXT_LINE))
    ids.push(match[1] ?? match[2] ?? match[3] ?? "");
  return ids;
}

async function readTranscript(file: string): Promise<BashCall[]> {
  const calls: BashCall[] = [];
  const byId = new Map<string, BashCall>();
  for (const entry of await readJsonLines(file)) {
    if (entry.type !== "message") continue;
    const message = entry.message as Record<string, unknown> | undefined;
    if (!message) continue;
    if (message.role === "assistant" && Array.isArray(message.content)) {
      for (const part of message.content as Record<string, unknown>[]) {
        if (part.type !== "toolCall" || part.name !== "bash") continue;
        const args = (part.arguments ?? {}) as Record<string, unknown>;
        const command = typeof args.command === "string" ? args.command : "";
        const env =
          typeof args.env === "object" && args.env !== null
            ? (args.env as Record<string, unknown>)
            : undefined;
        const call: BashCall = {
          id: String(part.id),
          command,
          envGrant: typeof env?.LEGION_GRANT === "string" ? env.LEGION_GRANT : undefined,
          textIds: textIdsIn(command),
        };
        calls.push(call);
        byId.set(call.id, call);
      }
    } else if (message.role === "toolResult") {
      const call = byId.get(String(message.toolCallId));
      if (!call) continue;
      const content = Array.isArray(message.content)
        ? (message.content as Record<string, unknown>[])
        : [];
      call.result = {
        isError: message.isError === true,
        text: content
          .map((part) => (typeof part.text === "string" ? part.text : ""))
          .join("\n")
          .trim(),
      };
    }
  }
  return calls;
}

async function readOmpLog(
  file: string
): Promise<{ readonly instances: Set<string>; readonly hooks: HookLine[] }> {
  const instances = new Set<string>();
  const hooks: HookLine[] = [];
  for (const entry of await readJsonLines(file)) {
    if (entry.message === "extension instance loaded" && typeof entry.instance === "string") {
      instances.add(entry.instance);
    } else if (entry.message === "legion tool_call hook") {
      hooks.push({
        instance: String(entry.instance),
        toolCallId: String(entry.toolCallId),
        toolName: String(entry.toolName),
      });
    }
  }
  return { instances, hooks };
}

interface Analysis {
  readonly label: string;
  readonly bashCalls: number;
  readonly mints: number;
  readonly rows: string[];
  readonly redemptions: string[];
  readonly executed: string[];
  readonly results: string[];
  readonly instances: number;
  readonly hookIssues: string[];
  readonly verdict: string[];
}

function classifyExtra(
  id: string,
  earlier: Set<string>,
  minted: Set<string>,
  ownMint: string | undefined
): string {
  if (id === ownMint) return "own-mint";
  if (!UUID_V4.test(id)) return "non-v4";
  if (earlier.has(id)) return "copy";
  if (minted.has(id)) return "earlier-mint";
  return "unminted";
}

async function analyze(input: {
  readonly label: string;
  readonly rig: string;
  readonly transcript: string;
  readonly standinLog: string;
  readonly ompLog: string;
}): Promise<Analysis> {
  const calls = await readTranscript(input.transcript);
  const standin = (await readJsonLines(input.standinLog)) as unknown as StandinLine[];
  const { instances, hooks } = await readOmpLog(input.ompLog);
  const seenGrants = (
    await Bun.file(path.join(input.rig, "seen-grants.log"))
      .text()
      .catch(() => "")
  )
    .split("\n")
    .filter((line) => line.length > 0);

  const mintedInOrder = standin
    .filter((line) => line.path === "/legion/v1/grants" && line.status === 200)
    .map((line) => line.mintedGrantId ?? "");
  const minted = new Set(mintedInOrder);
  const oneToOne = mintedInOrder.length === calls.length;

  const hooksByCall = new Map<string, HookLine[]>();
  for (const hook of hooks) {
    const list = hooksByCall.get(hook.toolCallId) ?? [];
    list.push(hook);
    hooksByCall.set(hook.toolCallId, list);
  }

  const rows: string[] = [];
  const executed: string[] = [];
  const results: string[] = [];
  const hookIssues: string[] = [];
  const earlier = new Set<string>();
  let seenIndex = 0;
  let textFree = true;
  let envAll = true;
  let executedAll = true;
  let hooksOne = true;
  const parentInstances = new Set<string>();

  calls.forEach((call, k) => {
    const ownMint = oneToOne ? mintedInOrder[k] : undefined;
    const callHooks = hooksByCall.get(call.id) ?? [];
    const H = callHooks.length;
    const distinct = new Set(callHooks.map((hook) => hook.instance));
    for (const instance of distinct) parentInstances.add(instance);
    if (H !== 1) {
      hooksOne = false;
      hookIssues.push(
        `call ${k + 1} (${call.id}): ${H} hook lines, instances ${[...distinct].join(",")}`
      );
    }
    const T = call.textIds.length;
    if (T > 0) textFree = false;
    if (call.envGrant === undefined || !UUID_V4.test(call.envGrant)) envAll = false;
    const envState =
      call.envGrant === undefined
        ? "-"
        : ownMint !== undefined && call.envGrant === ownMint
          ? "env=own-mint"
          : `env=${classifyExtra(call.envGrant, earlier, minted, ownMint)}`;
    const extras = call.textIds.map((id, i) =>
      i === 0 && id === ownMint ? "hook" : classifyExtra(id, earlier, minted, ownMint)
    );
    rows.push(
      `${String(k + 1).padStart(2)}  H=${H}/${distinct.size}  G=${oneToOne ? 1 : "?"}  T=${T}  ${envState}  ${extras.length > 0 ? `X=[${extras.join(",")}]` : ""}`
    );
    for (const id of call.textIds) earlier.add(id);

    if (call.command.includes("record-grant;")) {
      const seen = seenGrants[seenIndex++];
      const ok = ownMint !== undefined && seen === ownMint;
      if (!ok) executedAll = false;
      executed.push(
        `call ${k + 1}: ran under ${seen ?? "(nothing recorded)"} ${ok ? "== minted" : `!= minted ${ownMint ?? "?"}`}`
      );
    }
    const probe = [
      "which gh",
      "gh --version",
      "legion credential get",
      "legion gh --",
      "legion handoff complete",
    ].find((needle) => call.command.includes(needle));
    if (probe) {
      const text = call.result?.text ?? "(no result)";
      results.push(
        `call ${k + 1} [${probe}] isError=${call.result?.isError ?? "?"}: ${text.replaceAll("\n", " | ").slice(0, 200)}`
      );
    }
  });

  const redemptions = standin
    .filter((line) =>
      ["/legion/v1/git-credential", "/legion/v1/gh-token", "/legion/v1/phase/complete"].includes(
        line.path
      )
    )
    .map(
      (line) =>
        `${line.at} ${line.path} -> ${line.status} grant ${line.grantId ?? "?"} ${
          line.grantId !== undefined && minted.has(line.grantId) ? "(minted)" : "(never minted)"
        }`
    );
  const redemptionLines = standin.filter((line) =>
    ["/legion/v1/git-credential", "/legion/v1/gh-token", "/legion/v1/phase/complete"].includes(
      line.path
    )
  );
  const redemptionsAll200 =
    redemptionLines.length > 0 && redemptionLines.every((line) => line.status === 200);
  const probeCalls = calls.filter((call) =>
    /legion (credential get|gh --|handoff complete)/.test(call.command)
  );
  const resultsClean =
    probeCalls.length > 0 &&
    probeCalls.every(
      (call) =>
        call.result?.text.includes("exit=0") && !call.result.text.includes("Unable to redeem")
    );
  // A run with no bash calls proves nothing: every verdict below needs calls to judge.
  const ran = calls.length > 0;

  if (parentInstances.size > 1) {
    hookIssues.push(
      `parent bash calls logged under ${parentInstances.size} instances: ${[...parentInstances].join(",")}`
    );
  }

  const verdict = [
    `A. command text free of credential lines: ${ran && textFree ? "PASS" : "FAIL"}; env carries a uuid grant on every call: ${ran && envAll ? "PASS" : "FAIL"}`,
    `B. one mint per bash call: ${ran && oneToOne ? "PASS" : `FAIL (${mintedInOrder.length} mints, ${calls.length} calls)`}; redemptions all 200: ${redemptionsAll200 ? "PASS" : "FAIL"}`,
    `C. every record-grant ran under its own mint: ${executedAll && executed.length > 0 ? "PASS" : "FAIL"}`,
    `D. legion commands exit=0 and never 'Unable to redeem': ${resultsClean ? "PASS" : "FAIL"}`,
    `E. exactly one hook line per bash call, one parent instance: ${hooksOne && parentInstances.size === 1 ? "PASS" : "FAIL"} (${instances.size} legion extension instances: the parent plus one per task spawn)`,
  ];

  return {
    label: input.label,
    bashCalls: calls.length,
    mints: mintedInOrder.length,
    rows,
    redemptions,
    executed,
    results,
    instances: instances.size,
    hookIssues,
    verdict,
  };
}

function renderAnalysis(analysis: Analysis): string {
  return [
    `== grant rig: ${analysis.label} ==`,
    `bash calls: ${analysis.bashCalls}; grant mints: ${analysis.mints}; legion extension instances (parent + task spawns): ${analysis.instances}`,
    "",
    "per call  H=hook lines/distinct instances  G=mints  T=credential lines in command text  env  X=classification of text ids",
    ...analysis.rows,
    "",
    "redemptions (stand-in log):",
    ...(analysis.redemptions.length > 0 ? analysis.redemptions : ["(none)"]),
    "",
    "executed grant (seen-grants.log vs mint for that call):",
    ...(analysis.executed.length > 0 ? analysis.executed : ["(none)"]),
    "",
    "tool results for the probe commands:",
    ...(analysis.results.length > 0 ? analysis.results : ["(none)"]),
    "",
    ...(analysis.hookIssues.length > 0 ? ["hook issues:", ...analysis.hookIssues, ""] : []),
    "verdict:",
    ...analysis.verdict,
  ].join("\n");
}

async function newestFile(directory: string, pattern: RegExp): Promise<string | undefined> {
  let best: { readonly file: string; readonly mtime: number } | undefined;
  const walk = async (dir: string): Promise<void> => {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) await walk(full);
      else if (pattern.test(entry.name)) {
        const mtime = (await Bun.file(full).stat()).mtimeMs;
        if (!best || mtime > best.mtime) best = { file: full, mtime };
      }
    }
  };
  await walk(directory);
  return best?.file;
}

// ---------------------------------------------------------------------------------------------
// Terminal leg: the same worker environment in an interactive `omp` under a private tmux server
// ---------------------------------------------------------------------------------------------

const TMUX = ["tmux", "-L", "l12rig"];

async function tmux(...args: string[]): Promise<string> {
  const child = Bun.spawn([...TMUX, ...args], { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr] = await Promise.all([
    new Response(child.stdout as ReadableStream<Uint8Array>).text(),
    new Response(child.stderr as ReadableStream<Uint8Array>).text(),
  ]);
  if ((await child.exited) !== 0) throw new Error(`tmux ${args.join(" ")} failed: ${stderr}`);
  return stdout;
}

/** True once the transcript's newest assistant message is a plain reply (no tool call), which
 * the prompt asks for only after the last step. */
async function turnFinished(transcript: string): Promise<boolean> {
  const entries = await readJsonLines(transcript);
  for (let i = entries.length - 1; i >= 0; i--) {
    const entry = entries[i];
    if (entry?.type !== "message") continue;
    const message = entry.message as Record<string, unknown>;
    if (message.role !== "assistant") return false;
    const content = Array.isArray(message.content)
      ? (message.content as Record<string, unknown>[])
      : [];
    if (content.some((part) => part.type === "toolCall")) return false;
    return content.some((part) => typeof part.text === "string" && /\bdone\b/i.test(part.text));
  }
  return false;
}

async function driveTui(launch: WorkerLaunch, prompt: string, label: string): Promise<DriveResult> {
  const stamp = new Date().toISOString().replaceAll(/[:.]/g, "-");
  const runDir = path.join(launch.rig, "runs", `${label}-${stamp}`);
  await mkdir(runDir, { recursive: true });
  const promptFile = path.join(runDir, "prompt.txt");
  await writeFile(promptFile, prompt);
  const startedAt = Date.now();
  const standinLog = path.join(launch.rig, "standin.log");
  const sessionsDir = path.join(
    os.homedir(),
    ".omp",
    "profiles",
    launch.profile,
    "agent",
    "sessions"
  );

  await Bun.spawn([...TMUX, "kill-server"], { stdout: "ignore", stderr: "ignore" }).exited;
  // A fresh private server inherits this spawn's environment, so the pane gets exactly the
  // headless leg's `workerEnvironment`.
  const server = Bun.spawn(
    [
      ...TMUX,
      "new-session",
      "-d",
      "-s",
      "l12rig",
      "-x",
      "200",
      "-y",
      "50",
      "-c",
      path.join(launch.rig, "ws"),
      ...launchArgv(launch, "tui"),
    ],
    { env: workerEnvironment(launch), stdout: "pipe", stderr: "pipe" }
  );
  if ((await server.exited) !== 0) {
    throw new Error(
      `tmux new-session failed: ${await new Response(server.stderr as ReadableStream<Uint8Array>).text()}`
    );
  }

  // Boot is complete when the stand-in has answered `/worker/ready` for this run.
  const bootDeadline = Date.now() + 120_000;
  for (;;) {
    const ready = ((await readJsonLines(standinLog)) as unknown as StandinLine[]).some(
      (line) =>
        line.path === "/legion/v1/worker/ready" &&
        line.status === 200 &&
        Date.parse(line.at) >= startedAt
    );
    if (ready) break;
    if (Date.now() > bootDeadline)
      throw new Error("the interactive worker never reached /worker/ready");
    await Bun.sleep(2_000);
  }
  await Bun.sleep(5_000);
  // Bracketed paste keeps a multi-line prompt in the composer until Enter.
  await tmux("load-buffer", promptFile);
  await tmux("paste-buffer", "-p", "-t", "l12rig");
  await Bun.sleep(1_000);
  await tmux("send-keys", "-t", "l12rig", "Enter");
  console.log(`[rig] prompt sent to tmux -L l12rig; attach with: tmux -L l12rig attach -t l12rig`);

  let endedBy: DriveResult["endedBy"] = "agent_end";
  let transcript: string | undefined;
  const deadline = Date.now() + RUN_DEADLINE_MS;
  for (;;) {
    transcript = await newestFile(sessionsDir, /\.jsonl$/);
    if (
      transcript &&
      (await Bun.file(transcript).stat()).mtimeMs >= startedAt &&
      (await turnFinished(transcript))
    )
      break;
    if (Date.now() > deadline) {
      endedBy = "deadline";
      console.error("[rig] run deadline reached; stopping the interactive worker");
      break;
    }
    await Bun.sleep(5_000);
  }
  await writeFile(
    path.join(runDir, "pane.txt"),
    await tmux("capture-pane", "-p", "-t", "l12rig", "-S", "-200")
  );
  await tmux("kill-server");
  return { runDir, pid: 0, sessionFile: transcript, exitCode: null, endedBy };
}

async function report(
  launch: WorkerLaunch,
  result: DriveResult,
  label: string,
  since: number
): Promise<void> {
  console.log(
    `[rig] worker ended by ${result.endedBy}; exit code ${result.exitCode}; session file ${result.sessionFile ?? "(unknown)"}`
  );
  const sessionsDir = path.join(
    os.homedir(),
    ".omp",
    "profiles",
    launch.profile,
    "agent",
    "sessions"
  );
  const transcript = result.sessionFile ?? (await newestFile(sessionsDir, /\.jsonl$/));
  const logsDir = path.join(os.homedir(), ".omp", "profiles", launch.profile, "logs");
  const ompLog =
    result.pid > 0
      ? await newestFile(logsDir, new RegExp(`^omp\\..*\\.${result.pid}\\.log$`))
      : await newestFile(logsDir, /^omp\..*\.log$/);
  if (!transcript || !ompLog || (await Bun.file(ompLog).stat()).mtimeMs < since) {
    console.error(`[rig] missing transcript (${transcript}) or omp log (${ompLog})`);
    process.exit(1);
  }
  const analysis = await analyze({
    label,
    rig: launch.rig,
    transcript,
    standinLog: path.join(launch.rig, "standin.log"),
    ompLog,
  });
  const rendered = renderAnalysis(analysis);
  const table = path.join(result.runDir, "table.txt");
  await writeFile(table, `${rendered}\n`);
  await writeFile(
    path.join(result.runDir, "report.json"),
    `${JSON.stringify({ ...result, transcript, ompLog, analysis }, null, 2)}\n`
  );
  console.log(rendered);
  console.log(`[rig] transcript ${transcript}\n[rig] omp log ${ompLog}\n[rig] table ${table}`);
}

// ---------------------------------------------------------------------------------------------
// Entry
// ---------------------------------------------------------------------------------------------

const { positionals, values } = parseArgs({
  args: Bun.argv.slice(2),
  allowPositionals: true,
  options: {
    rig: { type: "string" },
    port: { type: "string" },
    omp: { type: "string" },
    profile: { type: "string", default: "l12rig" },
    label: { type: "string", default: "run" },
    short: { type: "boolean", default: false },
    "no-secrets": { type: "boolean", default: false },
    transcript: { type: "string" },
    "standin-log": { type: "string" },
    "omp-log": { type: "string" },
  },
});

function required(name: keyof typeof values): string {
  const value = values[name];
  if (typeof value !== "string" || value.length === 0) {
    console.error(`--${name} is required`);
    process.exit(2);
  }
  return value;
}

const subcommand = positionals[0];
switch (subcommand) {
  case "prompt": {
    console.log(buildPrompt(buildSteps(values.short)));
    break;
  }
  case "drive":
  case "tui": {
    const rig = required("rig");
    const launch: WorkerLaunch = {
      rig,
      port: Number(required("port")),
      omp: required("omp"),
      profile: values.profile,
      useSecrets: !values["no-secrets"],
    };
    const since = Date.now();
    const prompt = buildPrompt(buildSteps(values.short));
    const result =
      subcommand === "drive"
        ? await drive(launch, prompt, values.label)
        : await driveTui(launch, prompt, values.label);
    await report(launch, result, values.label, since);
    break;
  }
  case "analyze": {
    const analysis = await analyze({
      label: values.label,
      rig: required("rig"),
      transcript: required("transcript"),
      standinLog: required("standin-log"),
      ompLog: required("omp-log"),
    });
    console.log(renderAnalysis(analysis));
    break;
  }
  default:
    console.error("usage: bun run.ts <prompt|drive|tui|analyze> [options]");
    process.exit(2);
}

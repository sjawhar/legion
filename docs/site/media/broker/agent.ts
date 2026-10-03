// docs/site/media/broker/agent.ts
//
// Drives the rig's agent machine (docs/site/media/broker/rig.sh) from a script: reads the state
// file the rig writes, starts a machine login and a secret request inside the agent container,
// and hands back what each prints that a person acts on (the machine login's code, the request's
// Dispatch record), so the browser side can approve it.
import { type ChildProcess, spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { setTimeout as sleep } from "node:timers/promises";

export interface RigState {
  /** rig.sh's agent-exec: runs its arguments on the agent machine, in its demo directory. */
  agentExec: string;
  brokerUrl: string;
  dispatchUrl: string;
  operator: string;
}

/** The rig's state file, which `rig.sh -- <command>` names in BROKER_RIG_STATE. */
export function rigState(): RigState {
  const path = process.env.BROKER_RIG_STATE;
  if (path === undefined || path === "") {
    throw new Error("BROKER_RIG_STATE is unset: run this under docs/site/media/broker/rig.sh -- <command>");
  }
  const values = new Map(
    readFileSync(path, "utf8")
      .split("\n")
      .filter((line) => line.includes("="))
      .map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)])
  );
  const value = (key: string): string => {
    const found = values.get(key);
    if (found === undefined || found === "") throw new Error(`${path} has no ${key}`);
    return found;
  };
  return {
    agentExec: value("BROKER_RIG_AGENT_EXEC"),
    brokerUrl: value("BROKER_RIG_BROKER_URL"),
    dispatchUrl: value("BROKER_RIG_DISPATCH_URL"),
    operator: value("BROKER_RIG_OPERATOR"),
  };
}

export interface Running {
  /** Resolves with the command's exit code and everything it printed, once it exits. */
  done: Promise<{ code: number | null; stdout: string; stderr: string }>;
  child: ChildProcess;
}

function run(agentExec: string, argv: string[]): Running & { output: () => string } {
  const child = spawn(agentExec, argv, { stdio: ["ignore", "pipe", "pipe"] });
  let stdout = "";
  let stderr = "";
  child.stdout?.on("data", (chunk) => {
    stdout += chunk;
  });
  child.stderr?.on("data", (chunk) => {
    stderr += chunk;
  });
  const finished = Promise.withResolvers<{ code: number | null; stdout: string; stderr: string }>();
  child.on("close", (code) => finished.resolve({ code, stdout, stderr }));
  const done = finished.promise;
  return { child, done, output: () => stdout + stderr };
}

/** Polls a running command's output until `pattern` matches, failing with what it printed when
 *  it exits first or `timeoutMs` passes. */
async function waitForOutput(
  running: Running & { output: () => string },
  pattern: RegExp,
  what: string,
  timeoutMs = 60_000
): Promise<RegExpMatchArray> {
  let exited = false;
  void running.done.then(() => {
    exited = true;
  });
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const match = running.output().match(pattern);
    if (match !== null) return match;
    if (exited) break;
    await sleep(200);
  }
  throw new Error(`no ${what} in the agent's output:\n${running.output()}`);
}

/** `agent-secrets launcher login` on the agent machine: resolves once it prints its code. */
export async function startMachineLogin(agentExec: string): Promise<Running & { code: string }> {
  const running = run(agentExec, ["agent-secrets", "launcher", "login"]);
  const match = await waitForOutput(running, /machine login code: ([A-Z0-9]{4}-[A-Z0-9]{4})/, "machine login code");
  return { ...running, code: match[1] };
}

/** A session registered with the agent machine's helper, as an agent's session is, asking for
 *  DEMO_API_KEY to run ./check-demo-key.sh: resolves once it names the Dispatch record a person
 *  decides. */
export async function startSecretRequest(
  agentExec: string,
  reason: string
): Promise<Running & { recordId: string; requestId: string }> {
  const running = run(agentExec, [
    "agent-secrets",
    "register",
    "--wait",
    "20",
    "--exec",
    "--",
    "agent-secrets",
    "DEMO_API_KEY",
    "--reason",
    reason,
    "--",
    "./check-demo-key.sh",
  ]);
  const request = await waitForOutput(running, /request (\S+) is waiting for approval/, "pending request");
  const record = await waitForOutput(running, /\/credentials\/([0-9a-f]{64})/, "credential record link");
  return { ...running, recordId: record[1], requestId: request[1] };
}

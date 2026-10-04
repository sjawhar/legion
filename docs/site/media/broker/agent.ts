// docs/site/media/broker/agent.ts
//
// Drives the rig's agent machine (docs/site/media/broker/rig.sh) from a script, for the
// screenshots: starts a machine login and a secret request on the agent machine, and hands back
// what each prints that a person acts on (flow.ts's `printed`: the machine login's code, the
// request's Dispatch record), so the browser side can approve it.
import { type ChildProcess, spawn } from "node:child_process";
import { setTimeout as sleep } from "node:timers/promises";

import { printed, reason } from "./flow";

export interface Running {
  /** Resolves with the command's exit code and everything it printed, once it exits. */
  done: Promise<{ code: number | null; stdout: string; stderr: string }>;
  child: ChildProcess;
  /** Everything the command has printed so far, stdout then stderr. */
  output: () => string;
}

function run(agentExec: string, argv: string[], stdin: "ignore" | "pipe" = "ignore"): Running {
  const child = spawn(agentExec, argv, { stdio: [stdin, "pipe", "pipe"] });
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
export async function waitForOutput(
  running: Running,
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
  const match = await waitForOutput(running, printed.loginCode, "machine login code");
  return { ...running, code: match[1] };
}

/** A session registered with the agent machine's helper, as an agent's session is, asking for
 *  DEMO_API_KEY to run ./check-demo-key.sh: resolves once it names the Dispatch record a person
 *  decides. The session stays open after the command runs, as an agent's does, so its grant stays
 *  live (the helper ends a session's enrollment, and with it the grant, when its process exits);
 *  `end()` closes it. */
export async function startSecretRequest(
  agentExec: string
): Promise<Running & { recordId: string; requestId: string; end: () => void }> {
  const running = run(
    agentExec,
    [
      "agent-secrets",
      "register",
      "--wait",
      "20",
      "--exec",
      "--",
      "sh",
      "-c",
      'agent-secrets DEMO_API_KEY --reason "$1" -- ./check-demo-key.sh || exit; read -r _ || true',
      "session",
      reason,
    ],
    "pipe"
  );
  const request = await waitForOutput(running, printed.requestWaiting, "pending request");
  const record = await waitForOutput(running, printed.recordLink, "credential record link");
  return {
    ...running,
    end: () => running.child.stdin?.end(),
    recordId: record[1],
    requestId: request[1],
  };
}

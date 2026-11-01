/** `bun e2e/run.ts -- <command> [args...]`: starts the Dispatch e2e harness (e2e/harness-start.ts),
 *  runs the command with each listener's port in its environment, stops the harness once the
 *  command exits, and exits with the command's status. `bun run e2e` runs Playwright through it:
 *
 *    bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts [args]
 *
 *  The harness serves on ports the OS chooses (or those its port variables set), and only its
 *  processes know which until they report them, so the ports reach Playwright here, before it
 *  starts: its config, every worker that re-imports it, and every helper that reads a port at
 *  import see the real ones. The command also gets `DISPATCH_E2E_REUSE_SERVERS=1`, since the
 *  servers it targets are already up (e2e/playwright.config.ts starts none). With
 *  `PLAYWRIGHT_BASE_URL` set, only the fakes a deployed server is tested with start
 *  (`bun run e2e:deployed`).
 *
 *  A Ctrl-C reaches the command itself, which shares this process group, and the harness stops
 *  once the command has; a SIGTERM sent to this process alone is passed on to the command. A
 *  signal while the harness starts stops what has started. */
import { constants } from "node:os";
import type { Subprocess } from "bun";

import { type Harness, startHarness } from "./harness-start";
import { Interrupted } from "./local-instance-support";

// Bun drops the first `--` after the script; a runtime that keeps it gets the same command.
const command = process.argv.slice(process.argv[2] === "--" ? 3 : 2);
if (command.length === 0) {
  console.error(
    "usage: bun e2e/run.ts -- <command> [args...], for example\n" +
      "  bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts"
  );
  process.exit(2);
}

const stopping = new AbortController();
let received: NodeJS.Signals | undefined;
let child: Subprocess | undefined;
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => {
    received ??= signal;
    stopping.abort();
    if (signal === "SIGTERM") child?.kill(signal);
  });
}
const signalStatus = (signal: NodeJS.Signals) => 128 + constants.signals[signal];

let harness: Harness;
try {
  harness = await startHarness({ signal: stopping.signal });
} catch (error) {
  if (error instanceof Interrupted) process.exit(signalStatus(received ?? "SIGINT"));
  console.error(`e2e/run.ts: ${error instanceof Error ? error.message : String(error)}`);
  process.exit(1);
}

let status: number;
if (received !== undefined) {
  status = signalStatus(received);
} else {
  const ports = Object.entries(harness.env).map(([variable, port]) => `${variable}=${port}`);
  console.error(`e2e/run.ts: the harness is up (${ports.join(" ")}); running ${command.join(" ")}`);
  let commandDone = false;
  // A harness process that stops while the command runs is named, so rows failing against a
  // server that died say why.
  harness.firstExit().catch((error: Error) => {
    if (!commandDone) console.error(`e2e/run.ts: ${error.message}`);
  });
  try {
    child = Bun.spawn(command, {
      env: { ...process.env, ...harness.env, DISPATCH_E2E_REUSE_SERVERS: "1" },
      stdio: ["inherit", "inherit", "inherit"],
    });
    const code = await child.exited;
    status = child.signalCode ? signalStatus(child.signalCode) : code;
  } catch (error) {
    console.error(
      `e2e/run.ts: cannot run ${command[0]}: ${error instanceof Error ? error.message : String(error)}`
    );
    status = 127;
  }
  commandDone = true;
}
await harness.stop();
process.exit(status);

/** Starts the Dispatch e2e harness: every row of `e2e/harness-listeners.ts` a run needs, in the
 *  table's order (the fakes, then the Dispatch server, then the plain-HTTP proxy). A row whose
 *  port variable is unset serves on a port the OS chose and stays held until it serves — the
 *  server through a socket `scripts/e2e/lib/held-port.py` holds and hands it, each Bun listener by
 *  binding port 0 — and reports it on its listening line, which this module reads. A row whose
 *  variable is set serves on exactly that port, and an occupied one fails with "address already in
 *  use". Each row starts in the environment the rows before it reported, so the server's names the
 *  fakes' ports and the proxy's the server's.
 *
 *  This is the harness's one starter: `e2e/run.ts` (Playwright), `e2e/local-instance.ts`
 *  (`bun run dispatch:local`) and the docs media harness (`docs/site/media/harness.ts`) call it,
 *  then set the ports it returns in the environment of whatever addresses the harness. A run with
 *  `PLAYWRIGHT_BASE_URL` starts only the rows marked `deployed`, the fakes a deployed server is
 *  tested with. */
import { createWriteStream, mkdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";

import { type HarnessListener, targetedListeners } from "./harness-listeners";
import { harnessPorts, harnessVariables } from "./harness-ports";
import { type PipedRunning, Supervisor } from "./local-instance-support";

const dispatchPackage = fileURLToPath(new URL("..", import.meta.url));
/** How many of a process's last lines a refusal quotes. */
const tailLines = 20;

export interface HarnessProcess {
  readonly listener: HarnessListener;
  /** The process, the leader of a process group of its own. */
  readonly child: Subprocess;
  /** The file its output goes to under `logDir`, or undefined when it goes to this process's
   *  stderr. */
  readonly log: string | undefined;
  /** Settles once the process has closed its output and all of it is written. */
  readonly drained: Promise<void>;
}

export interface Harness {
  /** The port each started listener serves on, by its variable (`DISPATCH_E2E_PORT`,
   *  `FAKE_ENVOY_PORT`, `FAKE_GITHUB_PORT`, `FAKE_BROKER_PORT`, `PLAIN_HTTP_PORT`): the
   *  environment a process that addresses the harness needs. */
  readonly env: Readonly<Record<string, string>>;
  readonly processes: readonly HarnessProcess[];
  /** Rejects as soon as one of the processes exits by itself, naming it. */
  firstExit(): Promise<never>;
  /** SIGTERM to every process group, SIGKILL to whatever still runs 10 s later; resolves once
   *  every leader has exited. */
  stop(): Promise<void>;
}

export interface HarnessOptions {
  /** A directory for each process's output, as `<id>.log`; unset, it goes to this process's
   *  stderr. */
  readonly logDir?: string;
  /** Aborting it while the harness starts stops every process started so far, and the start
   *  rejects with `Interrupted`. */
  readonly signal?: AbortSignal;
}

interface Watched extends HarnessProcess {
  /** The port on its listening line, once the line has been read. */
  readonly reported: () => number | undefined;
  /** Its last lines of output. */
  readonly tail: readonly string[];
}

/** Copies the process's output to `logDir` or this process's stderr, and reads each line for the
 *  listener's report. */
function watch(running: PipedRunning, listener: HarnessListener, logDir?: string): Watched {
  const log = logDir === undefined ? undefined : join(logDir, `${listener.id}.log`);
  const file = log === undefined ? undefined : createWriteStream(log);
  const tail: string[] = [];
  let port: number | undefined;
  const see = (line: string) => {
    tail.push(line);
    if (tail.length > tailLines) tail.shift();
    const match = port === undefined ? listener.report.exec(line) : null;
    if (match !== null) port = Number(match[1]);
  };
  const pump = async (stream: ReadableStream<Uint8Array>) => {
    const reader = stream.getReader();
    const decoder = new TextDecoder();
    let partial = "";
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (file === undefined) process.stderr.write(value);
      else file.write(value);
      const lines = (partial + decoder.decode(value, { stream: true })).split("\n");
      partial = lines.pop() ?? "";
      for (const line of lines) see(line);
    }
    partial += decoder.decode();
    if (partial !== "") see(partial);
  };
  const { child } = running;
  // Both streams write the one file, so it ends only once the process has closed both.
  const drained = Promise.allSettled([pump(child.stdout), pump(child.stderr)]).then(
    () => new Promise<void>((resolve) => (file === undefined ? resolve() : file.end(resolve)))
  );
  return { child, drained, listener, log, reported: () => port, tail };
}

/** The refusal for a listener that did not come up, quoting its last lines. An explicitly set port
 *  it could not bind is named as such. */
function refusal(watched: Watched, given: number, what: string): Error {
  const { listener, log, tail } = watched;
  const lines = tail.join("\n");
  const inUse = given !== 0 && /EADDRINUSE|address already in use/.test(lines);
  const head = inUse
    ? `${listener.name} cannot listen on ${harnessVariables[listener.key]}=${given}: address already in use`
    : `${listener.name} ${what}`;
  const where = log === undefined ? "" : ` (${log})`;
  return new Error(
    `The Dispatch e2e harness cannot start: ${head}${where}. Its last lines:\n${lines}`
  );
}

/** Waits for the listener's report and, for a listener with a health path, for that path to
 *  answer 200, and returns the port it serves on. It fails as soon as the process exits, so a
 *  server whose build failed is reported at once rather than when the wait runs out. */
async function serving(watched: Watched, given: number, supervisor: Supervisor): Promise<number> {
  const { child, listener } = watched;
  const deadline = Date.now() + listener.bootMs;
  const check = async (waitingFor: string) => {
    if (child.exitCode !== null || child.signalCode !== null) {
      // The last lines are the reason; let the output reach the tail first.
      await Promise.race([watched.drained, Bun.sleep(1000)]);
      throw refusal(watched, given, `exited ${child.exitCode ?? child.signalCode} ${waitingFor}`);
    }
    if (Date.now() > deadline) {
      throw refusal(watched, given, `did not ${waitingFor} within ${listener.bootMs / 1000} s`);
    }
    await supervisor.untilInterrupted(Bun.sleep(100));
  };
  let port = watched.reported();
  while (port === undefined) {
    await check("before it reported its port");
    port = watched.reported();
  }
  if (listener.health === undefined) return port;
  const url = `http://127.0.0.1:${port}${listener.health}`;
  for (;;) {
    try {
      const response = await supervisor.untilInterrupted(
        fetch(url, { signal: AbortSignal.timeout(5000) })
      );
      if (response.ok) return port;
    } catch (error) {
      if (supervisor.signal.aborted) throw error;
      // not answering yet
    }
    await check(`answer 200 at ${url}`);
  }
}

/** Starts the harness (see the module comment) and returns once every listener serves. A failure
 *  stops every process it started before it throws. */
export async function startHarness(options: HarnessOptions = {}): Promise<Harness> {
  // Refuses a malformed or duplicated port before anything starts.
  const given = harnessPorts();
  const listeners = targetedListeners();
  if (options.logDir !== undefined) mkdirSync(options.logDir, { recursive: true });
  const supervisor = new Supervisor();
  const interrupt = () => supervisor.interrupt();
  options.signal?.addEventListener("abort", interrupt, { once: true });
  if (options.signal?.aborted) interrupt();
  const env: Record<string, string> = {};
  const processes: Watched[] = [];
  try {
    for (const listener of listeners) {
      const running = supervisor.startPiped(listener.name, listener.argv, dispatchPackage, {
        ...process.env,
        ...env,
      });
      const watched = watch(running, listener, options.logDir);
      processes.push(watched);
      env[harnessVariables[listener.key]] = String(
        await serving(watched, given[listener.key].port, supervisor)
      );
    }
  } catch (error) {
    await supervisor.stop();
    throw error;
  } finally {
    options.signal?.removeEventListener("abort", interrupt);
  }
  return {
    env,
    firstExit: () => supervisor.firstExit(),
    processes,
    stop: () => supervisor.stop(),
  };
}

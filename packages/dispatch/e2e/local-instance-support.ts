/** What `e2e/local-instance.ts` runs on: the processes it starts and stops, its refusal of a
 *  database it must not truncate, and the quoting of the commands it prints. Nothing here runs on
 *  import, so `e2e/__tests__/local-instance-support.test.ts` drives it without a server. */
import type { Subprocess } from "bun";

export class Interrupted extends Error {}

interface Running {
  readonly name: string;
  readonly child: Subprocess;
}

/** Every process the command starts, as the leader of a process group of its own, and the one
 *  signal that a stop is under way: a Ctrl-C (`interrupt`) or the end of the run (`stop`). */
export class Supervisor {
  readonly #children: Running[] = [];
  readonly #stopping = new AbortController();

  /** Aborted by `interrupt` or `stop`. A step that starts a process outside `start` (psql through
   *  e2e/psql.ts) passes it on, so the process ends with the others. */
  get signal(): AbortSignal {
    return this.#stopping.signal;
  }

  interrupt(): void {
    this.#stopping.abort();
  }

  /** `step`, or Interrupted as soon as a stop is under way. `step`'s own outcome is always
   *  handled, so a step the stop outruns settles later into nothing rather than an unhandled
   *  rejection; `stop` ends the processes it waits on. */
  untilInterrupted<T>(step: Promise<T>): Promise<T> {
    const { signal } = this.#stopping;
    return new Promise<T>((resolve, reject) => {
      const onInterrupt = () => reject(new Interrupted("interrupted"));
      signal.addEventListener("abort", onInterrupt, { once: true });
      step.then(resolve, reject).finally(() => signal.removeEventListener("abort", onInterrupt));
      if (signal.aborted) onInterrupt();
    });
  }

  /** Starts `argv` as the leader of a process group of its own, so stopping it reaches what it
   *  starts in turn: run-server.sh's `flock … go build` before it execs the server, and the vite
   *  process `bun run build:web` runs. A terminal's Ctrl-C reaches only the command, which stops
   *  them in order. Their output is the command's. */
  start(name: string, argv: readonly string[], cwd: string): Running {
    const child = Bun.spawn([...argv], {
      cwd,
      detached: true,
      env: process.env,
      stderr: "inherit",
      stdin: "ignore",
      stdout: "inherit",
    });
    const running = { child, name };
    this.#children.push(running);
    return running;
  }

  /** A foreground step. It throws on a non-zero exit, so a failed web build or dev-postgres.sh
   *  reaches the command's one catch (and `stop`) like every other failure. */
  async run(argv: readonly string[], cwd: string): Promise<void> {
    const step = this.start(argv.join(" "), argv, cwd);
    const code = await this.untilInterrupted(step.child.exited);
    this.#children.splice(this.#children.indexOf(step), 1);
    if (code !== 0) throw new Error(`${step.name} exited ${code}`);
  }

  /** Rejects as soon as any process started so far exits by itself, naming it. */
  firstExit(): Promise<never> {
    return Promise.race(
      this.#children.map(async ({ child, name }): Promise<never> => {
        const code = await child.exited;
        throw new Error(`${name} exited ${code ?? child.signalCode} while the instance was up`);
      })
    );
  }

  /** Aborts `signal`, sends SIGTERM to every process group started and not yet seen to stop,
   *  SIGKILL to whatever still runs `forceAfterMs` later, and returns the moment every leader has
   *  exited: the force-kill timer is cleared then, so a clean Ctrl-C does not keep the shell
   *  prompt waiting on it. */
  async stop(forceAfterMs = 10_000): Promise<void> {
    this.#stopping.abort();
    const running = this.#children.splice(0);
    const signalGroups = (signal: NodeJS.Signals) => {
      for (const { child } of running) {
        try {
          process.kill(-child.pid, signal);
        } catch {
          // ESRCH: every process of that group has already exited.
        }
      }
    };
    signalGroups("SIGTERM");
    const forceKill = setTimeout(() => signalGroups("SIGKILL"), forceAfterMs);
    await Promise.all(running.map(({ child }) => child.exited));
    clearTimeout(forceKill);
  }
}

const loopbackHosts = ["127.0.0.1", "localhost", "[::1]", "::1"];

/** Why `raw` may not be the database this command truncates, or undefined when it names this
 *  machine: every host libpq would connect to is a loopback name or address or a Unix socket
 *  directory. The host is the `host` query parameter when the URL has one, as for libpq, else the
 *  URL's own (percent-decoded, so `%2Fvar%2Frun%2Fpostgresql` is a socket directory); an empty
 *  one is libpq's default socket. A key=value string is not a URL and is refused.
 *
 *  This check is the command's early refusal, not its boundary. It reads the URL with the URL
 *  parser, not libpq's; what it misses is closed by ORDER: the Dispatch server's own pgx check
 *  (cmd/dispatch loopbackDatabase) refuses to boot on a non-loopback host, and resetDatabase runs
 *  only after /healthz answered, so a URL this check wrongly approves truncates nothing. Keep the
 *  health wait ahead of the reset. */
export function databaseUrlRefusal(raw: string): string | undefined {
  let named: string;
  try {
    const url = new URL(raw);
    named = url.searchParams.get("host") ?? decodeURIComponent(url.hostname);
  } catch {
    return "DATABASE_URL must be a single postgres:// URL naming a loopback host or a Unix socket: this command truncates it";
  }
  const remote = named
    .split(",")
    .map((host) => host.trim())
    .filter((host) => host !== "" && !host.startsWith("/"))
    .filter((host) => !loopbackHosts.includes(host.toLowerCase()));
  if (remote.length === 0) return undefined;
  return (
    "DATABASE_URL must name a loopback Postgres (127.0.0.1, localhost or [::1]) or a Unix " +
    `socket, not ${remote.join(", ")}: this command truncates it`
  );
}

/** `value` as one shell word, safe to paste whatever it holds. The same rule as `quote` in
 *  packages/envoy/scripts/dev-postgres.sh; both tests hold it to
 *  packages/envoy/scripts/fixtures/shell-quote.json. */
export function shellQuote(value: string): string {
  return /^[A-Za-z0-9_@%+=:,./-]+$/.test(value) ? value : `'${value.replaceAll("'", `'\\''`)}'`;
}

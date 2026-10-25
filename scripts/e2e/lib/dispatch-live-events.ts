import { parseArgs } from "node:util";

const USAGE =
  "usage: bun scripts/e2e/lib/dispatch-live-events.ts --url <dispatch> [--bearer-file <file> | --user <email>] [--since <id>] [--until <substring>] [--timeout <s>]";
const SERVICE_RESTART = 1012;
const SOCKET_ROTATED = 4001;
const BASE_RECONNECT_DELAY_MS = 1_000;
const MAX_RECONNECT_DELAY_MS = 30_000;

interface ArgumentValues {
  readonly "bearer-file"?: string;
  readonly since?: string;
  readonly timeout?: string;
  readonly until?: string;
  readonly url?: string;
  readonly user?: string;
}

interface Options {
  readonly authorization: Record<string, string>;
  readonly since: string | null;
  readonly timeoutMs: number;
  readonly until: string | undefined;
  readonly url: URL;
}

interface EventsStart {
  readonly since: string;
  readonly sub: string;
  readonly type: "events_start";
}

interface EventFrame {
  readonly event: unknown;
  readonly id: string;
  readonly sub: string;
  readonly type: "event";
}

class UsageError extends Error {}

const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

function required(value: string | undefined, flag: string): string {
  if (value === undefined || value === "") throw new UsageError(`${flag} is required`);
  return value;
}

function eventID(value: string, flag: string): string {
  if (!/^(?:0|[1-9][0-9]*)$/u.test(value)) {
    throw new UsageError(`${flag} is a non-negative decimal event id, not ${value}`);
  }
  return value;
}

async function optionsFrom(argv: readonly string[]): Promise<Options> {
  let values: ArgumentValues;
  try {
    values = parseArgs({
      args: [...argv],
      allowPositionals: false,
      options: {
        "bearer-file": { type: "string" },
        since: { type: "string" },
        timeout: { type: "string" },
        until: { type: "string" },
        url: { type: "string" },
        user: { type: "string" },
      },
      strict: true,
    }).values as ArgumentValues;
  } catch (error) {
    throw new UsageError(error instanceof Error ? error.message : String(error));
  }

  const bearerFile = values["bearer-file"];
  const user = values.user;
  if ((bearerFile === undefined) === (user === undefined)) {
    throw new UsageError("exactly one of --bearer-file or --user is required");
  }

  let url: URL;
  const rawURL = required(values.url, "--url");
  try {
    url = new URL(rawURL);
  } catch {
    throw new UsageError(`--url is not a URL: ${rawURL}`);
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new UsageError(`--url is Dispatch's http or https address, not ${rawURL}`);
  }

  const rawTimeout = values.timeout ?? "60";
  const timeoutSeconds = Number(rawTimeout);
  if (!Number.isFinite(timeoutSeconds) || timeoutSeconds <= 0) {
    throw new UsageError(`--timeout is a number of seconds above 0, not ${rawTimeout}`);
  }

  const authorization =
    bearerFile === undefined
      ? { "X-Dispatch-User": required(user, "--user") }
      : { Authorization: `Bearer ${await bearerFrom(bearerFile)}` };
  return {
    authorization,
    since: values.since === undefined ? null : eventID(values.since, "--since"),
    timeoutMs: timeoutSeconds * 1_000,
    until: values.until,
    url,
  };
}

async function bearerFrom(file: string): Promise<string> {
  let bearer: string;
  try {
    bearer = (await Bun.file(file).text()).trim();
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error);
    throw new UsageError(`cannot read --bearer-file ${file}: ${reason}`);
  }
  if (bearer === "") throw new UsageError(`--bearer-file ${file} holds no token`);
  return bearer;
}

function asEventsStart(value: unknown): EventsStart | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    !("type" in value) ||
    value.type !== "events_start" ||
    !("sub" in value) ||
    value.sub !== "events" ||
    !("since" in value) ||
    typeof value.since !== "string"
  ) {
    return undefined;
  }
  return value as EventsStart;
}

function asEventFrame(value: unknown): EventFrame | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    !("type" in value) ||
    value.type !== "event" ||
    !("sub" in value) ||
    value.sub !== "events" ||
    !("id" in value) ||
    typeof value.id !== "string" ||
    !("event" in value)
  ) {
    return undefined;
  }
  return value as EventFrame;
}

function reconnectDelay(attempt: number): number {
  return Math.min(BASE_RECONNECT_DELAY_MS * 2 ** attempt, MAX_RECONNECT_DELAY_MS);
}

function write(line: object): void {
  process.stdout.write(`${JSON.stringify(line)}\n`);
}

async function watch(options: Options): Promise<boolean> {
  const socketURL = new URL("/ws/live", options.url);
  socketURL.protocol = options.url.protocol === "https:" ? "wss:" : "ws:";
  const done = Promise.withResolvers<boolean>();
  let attempt = 0;
  let cursor = options.since;
  let reconnect: ReturnType<typeof setTimeout> | undefined;
  let socket: WebSocket | null = null;
  let stopped = false;

  const stop = (matched: boolean) => {
    if (stopped) return;
    stopped = true;
    clearTimeout(timeout);
    clearTimeout(reconnect);
    const closing = socket;
    socket = null;
    closing?.close(1000);
    done.resolve(matched);
  };

  const open = (resubscribing: boolean) => {
    if (stopped) return;
    if (resubscribing) write({ resubscribed: cursor });
    const next = new BunWebSocket(socketURL.href, { headers: options.authorization });
    socket = next;
    next.addEventListener("open", () => {
      if (socket !== next || stopped) return;
      const frame =
        cursor === null
          ? { sub: "events", type: "subscribe", view: { kind: "events" } }
          : {
              resume: { since: cursor },
              sub: "events",
              type: "subscribe",
              view: { kind: "events" },
            };
      next.send(JSON.stringify(frame));
    });
    next.addEventListener("message", (message) => {
      if (socket !== next || stopped || typeof message.data !== "string") return;
      let frame: unknown;
      try {
        frame = JSON.parse(message.data);
      } catch {
        return;
      }
      const start = asEventsStart(frame);
      if (start !== undefined) {
        if (cursor === null) cursor = eventID(start.since, "events_start.since");
        return;
      }
      const event = asEventFrame(frame);
      if (event === undefined) return;
      cursor = eventID(event.id, "event.id");
      const rendered = JSON.stringify(event.event);
      write({ event: event.event });
      if (options.until !== undefined && rendered.includes(options.until)) stop(true);
    });
    next.addEventListener("close", (event) => {
      if (socket !== next || stopped) return;
      socket = null;
      write({ close: event.code, reason: event.reason });
      const immediate = event.code === SERVICE_RESTART || event.code === SOCKET_ROTATED;
      const delay = immediate
        ? 0
        : reconnectDelay(attempt++) * (0.75 + Math.random() * 0.5);
      reconnect = setTimeout(() => {
        reconnect = undefined;
        open(true);
      }, delay);
    });
  };

  const timeout = setTimeout(() => stop(false), options.timeoutMs);
  open(false);
  return done.promise;
}

async function main(argv: readonly string[]): Promise<number> {
  try {
    const options = await optionsFrom(argv);
    return (await watch(options)) ? 0 : 1;
  } catch (error) {
    if (error instanceof UsageError) {
      process.stderr.write(`dispatch-live-events: ${error.message}\n${USAGE}\n`);
      return 2;
    }
    process.stderr.write(`dispatch-live-events: ${error instanceof Error ? error.message : String(error)}\n`);
    return 1;
  }
}

process.exit(await main(Bun.argv.slice(2)));

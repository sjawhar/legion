import { parseArgs } from "node:util";
import { LIVE_SOCKET_MAX_AGE_CLOSE } from "../../../packages/contracts/src/dispatch-api";

const USAGE =
  "usage: bun scripts/e2e/lib/dispatch-live-events.ts --url <dispatch> [--bearer-file <file> | --cookie-file <file> | --user <email>] [--since <id> | --from-whoami [--subscribe-after <s>]] [--until <substring>] [--timeout <s>]";
const SERVICE_RESTART = 1012;
const BASE_RECONNECT_DELAY_MS = 1_000;
const MAX_RECONNECT_DELAY_MS = 30_000;

interface ArgumentValues {
  readonly "bearer-file"?: string;
  readonly "cookie-file"?: string;
  readonly "from-whoami"?: boolean;
  readonly since?: string;
  readonly "subscribe-after"?: string;
  readonly timeout?: string;
  readonly until?: string;
  readonly url?: string;
  readonly user?: string;
}

interface Options {
  readonly authorization: Record<string, string>;
  /** Read the feed's start from `GET /auth/whoami`'s `event_head`, as a page does (INFRA-292). */
  readonly fromWhoami: boolean;
  readonly since: string | null;
  /** How long to wait between whoami's answer and the socket, so a write can commit in between. */
  readonly subscribeAfterMs: number;
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

interface Ended {
  readonly reason: string;
  readonly sub: string;
  readonly type: "ended";
}

interface Refusal {
  readonly code: string;
  readonly message: string;
  readonly type: "error";
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
        "cookie-file": { type: "string" },
        "from-whoami": { type: "boolean" },
        since: { type: "string" },
        "subscribe-after": { type: "string" },
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
  const cookieFile = values["cookie-file"];
  const user = values.user;
  if ([bearerFile, cookieFile, user].filter((value) => value !== undefined).length !== 1) {
    throw new UsageError("exactly one of --bearer-file, --cookie-file or --user is required");
  }
  const fromWhoami = values["from-whoami"] === true;
  if (fromWhoami && values.since !== undefined) {
    throw new UsageError("--from-whoami reads where the feed starts; it takes no --since");
  }
  // whoami names a person: a bearer is an agent's, and Dispatch answers it no head.
  if (fromWhoami && bearerFile !== undefined) {
    throw new UsageError("--from-whoami signs in as a person: give --cookie-file or --user");
  }
  if (!fromWhoami && values["subscribe-after"] !== undefined) {
    throw new UsageError("--subscribe-after waits after whoami's answer; it needs --from-whoami");
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
  const rawSubscribeAfter = values["subscribe-after"] ?? "0";
  const subscribeAfterSeconds = Number(rawSubscribeAfter);
  if (!Number.isFinite(subscribeAfterSeconds) || subscribeAfterSeconds < 0) {
    throw new UsageError(
      `--subscribe-after is a number of seconds, 0 or more, not ${rawSubscribeAfter}`
    );
  }

  const authorization =
    bearerFile !== undefined
      ? { Authorization: `Bearer ${await secretFrom(bearerFile, "--bearer-file", "token")}` }
      : cookieFile !== undefined
        ? { Cookie: await secretFrom(cookieFile, "--cookie-file", "cookie") }
        : { "X-Dispatch-User": required(user, "--user") };
  return {
    authorization,
    fromWhoami,
    since: values.since === undefined ? null : eventID(values.since, "--since"),
    subscribeAfterMs: subscribeAfterSeconds * 1_000,
    timeoutMs: timeoutSeconds * 1_000,
    until: values.until,
    url,
  };
}

/** The one credential `file` holds, trimmed: a bearer token, or the `Cookie` header a browser
 *  signed in to Dispatch sends it. */
async function secretFrom(file: string, flag: string, what: string): Promise<string> {
  let secret: string;
  try {
    secret = (await Bun.file(file).text()).trim();
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error);
    throw new UsageError(`cannot read ${flag} ${file}: ${reason}`);
  }
  if (secret === "") throw new UsageError(`${flag} ${file} holds no ${what}`);
  return secret;
}

/** The newest event id as of the sign-in check, `GET /auth/whoami`'s `event_head`: where a page's
 *  first events subscription resumes from, so what commits after it reaches the page. */
async function whoamiHead(options: Options): Promise<string> {
  const response = await fetch(new URL("/auth/whoami", options.url), {
    headers: options.authorization,
  });
  const body: unknown = await response.json().catch(() => null);
  if (
    !response.ok ||
    typeof body !== "object" ||
    body === null ||
    !("event_head" in body) ||
    typeof body.event_head !== "string"
  ) {
    throw new Error(
      `GET /auth/whoami answered ${response.status} ${JSON.stringify(body)}, with no event_head`
    );
  }
  write({
    whoami: {
      event_head: body.event_head,
      login: "login" in body ? body.login : null,
    },
  });
  return eventID(body.event_head, "whoami.event_head");
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

function asEnded(value: unknown): Ended | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    !("type" in value) ||
    value.type !== "ended" ||
    !("sub" in value) ||
    value.sub !== "events" ||
    !("reason" in value) ||
    typeof value.reason !== "string"
  ) {
    return undefined;
  }
  return value as Ended;
}

/** An `error` frame refusing the feed, or, without `sub`, a frame this script sent. */
function asRefusal(value: unknown): Refusal | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    !("type" in value) ||
    value.type !== "error" ||
    ("sub" in value && value.sub !== "events") ||
    !("code" in value) ||
    typeof value.code !== "string" ||
    !("message" in value) ||
    typeof value.message !== "string"
  ) {
    return undefined;
  }
  return value as Refusal;
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

  /** Ends the watch: true once an event matched, false at the timeout, or Dispatch's refusal. */
  const stop = (outcome: boolean | Error) => {
    if (stopped) return;
    stopped = true;
    clearTimeout(timeout);
    clearTimeout(reconnect);
    const closing = socket;
    socket = null;
    closing?.close(1000);
    if (outcome instanceof Error) done.reject(outcome);
    else done.resolve(outcome);
  };

  /** Subscribes the feed on `target`, resuming after the cursor once there is one. */
  const subscribe = (target: WebSocket) => {
    const frame =
      cursor === null
        ? { sub: "events", type: "subscribe", view: { kind: "events" } }
        : {
            resume: { since: cursor },
            sub: "events",
            type: "subscribe",
            view: { kind: "events" },
          };
    target.send(JSON.stringify(frame));
  };

  const open = (resubscribing: boolean) => {
    if (stopped) return;
    if (resubscribing) write({ resubscribed: cursor });
    const next = new BunWebSocket(socketURL.href, { headers: options.authorization });
    socket = next;
    next.addEventListener("open", () => {
      if (socket !== next || stopped) return;
      subscribe(next);
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
        // The feed is carried again: the next loss backs off from the first delay. Dispatch starts
        // a feed at what it was asked or below it, never above (a `--since` past its head, or a
        // resume across a reset of its event log), so the start is the cursor: an event after it
        // is new, not a duplicate of an id the probe asked past.
        attempt = 0;
        cursor = eventID(start.since, "events_start.since");
        return;
      }
      const ended = asEnded(frame);
      if (ended !== undefined) {
        // Dispatch dropped the feed and left the socket open, where it would stay silent: subscribe
        // again from the cursor.
        write({ ended: ended.reason, resubscribed: cursor });
        subscribe(next);
        return;
      }
      const refusal = asRefusal(frame);
      if (refusal !== undefined) {
        stop(
          new Error(`Dispatch refused the events subscription: ${refusal.code}: ${refusal.message}`)
        );
        return;
      }
      const event = asEventFrame(frame);
      if (event === undefined) return;
      const id = eventID(event.id, "event.id");
      // An event only raises the cursor: one at or below it was written already (an older frame,
      // or a redial's replay), so it is noted rather than written again, and the audits that count
      // the event lines count it once.
      if (cursor !== null && BigInt(id) <= BigInt(cursor)) {
        write({ duplicate: id });
        return;
      }
      cursor = id;
      const rendered = JSON.stringify(event.event);
      write({ event: event.event });
      if (options.until !== undefined && rendered.includes(options.until)) stop(true);
    });
    next.addEventListener("close", (event) => {
      if (socket !== next || stopped) return;
      socket = null;
      write({ close: event.code, reason: event.reason });
      const immediate = event.code === SERVICE_RESTART || event.code === LIVE_SOCKET_MAX_AGE_CLOSE;
      const delay = immediate ? 0 : reconnectDelay(attempt++) * (0.75 + Math.random() * 0.5);
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
    if (!options.fromWhoami) return (await watch(options)) ? 0 : 1;
    const since = await whoamiHead(options);
    if (options.subscribeAfterMs > 0) {
      write({ subscribing_in: options.subscribeAfterMs / 1_000 });
      await Bun.sleep(options.subscribeAfterMs);
    }
    return (await watch({ ...options, since })) ? 0 : 1;
  } catch (error) {
    if (error instanceof UsageError) {
      process.stderr.write(`dispatch-live-events: ${error.message}\n${USAGE}\n`);
      return 2;
    }
    process.stderr.write(
      `dispatch-live-events: ${error instanceof Error ? error.message : String(error)}\n`
    );
    return 1;
  }
}

process.exit(await main(Bun.argv.slice(2)));

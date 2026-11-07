import { readFile } from "node:fs/promises";
import { parseArgs } from "node:util";
import { SQL, type TransactionSQL } from "bun";

/**
 * Measures how long every Envoy message waited, in the role session it
 * reached, for that session's next assistant message, and which tool call was open when it
 * arrived. The source is the project's Oh My Pi session store, a shared database, so it is read
 * through the deployment's read-only login: the connection URL comes from the file
 * `SESSION_READER_DSN_FILE` names, or from `--dsn-file`. The session URL the file
 * `OMP_SESSION_SQL_DSN_FILE` names is never a route for a read: it is what the role's own agent
 * writes its transcript with. Each session is its `omp_session_files.content` row followed
 * by its `omp_session_files_parts` rows in `start_offset` order, which is the shape
 * `packages/daemon/internal/ompsessions/ompsessions.go` defines and reads back, and every read is
 * scoped by `path`, the primary key of both tables, since a scan of the parts table takes
 * minutes.
 *
 * Every read runs in one transaction begun read-only, which this script confirms from the server
 * before it reads and which ends with that transaction. A store behind a pool in transaction mode
 * keeps a read-only setting made for a whole session or a whole connection on the backend the
 * connection goes back to, and the pool's next client's writes then fail.
 *
 * Nothing it prints carries the connection URL, a credential or an exception's text: its lines
 * hold a role name, a session id, a tool name, an instant and a count, and a failure is a fixed
 * category with at most the driver's own error code.
 */

/** An arrival: the record Oh My Pi writes when an Envoy message reaches the session. */
const ARRIVAL_TYPE = "custom_message";
const ARRIVAL_CUSTOM_TYPE = "envoy-message";
/** The answer: the session's next assistant message. */
const ASSISTANT_ROLE = "assistant";
/** A call opens with this record and closes with the `toolResult` message of the same id. */
const TOOL_START_CUSTOM_TYPE = "tool_execution_start";
const TOOL_RESULT_ROLE = "toolResult";
/** The role a session claimed, which is the name a message was delivered to. */
const ROLE_CLAIM_CUSTOM_TYPE = "envoy-role-claim";
/** The calls that can hold a turn open: the verdict fails only on a wait one of these held. */
const HOLDING_TOOLS: Readonly<Record<string, true>> = { bash: true, task: true };

const DEFAULT_THRESHOLD_MS = 10 * 60_000;
/** The launch instant every session path carries, as Oh My Pi names the file. */
const LAUNCHED_AT = /(\d{4}-\d{2}-\d{2})T(\d{2})-(\d{2})-(\d{2})-(\d{3})Z/;
/** The session file the daemon launches, against a subagent's file in the directory beside it. */
const ROOT_SESSION = /\/\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-\d{3}Z_[^/]+\.jsonl$/;
/** The session's own id, which the launch names its file, or its subagents' directory, with. */
const SESSION_ID = /_([0-9a-f]{8}-[0-9a-f-]{23,27})(?:\.jsonl$|\/)/;
const DURATION = /^(\d+)(s|m|h|d)$/;
const DURATION_MS: Readonly<Record<string, number>> = {
  s: 1_000,
  m: 60_000,
  h: 3_600_000,
  d: 86_400_000,
};
/** A driver's error code is printed only in this shape, so no message text ever is. */
const SAFE_CODE = /^[A-Z][A-Z0-9_]*$/;
/**
 * The host state pi-envoy stamps on a card it hands the host, as the `handOver` field of the
 * `details` the host persists with the message. It is printed only in this shape, a lower-case
 * word of letters and hyphens, starting with a letter and at most 20 long, so no message text
 * ever is, and a state the plugin adds later prints as it stands.
 */
const SAFE_STATE = /^[a-z][a-z-]{0,19}$/;
/**
 * What a line prints in place of that state: for a card that carries none in that shape, and for
 * a card written before the plugin stamped the state.
 */
const UNSTAMPED = "unstamped";
const SUFFIX = ".jsonl";

const USAGE = `usage: bun scripts/sessions/message-wait.ts --since <duration> [options]

Measures, over the project's Oh My Pi session store, how long each Envoy message waited in the
role session it reached for that session's next assistant message, and which tool call was open
when it arrived.

Options:
  --since <duration>      Required. Measure the sessions launched within this much of now, read
                          from the launch instant in each session's path: 24h, 90m, 2d, 1s.
  --threshold <duration>  Print and count a wait of at least this long. Default 10m.
  --include-subagents     Measure the sessions a role's own task subagents keep as well. By
                          default only the role sessions themselves are measured: the session
                          file the daemon launches, named for its launch.
  --path-contains <text>  Measure only the sessions whose path holds this text, e.g. infra-338.
  --dsn-file <path>       The explicit route: the file holding the store's connection URL.
                          Without it the URL comes from the file SESSION_READER_DSN_FILE names,
                          the deployment's read-only login, and there is no other route.
  --help                  Print this.

Prints one line per wait of at least the threshold, longest first, in two groups: the waits a
bash or task call was open for, which the verdict fails on, and every other wait. Each line carries the
wait, the message's arrival, the calls open at the arrival, the call that took the longest part
of the wait, the host state when the plugin handed the card over, and the session. Then the
arrivals no assistant message ever answered, which are no measured wait, and a verdict line.

The plugin's own notices, and the wake it sends itself, are no Envoy message from another
session: they are in no count and on no line, and no wait is measured for them. A card the
plugin wrote before it stamped the host state prints that state as unstamped.

A wait runs from the message's own arrival, which its record's timestamp gives, to the first
assistant message written after that record. Oh My Pi writes the record when the session folds
the message in, which is after the call that held the turn returned, so the calls open at the
arrival are the ones whose own span holds that instant, not the ones open where the record sits.

Exit status: 0 when the measurement ran, whatever it found; 2 when it could not run.`;

interface Options {
  readonly sinceMs: number;
  readonly thresholdMs: number;
  readonly includeSubagents: boolean;
  readonly pathContains?: string;
  readonly dsnFile?: string;
}

/** A command line the script refuses. Its message is the usage, never an argument's text. */
class UsageError extends Error {}

/** One Envoy message's arrival in a session, and what became of it. */
interface Arrival {
  /** The role the session had claimed, the session's id, and a subagent's name. */
  readonly session: string;
  readonly at: number;
  /** The wait to the session's next assistant message, or null when none followed. */
  readonly waitMs: number | null;
  /** How long the session went on after the arrival; the wait itself once one answered. */
  readonly openMs: number;
  /** The tool calls open at the arrival, in the order they opened. */
  readonly open: readonly string[];
  /**
   * The call that ran for the longest part of the wait, which is what the wait went on, and how
   * much of the wait it took. None when no call ran at all: the session sat idle.
   */
  readonly longest?: { readonly toolName: string; readonly ms: number };
  /** The host state when the plugin handed the card over, printed in the shape above. */
  readonly handOver: string;
}

interface Measurement {
  readonly sessions: number;
  readonly sessionsWithArrivals: number;
  readonly arrivals: readonly Arrival[];
  /** Lines that were no JSON. A session Oh My Pi is mid-write on ends in one. */
  readonly unparsedLines: number;
}

/**
 * A session record. The store's history is older than today's record shapes, so the cast at the
 * parse names only the fields this measurement reads, each `unknown` and checked before use.
 */
interface SessionRecord {
  readonly type?: unknown;
  readonly customType?: unknown;
  readonly timestamp?: unknown;
  /** The card's own fields, which pi-envoy writes and the host persists: `localNotice` marks a
   *  notice of the plugin's own (`LOCAL_ENVOY_NOTICE`, `packages/pi-shared/src/envoy-notice.ts`,
   *  written by `packages/pi-envoy/extensions/envoy.ts` and `src/card-wake.ts`), and `handOver`
   *  the host state at the hand-over. The shapes are read here, never imported: this script
   *  imports nothing of the packages. */
  readonly details?: { readonly localNotice?: unknown; readonly handOver?: unknown };
  readonly message?: { readonly role?: unknown; readonly toolCallId?: unknown };
  readonly data?: {
    readonly toolName?: unknown;
    readonly toolCallId?: unknown;
    readonly role?: unknown;
  };
}

/** The flags as `parseArgs` reads them, before their rules are checked. */
interface Flags {
  readonly since?: string;
  readonly threshold?: string;
  readonly "include-subagents"?: boolean;
  readonly "path-contains"?: string;
  readonly "dsn-file"?: string;
  readonly help?: boolean;
}

function readFlags(argv: readonly string[]): Flags {
  try {
    return parseArgs({
      args: [...argv],
      options: {
        since: { type: "string" },
        threshold: { type: "string" },
        "include-subagents": { type: "boolean" },
        "path-contains": { type: "string" },
        "dsn-file": { type: "string" },
        help: { type: "boolean" },
      },
      strict: true,
    }).values;
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
}

/** A whole number of seconds, minutes, hours or days, in milliseconds. */
function parseDuration(value: string): number {
  const match = DURATION.exec(value);
  const unit = match === null ? undefined : DURATION_MS[match[2] as string];
  if (match === null || unit === undefined) throw new UsageError(USAGE);
  return Number(match[1]) * unit;
}

function parseArguments(argv: readonly string[]): Options | "help" {
  const flags = readFlags(argv);
  if (flags.help === true) return "help";
  const since = flags.since;
  if (since === undefined) throw new UsageError(USAGE);
  const pathContains = flags["path-contains"];
  const dsnFile = flags["dsn-file"];
  if (pathContains === "" || dsnFile === "") throw new UsageError(USAGE);
  const threshold = flags.threshold;
  return {
    sinceMs: parseDuration(since),
    thresholdMs: threshold === undefined ? DEFAULT_THRESHOLD_MS : parseDuration(threshold),
    includeSubagents: flags["include-subagents"] === true,
    ...(pathContains === undefined ? {} : { pathContains }),
    ...(dsnFile === undefined ? {} : { dsnFile }),
  };
}

/** The instant the session at this path was launched, or null when its name carries none. */
function launchedAt(path: string): number | null {
  const match = LAUNCHED_AT.exec(path);
  if (match === null) return null;
  const at = Date.parse(`${match[1]}T${match[2]}:${match[3]}:${match[4]}.${match[5]}Z`);
  return Number.isNaN(at) ? null : at;
}

/** The sessions to measure: launched inside the window, and named as the options ask. */
function sessionsInScope(
  paths: readonly string[],
  options: Options,
  now: number
): readonly string[] {
  const floor = now - options.sinceMs;
  return paths.filter((path) => {
    if (options.pathContains !== undefined && !path.includes(options.pathContains)) return false;
    if (!options.includeSubagents && !ROOT_SESSION.test(path)) return false;
    const at = launchedAt(path);
    return at !== null && at >= floor;
  });
}

/** What the reader calls a session: the role it claimed, its own id, and a subagent's name. */
function sessionLabel(path: string, role: string | undefined): string {
  const id = SESSION_ID.exec(path)?.[1] ?? "unknown-session";
  const name = path.slice(path.lastIndexOf("/") + 1, -SUFFIX.length);
  const subagent = ROOT_SESSION.test(path) ? "" : `/${name}`;
  return `${role ?? "no role claimed"} ${id}${subagent}`;
}

interface SessionMeasurement {
  readonly arrivals: readonly Arrival[];
  readonly unparsed: number;
}

/** A tool call's own span, from its start record to the `toolResult` message that closed it. */
interface ToolCall {
  readonly toolName: string;
  readonly from: number;
  /** The result's instant, or `Infinity` while the text shows the call still running. */
  to: number;
}

/**
 * An arrival as the walk holds it: its instant, the host state the card was handed over in, and
 * the wait once an assistant message lands.
 */
interface PendingArrival {
  readonly at: number;
  readonly handOver: string;
  waitMs: number | null;
}

/** The host state the card carries, in the shape above, or `UNSTAMPED` when it carries none. */
function handOverState(details: SessionRecord["details"]): string {
  const state = details?.handOver;
  return typeof state === "string" && SAFE_STATE.test(state) ? state : UNSTAMPED;
}

/**
 * Every arrival in one session's text.
 *
 * The two halves of an arrival are read differently, because Oh My Pi writes the record when the
 * session folds the message into its context, which is after the call that held the turn
 * returned, while the record's `timestamp` is the message's own arrival. So the wait runs from
 * that timestamp to the first assistant message *written after* the record — the first answer
 * that could have read the message — and the calls open at the arrival are the ones whose own
 * span, start record to `toolResult` of the same id, holds that timestamp. Reading the open call
 * off the record's position instead would find every wait unheld, since the call that held it
 * has closed by then. The arrivals no assistant message follows are the ones nothing answered.
 */
function measureSession(path: string, text: string): SessionMeasurement {
  const calls: ToolCall[] = [];
  const running = new Map<string, ToolCall>();
  const measured: PendingArrival[] = [];
  const waiting: PendingArrival[] = [];
  let role: string | undefined;
  let last = 0;
  let unparsed = 0;
  for (const line of text.split("\n")) {
    if (line === "") continue;
    let record: SessionRecord;
    try {
      // The store's own JSONL, read field by field below: every field is checked before use.
      record = JSON.parse(line) as SessionRecord;
    } catch {
      unparsed += 1;
      continue;
    }
    const at = typeof record.timestamp === "string" ? Date.parse(record.timestamp) : Number.NaN;
    if (!Number.isNaN(at)) last = at;
    if (record.type === "custom") {
      if (record.customType === TOOL_START_CUSTOM_TYPE) {
        const { toolCallId, toolName } = record.data ?? {};
        if (typeof toolCallId === "string" && typeof toolName === "string" && !Number.isNaN(at)) {
          const call: ToolCall = { toolName, from: at, to: Number.POSITIVE_INFINITY };
          calls.push(call);
          running.set(toolCallId, call);
        }
      } else if (record.customType === ROLE_CLAIM_CUSTOM_TYPE) {
        const claimed = record.data?.role;
        if (typeof claimed === "string") role = claimed;
      }
      continue;
    }
    if (record.type === ARRIVAL_TYPE) {
      // A record the plugin marked as a notice of its own is no arrival at all: it is left out of
      // every count and every line, and it ends nobody's wait.
      if (
        record.customType === ARRIVAL_CUSTOM_TYPE &&
        !Number.isNaN(at) &&
        record.details?.localNotice !== true
      ) {
        const arrival: PendingArrival = {
          at,
          handOver: handOverState(record.details),
          waitMs: null,
        };
        measured.push(arrival);
        waiting.push(arrival);
      }
      continue;
    }
    if (record.type !== "message") continue;
    const messageRole = record.message?.role;
    const toolCallId = record.message?.toolCallId;
    if (messageRole === TOOL_RESULT_ROLE && typeof toolCallId === "string") {
      const call = running.get(toolCallId);
      if (call !== undefined) {
        call.to = Number.isNaN(at) ? last : at;
        running.delete(toolCallId);
      }
      continue;
    }
    if (messageRole !== ASSISTANT_ROLE || Number.isNaN(at)) continue;
    for (const arrival of waiting) arrival.waitMs = Math.max(0, at - arrival.at);
    waiting.length = 0;
  }
  const session = sessionLabel(path, role);
  const arrivals = measured.map(({ at, handOver, waitMs }) => {
    const openMs = waitMs ?? Math.max(0, last - at);
    const open: string[] = [];
    let longest: { toolName: string; ms: number } | undefined;
    for (const call of calls) {
      if (call.from <= at && call.to > at) open.push(call.toolName);
      const ms = Math.min(call.to, at + openMs) - Math.max(call.from, at);
      if (ms > 0 && (longest === undefined || ms > longest.ms)) {
        longest = { toolName: call.toolName, ms };
      }
    }
    return {
      session,
      at,
      waitMs,
      openMs,
      open,
      handOver,
      ...(longest === undefined ? {} : { longest }),
    };
  });
  return { arrivals, unparsed };
}

/** The store, read one session at a time: every statement is scoped by `path`. */
interface SessionStore {
  paths(): Promise<readonly string[]>;
  text(path: string): Promise<string>;
}

async function measure(store: SessionStore, options: Options, now: number): Promise<Measurement> {
  const paths = sessionsInScope(await store.paths(), options, now);
  const arrivals: Arrival[] = [];
  let sessionsWithArrivals = 0;
  let unparsedLines = 0;
  for (const path of paths) {
    const measured = measureSession(path, await store.text(path));
    unparsedLines += measured.unparsed;
    if (measured.arrivals.length > 0) sessionsWithArrivals += 1;
    arrivals.push(...measured.arrivals);
  }
  return { sessions: paths.length, sessionsWithArrivals, arrivals, unparsedLines };
}

function formatDuration(ms: number): string {
  const whole = Math.round(ms / 1000);
  const hours = Math.floor(whole / 3600);
  const minutes = Math.floor((whole % 3600) / 60);
  const rest = `${String(minutes).padStart(2, "0")}m${String(whole % 60).padStart(2, "0")}s`;
  return hours > 0 ? `${hours}h${rest}` : rest;
}

/** What a wait line says held it: the calls open at the arrival, then what the wait went on. */
function describe(arrival: Arrival): string {
  const open = arrival.open.length === 0 ? "none" : arrival.open.join(" + ");
  const longest =
    arrival.longest === undefined
      ? "no call ran during the wait"
      : `longest call in the wait ${arrival.longest.toolName} ${formatDuration(arrival.longest.ms)}`;
  return `open: ${open}  ${longest}  hand-over: ${arrival.handOver}`;
}

/** The report, line by line, so a reader and a test see the same run. */
function report(measurement: Measurement, options: Options): string[] {
  const { arrivals } = measurement;
  const answered = arrivals.filter((arrival) => arrival.waitMs !== null);
  const over = answered
    .filter((arrival) => (arrival.waitMs ?? 0) >= options.thresholdMs)
    .sort((a, b) => (b.waitMs ?? 0) - (a.waitMs ?? 0));
  const byHold = (holding: boolean) =>
    over.filter((arrival) => arrival.open.some((tool) => HOLDING_TOOLS[tool] === true) === holding);
  const threshold = formatDuration(options.thresholdMs);
  const scope = [
    `${measurement.sessions} sessions launched within ${formatDuration(options.sinceMs)} of now`,
    options.includeSubagents ? ", subagents included" : "",
    options.pathContains === undefined ? "" : `, path holding ${options.pathContains}`,
    `; ${measurement.sessionsWithArrivals} of them took an Envoy message`,
  ].join("");
  const lines = [
    scope,
    "each line below: the wait, the arrival, the calls open at the arrival (the calls the " +
      "verdict turns on), the call that took the longest part of the wait, the host state when " +
      "the plugin handed the card over, and the session; the plugin's own notices and its own " +
      "wake are in no count and on no line, and a card written before the plugin stamped the " +
      "state prints unstamped",
  ];
  const groups = [
    [
      `waits of ${threshold} or more with a bash or task call open (the verdict fails on these)`,
      true,
    ],
    [`waits of ${threshold} or more with no bash or task call open`, false],
  ] as const;
  for (const [title, holding] of groups) {
    const group = byHold(holding);
    lines.push("", `${title}: ${group.length}`);
    for (const arrival of group) {
      lines.push(
        `  ${formatDuration(arrival.waitMs ?? 0)}  ${new Date(arrival.at).toISOString()}  ` +
          `${describe(arrival)}  ${arrival.session}`
      );
    }
  }
  const unanswered = arrivals
    .filter((arrival) => arrival.waitMs === null)
    .sort((a, b) => b.openMs - a.openMs);
  lines.push("", `arrivals no assistant message answered (no measured wait): ${unanswered.length}`);
  for (const arrival of unanswered) {
    lines.push(
      `  ${formatDuration(arrival.openMs)} to the session's last record  ` +
        `${new Date(arrival.at).toISOString()}  ${describe(arrival)}  ${arrival.session}`
    );
  }
  if (measurement.unparsedLines > 0) {
    lines.push("", `${measurement.unparsedLines} lines of the store were no JSON record`);
  }
  lines.push(
    "",
    `verdict: ${answered.length} arrivals measured, ${over.length} waited ${threshold} or more, ` +
      `${byHold(true).length} of those with a bash or task call open. ` +
      "The measurement passes only while that last count is 0."
  );
  return lines;
}

interface LiveStore extends SessionStore {
  close(): Promise<void>;
}

/** A read the store refused to run, because the transaction it was to run in was not read-only. */
class NotReadOnlyError extends Error {}

/** The transaction options every read is begun with, so the server holds it read-only. */
const READ_ONLY = "read only";

/**
 * Runs `read` in one transaction begun read-only, after confirming from the server, inside that
 * transaction and before anything is read in it, that the begin took. A deployment can point this
 * script at a server or a route that dropped it, and reading on a write-capable connection is what
 * the shared store's pool cannot take. Every read of the store goes through here, so a read added
 * later cannot be the one that skips the confirmation.
 */
async function readOnly<T>(sql: SQL, read: (tx: TransactionSQL) => Promise<T>): Promise<T> {
  return await sql.begin(READ_ONLY, async (tx) => {
    const rows: { transaction_read_only: string }[] = await tx`show transaction_read_only`;
    if (rows[0]?.transaction_read_only !== "on") throw new NotReadOnlyError();
    return await read(tx);
  });
}

/**
 * The live store. Each read is one read-only transaction, which drops that state when it commits;
 * both statements are scoped by `path`, and neither scans the parts table.
 */
function liveStore(dsn: string): LiveStore {
  const sql = new SQL(dsn);
  return {
    async paths() {
      return await readOnly(sql, async (tx) => {
        const rows: { path: string }[] = await tx`select path from omp_session_files order by path`;
        return rows.map((row) => row.path);
      });
    },
    async text(path) {
      return await readOnly(sql, async (tx) => {
        const rows: { text: string | null }[] = await tx`
          select f.content || coalesce(
            (select string_agg(p.content, '' order by p.start_offset)
               from omp_session_files_parts p where p.path = f.path), '') as text
          from omp_session_files f where f.path = ${path}
        `;
        return rows[0]?.text ?? "";
      });
    },
    async close() {
      await sql.close().catch(() => undefined);
    },
  };
}

/** The driver's own error code, when it gave one in the shape above; never a message's text. */
function errorCode(error: unknown): string {
  if (typeof error !== "object" || error === null || !("code" in error)) return "";
  const code = error.code;
  return typeof code === "string" && SAFE_CODE.test(code) ? ` (${code})` : "";
}

async function main(argv: readonly string[]): Promise<number> {
  let options: Options | "help";
  try {
    options = parseArguments(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(error.message);
    return 2;
  }
  if (options === "help") {
    console.log(USAGE);
    return 0;
  }
  const dsnFile = options.dsnFile ?? process.env.SESSION_READER_DSN_FILE;
  if (dsnFile === undefined || dsnFile === "") {
    console.error(
      "unavailable: SESSION_READER_DSN_FILE names no file, so this deployment gives no read " +
        "route to the session store, which is a fault of the deployment to report; --dsn-file " +
        "names the file holding the URL of a read-only login where you have one"
    );
    return 2;
  }
  let dsn: string;
  try {
    dsn = (await readFile(dsnFile, "utf8")).trim();
  } catch (error) {
    console.error(`unavailable: that file could not be read${errorCode(error)}`);
    return 2;
  }
  if (dsn === "") {
    console.error("unavailable: that file is empty");
    return 2;
  }
  const store = liveStore(dsn);
  try {
    for (const line of report(await measure(store, options, Date.now()), options)) {
      console.log(line);
    }
    return 0;
  } catch (error) {
    if (error instanceof NotReadOnlyError) {
      console.error(
        "unavailable: that transaction is not read-only, so the run stopped and reported nothing"
      );
      return 2;
    }
    console.error(`unavailable: the session store could not be read${errorCode(error)}`);
    return 2;
  } finally {
    await store.close();
  }
}

if (import.meta.main) {
  process.exit(await main(Bun.argv.slice(2)));
}

/** `bun e2e/delivery-live-probe.ts`: an agent's client for `GET /ws/delivery`, the websocket the
 *  Delivery and Pipeline pages keep themselves current over. It subscribes one view under a bearer
 *  token and writes one JSON line per frame Dispatch sends, applying each to the body it holds with
 *  the page's own `applyDelta` and `applyRevs`. It reconnects as the page does: at once after
 *  Dispatch's restart close (1012), with backoff after any other close, and in place of a socket
 *  that has carried no frame for two heartbeats; each new connection resumes from the last change
 *  it applied. The watch ends after `--minutes`, on Ctrl-C, or when Dispatch refuses the view.
 *  Then `--body-file` writes the body it holds, and `--compare` reads the matching GET routes for
 *  the last window it applied and writes, for each, whether the pull request ids, run ids and every
 *  measure are equal.
 *
 *    bun e2e/delivery-live-probe.ts --url <dispatch> --token-file <file> \
 *      --view '{"kind":"timeline","window":{"preset":28},"query":""}' --minutes 10 --compare
 *
 *  It exits 0 when every comparison is equal or none was asked for; 1 when one is not, a read
 *  fails, the first connection is refused, Dispatch refuses the view, or a frame breaks the
 *  contract; 2 on a usage error. */
import { writeFileSync } from "node:fs";
import { parseArgs } from "node:util";

import { DELIVERY_LIVE_SILENT_MS } from "../web/src/api/delivery-live";
import { reconnectDelayMs } from "../web/src/api/live";
import type {
  DeliveryLiveBody,
  DeliveryLiveCounts,
  DeliveryLiveDelta,
  DeliveryLiveMap,
  DeliveryLiveRevs,
  DeliveryLiveServerFrame,
  DeliveryLiveSubscribe,
  DeliveryLiveView,
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  DeliveryTimelineResponse,
} from "../web/src/api/types";
import { applyDelta, applyRevs } from "../web/src/features/delivery/lib/applyDelta";

/** A delta's changes to one list: the ids of the items it upserts, whole, and of those that left. */
export interface ProbeListChanges {
  readonly upsert: readonly (string | number)[];
  readonly remove: readonly string[];
}

/** One frame Dispatch sent: when it arrived and its size on the wire in UTF-8 bytes. A snapshot
 *  counts what it holds: its lists' items, each map's keys, and each count map's values by facet.
 *  A delta names what it changes: each list's upserted and removed ids, each map's keys, each
 *  count map's values by facet, and the fields `set` replaces whole. A section the frame does not
 *  carry is null; an error frame adds its code and message, a freshness frame the verdict. */
export interface ProbeFrameLine {
  readonly at: string;
  readonly type: DeliveryLiveServerFrame["type"];
  readonly bytes: number;
  readonly seq: string | null;
  readonly changed_at: string | null;
  readonly prs: number | ProbeListChanges | null;
  readonly runs: number | ProbeListChanges | null;
  readonly session_groups: number | ProbeListChanges | null;
  readonly maps: Readonly<Record<string, number | readonly string[]>> | null;
  readonly counts: Readonly<
    Record<string, Readonly<Record<string, number | readonly string[]>>>
  > | null;
  readonly set: readonly string[] | null;
  readonly code?: string;
  readonly message?: string;
  readonly freshness?: DeliveryTimelineResponse["freshness"];
  readonly stale?: readonly string[];
}

/** A connection opening, and the change it resumes from (null on the first subscribe). */
export interface ProbeOpenLine {
  readonly at: string;
  readonly type: "open";
  readonly resume: string | null;
}

/** A connection closing, by Dispatch or by the probe replacing a silent socket. */
export interface ProbeCloseLine {
  readonly at: string;
  readonly type: "close";
  readonly code: number;
  readonly reason: string;
}

/** The ids a GET answer holds that the probe's body lacks (`missing`), and the reverse (`extra`). */
export interface ProbeIdsComparison {
  readonly equal: boolean;
  readonly missing: readonly string[];
  readonly extra: readonly string[];
}

/** One GET route read for the last window the probe applied, against the body it holds. `prs`
 *  and `runs` are null for a route that answers neither; `measures` lists the JSON path of every
 *  measure (every card, for the pipeline) whose value differs. A refused read carries its error. */
export interface ProbeCompareLine {
  readonly at: string;
  readonly type: "compare";
  readonly route: string;
  readonly status: number;
  readonly equal: boolean;
  readonly prs: ProbeIdsComparison | null;
  readonly runs: ProbeIdsComparison | null;
  readonly measures: { readonly equal: boolean; readonly differ: readonly string[] } | null;
  readonly error?: { readonly code: string; readonly message: string };
}

export type ProbeLine = ProbeFrameLine | ProbeOpenLine | ProbeCloseLine | ProbeCompareLine;

/** What `--body-file` holds: the last change the probe applied, the body and its revisions. */
export interface ProbeHeld {
  readonly seq: string;
  readonly changed_at: string | null;
  readonly body: DeliveryLiveBody;
  readonly revs: DeliveryLiveRevs;
}

const USAGE =
  "usage: bun e2e/delivery-live-probe.ts --url <dispatch> --token-file <file> --view <json> --minutes <n> [--compare] [--body-file <file>]";

/** The probe's one subscription. */
const SUB = "probe";
/** Dispatch closes every socket with this code when it shuts down; the page reconnects at once. */
const SERVICE_RESTART = 1012;
/** The code the page closes a silent socket with before it opens another (api/delivery-live.ts). */
const REPLACED = 4000;
/** The error codes that refuse what the probe sent: its view or its frame. Waiting cannot change
 *  either answer, so the watch ends; any other code (DELIVERY_NOT_CONFIGURED) is answered again
 *  once Dispatch can, and the watch goes on. */
const REFUSALS: Readonly<Record<string, true>> = { INVALID_FRAME: true, INVALID_QUERY: true };

const MAPS = [
  "issue_titles",
  "session_titles",
  "session_pr_counts",
  "components",
] as const satisfies readonly DeliveryLiveMap[];
const COUNTS = ["facet_counts", "color_counts"] as const satisfies readonly DeliveryLiveCounts[];

type MeasureField = Exclude<keyof DeliveryMeasuresResponse, "window" | "computed_at" | "freshness">;
type PipelineField = Exclude<keyof DeliveryPipelineResponse, "computed_at" | "freshness">;

/** The measures every comparison holds equal: each field but the window, computed_at and
 *  freshness, which a live body keeps as they were last sent. A record, so a field the response
 *  gains fails the type check until it is named here. */
const MEASURE_FIELDS: Readonly<Record<MeasureField, true>> = {
  flags_source: true,
  measures: true,
  status: true,
  targets: true,
  unowned_p0: true,
};
/** The pipeline's days and cards, every field but computed_at and freshness. */
const PIPELINE_FIELDS: Readonly<Record<PipelineField, true>> = {
  days: true,
  merge_to_production: true,
  opened_to_merge: true,
  pr_checks: true,
  production_apply: true,
  staging_apply: true,
  staging_e2e: true,
  targets: true,
};

const SERVER_FRAME_TYPES: Readonly<Record<DeliveryLiveServerFrame["type"], true>> = {
  delta: true,
  error: true,
  freshness: true,
  heartbeat: true,
  snapshot: true,
};

// Bun's WebSocket client accepts request headers; the repository's DOM WebSocket type does not.
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

class UsageError extends Error {}

interface Options {
  readonly url: URL;
  readonly tokenFile: string;
  readonly view: DeliveryLiveView;
  readonly minutes: number;
  readonly compare: boolean;
  readonly bodyFile: string | undefined;
}

type Sections = Pick<ProbeFrameLine, "prs" | "runs" | "session_groups" | "maps" | "counts" | "set">;

const now = () => new Date().toISOString();
const write = (line: ProbeLine) => console.log(JSON.stringify(line));
const message = (error: unknown) => (error instanceof Error ? error.message : String(error));

function isServerFrame(value: unknown): value is DeliveryLiveServerFrame {
  return (
    typeof value === "object" &&
    value !== null &&
    "type" in value &&
    typeof value.type === "string" &&
    Object.hasOwn(SERVER_FRAME_TYPES, value.type)
  );
}

const isTimeline = (body: DeliveryLiveBody): body is DeliveryTimelineResponse => "prs" in body;
const isMeasures = (body: DeliveryLiveBody): body is DeliveryMeasuresResponse =>
  "flags_source" in body;
const isPipeline = (body: DeliveryLiveBody): body is DeliveryPipelineResponse => "days" in body;

function mapValues<V, W>(
  record: Readonly<Record<string, V>>,
  fn: (value: V) => W
): Record<string, W> {
  return Object.fromEntries(Object.entries(record).map(([key, value]) => [key, fn(value)]));
}

function readArgs(argv: readonly string[]) {
  try {
    return parseArgs({
      args: [...argv],
      allowPositionals: false,
      options: {
        "body-file": { type: "string" },
        compare: { type: "boolean" },
        minutes: { type: "string" },
        "token-file": { type: "string" },
        url: { type: "string" },
        view: { type: "string" },
      },
      strict: true,
    }).values;
  } catch (error) {
    throw new UsageError(message(error));
  }
}

function required(value: string | undefined, flag: string): string {
  if (value === undefined || value === "") throw new UsageError(`${flag} is required`);
  return value;
}

/** The view `--view` names. The probe checks what it reads itself (the kind, a timeline or
 *  measures view's window and query, a pipeline view's `at`); Dispatch judges the rest and answers
 *  a view it refuses with an error frame. */
function parseView(text: string): DeliveryLiveView {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (error) {
    throw new UsageError(`--view is not JSON: ${message(error)}`);
  }
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new UsageError("--view is a JSON object");
  }
  const kind = "kind" in value ? value.kind : undefined;
  if (kind === "timeline" || kind === "measures") {
    const window = "window" in value ? value.window : undefined;
    if (typeof window !== "object" || window === null) {
      throw new UsageError(
        `a ${kind} view's "window" is {"preset": 7 | 14 | 28 | 84} or {"from": ..., "to": ...}`
      );
    }
    if (!("query" in value) || typeof value.query !== "string") {
      throw new UsageError(
        `a ${kind} view's "query" is the GET route's query string without from and to ("" for none)`
      );
    }
  } else if (kind === "pipeline") {
    if ("at" in value && typeof value.at !== "string") {
      throw new UsageError(`a pipeline view's "at" is an RFC 3339 time`);
    }
  } else {
    throw new UsageError(`--view's "kind" is timeline, measures or pipeline, not ${String(kind)}`);
  }
  // Checked above as far as the probe reads it; Dispatch refuses the rest of a malformed view.
  const view = value as DeliveryLiveView;
  return view;
}

function parseOptions(argv: readonly string[]): Options {
  const values = readArgs(argv);
  const rawUrl = required(values.url, "--url");
  let url: URL;
  try {
    url = new URL(rawUrl);
  } catch {
    throw new UsageError(`--url is not a URL: ${rawUrl}`);
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new UsageError(`--url is Dispatch's http or https address, not ${rawUrl}`);
  }
  const rawMinutes = required(values.minutes, "--minutes");
  const minutes = Number(rawMinutes);
  if (!Number.isFinite(minutes) || minutes <= 0) {
    throw new UsageError(`--minutes is a number of minutes above 0, not ${rawMinutes}`);
  }
  return {
    bodyFile: values["body-file"],
    compare: values.compare === true,
    minutes,
    tokenFile: required(values["token-file"], "--token-file"),
    url,
    view: parseView(required(values.view, "--view")),
  };
}

async function readToken(file: string): Promise<string> {
  let token: string;
  try {
    token = (await Bun.file(file).text()).trim();
  } catch (error) {
    throw new UsageError(`cannot read --token-file ${file}: ${message(error)}`);
  }
  if (token === "") throw new UsageError(`--token-file ${file} holds no token`);
  return token;
}

function listChanges(
  changes:
    | {
        readonly upsert: readonly { readonly id: string | number }[];
        readonly remove: readonly string[];
      }
    | undefined
): ProbeListChanges | null {
  if (changes === undefined) return null;
  return { remove: [...changes.remove], upsert: changes.upsert.map((item) => item.id) };
}

/** What a snapshot's body holds, counted. Only a timeline body has lists, maps and counts. */
function holds(body: DeliveryLiveBody): Sections {
  if (!isTimeline(body)) {
    return { counts: null, maps: null, prs: null, runs: null, session_groups: null, set: null };
  }
  return {
    counts: Object.fromEntries(
      COUNTS.map((name) => [name, mapValues(body[name], (values) => Object.keys(values).length)])
    ),
    maps: Object.fromEntries(MAPS.map((name) => [name, Object.keys(body[name]).length])),
    prs: body.prs.length,
    runs: body.runs.length,
    session_groups: body.session_groups.length,
    set: null,
  };
}

/** What a delta changes, by id and key. */
function changes(delta: DeliveryLiveDelta): Sections {
  return {
    counts: mapValues(delta.counts, (facets) =>
      mapValues(facets ?? {}, (values) => Object.keys(values))
    ),
    maps: mapValues(delta.maps, (entries) => Object.keys(entries ?? {})),
    prs: listChanges(delta.items.prs),
    runs: listChanges(delta.items.runs),
    session_groups: listChanges(delta.items.session_groups),
    set: Object.keys(delta.set),
  };
}

function frameLine(frame: DeliveryLiveServerFrame, bytes: number): ProbeFrameLine {
  const line: ProbeFrameLine = {
    at: now(),
    bytes,
    changed_at: null,
    counts: null,
    maps: null,
    prs: null,
    runs: null,
    seq: null,
    session_groups: null,
    set: null,
    type: frame.type,
  };
  switch (frame.type) {
    case "snapshot":
      return { ...line, ...holds(frame.body), changed_at: frame.changed_at, seq: frame.seq };
    case "delta":
      return { ...line, ...changes(frame), changed_at: frame.changed_at, seq: frame.seq };
    case "error":
      return { ...line, code: frame.code, message: frame.message };
    case "freshness":
      return { ...line, freshness: frame.freshness, stale: frame.stale };
    case "heartbeat":
      return line;
  }
}

/** Watches `options.view` until `--minutes` pass, a Ctrl-C, or Dispatch refuses the view, and
 *  resolves with the body the probe holds then, or null when no snapshot arrived. Rejects when the
 *  first connection is refused, the view is refused, or a frame breaks the contract. */
async function watch(options: Options, token: string): Promise<ProbeHeld | null> {
  const liveUrl = new URL("/ws/delivery", options.url);
  liveUrl.protocol = options.url.protocol === "https:" ? "wss:" : "ws:";
  let held: ProbeHeld | null = null;
  let socket: WebSocket | null = null;
  let opened = false;
  let attempt = 0;
  let lastFrameAt = 0;
  let cancelReconnect: (() => void) | undefined;
  // Settles once, with why the watch ended early, or with nothing once its time is up.
  const ended = Promise.withResolvers<Error | undefined>();
  const fail = (reason: string) => ended.resolve(new Error(reason));

  const subscribeFrame = (): DeliveryLiveSubscribe =>
    held === null
      ? { sub: SUB, type: "subscribe", view: options.view }
      : {
          resume: { revs: held.revs, seq: held.seq },
          sub: SUB,
          type: "subscribe",
          view: options.view,
        };

  const apply = (frame: DeliveryLiveServerFrame) => {
    if (frame.type === "error") {
      if (frame.sub === SUB && Object.hasOwn(REFUSALS, frame.code)) {
        fail(`Dispatch refused the view: ${frame.code}: ${frame.message}`);
      }
      return;
    }
    if (frame.type !== "snapshot" && frame.type !== "delta") return;
    if (frame.sub !== SUB) {
      fail(`a ${frame.type} for subscription ${frame.sub}, which the probe never made`);
      return;
    }
    if (frame.type === "snapshot") {
      held = { body: frame.body, changed_at: frame.changed_at, revs: frame.revs, seq: frame.seq };
      return;
    }
    if (held === null) {
      fail("a delta arrived before its subscription's snapshot");
      return;
    }
    held = {
      body: applyDelta(held.body, frame),
      changed_at: frame.changed_at,
      revs: applyRevs(held.revs, frame.revs),
      seq: frame.seq,
    };
  };

  const receive = (data: unknown) => {
    if (typeof data !== "string") {
      fail("Dispatch sent a binary frame; the live route sends text");
      return;
    }
    lastFrameAt = Date.now();
    attempt = 0;
    let frame: unknown;
    try {
      frame = JSON.parse(data);
    } catch (error) {
      fail(`a frame that is not JSON (${message(error)})`);
      return;
    }
    if (!isServerFrame(frame)) {
      fail(`a frame of no type the contract names: ${data.slice(0, 200)}`);
      return;
    }
    write(frameLine(frame, Buffer.byteLength(data)));
    apply(frame);
  };

  const open = () => {
    cancelReconnect = undefined;
    lastFrameAt = Date.now();
    const next = new BunWebSocket(liveUrl.href, { headers: { Authorization: `Bearer ${token}` } });
    socket = next;
    // Each listener ignores a socket the probe has already let go, whose events still arrive.
    next.addEventListener("open", () => {
      if (socket !== next) return;
      opened = true;
      lastFrameAt = Date.now();
      write({ at: now(), resume: held?.seq ?? null, type: "open" });
      next.send(JSON.stringify(subscribeFrame()));
    });
    next.addEventListener("message", (event) => {
      if (socket === next) receive(event.data);
    });
    next.addEventListener("close", (event) => {
      if (socket === next) lost(event.code, event.reason);
    });
  };

  const lost = (code: number, reason: string) => {
    socket = null;
    write({ at: now(), code, reason, type: "close" });
    if (!opened) {
      fail(`Dispatch refused the connection to ${liveUrl.href}: ${reason || `close ${code}`}`);
      return;
    }
    if (code === SERVICE_RESTART) {
      open();
      return;
    }
    const delay = reconnectDelayMs(attempt) * (0.75 + Math.random() * 0.5);
    attempt += 1;
    const handle = setTimeout(open, delay);
    cancelReconnect = () => clearTimeout(handle);
  };

  // A socket that carried no frame for two heartbeats is dead without knowing it: replace it.
  const silence = setInterval(() => {
    if (socket === null || Date.now() - lastFrameAt <= DELIVERY_LIVE_SILENT_MS) return;
    const silent = socket;
    socket = null;
    const reason = `no frame for ${DELIVERY_LIVE_SILENT_MS / 1000} s`;
    write({ at: now(), code: REPLACED, reason, type: "close" });
    silent.close(REPLACED, reason);
    open();
  }, 1_000);
  const stop = () => ended.resolve(undefined);
  const timer = setTimeout(stop, options.minutes * 60_000);
  process.once("SIGINT", stop);
  process.once("SIGTERM", stop);
  const shutDown = () => {
    clearTimeout(timer);
    clearInterval(silence);
    cancelReconnect?.();
    process.off("SIGINT", stop);
    process.off("SIGTERM", stop);
    const closing = socket;
    socket = null;
    closing?.close(1000);
  };

  open();
  const failure = await ended.promise;
  shutDown();
  if (failure !== undefined) throw failure;
  return held;
}

/** The comparison of one route's answer with the body the probe holds. */
type Checked = Pick<ProbeCompareLine, "prs" | "runs" | "measures">;

interface Read {
  readonly route: string;
  readonly check: (answer: unknown) => Checked;
}

function read<A>(route: string, check: (answer: A) => Checked): Read {
  // The route answers its own body, as its contract types it; the check reads the fields it names.
  return { check: (answer) => check(answer as A), route };
}

function compareIds(
  held: readonly (string | number)[],
  answer: readonly (string | number)[]
): ProbeIdsComparison {
  const mine = new Set(held.map(String));
  const theirs = new Set(answer.map(String));
  const missing = [...theirs].filter((id) => !mine.has(id)).sort();
  const extra = [...mine].filter((id) => !theirs.has(id)).sort();
  return { equal: missing.length === 0 && extra.length === 0, extra, missing };
}

/** The JSON paths at or below `path` where `a` and `b` differ; an array whose length differs is
 *  one difference, at its own path. */
function differences(a: unknown, b: unknown, path: string): string[] {
  if (a === b) return [];
  if (Array.isArray(a) && Array.isArray(b)) {
    if (a.length !== b.length) return [path];
    return a.flatMap((item, index) => differences(item, b[index], `${path}[${index}]`));
  }
  if (
    typeof a === "object" &&
    a !== null &&
    !Array.isArray(a) &&
    typeof b === "object" &&
    b !== null &&
    !Array.isArray(b)
  ) {
    const left = new Map<string, unknown>(Object.entries(a));
    const right = new Map<string, unknown>(Object.entries(b));
    const keys = [...new Set([...left.keys(), ...right.keys()])].sort();
    return keys.flatMap((key) => differences(left.get(key), right.get(key), `${path}.${key}`));
  }
  return [path];
}

function compareFields<F extends string>(
  held: Readonly<Record<F, unknown>>,
  answer: Readonly<Record<F, unknown>>,
  fields: Readonly<Record<F, true>>
): NonNullable<Checked["measures"]> {
  // Object.keys of a literal record holds exactly the keys its type names.
  const names = Object.keys(fields) as F[];
  const differ = names.flatMap((field) => differences(held[field], answer[field], field));
  return { differ, equal: differ.length === 0 };
}

/** The GET routes that answer the figures `body` holds, for the window it last applied. */
function readsFor(view: DeliveryLiveView, body: DeliveryLiveBody): Read[] {
  const query = (base: string, window: DeliveryTimelineResponse["window"]) => {
    const params = new URLSearchParams(base);
    params.set("from", window.from);
    params.set("to", window.to);
    return params.toString();
  };
  switch (view.kind) {
    case "timeline": {
      if (!isTimeline(body)) throw new Error("the timeline view's snapshot is not a timeline body");
      const params = query(view.query, body.window);
      return [
        read<DeliveryTimelineResponse>(`/api/v1/delivery/timeline?${params}`, (answer) => ({
          measures: compareFields(body.measures, answer.measures, MEASURE_FIELDS),
          prs: compareIds(
            body.prs.map((pr) => pr.id),
            answer.prs.map((pr) => pr.id)
          ),
          runs: compareIds(
            body.runs.map((run) => run.id),
            answer.runs.map((run) => run.id)
          ),
        })),
        read<DeliveryMeasuresResponse>(`/api/v1/delivery/measures?${params}`, (answer) => ({
          measures: compareFields(body.measures, answer, MEASURE_FIELDS),
          prs: null,
          runs: null,
        })),
      ];
    }
    case "measures": {
      if (!isMeasures(body)) throw new Error("the measures view's snapshot is not a measures body");
      return [
        read<DeliveryMeasuresResponse>(
          `/api/v1/delivery/measures?${query(view.query, body.window)}`,
          (answer) => ({
            measures: compareFields(body, answer, MEASURE_FIELDS),
            prs: null,
            runs: null,
          })
        ),
      ];
    }
    case "pipeline": {
      if (!isPipeline(body)) throw new Error("the pipeline view's snapshot is not a pipeline body");
      const route =
        view.at === undefined
          ? "/api/v1/delivery/pipeline"
          : `/api/v1/delivery/pipeline?${new URLSearchParams({ at: view.at })}`;
      return [
        read<DeliveryPipelineResponse>(route, (answer) => ({
          measures: compareFields(body, answer, PIPELINE_FIELDS),
          prs: null,
          runs: null,
        })),
      ];
    }
  }
}

async function compareRead(base: URL, token: string, route: Read): Promise<ProbeCompareLine> {
  const response = await fetch(new URL(route.route, base), {
    headers: { Authorization: `Bearer ${token}` },
  });
  const text = await response.text();
  const refused = (code: string, reason: string): ProbeCompareLine => ({
    at: now(),
    equal: false,
    error: { code, message: reason },
    measures: null,
    prs: null,
    route: route.route,
    runs: null,
    status: response.status,
    type: "compare",
  });
  let answer: unknown;
  try {
    answer = JSON.parse(text);
  } catch {
    return refused(response.ok ? "NOT_JSON" : `HTTP_${response.status}`, text.slice(0, 200));
  }
  if (!response.ok) {
    if (typeof answer !== "object" || answer === null) {
      return refused(`HTTP_${response.status}`, text.slice(0, 200));
    }
    return refused(
      "code" in answer && typeof answer.code === "string" ? answer.code : `HTTP_${response.status}`,
      "error" in answer && typeof answer.error === "string" ? answer.error : text.slice(0, 200)
    );
  }
  const checked = route.check(answer);
  const equal = [checked.prs, checked.runs, checked.measures].every(
    (part) => part === null || part.equal
  );
  return {
    at: now(),
    equal,
    ...checked,
    route: route.route,
    status: response.status,
    type: "compare",
  };
}

/** Reads each GET route for `held`'s window and writes its comparison; true when all are equal. */
async function compare(options: Options, token: string, held: ProbeHeld): Promise<boolean> {
  let equal = true;
  for (const route of readsFor(options.view, held.body)) {
    const line = await compareRead(options.url, token, route);
    write(line);
    equal &&= line.equal;
  }
  return equal;
}

async function main(argv: readonly string[]): Promise<number> {
  let options: Options;
  let token: string;
  try {
    options = parseOptions(argv);
    token = await readToken(options.tokenFile);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(`${error.message}\n${USAGE}`);
    return 2;
  }
  try {
    const held = await watch(options, token);
    if (options.bodyFile !== undefined) {
      writeFileSync(options.bodyFile, `${JSON.stringify(held)}\n`);
    }
    if (!options.compare) return 0;
    if (held === null) {
      console.error("delivery-live-probe: nothing to compare: no snapshot arrived");
      return 1;
    }
    return (await compare(options, token, held)) ? 0 : 1;
  } catch (error) {
    console.error(`delivery-live-probe: ${message(error)}`);
    return 1;
  }
}

process.exit(await main(Bun.argv.slice(2)));

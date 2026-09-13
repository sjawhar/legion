// Relays one smoke rig's own Dispatch issue events — the root issue up.sh recorded and its
// transitive children — from the Dispatch HTTP API into the rig's isolated NATS, as the exact
// envelope the production outbox (packages/envoy/internal/dispatch/outbox/publisher.go) publishes.
// SMOKE_WEBHOOK_MODE=isolated starts it; nothing else reaches the rig, so two rigs on one machine
// never consume each other's issues, and no production NATS or personal GitHub identity is needed.

import { type ConnectionOptions, connect } from "nats";
import { z } from "zod";
import type { Envelope } from "../../packages/contracts/src/envelope";
import { dispatchIssueSubject } from "../../packages/contracts/src/subject";
import { ISSUE_KEY_PATTERN } from "../../packages/daemon/src/daemon/legion-state";

export const POLL_INTERVAL_MS = 2_000;
/** Dispatch caps `GET /api/v1/issues/<key>/events?limit=` at 200. */
export const EVENT_PAGE_LIMIT = 200;
export const RETRY_INITIAL_MS = 1_000;
export const RETRY_CAP_MS = 30_000;
const REQUEST_TIMEOUT_MS = 10_000;

export interface RelayConfig {
  rootIssue: string;
  project: string;
  dispatchUrl: string;
  token: string;
  downstreamUrl: string;
}

/** One row of `GET /api/v1/issues/<key>/events`: the Go `model.Event`, which the outbox marshals
 * whole into the envelope's `payload` string. Only the fields the relay reads are validated;
 * everything else passes through untouched. */
const DispatchEventRowSchema = z
  .object({
    id: z.number().int().positive(),
    issue_key: z.string().regex(ISSUE_KEY_PATTERN),
    seq: z.number().int().positive(),
    type: z.string().min(1),
    actor: z.object({ kind: z.string(), id: z.string() }).passthrough(),
    notify: z.boolean(),
    created_at: z.string().min(1),
    payload: z.unknown(),
  })
  .passthrough();
export type DispatchEventRow = z.infer<typeof DispatchEventRowSchema>;
const EventPageSchema = z.array(DispatchEventRowSchema);
const IssueSummariesSchema = z.array(z.object({ key: z.string() }).passthrough());

export interface RelayPublisher {
  publish(
    subject: string,
    data: Uint8Array,
    options?: { msgID?: string }
  ): Promise<{ seq: number; duplicate: boolean }>;
}

export interface RelayConnection {
  jetstream(): RelayPublisher;
  drain(): Promise<void>;
}

export interface RelayDeps {
  fetch: (url: string, init: RequestInit) => Promise<Response>;
  connect: (options: ConnectionOptions) => Promise<RelayConnection>;
  sleep: (ms: number, signal: AbortSignal) => Promise<void>;
  log: (line: string) => void;
  signal: AbortSignal;
}

/** A condition waiting cannot heal: a rejected bearer, or a root issue Dispatch does not know. */
export class RelayUnhealthy extends Error {
  constructor(message: string) {
    super(message);
    this.name = "RelayUnhealthy";
  }
}

export function relayConfigFromEnvironment(
  environment: Record<string, string | undefined>
): RelayConfig {
  const rootIssue = environment.SMOKE_ROOT_ISSUE?.trim() ?? "";
  if (!ISSUE_KEY_PATTERN.test(rootIssue)) {
    throw new Error(
      `SMOKE_ROOT_ISSUE must be a literal Dispatch issue key such as LEGSMOKE-7 (got ${JSON.stringify(rootIssue)})`
    );
  }
  const downstreamUrl = environment.SMOKE_RIG_NATS?.trim() ?? "";
  if (!downstreamUrl) throw new Error("SMOKE_RIG_NATS is required");
  const dispatchUrl = (environment.DISPATCH_URL?.trim() ?? "").replace(/\/+$/, "");
  if (!dispatchUrl) throw new Error("DISPATCH_URL is required");
  const token = environment.DISPATCH_TOKEN?.trim() ?? "";
  if (!token) throw new Error("DISPATCH_TOKEN is required");
  return {
    rootIssue,
    project: rootIssue.slice(0, rootIssue.indexOf("-")),
    dispatchUrl,
    token,
    downstreamUrl,
  };
}

function payloadString(payload: unknown, key: string): string {
  if (typeof payload !== "object" || payload === null || Array.isArray(payload)) return "";
  const value = (payload as Record<string, unknown>)[key];
  return typeof value === "string" ? value : "";
}

function truncateRunes(value: string, limit: number): string {
  const runes = Array.from(value);
  return runes.length <= limit ? value : runes.slice(0, limit).join("");
}

/** Port of publisher.go `payloadSummary` for issue-owned events (the owner is the issue key). */
function payloadSummary(row: DispatchEventRow): string {
  const kind = row.type.replaceAll(".", " ");
  let text = "";
  if (row.type.startsWith("ask.")) {
    text = truncateRunes(payloadString(row.payload, "question"), 120);
  } else if (row.type === "message.created") {
    text = payloadString(row.payload, "body");
  } else if (row.type.startsWith("comment.") || row.type.startsWith("suggestion.")) {
    text = payloadString(row.payload, "body");
  } else if (row.type === "subscription.removed") {
    text = payloadString(row.payload, "session_id");
  }
  let summary = `${row.issue_key} ${kind}`;
  if (text !== "") summary += `: ${text}`;
  return truncateRunes(summary.split(/\s+/).filter(Boolean).join(" "), 160);
}

/** The envelope publisher.go `envelope()` builds for an issue event, field for field. */
export function envelopeForEvent(row: DispatchEventRow): Envelope {
  const eventId = `dispatch-${row.id}`;
  const envelope: Envelope = {
    event_id: eventId,
    source: "dispatch",
    source_event_id: String(row.id),
    topic: dispatchIssueSubject(row.issue_key, row.type),
    dedupe_key: eventId,
    issued_at: Date.parse(row.created_at),
    payload_summary: payloadSummary(row),
    payload: JSON.stringify(row),
    trace_id: eventId,
  };
  if (row.actor.kind === "session") envelope.source_session = row.actor.id;
  let inReplyTo = "";
  if (row.type === "ask.answered" || row.type === "ask.resolved") {
    inReplyTo = payloadString(row.payload, "id");
  } else if (row.type === "comment.created") {
    inReplyTo = payloadString(row.payload, "ask_id");
  } else if (row.type === "message.created" || row.type === "message.answered") {
    inReplyTo = payloadString(row.payload, "in_reply_to");
  }
  if (inReplyTo !== "") envelope.in_reply_to = inReplyTo;
  if (row.type.startsWith("ask.")) {
    const urgency = payloadString(row.payload, "urgency");
    if (urgency === "low" || urgency === "med" || urgency === "high" || urgency === "blocking") {
      envelope.urgency = urgency;
    }
  }
  return envelope;
}

/** What packages/envoy/internal/bus/nats.go sets as the JetStream message id for a Dispatch
 * envelope: the stream's duplicate window makes a replayed row a no-op. */
export function messageId(envelope: Envelope): string {
  return `${envelope.dedupe_key}:${envelope.topic}`;
}

/** Dispatch allocates event ids in commit order across every issue, so ascending id is the
 * order the production outbox publishes in; within one key it is also ascending seq. */
export function orderEvents(batches: DispatchEventRow[][]): DispatchEventRow[] {
  return batches.flat().sort((left, right) => left.id - right.id);
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  if (signal.aborted) {
    resolve();
    return promise;
  }
  const onAbort = () => {
    clearTimeout(timer);
    resolve();
  };
  const timer = setTimeout(() => {
    signal.removeEventListener("abort", onAbort);
    resolve();
  }, ms);
  signal.addEventListener("abort", onAbort, { once: true });
  return promise;
}

function defaultDeps(): RelayDeps {
  const controller = new AbortController();
  const stop = () => controller.abort();
  process.once("SIGINT", stop);
  process.once("SIGTERM", stop);
  return {
    fetch: (url, init) => fetch(url, init),
    connect,
    sleep,
    log: (line) => console.log(line),
    signal: controller.signal,
  };
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export async function runRelay(
  config: RelayConfig,
  deps: RelayDeps = defaultDeps()
): Promise<void> {
  const encoder = new TextEncoder();
  // key -> parent key (null for the root); insertion order is discovery order.
  const tracked = new Map<string, string | null>([[config.rootIssue, null]]);
  // key -> seq of the last row whose publish was acked; only an ack advances it.
  const cursors = new Map<string, number>();
  let rootVerified = false;
  let ready = false;
  let attempt = 0;
  let backoff = RETRY_INITIAL_MS;

  const request = async (path: string): Promise<unknown> => {
    const url = `${config.dispatchUrl}${path}`;
    const response = await deps.fetch(url, {
      headers: { Authorization: `Bearer ${config.token}`, Accept: "application/json" },
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
    if (response.status === 401 || response.status === 403) {
      throw new RelayUnhealthy(
        `Dispatch answered ${response.status} for GET ${path}: the bearer token is not accepted`
      );
    }
    if (response.status === 404 && path === `/api/v1/issues/${config.rootIssue}`) {
      throw new RelayUnhealthy(
        `root issue ${config.rootIssue} does not exist on ${config.dispatchUrl} (404)`
      );
    }
    if (!response.ok) {
      throw new Error(`GET ${path} answered HTTP ${response.status}`);
    }
    try {
      return await response.json();
    } catch (error) {
      throw new Error(`GET ${path} returned a non-JSON body: ${errorMessage(error)}`);
    }
  };

  const requestParsed = async <Shape>(path: string, schema: z.ZodType<Shape>): Promise<Shape> => {
    const parsed = schema.safeParse(await request(path));
    if (!parsed.success) {
      throw new Error(`GET ${path} returned an unexpected shape: ${parsed.error.message}`);
    }
    return parsed.data;
  };

  const newEvents = async (key: string): Promise<DispatchEventRow[]> => {
    const rows: DispatchEventRow[] = [];
    let after = cursors.get(key) ?? 0;
    for (;;) {
      const page = await requestParsed(
        `/api/v1/issues/${key}/events?after=${after}&limit=${EVENT_PAGE_LIMIT}`,
        EventPageSchema
      );
      rows.push(...page);
      if (page.length < EVENT_PAGE_LIMIT) return rows;
      after = page[page.length - 1].seq;
    }
  };

  const children = async (parent: string): Promise<string[]> => {
    const summaries = await requestParsed(
      `/api/v1/issues?project=${config.project}&parent=${parent}`,
      IssueSummariesSchema
    );
    return summaries.map((summary) => summary.key).filter((key) => ISSUE_KEY_PATTERN.test(key));
  };

  const tick = async (): Promise<void> => {
    if (!rootVerified) {
      await request(`/api/v1/issues/${config.rootIssue}`);
      rootVerified = true;
    }
    // Events before discovery, per level: a row that names a child was appended after that
    // child's own issue.created, so a child first seen here is always fetched in this same
    // tick and its earlier rows sort ahead of the row that named it.
    const batches: DispatchEventRow[][] = [];
    let pending = [...tracked.keys()];
    while (pending.length > 0) {
      const keys = pending;
      pending = [];
      for (const key of keys) batches.push(await newEvents(key));
      for (const key of keys) {
        for (const child of await children(key)) {
          if (tracked.has(child)) continue;
          tracked.set(child, key);
          pending.push(child);
          deps.log(`RELAY TRACKING ${child} parent=${key}`);
        }
      }
    }
    for (const row of orderEvents(batches)) {
      const envelope = envelopeForEvent(row);
      const ack = await publisher.publish(
        envelope.topic,
        encoder.encode(JSON.stringify(envelope)),
        { msgID: messageId(envelope) }
      );
      cursors.set(row.issue_key, row.seq);
      deps.log(
        `RELAYED subject=${envelope.topic} event=${envelope.event_id} seq=${row.seq} stream_seq=${ack.seq} duplicate=${ack.duplicate}`
      );
    }
  };

  const connection = await deps.connect({
    servers: config.downstreamUrl,
    name: `legion-smoke-issue-relay-${config.rootIssue}`,
    reconnect: true,
    maxReconnectAttempts: -1,
    reconnectTimeWait: 2_000,
  });
  const publisher = connection.jetstream();
  try {
    while (!deps.signal.aborted) {
      try {
        await tick();
      } catch (error) {
        if (error instanceof RelayUnhealthy) throw error;
        attempt += 1;
        deps.log(`RELAY RETRY attempt=${attempt} next=${backoff}ms: ${errorMessage(error)}`);
        await deps.sleep(backoff, deps.signal);
        backoff = Math.min(backoff * 2, RETRY_CAP_MS);
        continue;
      }
      if (attempt > 0) {
        deps.log(`RELAY RECOVERED after ${attempt} attempts`);
        attempt = 0;
        backoff = RETRY_INITIAL_MS;
      }
      if (!ready) {
        ready = true;
        deps.log(
          `RELAY READY root=${config.rootIssue} project=${config.project} dispatch=${config.dispatchUrl} downstream=${config.downstreamUrl}`
        );
      }
      await deps.sleep(POLL_INTERVAL_MS, deps.signal);
    }
  } finally {
    await connection.drain();
  }
}

if (import.meta.main) {
  try {
    await runRelay(relayConfigFromEnvironment(process.env));
  } catch (error) {
    console.error(`RELAY UNHEALTHY ${errorMessage(error)}`);
    process.exitCode = 1;
  }
}

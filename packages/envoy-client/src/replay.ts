import {
  DELIVERY_DUPLICATE_WINDOW_MS,
  dedupeKeyNamesItsEvent,
  type Envelope,
  EnvelopeSchema,
  ROLE_TOPIC_PREFIX,
} from "@legion/contracts";
import { z } from "zod";
import {
  type DedupeIdentity,
  type DeliveryDedupe,
  type InboundEnvelope,
  inboundTimestamp,
  senderLabel,
} from "./delivery";
import { messageFor } from "./errors";
import {
  EnvoyApiError,
  type EnvoyClient,
  REPLAY_UNAVAILABLE_CODE,
  type ReplayMessage,
  type ReplayResult,
  type ReplaySubjectInput,
} from "./transport";

/**
 * How long the ledger remembers a frame, from when it recorded it. The stream keeps a message no
 * longer (`DELIVERY_DUPLICATE_WINDOW_MS` is its retention), so a frame the ledger has forgotten can
 * no longer come back from a sweep. A restore never refreshes an entry's time.
 */
export const REPLAY_LEDGER_TTL_MS = DELIVERY_DUPLICATE_WINDOW_MS;

/**
 * The most frames the ledger holds; past it the oldest is forgotten first. This is the documented
 * limit of the once-only promise: a session shown more frames than this inside 72 hours can be
 * shown the oldest of them again by a sweep that re-reads it. It also bounds what a host persists.
 */
export const REPLAY_LEDGER_LIMIT = 20_000;

/** The most replay calls one sweep makes; a backlog past them waits for the next trigger. */
export const REPLAY_PAGES_PER_SWEEP = 20;

/**
 * How many times a sweep tries to show a stored frame before it asks the host to show a notice
 * naming it instead: a frame the host can never show must not hold its subject's cursor forever.
 */
export const REPLAY_FAILURES_BEFORE_NOTICE = 3;

/** The listener refuses a replay of more subjects than this with a 400. */
const REPLAY_MAX_SUBJECTS = 32;

/** The dedupe key the listener's role arbiter marks a role-lane forward with. */
const ROLE_FORWARD_KEY_PREFIX = "envoy.role.forward.";

/**
 * One frame the session finished with: shown, answered with a correlated reply, or reported in a
 * failure notice. It is the frame's `event_id`, and its `dedupe_key` only when that key names its
 * event (`dedupeKeyNamesItsEvent`), so a re-send of it under a new event id is recognised too.
 */
export const ReplayLedgerEntrySchema = z.object({
  event_id: z.string().min(1).optional(),
  dedupe_key: z.string().min(1).optional(),
  at: z.number(),
});

export type ReplayLedgerEntry = z.infer<typeof ReplayLedgerEntrySchema>;

/**
 * A followed subject's place in the stream: `since`, the sequence the session started following
 * it at, and `after`, the last sequence handled. Both are absent until the first sweep that reads
 * the subject after it was followed.
 */
export const ReplayCursorSchema = z.object({
  since: z.number().int().nonnegative().optional(),
  after: z.number().int().nonnegative().optional(),
});

export type ReplayCursor = z.infer<typeof ReplayCursorSchema>;

/**
 * The cursors and the stream they belong to. Every subject the session follows is a key, with or
 * without a cursor yet, so a host can rebuild what it followed from the newest state alone after
 * the listener reaped its interests.
 */
export const ReplayStateSchema = z.object({
  stream_created: z.string().optional(),
  subjects: z.record(z.string(), ReplayCursorSchema),
});

export type ReplayState = z.infer<typeof ReplayStateSchema>;

/**
 * What a host keeps to restore an engine: the newest state, and the ledger. A host that rewrites
 * one file holds exactly this; a host that appends entries keeps the ledger as one entry per
 * record and gathers them back into this shape.
 */
export const ReplaySnapshotSchema = z.object({
  state: ReplayStateSchema.optional(),
  ledger: z.array(ReplayLedgerEntrySchema),
});

export type ReplaySnapshot = z.infer<typeof ReplaySnapshotSchema>;

/** A stored message a sweep asks the host to show. */
export interface ReplayFrame {
  readonly envelope: Envelope;
  /** The envelope JSON as the stream stored it, for a renderer that reads raw frames. */
  readonly data: string;
  readonly seq: number;
  /** The concrete subject it was published on. */
  readonly subject: string;
  /** The followed subject it was read under; a wildcard topic when the session follows one. */
  readonly followed: string;
  /** When the stream stored it (RFC 3339): when it was published while the session was away. */
  readonly time: string;
  /** Where the followed subject's cursor started: a frame at or below it predates the follow. */
  readonly since: number | undefined;
}

/**
 * `shown`: the host took the frame, so the ledger records it. `suppressed`: the host's renderer
 * skips it for this session (`notify: false`, its own event, another session's follow); nothing is
 * recorded, so a copy on another subject that the renderer does show still reaches the session.
 * `failed`, or a throw: the frame stays for a later sweep.
 */
export type ReplayDeliveryOutcome = "shown" | "suppressed" | "failed";

/**
 * Something the session is told once, because replay could not keep its promise: the stream was
 * replaced, retention dropped messages before they were read, or the listener does not replay.
 * `message` is ready to show; `kind` lets a host word it its own way.
 */
export type ReplayNotice =
  | { readonly kind: "stream_recreated"; readonly message: string }
  | { readonly kind: "retention_gap"; readonly message: string }
  | { readonly kind: "not_served"; readonly message: string }
  | {
      readonly kind: "unavailable";
      readonly status: number;
      readonly error: string;
      readonly message: string;
    };

/** The parts of an envelope a failure notice names; a live host's tolerant parse has them. */
export type ReplayNoticeEnvelope = Pick<
  InboundEnvelope,
  | "event_id"
  | "source"
  | "source_session"
  | "source_event_id"
  | "sender"
  | "dedupe_key"
  | "payload_summary"
  | "issued_at"
>;

/**
 * What the host shows in place of a frame it could not show: enough to find the message again,
 * since the frame itself is not coming back. The host renders it.
 */
export interface ReplayFailureNotice {
  readonly eventId: string;
  readonly sender: string;
  readonly summary: string;
  /** When it was published: the stream's time for a stored frame, `issued_at` for a live one. */
  readonly publishedAt: string;
  readonly error: string;
  /** How the session reads the message itself: `dispatch read` for a Dispatch event. */
  readonly readWith: "dispatch read" | "envoy_inbox";
  /** The concrete subject, for a stored frame. */
  readonly subject?: string;
  readonly envelope: ReplayNoticeEnvelope;
}

/**
 * One persistence write. A host that appends entries appends one per `delivered` record and one
 * for `state` when it is present; a host that rewrites a file writes `snapshot()`. `state` is
 * present only when it differs from the last state written or restored, so an unchanged sweep
 * writes nothing at all.
 */
export interface ReplayChange {
  readonly delivered: readonly ReplayLedgerEntry[];
  readonly state: ReplayState | undefined;
  snapshot(): ReplaySnapshot;
}

/**
 * The host side of the engine. Each callback runs for the session that was current when the
 * engine started the work that calls it; the engine re-reads `sessionID()` before each delivery
 * and each write and drops the work when it changed.
 */
export interface ReplayHost {
  /** The session the host is on now; undefined or empty before it has one. */
  sessionID(): string | undefined;
  /** The subjects the session follows and wants replayed: its agent subject and its topics. */
  followedSubjects(): readonly string[];
  /** Shows a stored frame; a throw is `failed`. It must run nothing a live frame would run. */
  deliver(frame: ReplayFrame): Promise<ReplayDeliveryOutcome>;
  /** Shows a failure notice; a throw means it was not shown. */
  showFailureNotice(notice: ReplayFailureNotice): Promise<void>;
  /** The notice could not be shown either: the host warns (log and UI). It is tried again. */
  failureNoticeFailed(notice: ReplayFailureNotice, error: unknown): void;
  /** Shows a notice the session is told once. */
  notify(notice: ReplayNotice): void;
  log(message: string, error?: unknown): void;
  /** Persists a change; a throw is logged, and its ledger records go with the next write. */
  write(change: ReplayChange): void | Promise<void>;
}

export interface ReplayEngineOptions {
  readonly host: ReplayHost;
  readonly transport: Pick<EnvoyClient, "replay">;
  /** The live path's claims: a frame it is delivering now is left for a later sweep. */
  readonly dedupe?: Pick<DeliveryDedupe, "status">;
  readonly now?: () => number;
  /** The most messages per replay call; the listener's default when absent. */
  readonly maxMessages?: number;
}

/**
 * Shows a session each message the stream stored for it while its live subscriptions were down,
 * once. The live path stays as it is and tells the engine what it showed; sweeps read the stream
 * after a cursor per followed subject and show what neither path showed yet.
 */
export interface ReplayEngine {
  /**
   * Reads and shows what the stream stored after the cursors. One runs at a time; a call while
   * one runs queues at most one more, and every call resolves when the run it joined ends. It
   * never rejects.
   */
  sweep(): Promise<void>;
  /**
   * Takes the host's followed subjects now: a new one is listed without a cursor (the next sweep
   * starts it at the stream's end, with no history), a dropped one loses its cursor. Persisted
   * at once, so an unfollow survives a restart that comes before the next sweep.
   */
  followedChanged(): Promise<void>;
  /** Whether the ledger holds the frame: a live repeat of it after a restart is dropped. */
  delivered(frame: DedupeIdentity): boolean;
  /** Records a frame the live path's host took, so no sweep shows it again. */
  recordDelivered(frame: DedupeIdentity): Promise<void>;
  /**
   * Shows the failure notice at once for a live frame no sweep will retry (a role forward after
   * its receipt, a subject the stream does not keep, or one `retries` says no sweep will read),
   * and records it when shown.
   */
  reportLiveFailure(envelope: ReplayNoticeEnvelope, error: unknown): Promise<void>;
  /**
   * Whether a sweep will show a frame on the followed subject `followed` that the host failed to
   * show live: the latest replay call succeeded, a call since the last failed one answered
   * `followed` without refusing it, and `followed` has a cursor. A host's live path asks this when
   * a frame fails, so a frame no sweep will show (the listener does not replay or refuses the
   * subject, or the subject has no cursor yet) gets its failure notice at once instead of being
   * dropped silently.
   */
  retries(followed: string): boolean;
  /**
   * Replaces everything the engine holds with what a host persisted for the session it is on now.
   * Ledger entries recorded 72 hours ago or more are dropped; the state is kept at any age, so a
   * session back after longer still follows its topics. Work in flight for before is dropped.
   */
  restore(snapshot: ReplaySnapshot): void;
  /** The current state, as the next write would persist it. */
  state(): ReplayState;
}

interface Ledger {
  has(identity: LedgerIdentity): boolean;
  record(identity: LedgerIdentity): ReplayLedgerEntry | undefined;
  restore(entries: readonly ReplayLedgerEntry[]): void;
  list(): ReplayLedgerEntry[];
}

interface LedgerIdentity {
  readonly event_id?: string;
  readonly dedupe_key?: string;
}

/** A stored message read in a sweep but not yet handled, with every followed subject it came on. */
interface Buffered {
  readonly message: ReplayMessage;
  readonly subjects: string[];
}

/** A followed subject's reading inside one sweep: how far it has been read, and whether past. */
interface Reading {
  read: number | undefined;
  more: boolean;
}

type Handled = "done" | "hole" | "stopped";

export function createReplayEngine(options: ReplayEngineOptions): ReplayEngine {
  const { host, transport, dedupe } = options;
  const now = options.now ?? Date.now;
  const ledger = createLedger(now);
  const cursors = new Map<string, ReplayCursor>();
  let streamCreated: string | undefined;
  // A frame's failures by its stream sequence; in memory only, so a restart gives it three more.
  const failures = new Map<number, { count: number; error: string }>();
  // The ledger records no write has persisted yet, and the state JSON the last write persisted.
  let unwritten: ReplayLedgerEntry[] = [];
  let writtenState: string | undefined;
  let writes: Promise<void> = Promise.resolve();
  // Bumped by restore: work started before it belongs to another session's record.
  let epoch = 0;
  // The listener outcome the session was last told of, until a replay call succeeds.
  let listenerNoticed: string | undefined;
  // The subjects the replay calls answered without refusing, since the latest one that failed.
  const answered = new Set<string>();
  let running: Promise<void> | undefined;
  let rerun: Promise<void> | undefined;

  const safely = (what: string, call: () => void) => {
    try {
      call();
    } catch (error) {
      try {
        host.log(`replay: ${what} failed`, error);
      } catch {
        // A host whose log throws has nowhere left to report to.
      }
    }
  };
  const log = (message: string, error?: unknown) => safely("log", () => host.log(message, error));
  const notify = (notice: ReplayNotice) => safely("notice", () => host.notify(notice));

  // A host whose session read throws has no session to work for, so its work stops as on a switch.
  const currentSession = (): string | undefined => {
    try {
      return host.sessionID();
    } catch {
      return undefined;
    }
  };
  const bind = (): { readonly sessionID: string; readonly alive: () => boolean } | undefined => {
    const sessionID = currentSession();
    if (sessionID === undefined || sessionID === "") return undefined;
    const bound = epoch;
    return { sessionID, alive: () => bound === epoch && currentSession() === sessionID };
  };

  // Keys sorted, fields in one order: equal states serialise equally, so a comparison of the text
  // says whether the state changed.
  const stateJSON = (): string => {
    const subjects: Record<string, ReplayCursor> = {};
    for (const subject of [...cursors.keys()].sort()) {
      const cursor = cursors.get(subject) ?? {};
      subjects[subject] = {
        ...(cursor.since === undefined ? {} : { since: cursor.since }),
        ...(cursor.after === undefined ? {} : { after: cursor.after }),
      };
    }
    return JSON.stringify({
      ...(streamCreated === undefined ? {} : { stream_created: streamCreated }),
      subjects,
    });
  };

  const record = (identity: LedgerIdentity) => {
    const entry = ledger.record(identity);
    if (entry !== undefined) unwritten.push(entry);
  };

  // Writes run one after another in call order, so a host that rewrites a file never has an older
  // snapshot land after a newer one.
  const flush = (alive: () => boolean): Promise<void> => {
    writes = writes.then(async () => {
      if (!alive()) return;
      const json = stateJSON();
      const delivered = unwritten;
      if (json === writtenState && delivered.length === 0) return;
      unwritten = [];
      const state = json === writtenState ? undefined : (JSON.parse(json) as ReplayState);
      try {
        await host.write({
          delivered,
          state,
          snapshot: () => ({ state: JSON.parse(json) as ReplayState, ledger: ledger.list() }),
        });
        writtenState = json;
      } catch (error) {
        unwritten = [...delivered, ...unwritten];
        log("replay: could not persist the replay state; the next write carries it", error);
      }
    });
    return writes;
  };

  const syncFollowed = () => {
    const followed = new Set(host.followedSubjects());
    for (const subject of [...cursors.keys()]) {
      if (!followed.has(subject)) cursors.delete(subject);
    }
    for (const subject of followed) {
      if (!cursors.has(subject)) cursors.set(subject, {});
    }
  };

  const failureNotice = (
    envelope: ReplayNoticeEnvelope,
    publishedAt: string,
    error: string,
    subject?: string
  ): ReplayFailureNotice => ({
    eventId: envelope.event_id ?? "unknown",
    sender: senderLabel(envelope),
    summary: envelope.payload_summary ?? "unknown",
    publishedAt,
    error,
    readWith: envelope.source === "dispatch" ? "dispatch read" : "envoy_inbox",
    ...(subject === undefined ? {} : { subject }),
    envelope,
  });

  // A shown notice settles the frame for good, here and after a restart; one that fails keeps it
  // outstanding, so the next sweep, or nothing for a live frame, tries again.
  const showFailureNotice = async (
    notice: ReplayFailureNotice,
    alive: () => boolean
  ): Promise<Handled> => {
    try {
      await host.showFailureNotice(notice);
    } catch (error) {
      if (!alive()) return "stopped";
      safely("failure notice warning", () => host.failureNoticeFailed(notice, error));
      return "hole";
    }
    if (!alive()) return "stopped";
    record(ledgerIdentity(notice.envelope));
    await flush(alive);
    return "done";
  };

  const handle = async (
    seq: number,
    buffered: Buffered,
    alive: () => boolean
  ): Promise<Handled> => {
    const { message } = buffered;
    const envelope = parseEnvelope(message.data);
    if (envelope === undefined) {
      log(`replay: skipping stored message ${seq} on ${message.subject}: not an Envoy envelope`);
      return "done";
    }
    // Role messages stay live only: the holder answered the forward's receipt, or it went on.
    if (isRoleForward(envelope)) return "done";
    const identity = ledgerIdentity(envelope);
    if (ledger.has(identity)) {
      failures.delete(seq);
      return "done";
    }
    // The live path is delivering a copy now; whether it shows it is not known yet.
    if (dedupe?.status(envelope) === "pending") return "hole";
    if (!alive()) return "stopped";
    const failure = failures.get(seq);
    if (failure !== undefined && failure.count >= REPLAY_FAILURES_BEFORE_NOTICE) {
      return showFailureNotice(
        failureNotice(envelope, message.time, failure.error, message.subject),
        alive
      );
    }
    const followed = buffered.subjects[0] ?? message.subject;
    let outcome: ReplayDeliveryOutcome;
    let error = "the host could not show it";
    try {
      outcome = await host.deliver({
        envelope,
        data: message.data,
        seq,
        subject: message.subject,
        followed,
        time: message.time,
        since: cursors.get(followed)?.since,
      });
    } catch (thrown) {
      outcome = "failed";
      error = messageFor(thrown);
    }
    if (!alive()) return "stopped";
    if (outcome === "shown") {
      failures.delete(seq);
      record(identity);
      await flush(alive);
      return "done";
    }
    if (outcome === "suppressed") {
      failures.delete(seq);
      return "done";
    }
    const count = (failure?.count ?? 0) + 1;
    failures.set(seq, { count, error });
    if (count >= REPLAY_FAILURES_BEFORE_NOTICE) {
      return showFailureNotice(
        failureNotice(envelope, message.time, error, message.subject),
        alive
      );
    }
    log(`replay: could not show stored message ${seq} (${envelope.event_id}): ${error}`);
    return "hole";
  };

  const listenerFailed = (error: unknown) => {
    if (error instanceof EnvoyApiError) {
      const status = error.details.status;
      if (status === 404) {
        if (listenerNoticed === "not_served") return;
        listenerNoticed = "not_served";
        notify({
          kind: "not_served",
          message:
            "This session's Envoy listener does not serve replay (POST /v1/replay answered 404), so messages published while its connection is down are not recovered.",
        });
        return;
      }
      if (
        (status === 503 && error.code === REPLAY_UNAVAILABLE_CODE) ||
        status === 401 ||
        status === 403
      ) {
        const key = `unavailable:${status}:${error.message}`;
        if (listenerNoticed === key) return;
        listenerNoticed = key;
        notify({
          kind: "unavailable",
          status,
          error: error.message,
          message: `Envoy replay is unavailable (${status}: ${error.message}), so messages published while this session's connection is down are not recovered until it answers.`,
        });
        return;
      }
    }
    log("replay: the listener's replay failed; the next trigger tries again", error);
  };

  const sweepOnce = async (): Promise<void> => {
    const binding = bind();
    if (binding === undefined) return;
    const { sessionID, alive } = binding;
    syncFollowed();
    const readings = new Map<string, Reading>();
    for (const [subject, cursor] of cursors) {
      readings.set(subject, { read: cursor.after, more: true });
    }
    const buffer = new Map<number, Buffered>();
    const holes = new Map<string, number>();
    let gapNoticed = false;

    for (let page = 0; page < REPLAY_PAGES_PER_SWEEP; page += 1) {
      // The subject read least far goes first: the listener reads subjects in request order under
      // one budget, and the first always gets a message, so the lowest reading moves every page.
      const asking = [...readings]
        .filter(([, reading]) => reading.more)
        .sort(([, a], [, b]) => (a.read ?? -1) - (b.read ?? -1))
        .slice(0, REPLAY_MAX_SUBJECTS);
      if (asking.length === 0) break;
      let response: ReplayResult;
      try {
        response = await transport.replay({
          sessionID,
          subjects: asking.map(
            ([subject, reading]): ReplaySubjectInput =>
              reading.read === undefined ? { subject } : { subject, after: reading.read }
          ),
          ...(options.maxMessages === undefined ? {} : { maxMessages: options.maxMessages }),
        });
      } catch (error) {
        if (alive()) {
          answered.clear();
          listenerFailed(error);
        }
        return;
      }
      if (!alive()) return;
      listenerNoticed = undefined;
      for (const result of response.subjects) {
        if (result.refused === undefined) answered.add(result.subject);
        else answered.delete(result.subject);
      }

      const { stream } = response;
      if (streamCreated !== undefined && streamCreated !== "" && stream.created !== streamCreated) {
        // Sequences of the old stream mean nothing in the new one: every cursor starts again at
        // its end, as a new follow does, and what the old stream held for the session is gone.
        streamCreated = stream.created;
        for (const subject of cursors.keys()) {
          cursors.set(subject, { since: stream.last_seq, after: stream.last_seq });
        }
        failures.clear();
        notify({
          kind: "stream_recreated",
          message:
            "Envoy's notification stream was recreated, so messages published to this session while its connection was down before that may be lost.",
        });
        await flush(alive);
        return;
      }
      streamCreated = stream.created;

      for (const [, reading] of asking) reading.more = false;
      for (const result of response.subjects) {
        const reading = readings.get(result.subject);
        if (reading === undefined || result.refused !== undefined) continue;
        if (reading.read === undefined) {
          // No history before the follow: the cursor starts where the stream ends now.
          const cursor = cursors.get(result.subject);
          if (cursor !== undefined && cursor.after === undefined) {
            cursors.set(result.subject, { since: result.next_after, after: result.next_after });
          }
          reading.read = result.next_after;
          continue;
        }
        if (!gapNoticed && reading.read < stream.first_seq - 1) {
          gapNoticed = true;
          notify({
            kind: "retention_gap",
            message:
              "The notification stream no longer holds some messages published to this session while its connection was down: messages older than 72 hours may be missing.",
          });
        }
        for (const message of result.messages) {
          if (message.seq <= reading.read) continue;
          const buffered = buffer.get(message.seq);
          if (buffered === undefined) {
            buffer.set(message.seq, { message, subjects: [result.subject] });
          } else if (!buffered.subjects.includes(result.subject)) {
            buffered.subjects.push(result.subject);
          }
        }
        reading.read = Math.max(reading.read, result.next_after);
        reading.more = result.more;
      }

      // A subject cut short may still hold messages above where it was read to, and they come
      // before anything later on another subject: only what is below every such point goes now.
      let bound = Number.POSITIVE_INFINITY;
      for (const reading of readings.values()) {
        if (reading.more && reading.read !== undefined) bound = Math.min(bound, reading.read);
      }
      const ready = [...buffer.keys()].filter((seq) => seq <= bound).sort((a, b) => a - b);
      for (const seq of ready) {
        const buffered = buffer.get(seq);
        if (buffered === undefined) continue;
        buffer.delete(seq);
        if (!buffered.subjects.some((subject) => cursors.has(subject))) continue;
        const handled = await handle(seq, buffered, alive);
        if (handled === "stopped") return;
        if (handled === "hole") {
          for (const subject of buffered.subjects) {
            if (!holes.has(subject)) holes.set(subject, seq);
          }
        }
      }

      if (!alive()) return;
      // A cursor goes no further than what was handled: the bound, and before its first hole.
      for (const [subject, reading] of readings) {
        const cursor = cursors.get(subject);
        if (cursor?.after === undefined || reading.read === undefined) continue;
        let after = Math.min(reading.read, bound);
        const hole = holes.get(subject);
        if (hole !== undefined) after = Math.min(after, hole - 1);
        if (after > cursor.after) cursors.set(subject, { ...cursor, after });
      }
      await flush(alive);
      if (![...readings.values()].some((reading) => reading.more)) break;
    }
  };

  const runSweep = async (): Promise<void> => {
    try {
      await sweepOnce();
    } catch (error) {
      log("replay: a sweep failed", error);
    }
  };

  const sweep = (): Promise<void> => {
    if (running === undefined) {
      running = runSweep().finally(() => {
        running = undefined;
      });
      return running;
    }
    rerun ??= running.then(() => {
      rerun = undefined;
      return sweep();
    });
    return rerun;
  };

  return {
    sweep,
    async followedChanged() {
      const alive = bind()?.alive;
      if (alive === undefined) return;
      try {
        syncFollowed();
        await flush(alive);
      } catch (error) {
        log("replay: could not take the followed subjects", error);
      }
    },
    delivered: (frame) => ledger.has(ledgerIdentity(frame)),
    async recordDelivered(frame) {
      const alive = bind()?.alive;
      if (alive === undefined) return;
      record(ledgerIdentity(frame));
      await flush(alive);
    },
    async reportLiveFailure(envelope, error) {
      const alive = bind()?.alive;
      if (alive === undefined) return;
      try {
        await showFailureNotice(
          failureNotice(envelope, inboundTimestamp(envelope.issued_at), messageFor(error)),
          alive
        );
      } catch (thrown) {
        log("replay: could not report a failed live frame", thrown);
      }
    },
    retries: (followed) => answered.has(followed) && cursors.get(followed)?.after !== undefined,
    restore(snapshot) {
      epoch += 1;
      ledger.restore(snapshot.ledger);
      cursors.clear();
      streamCreated = snapshot.state?.stream_created;
      for (const [subject, cursor] of Object.entries(snapshot.state?.subjects ?? {})) {
        cursors.set(subject, { ...cursor });
      }
      writtenState = snapshot.state === undefined ? undefined : stateJSON();
      unwritten = [];
      failures.clear();
      listenerNoticed = undefined;
      answered.clear();
    },
    state: () => JSON.parse(stateJSON()) as ReplayState,
  };
}

/** Whether a frame is a role lane's: those stay live only, so a sweep passes them over. */
export function isRoleForward(envelope: {
  readonly dedupe_key?: string | undefined;
  readonly topic?: string | undefined;
}): boolean {
  return (
    envelope.dedupe_key?.startsWith(ROLE_FORWARD_KEY_PREFIX) === true ||
    envelope.topic?.startsWith(ROLE_TOPIC_PREFIX) === true
  );
}

function ledgerIdentity(frame: DedupeIdentity): LedgerIdentity {
  return {
    ...(frame.event_id === undefined || frame.event_id === "" ? {} : { event_id: frame.event_id }),
    ...(frame.dedupe_key === undefined || !dedupeKeyNamesItsEvent(frame)
      ? {}
      : { dedupe_key: frame.dedupe_key }),
  };
}

function parseEnvelope(data: string): Envelope | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return undefined;
  }
  const result = EnvelopeSchema.safeParse(parsed);
  return result.success ? result.data : undefined;
}

function createLedger(now: () => number): Ledger {
  // Insertion order is recording order, so the expired entries and the oldest one the limit drops
  // are always at the front. Each index maps a field to the entry that holds it.
  let entries = new Set<ReplayLedgerEntry>();
  const byEvent = new Map<string, ReplayLedgerEntry>();
  const byKey = new Map<string, ReplayLedgerEntry>();

  const drop = (entry: ReplayLedgerEntry) => {
    entries.delete(entry);
    if (entry.event_id !== undefined && byEvent.get(entry.event_id) === entry) {
      byEvent.delete(entry.event_id);
    }
    if (entry.dedupe_key !== undefined && byKey.get(entry.dedupe_key) === entry) {
      byKey.delete(entry.dedupe_key);
    }
  };
  const add = (entry: ReplayLedgerEntry) => {
    if (entries.size >= REPLAY_LEDGER_LIMIT) {
      const oldest = entries.values().next();
      if (oldest.done !== true) drop(oldest.value);
    }
    entries.add(entry);
    if (entry.event_id !== undefined) byEvent.set(entry.event_id, entry);
    if (entry.dedupe_key !== undefined) byKey.set(entry.dedupe_key, entry);
  };
  const expire = () => {
    const at = now();
    for (const entry of entries) {
      if (at - entry.at < REPLAY_LEDGER_TTL_MS) break;
      drop(entry);
    }
  };
  const has = (identity: LedgerIdentity) => {
    expire();
    return (
      (identity.event_id !== undefined && byEvent.has(identity.event_id)) ||
      (identity.dedupe_key !== undefined && byKey.has(identity.dedupe_key))
    );
  };

  return {
    has,
    record(identity) {
      if (identity.event_id === undefined && identity.dedupe_key === undefined) return undefined;
      if (has(identity)) return undefined;
      const entry = { ...identity, at: now() };
      add(entry);
      return entry;
    },
    restore(restored) {
      entries = new Set();
      byEvent.clear();
      byKey.clear();
      const at = now();
      const kept = restored
        .filter(
          (entry) =>
            at - entry.at < REPLAY_LEDGER_TTL_MS &&
            (entry.event_id !== undefined || entry.dedupe_key !== undefined)
        )
        .sort((a, b) => a.at - b.at);
      for (const entry of kept) add({ ...entry });
    },
    list() {
      expire();
      return [...entries];
    },
  };
}

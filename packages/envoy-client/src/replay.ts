import {
  AGENT_TOPIC_PREFIX,
  DELIVERY_DUPLICATE_WINDOW_MS,
  dedupeKeyNamesItsEvent,
  type Envelope,
  EnvelopeSchema,
  LEGION_NOTICE_TOPIC_PREFIX,
  ROLE_TOPIC_PREFIX,
} from "@legion/contracts";
import { encode } from "@toon-format/toon";
import { z } from "zod";
import {
  type DedupeIdentity,
  type DeliveryDedupe,
  type InboundEnvelope,
  inboundTimestamp,
  type RenderInboundResult,
  senderLabel,
} from "./delivery";
import { subscriptionRemovedTopics } from "./dispatch-subscribe";
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

/**
 * How long a cursor that only followed the stream's end may go unwritten. A sweep that handled no
 * message still moves every cursor to the stream's last sequence, which other subjects' traffic
 * moves; that alone is not written at once, only with the next write or once the state last
 * written is this old, so a restart's cursors lag the stream by no more than this and its
 * retention check (a cursor below the oldest message the stream holds) stays accurate. A retention
 * gap's cursor move is written at once, with its notice pending in the same write, so a resume
 * neither loses the notice nor finds the gap again.
 */
export const REPLAY_STATE_REFRESH_MS = 60 * 60 * 1000;

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
 * A one-time notice the session has not been told yet, kept in the state written with the cursor
 * change that raised it: a process that ends before the notice is raised raises it after the
 * restart, at its first sweep, instead of losing it.
 */
const ReplayPendingNoticeSchema = z.enum(["retention_gap", "stream_recreated"]);

type ReplayPendingNotice = z.infer<typeof ReplayPendingNoticeSchema>;

/** What each pending notice tells the session. */
const PENDING_NOTICE_MESSAGES: Record<ReplayPendingNotice, string> = {
  stream_recreated:
    "Envoy's notification stream was recreated, so messages published to this session while its connection was down before that may be lost.",
  retention_gap:
    "The notification stream no longer holds some messages published to this session while its connection was down: messages past the stream's 72-hour age or its size limit may be missing.",
};

/**
 * The cursors and the stream they belong to. Every subject the session follows is a key, with or
 * without a cursor yet, so a host can rebuild what it followed from the newest state alone after
 * the listener reaped its interests. The stream is known by `stream_id` (its `envoy_stream_id`
 * metadata) when the listener answers one, and by `stream_created` otherwise. `notices` lists the
 * notices still to be told (`ReplayPendingNoticeSchema`), and is absent when there are none; a kind
 * this build does not know is skipped on restore, so a state a later build wrote still parses.
 */
export const ReplayStateSchema = z.object({
  stream_id: z.string().optional(),
  stream_created: z.string().optional(),
  subjects: z.record(z.string(), ReplayCursorSchema),
  notices: z.array(z.string()).optional(),
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
 * `shown`: the host wrote the frame to the session's record, so the ledger records it.
 * `queued`: the host handed the frame on but has not written it yet (Oh My Pi holds a card sent
 * mid-turn in memory until the turn takes it), and it claimed the frame in the engine's `dedupe`:
 * nothing is recorded and the frame's subjects keep their cursors before it, so a process that
 * ends first leaves it to the next sweep after the restart; while the claim is pending every sweep
 * leaves it there. Once the host wrote it, it calls `recordDelivered` and settles the claim; if it
 * learns the frame was dropped, it releases the claim and a sweep shows it again. A host may also
 * return `queued` for a frame it did not hand at all and could not hold (Oh My Pi's inert card of
 * a frame with no ledger key, sent while the host is busy): it released the claim, so the next
 * sweep shows the frame again.
 * `suppressed`: the host's renderer skips it for this session (`notify: false`, its own event,
 * another session's follow); nothing is recorded, so a copy on another subject that the renderer
 * does show still reaches the session. `failed`, or a throw: the frame stays for a later sweep.
 */
export type ReplayDeliveryOutcome = "shown" | "queued" | "suppressed" | "failed";

/**
 * What `showFailureNotice` resolves: `shown` (or nothing) when the notice is written, `queued`
 * when the host handed it on, claimed the notice's frame in the engine's `dedupe` and records it
 * itself once written, as for a `queued` frame.
 */
export type ReplayNoticeOutcome = "shown" | "queued";

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
  | "topic"
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
 * present only when it differs from the last state written or restored. A sweep that handled no
 * message, found no retention gap, and changed nothing but where its cursors stand writes nothing
 * (see `REPLAY_STATE_REFRESH_MS`), so an unchanged sweep writes nothing at all. A retention gap's
 * cursor move, or a recreated stream's cursor reset, is written with its notice pending in the
 * same write, before the notice is raised, so a resume neither loses the notice nor finds the gap
 * again.
 */
export interface ReplayChange {
  readonly delivered: readonly ReplayLedgerEntry[];
  readonly state: ReplayState | undefined;
  snapshot(): ReplaySnapshot;
}

/**
 * The host side of the engine. Each callback runs for the session that was current when the
 * engine started the work that calls it; the engine re-reads `sessionID()` before each delivery
 * and each write and drops the work when it changed (`writeSessionID()` for `recordDeliveredFor`).
 */
export interface ReplayHost {
  /** The session the host is on now; undefined or empty before it has one. */
  sessionID(): string | undefined;
  /**
   * The session whose record `write` persists to now: a host shutting down still writes it while
   * `sessionID()` already answers none, so no new work starts. `sessionID()` when absent. Only
   * `recordDeliveredFor` reads it.
   */
  writeSessionID?(): string | undefined;
  /** The subjects the session follows and wants replayed: its agent subject and its topics. */
  followedSubjects(): readonly string[];
  /** Shows a stored frame; a throw is `failed`. It must run nothing a live frame would run. */
  deliver(frame: ReplayFrame): Promise<ReplayDeliveryOutcome>;
  /**
   * Shows a failure notice; a throw means it was not shown. A host that resolves `queued` records
   * it later (`ReplayNoticeOutcome`); one that resolves nothing showed it.
   */
  showFailureNotice(notice: ReplayFailureNotice): Promise<ReplayNoticeOutcome> | Promise<void>;
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
  /**
   * The host's claims: a frame the live path is delivering now, or one the host handed on and has
   * not written yet (`queued`), is left for a later sweep.
   */
  readonly dedupe?: Pick<DeliveryDedupe, "pending">;
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
  /**
   * Records a frame the host wrote to the session's record: one the live path showed, or one a
   * delivery or notice answered `queued` for, so no sweep shows it again. It never rejects: a
   * write that fails is logged, and its record goes with the next write.
   */
  recordDelivered(frame: DedupeIdentity): Promise<void>;
  /**
   * Records a frame for the session `sessionID`, as `recordDelivered` does, while the host may be
   * shutting down: it records only while the host's `writeSessionID()` names `sessionID` and no
   * `restore` has moved the engine since, and the write goes ahead even once `sessionID()` answers
   * none. The ledger holds the frame from the call on, so a sweep passes it over before the write
   * lands. It never rejects.
   */
  recordDeliveredFor(sessionID: string, frame: DedupeIdentity): Promise<void>;
  /**
   * The host failed to show a live frame on the followed subject `followed`, which it received at
   * `receivedAt` (the engine's clock, `Date.now` unless `now` is given): shows its failure notice
   * at once, and records it when shown, unless a sweep will show the stream's copy. A sweep will
   * when the frame is no role forward (those stay live only), the latest replay call succeeded, a
   * call since the last failed one answered `followed` without refusing it, `followed` has a
   * cursor, and that cursor did not start after the frame was received: a cursor starts at the
   * stream's end, which can be at or past a frame the live path still held, so no sweep reads that
   * frame. Any other frame (a role forward after its receipt, a subject the stream does not keep,
   * a listener that does not replay or refuses the subject, a subject with no cursor yet, or one
   * whose cursor started, or started again after a stream recreation, since the frame arrived)
   * gets its notice at once instead of being dropped silently. It never rejects.
   */
  liveFailed(
    followed: string,
    envelope: ReplayNoticeEnvelope,
    error: unknown,
    receivedAt: number
  ): Promise<void>;
  /**
   * Whether the session followed `topic` before the stream stored sequence `seq`: the topic has a
   * cursor that started below `seq`. A replayed removal closes a topic only then, so an old one
   * never closes a topic followed again since.
   */
  followedBefore(topic: string, seq: number): boolean;
  /**
   * Replaces everything the engine holds with what a host persisted for the session it is on now.
   * Ledger entries recorded 72 hours ago or more are dropped; the state is kept at any age, so a
   * session back after longer still follows its topics. Work in flight for before is dropped.
   * Notices the state holds pending (`ReplayState.notices`) are told by the next sweep, once.
   */
  restore(snapshot: ReplaySnapshot): void;
  /** The current state, as the next write would persist it. */
  state(): ReplayState;
}

interface Ledger {
  has(identity: ReplayLedgerIdentity): boolean;
  record(identity: ReplayLedgerIdentity): ReplayLedgerEntry | undefined;
  restore(entries: readonly ReplayLedgerEntry[]): void;
  list(): ReplayLedgerEntry[];
}

/** What the ledger knows a frame by (`replayLedgerIdentity`). */
export interface ReplayLedgerIdentity {
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
  // When each cursor started in this process (a first read, or a reset after a stream
  // recreation): a live frame received before then may sit at or below where it started. The
  // start is stamped when the replay answer is handled, after the listener read its watermark
  // (`last_seq`), so a frame published before that read but taken off the NATS socket after the
  // answer is stamped after the start: if it then fails live it is left to replay, yet its stored
  // copy is at or below the cursor, so no sweep shows it and its failure is only logged. This
  // window is reasoned from the order of the reads, not driven by a test.
  const cursorStarts = new Map<string, number>();
  // The stream the cursors were read on: its id when a listener answered one, and its creation
  // time (`ReplayStateSchema`).
  let streamID: string | undefined;
  let streamCreated: string | undefined;
  // The notices not told yet, written with the cursor change that raised them, so a process that
  // ends before telling one tells it after the restart (`ReplayPendingNoticeSchema`).
  const pending = new Set<ReplayPendingNotice>();
  // A frame's failures by its stream sequence; in memory only, so a restart gives it three more.
  const failures = new Map<number, { count: number; error: string }>();
  // The ledger records no write has persisted yet; the state JSON the last write persisted, that
  // state without its `after`s, and when it was written (unknown after a restore).
  let unwritten: ReplayLedgerEntry[] = [];
  let writtenState: string | undefined;
  let writtenShape: string | undefined;
  let writtenAt: number | undefined;
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
  // Raises a pending notice and drops it from the pending list; the next write persists the drop.
  const tell = (kind: ReplayPendingNotice) => {
    pending.delete(kind);
    notify({ kind, message: PENDING_NOTICE_MESSAGES[kind] });
  };

  // A host whose session read throws has no session to work for, so its work stops as on a switch.
  const sessionRead = (read: () => string | undefined) => (): string | undefined => {
    try {
      return read();
    } catch {
      return undefined;
    }
  };
  const currentSession = sessionRead(() => host.sessionID());
  const writeSession = sessionRead(() =>
    host.writeSessionID === undefined ? host.sessionID() : host.writeSessionID()
  );
  const bindTo = (
    sessionID: string | undefined,
    read: () => string | undefined
  ): { readonly sessionID: string; readonly alive: () => boolean } | undefined => {
    if (sessionID === undefined || sessionID === "" || read() !== sessionID) return undefined;
    const bound = epoch;
    return { sessionID, alive: () => bound === epoch && read() === sessionID };
  };
  const bind = () => bindTo(currentSession(), currentSession);

  // Keys sorted, fields in one order: equal states serialise equally, so a comparison of the text
  // says whether the state changed. Without `afters`, it says whether anything but where the
  // cursors stand changed.
  const stateJSON = (afters = true): string => {
    const subjects: Record<string, ReplayCursor> = {};
    for (const subject of [...cursors.keys()].sort()) {
      const cursor = cursors.get(subject) ?? {};
      subjects[subject] = {
        ...(cursor.since === undefined ? {} : { since: cursor.since }),
        ...(!afters || cursor.after === undefined ? {} : { after: cursor.after }),
      };
    }
    return JSON.stringify({
      ...(streamID === undefined ? {} : { stream_id: streamID }),
      ...(streamCreated === undefined ? {} : { stream_created: streamCreated }),
      subjects,
      ...(pending.size === 0 ? {} : { notices: [...pending].sort() }),
    });
  };

  const record = (identity: ReplayLedgerIdentity) => {
    const entry = ledger.record(identity);
    if (entry !== undefined) unwritten.push(entry);
  };

  // Writes run one after another in call order, so a host that rewrites a file never has an older
  // snapshot land after a newer one. `atOnce`: the cursors moved past messages a sweep handled, or
  // past a retention gap, which is a change written at once. Without it, a change of nothing but
  // the `after`s is the stream's end moving, held until the state written is
  // `REPLAY_STATE_REFRESH_MS` old. A change to the pending notices is a change of more than the
  // `after`s, so it is always written at once.
  const flush = (alive: () => boolean, atOnce = false): Promise<void> => {
    writes = writes.then(async () => {
      if (!alive()) return;
      const json = stateJSON();
      const delivered = unwritten;
      if (json === writtenState && delivered.length === 0) return;
      const shape = stateJSON(false);
      if (
        delivered.length === 0 &&
        !atOnce &&
        shape === writtenShape &&
        writtenAt !== undefined &&
        now() - writtenAt < REPLAY_STATE_REFRESH_MS
      ) {
        return;
      }
      unwritten = [];
      const state = json === writtenState ? undefined : (JSON.parse(json) as ReplayState);
      try {
        await host.write({
          delivered,
          state,
          snapshot: () => ({ state: JSON.parse(json) as ReplayState, ledger: ledger.list() }),
        });
        if (state !== undefined) {
          writtenState = json;
          writtenShape = shape;
          writtenAt = now();
        }
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
      if (!followed.has(subject)) {
        cursors.delete(subject);
        cursorStarts.delete(subject);
      }
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

  // A notice written settles the frame for good, here and after a restart; one that fails keeps
  // it outstanding, so the next sweep, or nothing for a live frame, tries again. One the host
  // queued is the host's to record once written: until then the frame's claim keeps it a hole.
  const showFailureNotice = async (
    notice: ReplayFailureNotice,
    alive: () => boolean
  ): Promise<Handled> => {
    let queued: boolean;
    try {
      queued = (await host.showFailureNotice(notice)) === "queued";
    } catch (error) {
      if (!alive()) return "stopped";
      safely("failure notice warning", () => host.failureNoticeFailed(notice, error));
      return "hole";
    }
    if (!alive()) return "stopped";
    if (queued) return "hole";
    record(replayLedgerIdentity(notice.envelope));
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
    const identity = replayLedgerIdentity(envelope);
    if (ledger.has(identity)) {
      failures.delete(seq);
      return "done";
    }
    // The live path is delivering a copy now; whether it shows it is not known yet.
    if (dedupe?.pending(envelope) === true) return "hole";
    if (!alive()) return "stopped";
    // A notice shown settles the frame for good: its failures are no longer counted.
    const giveUp = async (error: string): Promise<Handled> => {
      const handled = await showFailureNotice(
        failureNotice(envelope, message.time, error, message.subject),
        alive
      );
      if (handled === "done") failures.delete(seq);
      return handled;
    };
    const failure = failures.get(seq);
    if (failure !== undefined && failure.count >= REPLAY_FAILURES_BEFORE_NOTICE) {
      return giveUp(failure.error);
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
    // Handed on, not yet written: the host's claim keeps it a hole until it records it.
    if (outcome === "queued") {
      failures.delete(seq);
      return "hole";
    }
    const count = (failure?.count ?? 0) + 1;
    failures.set(seq, { count, error });
    if (count >= REPLAY_FAILURES_BEFORE_NOTICE) return giveUp(error);
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

  // Whether the answer's stream replaced the one the cursors were read on. An answer with an id is
  // judged by it alone: a stored id that differs is a new stream, and a creation time that changed
  // is not, since a restore changes it, and after a clustered restore a cluster's servers can
  // report different ones, so a later leader election can change it too. A state stored before
  // ids (a creation time, no id) takes the answer's id, unless a cursor stands past the stream's
  // end, which only a new stream explains. An answer without an id is judged by its creation time.
  const streamReplaced = (stream: ReplayResult["stream"]): boolean => {
    if (stream.id === undefined) {
      return (
        streamCreated !== undefined && streamCreated !== "" && stream.created !== streamCreated
      );
    }
    if (streamID !== undefined && streamID !== "") return stream.id !== streamID;
    if (streamCreated === undefined || streamCreated === "") return false;
    for (const cursor of cursors.values()) {
      if (cursor.after !== undefined && cursor.after > stream.last_seq) return true;
    }
    return false;
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
    // Notices a restore brought back, which the process before did not live to tell. A retention
    // gap told here is this sweep's one gap notice.
    if (pending.size > 0) {
      for (const kind of [...pending].sort()) {
        if (kind === "retention_gap") gapNoticed = true;
        tell(kind);
      }
      await flush(alive);
      if (!alive()) return;
    }

    for (let page = 0; page < REPLAY_PAGES_PER_SWEEP; page += 1) {
      let handledAny = false;
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
      const replaced = streamReplaced(stream);
      // An answer without an id keeps the one stored, unless its stream replaced that one.
      streamID = stream.id ?? (replaced ? undefined : streamID);
      streamCreated = stream.created;
      if (replaced) {
        // Sequences of the old stream mean nothing in the new one: every cursor starts again at
        // its end, as a new follow does, and what the old stream held for the session is gone.
        // The reset is written with its notice pending, so a process that ends before telling it
        // tells it after the restart.
        const at = now();
        for (const subject of cursors.keys()) {
          cursors.set(subject, { since: stream.last_seq, after: stream.last_seq });
          cursorStarts.set(subject, at);
        }
        failures.clear();
        pending.add("stream_recreated");
        await flush(alive);
        if (!alive()) return;
        tell("stream_recreated");
        await flush(alive);
        return;
      }

      for (const [, reading] of asking) reading.more = false;
      // Retention dropped messages below `first_seq` before a cursor read them. Nothing below it
      // can be read on any subject, so each such cursor moves to just before it, skipping nothing,
      // and that move is written with the notice pending before it is raised: a resume neither
      // loses the notice nor finds the gap again. One notice tells every gap a sweep finds; a gap
      // found after it is still moved past and written.
      let gapped = false;
      for (const result of response.subjects) {
        const reading = readings.get(result.subject);
        if (reading?.read === undefined || result.refused !== undefined) continue;
        if (reading.read >= stream.first_seq - 1) continue;
        gapped = true;
        reading.read = stream.first_seq - 1;
        const cursor = cursors.get(result.subject);
        if (cursor?.after !== undefined && cursor.after < reading.read) {
          cursors.set(result.subject, { ...cursor, after: reading.read });
        }
      }
      if (gapped) {
        if (!gapNoticed) pending.add("retention_gap");
        await flush(alive, true);
        if (!alive()) return;
        if (pending.has("retention_gap")) {
          gapNoticed = true;
          tell("retention_gap");
          await flush(alive);
        }
      }
      for (const result of response.subjects) {
        const reading = readings.get(result.subject);
        if (reading === undefined || result.refused !== undefined) continue;
        if (reading.read === undefined) {
          // No history before the follow: the cursor starts where the stream ends now.
          const cursor = cursors.get(result.subject);
          if (cursor !== undefined && cursor.after === undefined) {
            cursors.set(result.subject, { since: result.next_after, after: result.next_after });
            cursorStarts.set(result.subject, now());
          }
          reading.read = result.next_after;
          continue;
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
        if (handled === "done") handledAny = true;
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
      await flush(alive, handledAny);
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
    delivered: (frame) => ledger.has(replayLedgerIdentity(frame)),
    async recordDelivered(frame) {
      const alive = bind()?.alive;
      if (alive === undefined) return;
      record(replayLedgerIdentity(frame));
      await flush(alive);
    },
    async recordDeliveredFor(sessionID, frame) {
      const alive = bindTo(sessionID, writeSession)?.alive;
      if (alive === undefined) return;
      record(replayLedgerIdentity(frame));
      await flush(alive);
    },
    async liveFailed(followed, envelope, error, receivedAt) {
      const started = cursorStarts.get(followed);
      if (
        !isRoleForward(envelope) &&
        answered.has(followed) &&
        cursors.get(followed)?.after !== undefined &&
        (started === undefined || started <= receivedAt)
      ) {
        return;
      }
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
    followedBefore: (topic, seq) => {
      const since = cursors.get(topic)?.since;
      return since !== undefined && seq > since;
    },
    restore(snapshot) {
      epoch += 1;
      ledger.restore(snapshot.ledger);
      cursors.clear();
      cursorStarts.clear();
      streamID = snapshot.state?.stream_id;
      streamCreated = snapshot.state?.stream_created;
      pending.clear();
      for (const kind of snapshot.state?.notices ?? []) {
        const known = ReplayPendingNoticeSchema.safeParse(kind);
        if (known.success) pending.add(known.data);
      }
      for (const [subject, cursor] of Object.entries(snapshot.state?.subjects ?? {})) {
        cursors.set(subject, { ...cursor });
      }
      writtenState = snapshot.state === undefined ? undefined : stateJSON();
      writtenShape = snapshot.state === undefined ? undefined : stateJSON(false);
      writtenAt = undefined;
      unwritten = [];
      failures.clear();
      listenerNoticed = undefined;
      answered.clear();
    },
    state: () => JSON.parse(stateJSON()) as ReplayState,
  };
}

/**
 * Whether a replayed frame is suppressed: the renderer skips it for this session, or it is a
 * targeted Dispatch delivery the renderer could not validate, which a replay answers nothing for.
 * (A delivery the renderer rejects with a reply address is always malformed too.)
 */
export function replaySuppressed(
  rendered: Pick<RenderInboundResult, "skip" | "malformedDelivery">
): boolean {
  return rendered.skip || rendered.malformedDelivery === true;
}

/** A replayed frame's text: the rendered frame, then when it was published. */
export function replayedText(content: string, publishedAt: string): string {
  return `${content}\nPublished at ${publishedAt} while this session's connection was down.`;
}

/** What the notice shown in place of a frame the session could not be shown says, field by field. */
export function failureNoticeFields(notice: ReplayFailureNotice): Readonly<Record<string, string>> {
  const readWith =
    notice.readWith === "dispatch read"
      ? "Read it with the `dispatch read` command on the issue or ask it concerns."
      : "Read it with the envoy_inbox tool.";
  return {
    notice: "message not shown",
    event_id: notice.eventId,
    from: notice.sender,
    summary: notice.summary,
    published_at: notice.publishedAt,
    ...(notice.subject === undefined ? {} : { subject: notice.subject }),
    error: notice.error,
    detail: `This session could not be shown this message, so it is not coming back on its own. ${readWith}`,
  };
}

/** What the one-time notice that replay may have missed messages says, field by field. */
export function replayNoticeFields(notice: ReplayNotice): Readonly<Record<string, string>> {
  return {
    notice: notice.kind === "stream_recreated" ? "stream recreated" : "messages may be missing",
    detail: notice.message,
  };
}

/**
 * A notice's text, as each host shows it: its fields (`failureNoticeFields`,
 * `replayNoticeFields`) under `envoy:` in TOON, as every live card is rendered. TOON quotes a
 * value that holds a newline or would read as structure, so a sender-controlled field (`summary`,
 * `from`, `error`) can never start a field of its own inside the block.
 */
export function envoyNoticeText(fields: Readonly<Record<string, string>>): string {
  return encode({ envoy: fields });
}

/** The topic a failed delivery's report goes to is this prefix followed by the original topic. */
const EXCEPTION_TOPIC_PREFIX = "notifications.envoy.exceptions.";

/**
 * The stream subjects whose topics carry one agent session's private payload: each session's
 * inbox, the Legion daemon's notices for a project's controller, and a failed delivery's report
 * on a session's subject (a session's own lane included).
 */
const PRIVATE_STREAM_SUBJECTS = [
  `${AGENT_TOPIC_PREFIX}>`,
  `${LEGION_NOTICE_TOPIC_PREFIX}>`,
  `${EXCEPTION_TOPIC_PREFIX}${AGENT_TOPIC_PREFIX}>`,
];

/** Whether some subject matches both patterns, under NATS wildcard rules. */
function subjectsOverlap(a: string, b: string): boolean {
  const aTokens = a.split(".");
  const bTokens = b.split(".");
  for (let index = 0; index < aTokens.length && index < bTokens.length; index += 1) {
    const aToken = aTokens[index];
    const bToken = bTokens[index];
    if (aToken === ">" || bToken === ">") return true;
    if (aToken !== "*" && bToken !== "*" && aToken !== bToken) return false;
  }
  return aTokens.length === bTokens.length;
}

/**
 * Whether a topic reaches a private stream subject other than `ownAgentSubject`: it overlaps one
 * under NATS wildcard rules, so another session's agent subject, a failed-delivery lane
 * (`notifications.envoy.exceptions.notifications.agent.<id>`, the session's own included), the
 * Legion controller lane (`notifications.legion.>`), and every wildcard reaching any of them
 * (`notifications.agent.*`, `notifications.>`, `notifications.*.<x>`, `notifications.envoy.>`)
 * count, and it is not the session's own agent subject as one literal subject. This mirrors the
 * listener's rule, `privateStreamSubjects` and `ReachesPrivateStreamSubject` in
 * packages/envoy/internal/bus/stream.go: the listener stores no such interest and replays no such
 * subject, since either would read another session's private payload, so a host neither
 * registers nor replays one.
 */
export function reachesPrivateStreamSubject(ownAgentSubject: string, topic: string): boolean {
  if (!PRIVATE_STREAM_SUBJECTS.some((subject) => subjectsOverlap(topic, subject))) return false;
  // A session id holding `*` or `>` makes a wildcard of its "own" agent subject, no session's alone.
  return topic !== ownAgentSubject || /[*>]/.test(ownAgentSubject);
}

/**
 * The subjects a session follows for replay, which are what it registers with the listener: its
 * own agent subject, then every topic it follows but a role's and one reaching a private stream
 * subject. Role messages stay live only, whether on a role lane or on a subject the host holds for
 * a role (`heldForRole`); another session's agent subject (a fork's parent, until the host closes
 * it), a failed-delivery lane, the Legion controller lane, or a wildcard reaching one of them, is
 * not this session's to replay, and the listener would refuse it (`reachesPrivateStreamSubject`).
 */
export function replayFollowedSubjects(
  ownAgentSubject: string,
  topics: Iterable<string>,
  heldForRole: (topic: string) => boolean = () => false
): string[] {
  const followed = [...topics].filter(
    (topic) =>
      !topic.startsWith(ROLE_TOPIC_PREFIX) &&
      !reachesPrivateStreamSubject(ownAgentSubject, topic) &&
      !heldForRole(topic)
  );
  return [...new Set([ownAgentSubject, ...followed])];
}

/**
 * The topics a persisted state says the session followed that a restore follows again, so a
 * session back after the listener reaped its interests still follows and recovers them: every
 * subject of the state but agent subjects (the host opens its own, and another's is not its to
 * follow) and role lanes (live only, held through a role's claim). An unfollowed topic is absent
 * from the newest state, so it stays unfollowed.
 */
export function rememberedTopics(state: ReplayState | undefined): string[] {
  return Object.keys(state?.subjects ?? {}).filter(
    (topic) => !topic.startsWith(AGENT_TOPIC_PREFIX) && !topic.startsWith(ROLE_TOPIC_PREFIX)
  );
}

/** What `closeReplayedRemoval` asks of the host. */
export interface ReplayedRemovalHost {
  /** The session the frame is replayed for: a removal naming another session closes nothing. */
  readonly sessionID: string;
  /** The engine replaying the frame, whose cursors say since when each topic was followed. */
  readonly engine: Pick<ReplayEngine, "followedBefore">;
  /** Stops following `topic`; false when the session did not follow it, so nothing closed. */
  close(topic: string): boolean;
  /**
   * Takes the closed topics out of the listener's registry, as `envoy_unsubscribe` does, with
   * whatever else the host drops them from in the same step; its failure is the host's to report.
   * The helper waits on what it returns.
   */
  unregister(topics: readonly string[]): Promise<void> | void;
  /** The followed subjects changed: the host tells its engine (`ReplayEngine.followedChanged`). */
  followedChanged(): Promise<void>;
}

/**
 * Acts on a replayed `subscription.removed` naming the session, and answers the topics it closed.
 * A topic closes only when the frame was stored after the session started following it
 * (`followedBefore`: above its cursor's `since`), so an old removal never closes a topic followed
 * again since. The closed topics leave the engine's followed subjects, and the listener's registry
 * too: a resumed session registered them again before this sweep, and the registry, which only
 * ever adds a registered topic, would have the next restart follow them again. The registry write
 * starts first, so a registration the host queues meanwhile cannot advertise them again.
 */
export async function closeReplayedRemoval(
  frame: Pick<ReplayFrame, "data" | "seq">,
  host: ReplayedRemovalHost
): Promise<readonly string[]> {
  const closed = (subscriptionRemovedTopics(frame.data, host.sessionID) ?? []).filter(
    (topic) => host.engine.followedBefore(topic, frame.seq) && host.close(topic)
  );
  if (closed.length === 0) return closed;
  const unregistering = host.unregister(closed);
  await host.followedChanged();
  await unregistering;
  return closed;
}

/** Where `routeReplayNotice` sends a one-time notice. */
export interface ReplayNoticeRoutes {
  /** The listener does not replay (`not_served`, `unavailable`): a standing condition of the
   *  session's setup, which the person is warned of as a start notice is, not a model's card. */
  warn(message: string): void;
  /** Messages may be missing (`stream_recreated`, `retention_gap`): a card the session is shown,
   *  its text rendered as every notice is (`envoyNoticeText`). */
  card(text: string): void;
}

/** Sends a one-time notice (`ReplayHost.notify`) where its kind belongs: a warning, or a card. */
export function routeReplayNotice(notice: ReplayNotice, routes: ReplayNoticeRoutes): void {
  if (notice.kind === "not_served" || notice.kind === "unavailable") {
    routes.warn(notice.message);
    return;
  }
  routes.card(envoyNoticeText(replayNoticeFields(notice)));
}

/** Whether a frame is a role lane's: those stay live only, so a sweep passes them over. */
function isRoleForward(envelope: {
  readonly dedupe_key?: string | undefined;
  readonly topic?: string | undefined;
}): boolean {
  return (
    envelope.dedupe_key?.startsWith(ROLE_FORWARD_KEY_PREFIX) === true ||
    envelope.topic?.startsWith(ROLE_TOPIC_PREFIX) === true
  );
}

/**
 * What the ledger records a frame as: its `event_id`, and its `dedupe_key` only when that key
 * names its event (`ReplayLedgerEntrySchema`). A host that marks what it wrote with it can read
 * the frame back from its own record.
 */
export function replayLedgerIdentity(frame: DedupeIdentity): ReplayLedgerIdentity {
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
  const has = (identity: ReplayLedgerIdentity) => {
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

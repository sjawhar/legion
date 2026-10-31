import { DELIVERY_DUPLICATE_WINDOW_MS, dedupeKeyNamesItsEvent } from "@legion/contracts";
import { encode } from "@toon-format/toon";
import { z } from "zod";
import {
  type DedupeIdentity,
  type InboundEnvelope,
  inboundTimestamp,
  senderLabel,
} from "./delivery";
import { subscriptionRemovedTopics } from "./dispatch-subscribe";
import { messageFor } from "./errors";

/**
 * How long the ledger remembers a frame, from when it recorded it. The stream keeps a message no
 * longer (`DELIVERY_DUPLICATE_WINDOW_MS` is its retention), so a frame the ledger has forgotten
 * can no longer come back from the session's consumer. A restore never refreshes an entry's time.
 */
export const DELIVERY_LEDGER_TTL_MS = DELIVERY_DUPLICATE_WINDOW_MS;

/**
 * The most frames the ledger holds; past it the oldest is forgotten first. This is the documented
 * limit of the once-only promise: a session shown more frames than this inside 72 hours can be
 * shown the oldest of them again by a redelivery. It also bounds what a host persists.
 */
export const DELIVERY_LEDGER_LIMIT = 20_000;

/**
 * One frame the session finished with: shown, answered with a correlated reply, or reported in a
 * failure notice. It is the frame's `event_id`, and its `dedupe_key` only when that key names its
 * event (`dedupeKeyNamesItsEvent`), so a re-send of it under a new event id is recognised too.
 */
export const LedgerEntrySchema = z.object({
  event_id: z.string().min(1).optional(),
  dedupe_key: z.string().min(1).optional(),
  at: z.number(),
});

export type LedgerEntry = z.infer<typeof LedgerEntrySchema>;

/**
 * What the ledger persists beside its entries: the topics the session follows. Every topic the
 * host follows is listed, so a host can rebuild what it followed from the newest state alone
 * after the listener reaped its interests (`rememberedTopics`). A state an earlier build wrote
 * held cursors instead and no topic list; it parses as no topics, and the listener's registry is
 * then the one source of what to follow again.
 */
export const LedgerStateSchema = z.object({
  topics: z.array(z.string()).default([]),
});

export type LedgerState = z.infer<typeof LedgerStateSchema>;

/**
 * What a host keeps to restore a ledger: the newest state, and the entries. A host that rewrites
 * one file holds exactly this; a host that appends entries keeps them as one entry per record and
 * gathers them back into this shape.
 */
export const LedgerSnapshotSchema = z.object({
  state: LedgerStateSchema.optional(),
  ledger: z.array(LedgerEntrySchema),
});

export type LedgerSnapshot = z.infer<typeof LedgerSnapshotSchema>;

/**
 * What `showFailureNotice` resolves: `shown` (or nothing) when the notice is written, `queued`
 * when the host handed it on, claimed the notice's frame in its own dedupe and records it itself
 * once written.
 */
export type FailureNoticeOutcome = "shown" | "queued";

/** The parts of an envelope a failure notice names; a live host's tolerant parse has them. */
export type FailureNoticeEnvelope = Pick<
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
export interface FailureNotice {
  readonly eventId: string;
  readonly sender: string;
  readonly summary: string;
  /** When it was published (`issued_at`). */
  readonly publishedAt: string;
  readonly error: string;
  /** How the session reads the message itself: `dispatch read` for a Dispatch event. */
  readonly readWith: "dispatch read" | "envoy_inbox";
  readonly envelope: FailureNoticeEnvelope;
}

/**
 * One persistence write. A host that appends entries appends one per `delivered` record and one
 * for `state` when it is present; a host that rewrites a file writes `snapshot()`. `state` is
 * present only when it differs from the last state written or restored.
 */
export interface LedgerChange {
  readonly delivered: readonly LedgerEntry[];
  readonly state: LedgerState | undefined;
  snapshot(): LedgerSnapshot;
}

/**
 * The host side of the ledger. Each callback runs for the session that was current when the
 * ledger started the work that calls it; the ledger re-reads `sessionID()` before each write and
 * drops the work when it changed (`writeSessionID()` for `recordDeliveredFor`).
 */
export interface DeliveryLedgerHost {
  /** The session the host is on now; undefined or empty before it has one. */
  sessionID(): string | undefined;
  /**
   * The session whose record `write` persists to now: a host shutting down still writes it while
   * `sessionID()` already answers none, so no new work starts. `sessionID()` when absent. Only
   * `recordDeliveredFor` reads it.
   */
  writeSessionID?(): string | undefined;
  /** The subjects the session follows: its agent subject and its topics. */
  followedSubjects(): readonly string[];
  /**
   * Shows a failure notice; a throw means it was not shown. A host that resolves `queued` records
   * it later (`FailureNoticeOutcome`); one that resolves nothing showed it.
   */
  showFailureNotice(notice: FailureNotice): Promise<FailureNoticeOutcome> | Promise<void>;
  /** The notice could not be shown either: the host warns (log and UI). */
  failureNoticeFailed(notice: FailureNotice, error: unknown): void;
  log(message: string, error?: unknown): void;
  /** Persists a change; a throw is logged, and its records go with the next write. */
  write(change: LedgerChange): void | Promise<void>;
}

export interface DeliveryLedgerOptions {
  readonly host: DeliveryLedgerHost;
  readonly now?: () => number;
}

/**
 * What one session was already shown, and the notice that names a frame it could not be shown.
 * Every message reaches the session through its own durable consumer; this is what keeps a
 * redelivery of a frame already written from being shown twice, across a restart too, since the
 * ledger is rebuilt from what the host persisted.
 */
export interface DeliveryLedger {
  /**
   * Takes the host's followed subjects now, so a restart follows them again
   * (`rememberedTopics`). Persisted at once, so an unfollow survives a restart.
   */
  followedChanged(): Promise<void>;
  /** Whether the ledger holds the frame: a repeat of it after a restart is dropped. */
  delivered(frame: DedupeIdentity): boolean;
  /**
   * Records a frame the host wrote to the session's record, so no redelivery shows it again. It
   * never rejects: a write that fails is logged, and its record goes with the next write.
   */
  recordDelivered(frame: DedupeIdentity): Promise<void>;
  /**
   * Records a frame for the session `sessionID`, as `recordDelivered` does, while the host may be
   * shutting down: it records only while the host's `writeSessionID()` names `sessionID` and no
   * `restore` has moved the ledger since, and the write goes ahead even once `sessionID()`
   * answers none. The ledger holds the frame from the call on. It never rejects.
   */
  recordDeliveredFor(sessionID: string, frame: DedupeIdentity): Promise<void>;
  /**
   * The host could not show a frame, and it is not coming back (the relay gave up on it): shows
   * its failure notice at once, and records it when shown, so the session learns of a message it
   * was never given rather than losing it silently. It never rejects.
   */
  liveFailed(envelope: FailureNoticeEnvelope, error: unknown): Promise<void>;
  /**
   * Replaces everything the ledger holds with what a host persisted for the session it is on now.
   * Entries recorded 72 hours ago or more are dropped; the state is kept at any age, so a session
   * back after longer still follows its topics. Work in flight for before is dropped.
   */
  restore(snapshot: LedgerSnapshot): void;
}

interface Ledger {
  has(identity: LedgerIdentity): boolean;
  record(identity: LedgerIdentity): LedgerEntry | undefined;
  restore(entries: readonly LedgerEntry[]): void;
  list(): LedgerEntry[];
}

/** What the ledger knows a frame by (`ledgerIdentity`). */
export interface LedgerIdentity {
  readonly event_id?: string;
  readonly dedupe_key?: string;
}

export function createDeliveryLedger(options: DeliveryLedgerOptions): DeliveryLedger {
  const { host } = options;
  const now = options.now ?? Date.now;
  const ledger = createLedger(now);
  // The topics the host follows, as the last sync read them.
  let topics: string[] = [];
  // The entries no write has persisted yet, and the state JSON the last write persisted.
  let unwritten: LedgerEntry[] = [];
  let writtenState: string | undefined;
  let writes: Promise<void> = Promise.resolve();
  // Bumped by restore: work started before it belongs to another session's record.
  let epoch = 0;

  const safely = (what: string, call: () => void) => {
    try {
      call();
    } catch (error) {
      try {
        host.log(`delivery ledger: ${what} failed`, error);
      } catch {
        // A host whose log throws has nowhere left to report to.
      }
    }
  };
  const log = (message: string, error?: unknown) => safely("log", () => host.log(message, error));

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

  // Topics sorted: equal states serialise equally, so a comparison of the text says whether the
  // state changed.
  const stateJSON = (): string => JSON.stringify({ topics: [...topics].sort() });

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
      const state = json === writtenState ? undefined : (JSON.parse(json) as LedgerState);
      try {
        await host.write({
          delivered,
          state,
          snapshot: () => ({ state: JSON.parse(json) as LedgerState, ledger: ledger.list() }),
        });
        if (state !== undefined) writtenState = json;
      } catch (error) {
        unwritten = [...delivered, ...unwritten];
        log("delivery ledger: could not persist the state; the next write carries it", error);
      }
    });
    return writes;
  };

  const syncFollowed = () => {
    topics = [...new Set(host.followedSubjects())];
  };

  const failureNotice = (envelope: FailureNoticeEnvelope, error: string): FailureNotice => ({
    eventId: envelope.event_id ?? "unknown",
    sender: senderLabel(envelope),
    summary: envelope.payload_summary ?? "unknown",
    publishedAt: inboundTimestamp(envelope.issued_at),
    error,
    readWith: envelope.source === "dispatch" ? "dispatch read" : "envoy_inbox",
    envelope,
  });

  // A notice written settles the frame for good, here and after a restart; one the host queued is
  // the host's to record once written.
  const showFailureNotice = async (notice: FailureNotice, alive: () => boolean): Promise<void> => {
    let queued: boolean;
    try {
      queued = (await host.showFailureNotice(notice)) === "queued";
    } catch (error) {
      if (!alive()) return;
      safely("failure notice warning", () => host.failureNoticeFailed(notice, error));
      return;
    }
    if (!alive() || queued) return;
    record(ledgerIdentity(notice.envelope));
    await flush(alive);
  };

  return {
    async followedChanged() {
      const alive = bind()?.alive;
      if (alive === undefined) return;
      try {
        syncFollowed();
        await flush(alive);
      } catch (error) {
        log("delivery ledger: could not take the followed subjects", error);
      }
    },
    delivered: (frame) => ledger.has(ledgerIdentity(frame)),
    async recordDelivered(frame) {
      const alive = bind()?.alive;
      if (alive === undefined) return;
      record(ledgerIdentity(frame));
      await flush(alive);
    },
    async recordDeliveredFor(sessionID, frame) {
      const alive = bindTo(sessionID, writeSession)?.alive;
      if (alive === undefined) return;
      record(ledgerIdentity(frame));
      await flush(alive);
    },
    async liveFailed(envelope, error) {
      const alive = bind()?.alive;
      if (alive === undefined) return;
      try {
        await showFailureNotice(failureNotice(envelope, messageFor(error)), alive);
      } catch (thrown) {
        log("delivery ledger: could not report a frame the session was not shown", thrown);
      }
    },
    restore(snapshot) {
      epoch += 1;
      ledger.restore(snapshot.ledger);
      topics = [...new Set(snapshot.state?.topics ?? [])];
      writtenState = snapshot.state === undefined ? undefined : stateJSON();
      unwritten = [];
    },
  };
}

/** What a frame's text ends with when it was stored while the session was away (`storedText`). */
export const STORED_WHILE_AWAY_SUFFIX = "while this session's connection was down.";

/** A stored frame's text: the rendered frame, then when it was published. */
export function storedText(content: string, publishedAt: string): string {
  return `${content}\nPublished at ${publishedAt} ${STORED_WHILE_AWAY_SUFFIX}`;
}

/** What the notice shown in place of a frame the session could not be shown says, field by field. */
export function failureNoticeFields(notice: FailureNotice): Readonly<Record<string, string>> {
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
    error: notice.error,
    detail: `This session could not be shown this message, so it is not coming back on its own. ${readWith}`,
  };
}

/**
 * A notice's text, as each host shows it: its fields (`failureNoticeFields`, `gapNoticeFields`)
 * under `envoy:` in TOON, as every live card is rendered. TOON quotes a value that holds a newline
 * or would read as structure, so a sender-controlled field (`summary`, `from`, `error`) can never
 * start a field of its own inside the block.
 */
export function envoyNoticeText(fields: Readonly<Record<string, string>>): string {
  return encode({ envoy: fields });
}

/** What `closeReplayedRemoval` asks of the host. */
export interface ReplayedRemovalHost {
  /** The session the frame is delivered for: a removal naming another session closes nothing. */
  readonly sessionID: string;
  /**
   * The relay delivering the frame, when it is the one open for `sessionID`; undefined when the
   * host holds none for it, which closes every topic the removal names: with no follow times to
   * read, nothing says the session followed a topic only after the frame was stored.
   */
  readonly relay: RelayFollowClock | undefined;
  /** Stops following `topic`; false when the session did not follow it, so nothing closed. */
  close(topic: string): boolean;
  /**
   * Takes the closed topics out of the listener's registry, as `envoy_unsubscribe` does, with
   * whatever else the host drops them from in the same step; its failure is the host's to report.
   * The helper waits on what it returns.
   */
  unregister(topics: readonly string[]): Promise<void> | void;
  /** The followed subjects changed: the host tells its ledger (`DeliveryLedger.followedChanged`). */
  followedChanged(): Promise<void>;
}

/** What the removal reads from the session's relay (`ConsumerDelivery.followedBefore`). */
export interface RelayFollowClock {
  followedBefore(topic: string, storedAtMs: number): boolean;
}

/**
 * Acts on a `subscription.removed` naming the session, and answers the topics it closed. A topic
 * closes only when the frame was stored after the session started following it
 * (`ConsumerDelivery.followedBefore`), so a removal the stream kept while the session was away
 * never closes a topic followed again since. The closed topics leave the ledger's followed
 * subjects, and the listener's registry too: a resumed session registered them again before this
 * frame arrived, and the registry, which only ever adds a registered topic, would have the next
 * restart follow them again. The registry write starts first, so a registration the host queues
 * meanwhile cannot advertise them again.
 */
export async function closeReplayedRemoval(
  frame: { readonly data: string; readonly storedAtMs: number },
  host: ReplayedRemovalHost
): Promise<readonly string[]> {
  const closed = (subscriptionRemovedTopics(frame.data, host.sessionID) ?? []).filter(
    (topic) => host.relay?.followedBefore(topic, frame.storedAtMs) !== false && host.close(topic)
  );
  if (closed.length === 0) return closed;
  const unregistering = host.unregister(closed);
  await host.followedChanged();
  await unregistering;
  return closed;
}

/**
 * What the ledger records a frame as: its `event_id`, and its `dedupe_key` only when that key
 * names its event (`LedgerEntrySchema`). A host that marks what it wrote with it can read the
 * frame back from its own record.
 */
export function ledgerIdentity(frame: DedupeIdentity): LedgerIdentity {
  return {
    ...(frame.event_id === undefined || frame.event_id === "" ? {} : { event_id: frame.event_id }),
    ...(frame.dedupe_key === undefined || !dedupeKeyNamesItsEvent(frame)
      ? {}
      : { dedupe_key: frame.dedupe_key }),
  };
}

function createLedger(now: () => number): Ledger {
  // Insertion order is recording order, so the expired entries and the oldest one the limit drops
  // are always at the front. Each index maps a field to the entry that holds it.
  let entries = new Set<LedgerEntry>();
  const byEvent = new Map<string, LedgerEntry>();
  const byKey = new Map<string, LedgerEntry>();

  const drop = (entry: LedgerEntry) => {
    entries.delete(entry);
    if (entry.event_id !== undefined && byEvent.get(entry.event_id) === entry) {
      byEvent.delete(entry.event_id);
    }
    if (entry.dedupe_key !== undefined && byKey.get(entry.dedupe_key) === entry) {
      byKey.delete(entry.dedupe_key);
    }
  };
  const add = (entry: LedgerEntry) => {
    if (entries.size >= DELIVERY_LEDGER_LIMIT) {
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
      if (at - entry.at < DELIVERY_LEDGER_TTL_MS) break;
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
            at - entry.at < DELIVERY_LEDGER_TTL_MS &&
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

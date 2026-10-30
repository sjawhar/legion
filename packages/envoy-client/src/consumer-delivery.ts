import { agentSubject } from "@legion/contracts";
import { z } from "zod";
import { type DedupeIdentity, DedupeIdentitySchema, renderInbound } from "./delivery";
import { type ReplayEngine, type ReplayNotice, subjectsOverlap } from "./replay";
import { streamStores } from "./stream-topics";
import {
  type DeliveryAnswer,
  type DeliveryRequest,
  expandSubscriptionTopics,
  type ResumePoint,
} from "./transport";

/**
 * The core subject the owning listener relays a session's durable consumer on: this prefix and the
 * session id. The stream does not store it. The listener's `DeliverSubjectPrefix` is the same.
 */
export const DELIVER_SUBJECT_PREFIX = "notifications.envoy.deliver.";

/** The subject a session's relay frames arrive on. */
export function deliverSubject(sessionID: string): string {
  return `${DELIVER_SUBJECT_PREFIX}${sessionID}`;
}

/** The headers of a relay frame. */
export const RelayHeader = {
  /** The subject the stream stored the message on. */
  subject: "Envoy-Subject",
  /** The stream sequence, decimal. */
  streamSeq: "Envoy-Stream-Seq",
  /** The stream's `envoy_stream_id`. */
  streamID: "Envoy-Stream-Id",
  /** JetStream's delivery count, decimal; 1 is the first delivery. */
  delivered: "Envoy-Delivered",
  /** When the stream stored the message: RFC 3339 with nanoseconds, UTC. */
  storedAt: "Envoy-Stored-At",
  /** `true` only on the first frame after the relay restarted below the stream's first sequence. */
  gap: "Envoy-Gap",
} as const;

/**
 * A session's answers on a frame's reply subject, each exactly one ASCII word. `received`: the
 * frame decoded, before any host work. `progress`: still held unwritten (the listener extends the
 * ack wait). `done`: written, or skipped, suppressed, rejected or a duplicate (the listener acks
 * it); it also counts as `received`. `nak`: the session cannot take it (the listener redelivers).
 */
export type RelayReply = "received" | "progress" | "done" | "nak";

/** How often a held frame's `progress` goes out; the listener's ack wait is 60 s. */
export const RELAY_PROGRESS_INTERVAL_MS = 20_000;

/**
 * How much earlier than a topic's follow time a frame stored on it may be and still be shown. The
 * follow time is the plugin's clock and the stored time the NATS server's: a frame stored just
 * after the follow must not be dropped for a clock a little ahead, which would lose it, while one
 * stored this little before the follow is shown, which costs nothing.
 */
export const FOLLOW_CLOCK_ALLOWANCE_MS = 2_000;

/**
 * How many deliveries of one frame may fail in the host (its `deliver` threw) before the frame is
 * answered `done` and its failure notice shown (`ReplayEngine.liveFailed`) instead of `nak`: a
 * frame the host can never show must not come back every second forever.
 */
const RELAY_ATTEMPTS_BEFORE_FAILED = 3;

/** What `decodeRelayFrame` reads headers through; a NATS message's `headers` is one. */
export interface RelayHeaders {
  get(name: string): string | null | undefined;
}

/** A message received on the relay subject, as any NATS client hands it over. */
export interface RelayMessage {
  readonly data: Uint8Array | string;
  readonly headers?: RelayHeaders | undefined;
  readonly reply?: string | undefined;
}

/** A decoded relay frame. */
export interface RelayFrame {
  /** The subject the stream stored the message on. */
  readonly subject: string;
  readonly streamSeq: number;
  readonly streamID: string;
  /** JetStream's delivery count; 1 is the first delivery. */
  readonly delivered: number;
  /** The stored time as the header carries it (RFC 3339, UTC). */
  readonly storedAt: string;
  /** The stored time in milliseconds since the epoch. */
  readonly storedAtMs: number;
  /** The relay restarted below the stream's first sequence just before this frame. */
  readonly gap: boolean;
  /** The stored message's bytes as text: the Envoy envelope JSON. */
  readonly data: string;
  /** The listener's inbox for this frame's replies. */
  readonly reply: string;
}

type DecodedRelayFrame = { readonly frame: RelayFrame } | { readonly error: string };

const decoder = new TextDecoder();
const DECIMAL = /^[0-9]+$/;
// RFC 3339 with up to nine fractional digits. `Date.parse` reads at most milliseconds portably,
// so the fraction is cut to three digits first.
const RFC3339 = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/;

function positiveInteger(text: string | null | undefined): number | undefined {
  if (text === null || text === undefined || !DECIMAL.test(text)) return undefined;
  const value = Number(text);
  return Number.isSafeInteger(value) && value > 0 ? value : undefined;
}

function parseStoredAt(text: string): number | undefined {
  const match = RFC3339.exec(text);
  if (match === null) return undefined;
  const fraction = (match[2] ?? "").padEnd(3, "0").slice(0, 3);
  const ms = Date.parse(`${match[1]}.${fraction}${match[3]}`);
  return Number.isNaN(ms) ? undefined : ms;
}

/** Reads a relay frame's headers; any header missing or malformed, or no reply, is an error. */
export function decodeRelayFrame(message: RelayMessage): DecodedRelayFrame {
  const reply = message.reply ?? "";
  if (reply === "") return { error: "no reply subject" };
  const headers = message.headers;
  if (headers === undefined) return { error: "no headers" };
  const subject = headers.get(RelayHeader.subject) ?? "";
  if (subject === "") return { error: `no ${RelayHeader.subject}` };
  const streamSeq = positiveInteger(headers.get(RelayHeader.streamSeq));
  if (streamSeq === undefined) return { error: `bad ${RelayHeader.streamSeq}` };
  const streamID = headers.get(RelayHeader.streamID) ?? "";
  if (streamID === "") return { error: `no ${RelayHeader.streamID}` };
  const delivered = positiveInteger(headers.get(RelayHeader.delivered));
  if (delivered === undefined) return { error: `bad ${RelayHeader.delivered}` };
  const storedAt = headers.get(RelayHeader.storedAt) ?? "";
  const storedAtMs = parseStoredAt(storedAt);
  if (storedAtMs === undefined) return { error: `bad ${RelayHeader.storedAt}` };
  return {
    frame: {
      subject,
      streamSeq,
      streamID,
      delivered,
      storedAt,
      storedAtMs,
      gap: headers.get(RelayHeader.gap) === "true",
      data: typeof message.data === "string" ? message.data : decoder.decode(message.data),
      reply,
    },
  };
}

/** A random string per plugin NATS connection: the registration's `delivery.epoch`. */
function newEpoch(): string {
  return crypto.randomUUID();
}

/** One NATS connection's epoch: its id, and when it began on the host's clock. */
export interface ConsumerEpoch {
  readonly id: string;
  readonly startedAt: number;
}

/**
 * A notice the session is told once: messages may be missing. A host shows it as it shows the
 * replay engine's notice of the same kind.
 */
export type ConsumerGapNotice = Extract<
  ReplayNotice,
  { readonly kind: "retention_gap" | "stream_recreated" }
>;

const GAP_MESSAGES: Record<ConsumerGapNotice["kind"], string> = {
  retention_gap:
    "The notification stream no longer holds some messages published to this session while it was away: messages older than 72 hours may be missing.",
  stream_recreated:
    "Envoy's notification stream was recreated, so messages published to this session before that may be lost.",
};

/**
 * A frame handed to the host. The host answers it once: `done` when the frame is written to the
 * session's record (or skipped, suppressed, rejected), `nak` when the session cannot take it.
 * Until then `progress` goes out every 20 s. `release` says the frame no longer blocks the next
 * one (a side question whose model call runs off the queue); `done` and `nak` release it too, and
 * so does `deliver` returning. Each is idempotent; nothing is answered after `done` or `nak`.
 */
export interface ConsumerFrame extends RelayFrame {
  /**
   * Delivered before (`delivered` > 1) or stored before the current epoch began: a late side
   * question never starts a side turn, it is shown as a card.
   */
  readonly late: boolean;
  done(): void;
  nak(): void;
  release(): void;
}

export interface ConsumerDeliveryHost {
  /**
   * Hands a frame to the session. Frames come in stream order, one at a time: the next waits until
   * this returns or the frame is released or settled. A throw on an unsettled frame answers `nak`,
   * or, from its third delivery on, `done` with the frame's failure notice (`ledger.liveFailed`).
   */
  deliver(frame: ConsumerFrame): void | Promise<void>;
  /** Tells the session once that messages may be missing (the replay engine's gap notices). */
  gap(notice: ConsumerGapNotice): void;
  log(message: string, error?: unknown): void;
}

export interface ConsumerDeliveryOptions {
  readonly sessionID: string;
  readonly host: ConsumerDeliveryHost;
  /** Publishes a reply body on a frame's reply subject (core NATS, no reply expected). */
  readonly publish: (reply: string, body: RelayReply) => void;
  /**
   * The persisted ledger of frames written (`ReplayEngine.delivered`), which also shows the failure
   * notice of a frame given up on (`ReplayEngine.liveFailed`). Its cursors seed the first
   * registration of a session that moves from the core path to consumer delivery (`registration`)
   * and say since which stream sequence that session followed each topic, so a frame stored while
   * it was away is shown rather than dropped as stored before its follow time (`receive`), and
   * `setConsumerMode` tells it whether the listener relays the stream now (`answered`).
   */
  readonly ledger: Pick<ReplayEngine, "delivered" | "liveFailed" | "state" | "setConsumerMode">;
  /**
   * Stores the snapshot the host persists, called only when it changed since the last write; the
   * host keeps it where it keeps the session's state, and answers whether it stored it. A `false`
   * leaves the snapshot unstored, so the next change offers it again.
   */
  readonly persist: (snapshot: ConsumerDeliverySnapshot) => boolean;
  readonly now?: () => number;
  /** Runs `callback` every `ms` until the returned stop runs; `setInterval` by default. */
  readonly every?: (ms: number, callback: () => void) => () => void;
}

/**
 * What a host persists and restores: the resume point, and when each followed topic was first
 * followed, so frames stored before a follow are dropped across a restart too.
 */
export const ConsumerDeliverySnapshotSchema = z.object({
  resume: z
    .object({ stream_id: z.string().min(1), seq: z.number().int().nonnegative() })
    .optional(),
  follows: z.record(z.string(), z.number()).default({}),
});

export type ConsumerDeliverySnapshot = z.infer<typeof ConsumerDeliverySnapshotSchema>;

/** One session's relay delivery: decode, reply, filter, order, and track the resume point. */
export interface ConsumerDelivery {
  /** The relay subject; the host subscribes to it before it registers. */
  readonly subject: string;
  /** Takes one message from the relay subscription. */
  receive(message: RelayMessage): void;
  /** The current epoch; one begins when the delivery is created. */
  epoch(): ConsumerEpoch;
  /** Begins a new epoch: call it for every new NATS connection, then register again. */
  beginEpoch(): ConsumerEpoch;
  /**
   * The registration's `delivery` field: the epoch, and the resume point. That is where this
   * session is up to when it has one; with none — a session that has never taken a frame from a
   * consumer — it is seeded from the ledger's cursors, so a session moving from the core path to
   * consumer delivery resumes where its sweeps left off instead of at the stream's end.
   */
  registration(): DeliveryRequest;
  /**
   * Takes the registration's answer and the topics that registration carried, and returns the
   * answer's mode. A consumer answer that reports a gap, or names another stream than the resume
   * the registration sent (its own point, or the one seeded from the ledger's cursors), tells the
   * session once; its stored topics (`streamStores`) become the ones the
   * listener relays (`relays`), and the ledger is put in consumer mode. Any other answer leaves
   * the core path running. Persists the snapshot when it changed.
   */
  answered(answer: DeliveryAnswer, topics: readonly string[]): DeliveryAnswer["mode"];
  /**
   * Whether the listener relays `topic` from the session's consumer: the core copy of a frame on
   * it is shown nothing, since the relay's copy is the one shown.
   */
  relays(topic: string): boolean;
  /** Whether the last registration was answered `consumer`. */
  relaying(): boolean;
  /**
   * Takes the topics the session follows now (its agent subject is always taken, and never kept
   * among them). A new one is stamped now, so frames stored before it are dropped unless the
   * ledger's cursors say the core path already followed it from below their sequence; a dropped
   * one is forgotten. Call it before the registration that adds a topic. Persists the snapshot
   * when it changed.
   */
  setFollowed(topics: Iterable<string>): void;
  /** Offers the snapshot to the host's `persist`, when it changed: call it after a frame is
   *  written. */
  persist(): void;
  /**
   * Below this point the session holds nothing unwritten: the lowest stream sequence received and
   * not yet written, minus one, or, with none held, the highest written. Undefined before any.
   */
  resumePoint(): ResumePoint | undefined;
  snapshot(): ConsumerDeliverySnapshot;
  /** Restores what a host persisted; call it before `setFollowed`. */
  restore(snapshot: ConsumerDeliverySnapshot): void;
  /** The session is replaced: every unsettled frame is answered `nak`, and so is any later one. */
  close(): void;
}

function parseIdentity(data: string) {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return undefined;
  }
  const identity = DedupeIdentitySchema.safeParse(parsed);
  if (!identity.success) return undefined;
  const { event_id, dedupe_key } = identity.data;
  return event_id === undefined && dedupe_key === undefined ? undefined : identity.data;
}

/**
 * Whether a followed topic, expanded as the subscription it stands for, reaches `subject`: the
 * one "does this topic reach this frame" check the follow filter asks.
 */
function reaches(topic: string, subject: string): boolean {
  return expandSubscriptionTopics([topic]).some((pattern) => subjectsOverlap(pattern, subject));
}

function every(ms: number, callback: () => void): () => void {
  const timer = setInterval(callback, ms);
  return () => clearInterval(timer);
}

interface Entry {
  readonly frame: RelayFrame;
  /** The frame's event identity, parsed once as it arrived. */
  readonly identity: DedupeIdentity | undefined;
  readonly late: boolean;
  readonly replies: string[];
  readonly key: string;
  stopProgress: () => void;
  settled: boolean;
}

export function createConsumerDelivery(options: ConsumerDeliveryOptions): ConsumerDelivery {
  const { host, ledger, sessionID } = options;
  const now = options.now ?? Date.now;
  const repeat = options.every ?? every;
  const ownSubject = agentSubject(sessionID);

  let currentEpoch: ConsumerEpoch = { id: newEpoch(), startedAt: now() };
  // When each followed topic was first followed, on the host's clock.
  let follows = new Map<string, number>();
  // The resume point's stream, the frames of it received and not yet written, and the highest
  // written. A `nak` leaves its frame held: it comes again and is not written yet.
  let streamID: string | undefined;
  let held = new Set<number>();
  let written: number | undefined;
  // The resume the last `registration()` sent, its own point or the seeded one (`seededResume`):
  // a session moving from the core path has no resume point of its own, so this is what the
  // answer's stream is compared with (`answered`).
  let registeredResume: ResumePoint | undefined;
  // A retention gap the registration answer told: the next frame's `Envoy-Gap` is the same one.
  let gapTold = false;
  let closed = false;
  // The stored topics of the last registration answered `consumer`, which the listener relays
  // (`streamStores`); undefined on the core path.
  let relayed: ReadonlySet<string> | undefined;
  // The snapshot the host last wrote (`persist`): an unchanged one is not offered again. A new
  // delivery holds nothing, which is what a host with no snapshot of its own stored.
  let persisted = JSON.stringify({ follows: {} } satisfies ConsumerDeliverySnapshot);
  // Frames received and unsettled, by stream and sequence: a redelivery joins its first copy.
  const entries = new Map<string, Entry>();
  const queue: Entry[] = [];
  let blocking: Entry | undefined;
  let pumping = false;

  const safely = (what: string, call: () => void) => {
    try {
      call();
    } catch (error) {
      try {
        host.log(`consumer delivery: ${what} failed`, error);
      } catch {
        // A host whose log throws has nowhere left to report to.
      }
    }
  };
  const log = (message: string, error?: unknown) =>
    safely("log", () => host.log(`consumer delivery: ${message}`, error));
  const send = (reply: string, body: RelayReply) =>
    safely(`reply ${body}`, () => options.publish(reply, body));
  const tellGap = (kind: ConsumerGapNotice["kind"]) =>
    safely("gap notice", () => host.gap({ kind, message: GAP_MESSAGES[kind] }));

  const switchStream = (id: string) => {
    if (streamID === id) return;
    streamID = id;
    held = new Set();
    written = undefined;
  };

  // Tells `stream_recreated` once when the stream the last registration asked to resume on is not
  // `id`, the stream the listener now relays: that stream was replaced and what it held is gone.
  // A session moving from the core path has no resume point of its own, only the seeded one, so
  // this is the only comparison that can see the replacement; it runs on whichever of the
  // registration's answer and the relay's first frame arrives first, since the listener starts the
  // relay before it writes the answer.
  const tellReplacedResume = (id: string) => {
    const asked = resumePoint() ?? registeredResume;
    if (asked === undefined || asked.stream_id === id) return;
    registeredResume = undefined;
    tellGap("stream_recreated");
  };

  const followedAt = (subject: string): number | undefined => {
    if (subject === ownSubject) return Number.NEGATIVE_INFINITY;
    let earliest: number | undefined;
    for (const [topic, at] of follows) {
      if (!reaches(topic, subject)) continue;
      if (earliest === undefined || at < earliest) earliest = at;
    }
    return earliest;
  };

  // Whether the core path's ledger says the session followed a topic reaching this frame's
  // subject from before the frame was stored. Its cursors are stream sequences, not wall clocks:
  // `since` is the sequence the session started following that subject at, so a frame above it
  // was stored while the session followed the topic however long ago it was away. A session
  // moving from the core path has those cursors and no consumer snapshot, so `setFollowed` stamps
  // every topic at the resume time; without this, every frame the relay owes it on a followed
  // topic would be dropped as stored before the follow. Cursors read on another stream say
  // nothing about this frame's sequence. Read only when the follow time would drop the frame.
  const followedOnCorePath = (frame: RelayFrame): boolean => {
    const state = ledger.state();
    if (state.stream_id !== frame.streamID) return false;
    for (const [topic, cursor] of Object.entries(state.subjects)) {
      if (cursor.since === undefined || frame.streamSeq <= cursor.since) continue;
      if (reaches(topic, frame.subject)) return true;
    }
    return false;
  };

  const settle = (entry: Entry, body: "done" | "nak") => {
    if (entry.settled) return;
    entry.settled = true;
    entry.stopProgress();
    entries.delete(entry.key);
    for (const reply of entry.replies) send(reply, body);
    if (body === "done" && entry.frame.streamID === streamID) {
      held.delete(entry.frame.streamSeq);
      written = Math.max(written ?? 0, entry.frame.streamSeq);
    }
    release(entry);
  };

  // The next frame goes at once: inside a hand-off the running pump takes it, else this call does.
  const release = (entry: Entry) => {
    if (blocking !== entry) return;
    blocking = undefined;
    pump();
  };

  const alreadyWritten = (identity: DedupeIdentity | undefined): boolean =>
    identity !== undefined && ledger.delivered(identity);

  const failed = (entry: Entry, error: unknown) => {
    if (entry.settled) return;
    if (entry.frame.delivered < RELAY_ATTEMPTS_BEFORE_FAILED) {
      log(`frame ${entry.frame.streamSeq} failed; it comes again`, error);
      settle(entry, "nak");
      return;
    }
    // The session gets the notice a failed live frame gets.
    safely("failure notice", () => {
      const { subject, data } = entry.frame;
      const { envelope } = renderInbound(data, sessionID, subject);
      if (envelope !== undefined) void ledger.liveFailed(subject, envelope, error, now());
    });
    settle(entry, "done");
  };

  const handOff = (entry: Entry) => {
    const handle: ConsumerFrame = {
      ...entry.frame,
      late: entry.late,
      done: () => settle(entry, "done"),
      nak: () => settle(entry, "nak"),
      release: () => release(entry),
    };
    blocking = entry;
    let result: void | Promise<void>;
    try {
      result = host.deliver(handle);
    } catch (error) {
      failed(entry, error);
      release(entry);
      return;
    }
    if (!(result instanceof Promise)) {
      release(entry);
      return;
    }
    result.then(
      () => release(entry),
      (error: unknown) => {
        failed(entry, error);
        release(entry);
      }
    );
  };

  function pump() {
    if (pumping) return;
    pumping = true;
    try {
      while (blocking === undefined && !closed) {
        const entry = queue.shift();
        if (entry === undefined) return;
        if (entry.settled) continue;
        // Written while it waited (another copy of the event on another subject).
        if (alreadyWritten(entry.identity)) {
          settle(entry, "done");
          continue;
        }
        handOff(entry);
      }
    } finally {
      pumping = false;
    }
  }

  const receive = (message: RelayMessage) => {
    const decoded = decodeRelayFrame(message);
    if ("error" in decoded) {
      // Redelivery cannot fix a frame that does not decode, so it is acked away.
      log(`dropping an undecodable relay frame: ${decoded.error}`);
      if (message.reply !== undefined && message.reply !== "") send(message.reply, "done");
      return;
    }
    const { frame } = decoded;
    if (closed) {
      send(frame.reply, "nak");
      return;
    }
    send(frame.reply, "received");

    if (frame.gap && !gapTold) tellGap("retention_gap");
    gapTold = false;
    if (frame.streamID !== streamID) {
      if (streamID !== undefined && (written !== undefined || held.size > 0)) {
        tellGap("stream_recreated");
      } else if (streamID === undefined) {
        tellReplacedResume(frame.streamID);
      }
      switchStream(frame.streamID);
    }

    const key = `${frame.streamID}:${frame.streamSeq}`;
    const first = entries.get(key);
    if (first !== undefined) {
      // A redelivery of a frame still held (the relay restarted at its ack floor): its answers go
      // to every inbox it came with.
      first.replies.push(frame.reply);
      return;
    }

    // Not followed (the agent subject always is), stored before its topic was followed and not
    // followed before that on the core path either (`followedOnCorePath`, which recovers what a
    // session moving from the core path was owed), or already written: answered done, and the
    // resume point passes it.
    const since = followedAt(frame.subject);
    const unfollowed =
      (since === undefined || frame.storedAtMs < since - FOLLOW_CLOCK_ALLOWANCE_MS) &&
      !followedOnCorePath(frame);
    const identity = unfollowed ? undefined : parseIdentity(frame.data);
    if (unfollowed || alreadyWritten(identity)) {
      send(frame.reply, "done");
      written = Math.max(written ?? 0, frame.streamSeq);
      return;
    }

    held.add(frame.streamSeq);
    const entry: Entry = {
      frame,
      identity,
      late: frame.delivered > 1 || frame.storedAtMs < currentEpoch.startedAt,
      replies: [frame.reply],
      key,
      stopProgress: () => undefined,
      settled: false,
    };
    entry.stopProgress = repeat(RELAY_PROGRESS_INTERVAL_MS, () => {
      if (entry.settled) return;
      for (const reply of entry.replies) send(reply, "progress");
    });
    entries.set(key, entry);
    queue.push(entry);
    pump();
  };

  const resumePoint = (): ResumePoint | undefined => {
    if (streamID === undefined) return undefined;
    if (held.size > 0) return { stream_id: streamID, seq: Math.min(...held) - 1 };
    return written === undefined ? undefined : { stream_id: streamID, seq: written };
  };

  // Where a session with no resume point of its own starts its first consumer: the lowest cursor
  // the core path left over the subjects a relay carries (its agent subject and the followed
  // topics the stream stores), on the stream those cursors were read on. Everything the sweeps
  // handled is at or below it and what they never reached is above it, so the consumer the
  // listener creates at that sequence plus one relays what is still owed and the stream's order
  // holds; a frame below another followed topic's own cursor is already written, and the ledger
  // drops it. A ledger with no cursor yet, or a state older than stream ids (a `resume` names the
  // stream), seeds nothing: that consumer starts at the stream's end, as a session with no
  // history does.
  const seededResume = (): ResumePoint | undefined => {
    const state = ledger.state();
    const stream = state.stream_id;
    if (stream === undefined) return undefined;
    let lowest: number | undefined;
    for (const [subject, cursor] of Object.entries(state.subjects)) {
      if (cursor.after === undefined || !streamStores(subject)) continue;
      if (lowest === undefined || cursor.after < lowest) lowest = cursor.after;
    }
    return lowest === undefined ? undefined : { stream_id: stream, seq: lowest };
  };

  const snapshot = (): ConsumerDeliverySnapshot => {
    const resume = resumePoint();
    return {
      ...(resume === undefined ? {} : { resume }),
      follows: Object.fromEntries(follows),
    };
  };

  const persist = (): void => {
    const current = snapshot();
    const serialized = JSON.stringify(current);
    if (serialized === persisted) return;
    if (!options.persist(current)) return;
    persisted = serialized;
  };

  return {
    subject: deliverSubject(sessionID),
    receive,
    epoch: () => currentEpoch,
    beginEpoch() {
      currentEpoch = { id: newEpoch(), startedAt: now() };
      return currentEpoch;
    },
    registration() {
      const resume = resumePoint() ?? seededResume();
      registeredResume = resume;
      return {
        consumer: true,
        epoch: currentEpoch.id,
        ...(resume === undefined ? {} : { resume }),
      };
    },
    answered(answer, topics) {
      if (answer.mode === "consumer") {
        if (answer.stream_id !== "" && answer.stream_id !== streamID) {
          // The stream the registration asked to resume on — the session's own resume point, or
          // the one seeded from the ledger's cursors — is not the one the listener answered: that
          // stream was replaced, the consumer starts at the new stream's end, and what the old
          // one held is gone. Told once: the switch below makes the answer's stream the current
          // one, so a later answer naming it tells nothing, and a relay frame that came first
          // already told it (`tellReplacedResume` in `receive`).
          tellReplacedResume(answer.stream_id);
          switchStream(answer.stream_id);
        }
        if (answer.gap) {
          tellGap("retention_gap");
          gapTold = true;
        }
        relayed = new Set([...topics].filter(streamStores));
      } else {
        relayed = undefined;
      }
      ledger.setConsumerMode(relayed !== undefined);
      persist();
      return answer.mode;
    },
    relays: (topic) => relayed?.has(topic) === true,
    relaying: () => relayed !== undefined,
    setFollowed(topics) {
      const at = now();
      const next = new Map<string, number>();
      for (const topic of topics) {
        // The session's own agent subject is always followed (`followedAt`), so it is never one of
        // the follow times a host persists.
        if (topic === ownSubject) continue;
        next.set(topic, follows.get(topic) ?? at);
      }
      follows = next;
      persist();
    },
    persist,
    resumePoint,
    snapshot,
    restore(restored) {
      const merged = new Map(Object.entries(restored.follows));
      for (const [topic, at] of follows) if (!merged.has(topic)) merged.set(topic, at);
      follows = merged;
      streamID = restored.resume?.stream_id;
      held = new Set();
      written = restored.resume?.seq;
      // What a host restored is already stored: only a change after it is written.
      persisted = JSON.stringify(snapshot());
    },
    close() {
      closed = true;
      relayed = undefined;
      ledger.setConsumerMode(false);
      queue.length = 0;
      for (const entry of [...entries.values()]) settle(entry, "nak");
      blocking = undefined;
    },
  };
}

/** The answer a registration that asked the listener for no relay reads as. */
const NOT_ASKED: DeliveryAnswer = { mode: "core", reason: "not_asked" };

/**
 * Takes the answer to a registration that asked for no relay, for the topics it carried: nothing
 * is relayed whatever the listener answered, so the session's delivery — when one is open since
 * the registration went out — takes none of the topics and the ledger leaves consumer mode, and
 * the core path's sweeps run on. With no delivery open the ledger alone is told, so this module
 * stays the one writer of its consumer mode.
 */
export function registeredWithoutRelay(
  ledger: Pick<ReplayEngine, "setConsumerMode">,
  delivery: ConsumerDelivery | undefined,
  topics: readonly string[]
): void {
  if (delivery === undefined) {
    ledger.setConsumerMode(false);
    return;
  }
  delivery.answered(NOT_ASKED, topics);
}

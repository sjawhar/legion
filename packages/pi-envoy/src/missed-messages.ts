import {
  type ReplayLedgerEntry,
  ReplayLedgerEntrySchema,
  type ReplayLedgerIdentity,
  type ReplaySnapshot,
  type ReplayState,
  ReplayStateSchema,
} from "@legion/envoy-client/replay";

/** The custom type of every card the extension shows the session. */
const ENVOY_MESSAGE = "envoy-message";

/**
 * Transcript entry recording one frame the replay ledger holds (`{event_id?, dedupe_key?, at}`):
 * one whose card the host wrote, live or replayed, or whose failure notice it wrote. One per
 * record, read back from every entry of the session file with the written cards themselves
 * (`writtenCardIdentity`), so a restarted session neither replays it nor shows a live repeat.
 */
export const REPLAY_DELIVERED_ENTRY = "envoy-replay-delivered";

/**
 * Transcript entry holding the replay cursors (`{stream_created, subjects}`), written only when
 * they changed, and a cursor that only followed the stream's end at most hourly
 * (`REPLAY_STATE_REFRESH_MS`); the newest one is the state. Its subjects are every subject the
 * session followed, so a restart after the listener reaped its interests still follows its topics.
 */
export const REPLAY_STATE_ENTRY = "envoy-replay-state";

/** The data of every `customType` entry among a session's entries, in the order written. */
function customEntries(entries: readonly unknown[], customType: string): unknown[] {
  const found: unknown[] = [];
  for (const entry of entries) {
    if (typeof entry !== "object" || entry === null) continue;
    if (!("type" in entry) || entry.type !== "custom") continue;
    if (!("customType" in entry) || entry.customType !== customType) continue;
    if (!("data" in entry)) continue;
    found.push(entry.data);
  }
  return found;
}

/** The newest `REPLAY_STATE_ENTRY` that parses, at any age. */
function newestState(entries: readonly unknown[]): ReplayState | undefined {
  const states = customEntries(entries, REPLAY_STATE_ENTRY);
  for (let index = states.length - 1; index >= 0; index -= 1) {
    const parsed = ReplayStateSchema.safeParse(states[index]);
    if (parsed.success) return parsed.data;
  }
  return undefined;
}

/**
 * What a session's entries hold for its replay engine: every ledger record (the engine drops the
 * expired ones), with every written card's frame (`writtenCardIdentity`), and the newest state. An
 * entry that does not parse is passed over.
 */
export function replaySnapshot(entries: readonly unknown[], now = Date.now()): ReplaySnapshot {
  const ledger: ReplayLedgerEntry[] = [];
  for (const data of customEntries(entries, REPLAY_DELIVERED_ENTRY)) {
    const parsed = ReplayLedgerEntrySchema.safeParse(data);
    if (parsed.success) ledger.push(parsed.data);
  }
  ledger.push(...writtenCards(entries, ledger, now));
  const state = newestState(entries);
  return state === undefined ? { ledger } : { state, ledger };
}

/** A non-empty string field of `value`, if it has one. */
function stringField(value: object, field: string): string | undefined {
  const found: unknown = field in value ? Reflect.get(value, field) : undefined;
  return typeof found === "string" && found !== "" ? found : undefined;
}

/** A card's text: its text, or its text blocks joined, when it carries pictures. */
function cardText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const texts: string[] = [];
  for (const block of content) {
    if (typeof block !== "object" || block === null) continue;
    if (stringField(block, "type") !== "text") continue;
    const text = stringField(block, "text");
    if (text !== undefined) texts.push(text);
  }
  return texts.join("\n");
}

/**
 * The event a card's rendered text names: the `id` of the `envoy:` block `renderInbound` renders
 * a frame as, or the `event_id` of a failure notice's. A TOON value is bare, or quoted as JSON.
 */
function cardEventID(text: string): string | undefined {
  const lines = text.split("\n");
  if (lines[0] !== "envoy:") return undefined;
  for (const line of lines.slice(1)) {
    const match = /^ {2}(id|event_id): (.+)$/.exec(line);
    const raw = match?.[2];
    if (raw === undefined) continue;
    let value: unknown = raw;
    if (raw.startsWith('"')) {
      try {
        value = JSON.parse(raw);
      } catch {
        return undefined;
      }
    }
    return typeof value === "string" && value !== "" && value !== "unknown" ? value : undefined;
  }
  return undefined;
}

/**
 * The frame an `envoy-message` card shows, or names in a failure notice, read from a message the
 * host wrote or the `custom_message` entry it wrote it as: the identity its `details` carry (the
 * frame's `replayLedgerIdentity`, which the extension puts there), else the event id its rendered
 * text names. Undefined for anything else, the extension's own notices included.
 */
export function writtenCardIdentity(message: unknown): ReplayLedgerIdentity | undefined {
  if (typeof message !== "object" || message === null) return undefined;
  if (stringField(message, "customType") !== ENVOY_MESSAGE) return undefined;
  const details: unknown = "details" in message ? message.details : undefined;
  if (typeof details === "object" && details !== null) {
    const eventID = stringField(details, "event_id");
    const key = stringField(details, "dedupe_key");
    if (eventID !== undefined || key !== undefined) {
      return {
        ...(eventID === undefined ? {} : { event_id: eventID }),
        ...(key === undefined ? {} : { dedupe_key: key }),
      };
    }
  }
  const eventID = cardEventID(cardText("content" in message ? message.content : undefined));
  return eventID === undefined ? undefined : { event_id: eventID };
}

/**
 * The ledger records the written cards among a session's entries add (`writtenCardIdentity`):
 * a card the host wrote whose record was not, the process having ended in between, or written
 * while its record waited on a turn that never took it. Each is dated by its entry, or `now`.
 */
function writtenCards(
  entries: readonly unknown[],
  recorded: readonly ReplayLedgerEntry[],
  now: number
): ReplayLedgerEntry[] {
  const events = new Set<string>();
  const keys = new Set<string>();
  const note = (identity: ReplayLedgerIdentity) => {
    if (identity.event_id !== undefined) events.add(identity.event_id);
    if (identity.dedupe_key !== undefined) keys.add(identity.dedupe_key);
  };
  for (const entry of recorded) note(entry);
  const found: ReplayLedgerEntry[] = [];
  for (const entry of entries) {
    if (typeof entry !== "object" || entry === null) continue;
    if (stringField(entry, "type") !== "custom_message") continue;
    const identity = writtenCardIdentity(entry);
    if (identity === undefined) continue;
    if (
      (identity.event_id !== undefined && events.has(identity.event_id)) ||
      (identity.dedupe_key !== undefined && keys.has(identity.dedupe_key))
    ) {
      continue;
    }
    note(identity);
    const written = Date.parse(stringField(entry, "timestamp") ?? "");
    found.push({ ...identity, at: Number.isNaN(written) ? now : written });
  }
  return found;
}

/** The subjects the newest replay state says the session followed. */
export function rememberedSubjects(entries: readonly unknown[]): readonly string[] {
  return Object.keys(newestState(entries)?.subjects ?? {});
}

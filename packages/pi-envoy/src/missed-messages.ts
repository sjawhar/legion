import {
  type ReplayLedgerEntry,
  ReplayLedgerEntrySchema,
  type ReplaySnapshot,
  type ReplayState,
  ReplayStateSchema,
} from "@legion/envoy-client/replay";

/**
 * Transcript entry recording one frame the replay ledger holds (`{event_id?, dedupe_key?, at}`):
 * shown, live or replayed, or reported in a failure notice. One per record, read back from every
 * entry of the session file, so a restarted session neither replays it nor shows a live repeat.
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
 * expired ones) and the newest state. An entry that does not parse is passed over.
 */
export function replaySnapshot(entries: readonly unknown[]): ReplaySnapshot {
  const ledger: ReplayLedgerEntry[] = [];
  for (const data of customEntries(entries, REPLAY_DELIVERED_ENTRY)) {
    const parsed = ReplayLedgerEntrySchema.safeParse(data);
    if (parsed.success) ledger.push(parsed.data);
  }
  const state = newestState(entries);
  return state === undefined ? { ledger } : { state, ledger };
}

/** The subjects the newest replay state says the session followed. */
export function rememberedSubjects(entries: readonly unknown[]): readonly string[] {
  return Object.keys(newestState(entries)?.subjects ?? {});
}

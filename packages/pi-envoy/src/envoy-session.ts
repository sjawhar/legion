import path from "node:path";

/**
 * The Envoy session each top-level session of this process registered, keyed by its transcript
 * path, so an extension instance with no identity of its own can find the one that spawned it.
 *
 * A `task` subagent loads its own instance of `extensions/envoy.ts` inside the parent's process,
 * and that instance registers nothing: its `session_start` returns early, so its module
 * `sessionID` stays empty (a registered subagent left an untitled row heartbeating for the life
 * of the parent process). The address a reply to such a subagent can reach is therefore its
 * parent's registered session. The record lives on `globalThis` under a process-wide symbol,
 * beside the bootstrapped-session record in `subagent-session.ts`, because each instance has a
 * closure of its own.
 *
 * It is a map, not one slot: an ACP host runs several top-level sessions in one process, each
 * `kind: "main"`, and a single slot would hand a subagent whichever of them last started or
 * switched. The key is OMP's own subagent layout — a subagent's transcript sits inside a
 * directory named after its parent's transcript file minus `.jsonl`, the same layout
 * `isSubagentSession` reads — so a subagent resolves its own parent by walking that layout up.
 */
const ENVOY_SESSIONS = Symbol.for("legion.pi-envoy.envoy-session");

interface GlobalEnvoySessionStore {
  [key: symbol]: Map<string, string> | undefined;
}

const store = globalThis as unknown as GlobalEnvoySessionStore;

function recordedSessions(): Map<string, string> {
  const existing = store[ENVOY_SESSIONS];
  if (existing !== undefined) return existing;
  const created = new Map<string, string>();
  store[ENVOY_SESSIONS] = created;
  return created;
}

/**
 * Publish this top-level session, replacing whatever this instance recorded before.
 *
 * Returns the key written, which the caller hands back as `previousKey` next time: a session
 * whose id or transcript path changes (`/fork`, `/handoff`, `/new`, a resume) must leave no
 * entry behind under its old key, or a later subagent walking that path up would be handed an
 * id its parent has retired. The same call with no transcript and no id — what
 * `session_shutdown` makes — deletes the entry and records nothing, so a subagent that outlives
 * its parent names no dead session.
 *
 * A session whose host has not minted its id yet is still recorded, under its transcript path,
 * with the empty id it has: its subagents must resolve *it* and report no address until the
 * heartbeat's drift heal fills the id in, rather than fall through to another live session.
 */
export function recordEnvoySession(entry: {
  readonly previousKey: string | undefined;
  readonly sessionFile: string | undefined;
  readonly sessionID: string;
}): string | undefined {
  const sessions = recordedSessions();
  // A transcript path is absolute, so the id-based key of a session with no transcript on disk
  // (SQL storage, `--no-session`) can never be mistaken for one a walk-up would produce. With
  // neither a transcript nor an id there is nothing to key on, and nothing to record.
  const key =
    entry.sessionFile ?? (entry.sessionID === "" ? undefined : `session:${entry.sessionID}`);
  if (entry.previousKey !== undefined && entry.previousKey !== key) {
    sessions.delete(entry.previousKey);
  }
  if (key === undefined) return undefined;
  sessions.set(key, entry.sessionID);
  return key;
}

/**
 * The top-level session that spawned the subagent whose transcript is `sessionFile`, or `""`
 * when this process cannot say.
 *
 * The walk is OMP's layout: `<parent>.jsonl` sits beside the directory holding `<parent>/`'s
 * subagent transcripts, and a subagent of a subagent nests one level deeper, so each step takes
 * the transcript's directory and appends `.jsonl` until a recorded session matches.
 *
 * When that resolves nothing — no transcript path at all, or no recorded session along it — one
 * recorded session is used only when it is the only one in the process and it has an id, where
 * there is nothing it could be confused with. Anything else reports no address: a subagent that
 * names an unrelated live session sends its peers to a session that never spawned it, which is
 * worse than a subagent that admits it has no reply address.
 */
export function resolveEnvoySession(sessionFile: string | undefined): string {
  const sessions = recordedSessions();
  for (let candidate = sessionFile; candidate !== undefined; ) {
    const directory = path.dirname(candidate);
    const parent = `${directory}.jsonl`;
    if (parent === candidate) break;
    const recorded = sessions.get(parent);
    if (recorded !== undefined) return recorded;
    // The filesystem root is its own parent; stop rather than walking it forever.
    if (path.dirname(directory) === directory) break;
    candidate = parent;
  }
  if (sessions.size !== 1) return "";
  return [...sessions.values()][0] ?? "";
}

// The record outlives every extension instance in the process; a suite that boots several
// sessions in one process clears it between tests. Production callers never call this.
export function resetEnvoySessionsForTests(): void {
  delete store[ENVOY_SESSIONS];
}

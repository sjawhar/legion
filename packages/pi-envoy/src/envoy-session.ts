/**
 * The Envoy session id this process's top-level session registered, published for the extension
 * instances that have none of their own.
 *
 * A `task` subagent loads its own instance of `extensions/envoy.ts` inside the parent's process,
 * and that instance registers nothing: its `session_start` returns early, so its module
 * `sessionID` stays empty (a registered subagent left an untitled row heartbeating for the life
 * of the parent process). The address a reply to such a subagent can reach is therefore its
 * parent's registered session — the id this record carries. It lives on `globalThis` under a
 * process-wide symbol, beside the bootstrapped-session record in `subagent-session.ts`, because
 * each instance has a closure of its own. A nested subagent reads the same record and gets the
 * same top-level session, which is the one that is registered.
 */
const ENVOY_SESSION = Symbol.for("legion.pi-envoy.envoy-session");

interface GlobalEnvoySessionStore {
  [key: symbol]: string | undefined;
}

const envoySessionStore = globalThis as unknown as GlobalEnvoySessionStore;

/** Published by the top-level instance for every id it adopts, an empty one included. */
export function recordEnvoySession(sessionID: string): void {
  envoySessionStore[ENVOY_SESSION] = sessionID;
}

/** The top-level session's id, or `""` when this process never took one. */
export function envoyProcessSession(): string {
  return envoySessionStore[ENVOY_SESSION] ?? "";
}

// The record outlives every extension instance in the process; a suite that boots several
// sessions in one process clears it between tests. Production callers never call this.
export function resetEnvoySessionForTests(): void {
  delete envoySessionStore[ENVOY_SESSION];
}

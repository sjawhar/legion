import fs from "node:fs";
import path from "node:path";
import type { SessionContext } from "./pi-types";

// The transcript path of the session this process bootstrapped as its Legion identity (root
// architect, phase worker, or controller). A `task` subagent's extension instance is a separate
// module instance with its own closure state, so the record lives on `globalThis` under a
// process-wide symbol, beside `LEGION_ROLE_CLAIM_BRIDGE` (role-claim-bridge.ts) and
// `LEGION_LOADED_MARKER` (extensions/legion.ts).
const LEGION_BOOTSTRAPPED_SESSION = Symbol.for("legion.pi-envoy.bootstrapped-session");

interface GlobalLegionBootstrappedSessionStore {
  [key: symbol]: string | undefined;
}

const bootstrappedSessionStore = globalThis as unknown as GlobalLegionBootstrappedSessionStore;

export function recordBootstrappedSession(sessionFile: string): void {
  bootstrappedSessionStore[LEGION_BOOTSTRAPPED_SESSION] = sessionFile;
}

// The record outlives every extension instance in the process; a test suite that boots several
// Legion sessions in one process clears it between tests. Production callers never call this.
export function resetLegionBootstrappedSessionForTests(): void {
  delete bootstrappedSessionStore[LEGION_BOOTSTRAPPED_SESSION];
}

/**
 * Whether this session is a `task`-spawned subagent of the process's top-level session.
 *
 * A subagent loads a fresh instance of every extension module in the same OS process and fires
 * its own `session_start`, so an extension that treats every `session_start` as a new top-level
 * session acts twice: `extensions/legion.ts` would re-run the Legion bootstrap with the parent's
 * already-consumed boot token (and its failure exit would kill the parent), and
 * `extensions/envoy.ts` would register the subagent with the Envoy listener as a session of its
 * own — an untitled row per subagent, kept alive by the subagent instance's heartbeat for as long
 * as the parent process runs. A subagent shares its parent's identity: it claims no role, calls
 * no daemon route, and registers no Envoy session.
 *
 * Two signals. When the transcript is a real file, OMP's own layout for file storage decides: a
 * subagent's transcript file sits inside a directory named after its parent's transcript file
 * minus the `.jsonl` extension, so `fs.existsSync(path.dirname(sessionFile) + ".jsonl")` finds
 * the parent (oh-my-pi `packages/coding-agent/src/session/session-manager.ts`,
 * `resolveInteractiveRoot`). That check is authoritative because it reads the current layout: a
 * top-level session that rotated its transcript (`/new`, `/fork`) and then reloaded its
 * extensions is still a top-level layout. When the transcript is not a file on disk (a SQL row,
 * or no transcript at all), the process-local record decides: once this process has bootstrapped
 * a Legion session — whose transcript always is a file — every session whose path differs is a
 * subagent. Outside a Legion process, a transcript-less session (an in-memory or `--no-session`
 * run) is taken to be top-level: nothing distinguishes it from an ephemeral top-level run.
 */
export async function isSubagentSession(context: SessionContext): Promise<boolean> {
  await context.sessionManager.ensureOnDisk();
  const sessionFile = context.sessionManager.getSessionFile();
  if (sessionFile !== undefined && fs.existsSync(sessionFile)) {
    return fs.existsSync(`${path.dirname(sessionFile)}.jsonl`);
  }
  const bootstrapped = bootstrappedSessionStore[LEGION_BOOTSTRAPPED_SESSION];
  return bootstrapped !== undefined && bootstrapped !== sessionFile;
}

/**
 * `isSubagentSession`, evaluated at most once per extension instance: a subagent session's
 * transcript path never changes over its lifetime, and the check gates several hooks.
 */
export function subagentSessionCheck(): (context: SessionContext) => Promise<boolean> {
  let subagentSession: Promise<boolean> | undefined;
  return (context) => {
    subagentSession ??= isSubagentSession(context);
    return subagentSession;
  };
}

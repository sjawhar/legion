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
 * OMP names a top-level transcript `<sessions bucket>/<file-safe ISO timestamp>_<uuid v7>.jsonl`
 * and puts a subagent's transcript at `<that path minus .jsonl>/<Agent>.jsonl`, so the directory
 * a subagent's transcript sits in is itself named like a transcript. A top-level transcript's
 * directory is the sessions bucket (a cwd slug such as `-home-ubuntu-tmp`), which never has this
 * shape. (oh-my-pi `packages/coding-agent/src/session/session-manager.ts`: `fileSafeTimestamp`,
 * `mintSessionId`, `resolveInteractiveRoot`.)
 */
const TRANSCRIPT_DIRECTORY =
  /^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-\d{3}Z_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

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
 * Three signals, checked in this order. When the transcript is a real file, OMP's own file
 * layout decides — `fs.existsSync(path.dirname(sessionFile) + ".jsonl")` finds a subagent's
 * parent — and that answer is final because it reads the current layout: a top-level session
 * that rotated its transcript (`/new`, `/fork`) is still a top-level layout. Otherwise (a SQL
 * row under `OMP_SESSION_STORAGE=sql` still carries the `.jsonl`-shaped logical path) the path's
 * own shape decides through `TRANSCRIPT_DIRECTORY`. Otherwise the process-local record decides:
 * once this process has bootstrapped a Legion session, every session whose path differs is a
 * subagent. A transcript-less session (in-memory, or a `--no-session` run) outside a Legion
 * process is taken to be top-level: nothing distinguishes it from an ephemeral top-level run.
 */
export async function isSubagentSession(context: SessionContext): Promise<boolean> {
  await context.sessionManager.ensureOnDisk();
  const sessionFile = context.sessionManager.getSessionFile();
  if (sessionFile !== undefined) {
    const directory = path.dirname(sessionFile);
    if (fs.existsSync(sessionFile)) return fs.existsSync(`${directory}.jsonl`);
    if (TRANSCRIPT_DIRECTORY.test(path.basename(directory))) return true;
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

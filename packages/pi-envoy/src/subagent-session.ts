import fs from "node:fs";
import path from "node:path";
import type { SessionContext } from "./pi-types";

// Process-wide records shared by every extension instance in the process. A `task` subagent's
// extension instance is a separate module instance with its own closure state, so anything the
// instances must agree on lives on `globalThis` under a process-wide symbol, beside
// `LEGION_ROLE_CLAIM_BRIDGE` (role-claim-bridge.ts) and `LEGION_LOADED_MARKER`
// (extensions/legion.ts).
const LEGION_BOOTSTRAPPED_SESSION = Symbol.for("legion.pi-envoy.bootstrapped-session");
const PRIMARY_ENVOY_INSTANCE = Symbol.for("legion.pi-envoy.primary-envoy-instance");

interface GlobalRecords {
  [LEGION_BOOTSTRAPPED_SESSION]?: string;
  [PRIMARY_ENVOY_INSTANCE]?: object;
}

const records = globalThis as unknown as GlobalRecords;

/**
 * The transcript path of the session this process bootstrapped as its Legion identity (root
 * architect, phase worker, or controller).
 */
export function recordBootstrappedSession(sessionFile: string): void {
  records[LEGION_BOOTSTRAPPED_SESSION] = sessionFile;
}

// The records outlive every extension instance in the process; a test suite that boots several
// sessions in one process clears them between tests. Production callers never call these.
export function resetLegionBootstrappedSessionForTests(): void {
  delete records[LEGION_BOOTSTRAPPED_SESSION];
}

export function resetPrimaryEnvoyInstanceForTests(): void {
  delete records[PRIMARY_ENVOY_INSTANCE];
}

/**
 * Whether `instance` is the extension instance that speaks for this process's top-level session.
 *
 * One OMP process runs one top-level session, and its envoy extension instance is the first in
 * the process to see a `session_start`; every extension instance a `task` spawns later belongs
 * to a subagent. The first caller claims the slot and keeps it for the life of the process (a
 * top-level session's `/new`, `/fork`, and `/reload-plugins` all keep the same instance — only a
 * process restart makes a new one, and a new process has an empty slot). Storage-independent:
 * it needs no transcript at all, so it covers a subagent whose parent runs `--no-session` (its
 * transcript lands in a temporary `omp-task-*` directory with no parent beside it) and a
 * subagent under `OMP_SESSION_STORAGE=sql`, where `isSubagentSession` has nothing on disk to read.
 */
export function claimPrimaryEnvoyInstance(instance: object): boolean {
  records[PRIMARY_ENVOY_INSTANCE] ??= instance;
  return records[PRIMARY_ENVOY_INSTANCE] === instance;
}

/**
 * Whether this session is a `task`-spawned subagent of the process's top-level session, read
 * from its transcript.
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
 * top-level session that rotated its transcript (`/new`, `/fork`) is still a top-level layout.
 * When the transcript is not a file on disk (a SQL row, or no transcript at all), the process-
 * local record decides: once this process has bootstrapped a Legion session — whose transcript
 * always is a file — every session whose path differs is a subagent. Outside a Legion process a
 * transcript-less or SQL-backed subagent is invisible here; `claimPrimaryEnvoyInstance` is what
 * catches it.
 */
export async function isSubagentSession(context: SessionContext): Promise<boolean> {
  await context.sessionManager.ensureOnDisk();
  const sessionFile = context.sessionManager.getSessionFile();
  if (sessionFile !== undefined && fs.existsSync(sessionFile)) {
    return fs.existsSync(`${path.dirname(sessionFile)}.jsonl`);
  }
  const bootstrapped = records[LEGION_BOOTSTRAPPED_SESSION];
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

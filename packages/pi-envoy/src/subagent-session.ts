import fs from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import * as host from "@oh-my-pi/pi-coding-agent";
import { logger } from "@oh-my-pi/pi-utils";
import type { CommandContext, SessionContext } from "./pi-types";

/**
 * What these checks read: the live session manager, which a `SessionContext` and the
 * `CommandContext` a slash command is handed both carry (the same object).
 */
export type SessionIdentityContext = Pick<SessionContext | CommandContext, "sessionManager">;

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

/** What `isSubagentSession` found. */
interface SubagentSessionAnswer {
  readonly subagent: boolean;
  /**
   * Whether the answer holds for the session's life. False only when the transcript could not be
   * published first: the answer is read from what is on disk at that moment.
   */
  readonly settled: boolean;
}

/**
 * Whether this session is a `task`-spawned subagent of the process's top-level session (or an
 * advisor's session).
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
 * The host's roster answers first (`registeredSubagent`): it needs no transcript and no write.
 * Only when it gives no opinion does the transcript decide (`transcriptSaysSubagent`), after
 * `ensureOnDisk` has published it so the on-disk layout can be read. That publish stays: with no
 * roster opinion the transcript is the only signal, and a top-level session whose own file is not
 * on disk yet would read as a subagent wherever this process recorded a different bootstrapped
 * transcript. It fails while another writer holds the transcript's publish lock (Oh My Pi's
 * `SessionLockError`); the check then answers from what is on disk now and reports itself
 * unsettled, so the next call asks again rather than failing the call that asked.
 */
async function isSubagentSession(context: SessionIdentityContext): Promise<SubagentSessionAnswer> {
  const registered = registeredSubagent(context);
  if (registered !== undefined) return { subagent: registered, settled: true };
  try {
    await context.sessionManager.ensureOnDisk();
  } catch (error) {
    const subagent = transcriptSaysSubagent(context);
    logger.warn(
      "subagent check: publishing the transcript failed; answered from the transcript on disk, and the next call asks again",
      { sessionFile: context.sessionManager.getSessionFile(), subagent, error: messageFor(error) }
    );
    return { subagent, settled: false };
  }
  return { subagent: transcriptSaysSubagent(context), settled: true };
}

/**
 * The transcript's answer, read without writing it.
 *
 * Two signals. OMP's own layout for file storage: a subagent's transcript file sits inside a
 * directory named after its parent's transcript file minus the `.jsonl` extension, so
 * `fs.existsSync(path.dirname(sessionFile) + ".jsonl")` finds the parent (oh-my-pi
 * `packages/coding-agent/src/session/session-manager.ts`, `resolveInteractiveRoot`), whether or
 * not the subagent's own file has been written yet. When the session's own transcript is a file
 * on disk with no parent beside it, that layout is a top-level one, even for a top-level session
 * that rotated its transcript (`/new`, `/fork`). When the transcript is not a file on disk (a SQL
 * row, no transcript at all, or a file storage publish that has not happened yet), the
 * process-local record decides: once this process has bootstrapped a Legion session — whose
 * transcript always is a file — every session whose path differs is a subagent. Outside a Legion
 * process a transcript-less or SQL-backed subagent is invisible here; the roster is what catches
 * it.
 */
function transcriptSaysSubagent(context: SessionIdentityContext): boolean {
  const sessionFile = context.sessionManager.getSessionFile();
  if (sessionFile !== undefined) {
    if (fs.existsSync(`${path.dirname(sessionFile)}.jsonl`)) return true;
    if (fs.existsSync(sessionFile)) return false;
  }
  const bootstrapped = bootstrappedSessionStore[LEGION_BOOTSTRAPPED_SESSION];
  return bootstrapped !== undefined && bootstrapped !== sessionFile;
}

/**
 * Whether the host registered this session as a subagent's (or an advisor's), read from OMP's
 * own roster: `createAgentSession` registers every session in the process-wide `AgentRegistry`
 * with `kind: "main"` for a top-level session and `"sub"`/`"advisor"` otherwise, and attaches
 * the live session before the extension runner's `session_start` fires (oh-my-pi
 * `packages/coding-agent/src/sdk.ts`). Storage-independent, and per session rather than per
 * process — an ACP host runs several top-level sessions in one process, each `"main"` — so it
 * covers a subagent whose parent runs `--no-session` (its transcript lands in a temporary
 * `omp-task-*` directory with no parent beside it) and a subagent under
 * `OMP_SESSION_STORAGE=sql`. `undefined` is no opinion: a host build that does not export the
 * registry, or a session the global roster does not list (the host's SDK lets an embedder give
 * a session a registry of its own).
 */
function registeredSubagent(context: SessionIdentityContext): boolean | undefined {
  // The namespace member is undefined on a host build without the export; a named import
  // would make that a load failure for the whole extension.
  const registry = host.AgentRegistry?.global();
  if (registry === undefined) return undefined;
  const sessionID = context.sessionManager.getSessionId();
  const sessionFile = context.sessionManager.getSessionFile();
  const own = registry
    .list()
    .filter((ref) =>
      ref.session === null
        ? sessionFile !== undefined && ref.sessionFile === sessionFile
        : ref.session.sessionManager.getSessionId() === sessionID
    );
  if (own.length === 0) return undefined;
  return own.some((ref) => ref.kind !== "main");
}

/**
 * `isSubagentSession` for one extension instance, which gates several of its hooks: a settled
 * answer is kept for the instance's life, since a session's kind and transcript path never
 * change; an unsettled one answers only the call that made it, and the next call asks again.
 * Each hook calls it once and passes the answer on (envoy.ts's `replyAddress` takes it; legion.ts
 * asks once per session change for both the title and the controller's re-claim).
 */
export function subagentSessionCheck(): (context: SessionIdentityContext) => Promise<boolean> {
  let settled: boolean | undefined;
  return async (context) => {
    if (settled !== undefined) return settled;
    const answer = await isSubagentSession(context);
    if (answer.settled) settled = answer.subagent;
    return answer.subagent;
  };
}

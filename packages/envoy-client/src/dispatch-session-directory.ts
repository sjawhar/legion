import { randomUUID } from "node:crypto";
import { renameSync, rmSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

/**
 * Where the `dispatch` CLI's per-session state lives (`<root>/sessions/<session id>`) and how a
 * file in it is replaced. A leaf: `dispatch-session-state.ts` (which imports `dispatch-execute`)
 * and `session-answers.ts` (which `dispatch-execute` imports) both use it without a cycle.
 */

const SESSION_ID = /^[A-Za-z0-9._-]{1,128}$/;
const PRIVATE_FILE = 0o600;

/** `DISPATCH_STATE_DIR`, else `<XDG_STATE_HOME or ~/.local/state>/dispatch`. Empty counts as unset. */
export function sessionStateRoot(env: Readonly<Record<string, string | undefined>>): string {
  // Bracket access, not a dot: claude-envoy type-checks this source under
  // noPropertyAccessFromIndexSignature, which refuses a dot on an index signature.
  // biome-ignore-start lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access
  const stateDir = env["DISPATCH_STATE_DIR"];
  if (stateDir) return stateDir;
  return join(env["XDG_STATE_HOME"] || join(homedir(), ".local", "state"), "dispatch");
  // biome-ignore-end lint/complexity/useLiteralKeys: see above
}

/** `<root>/sessions/<id>`; an id that is not one plain path segment is refused. */
export function sessionDirectory(
  env: Readonly<Record<string, string | undefined>>,
  sessionId: string
): string {
  if (!SESSION_ID.test(sessionId) || sessionId === "." || sessionId === "..") {
    throw new Error(
      `dispatch: the session id ${JSON.stringify(sessionId)} is not 1-128 letters, digits, ".", "_" or "-"`
    );
  }
  return join(sessionStateRoot(env), "sessions", sessionId);
}

/** Writes `text` to `path`, owner-only, through a temporary file beside it and `rename`, so a
 *  reader in any process sees the old content or the new, never part of one. A failed write
 *  leaves no temporary file. The directory must exist. */
export function writeAtomically(path: string, text: string): void {
  const temporary = `${path}.${process.pid}.${randomUUID()}.tmp`;
  try {
    writeFileSync(temporary, text, { mode: PRIVATE_FILE });
    renameSync(temporary, path);
  } catch (error) {
    rmSync(temporary, { force: true });
    throw error;
  }
}

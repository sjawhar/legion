/**
 * Write-existing-file guard hook.
 *
 * Prevents agents from overwriting existing files without reading them first —
 * a common cause of data loss. The guard adds a soft warning (does NOT block)
 * when a Write or Edit tool targets a file that exists on disk but has not been
 * Read in the current session.
 *
 * Integration points:
 *   - tool.execute.after: tracks which files were Read in each session
 *   - tool.execute.before: checks Write/Edit targets against the read registry
 *   - event (session.deleted): cleans up per-session tracking
 */

import { existsSync } from "node:fs";
import { normalize } from "node:path";
import { isRecord, resolveSessionID } from "./utils";

// ─── Read File Registry ──────────────────────────────────────────────────────

export interface ReadFileRegistry {
  trackRead: (sessionID: string, filePath: string) => void;
  hasRead: (sessionID: string, filePath: string) => boolean;
  cleanup: (sessionID: string) => void;
}

/**
 * Per-session registry of files that have been Read. Exported separately so
 * other hooks can query read state if needed.
 */
export function createReadFileRegistry(): ReadFileRegistry {
  const sessions = new Map<string, Set<string>>();

  return {
    trackRead(sessionID: string, filePath: string): void {
      let fileSet = sessions.get(sessionID);
      if (!fileSet) {
        fileSet = new Set<string>();
        sessions.set(sessionID, fileSet);
      }
      fileSet.add(normalize(filePath));
    },

    hasRead(sessionID: string, filePath: string): boolean {
      return sessions.get(sessionID)?.has(normalize(filePath)) ?? false;
    },

    cleanup(sessionID: string): void {
      sessions.delete(sessionID);
    },
  };
}

// ─── Tool Name Sets ──────────────────────────────────────────────────────────

/** Tools that count as "reading" a file's contents. */
const READ_TOOLS = new Set(["read", "mcp_read"]);

/** Tools that write/modify files and should be guarded. */
const WRITE_TOOLS = new Set(["write", "edit", "mcp_write", "mcp_edit"]);

// ─── Guard Hook ──────────────────────────────────────────────────────────────

export interface WriteExistingFileGuard {
  "tool.execute.before": (
    input: { tool: string; sessionID: string; callID: string; args?: unknown },
    output: { args: Record<string, unknown> }
  ) => void;
  "tool.execute.after": (
    input: { tool: string; sessionID: string; callID: string; args?: unknown },
    output: { title: string; output: string; metadata: unknown }
  ) => void;
  event: (input: { event: { type: string; properties?: unknown } }) => Promise<void>;
}

export function createWriteExistingFileGuard(): WriteExistingFileGuard {
  const registry = createReadFileRegistry();

  // ── tool.execute.after: track reads ──────────────────────────────────────

  const toolExecuteAfter = (
    input: { tool: string; sessionID: string; callID: string; args?: unknown },
    _output: { title: string; output: string; metadata: unknown }
  ): void => {
    const toolName = input.tool?.toLowerCase();
    if (!READ_TOOLS.has(toolName)) return;

    const sessionID = input.sessionID;
    if (!sessionID) return;

    const args = isRecord(input.args) ? input.args : undefined;
    const filePath = typeof args?.filePath === "string" ? args.filePath : undefined;
    if (!filePath) return;

    registry.trackRead(sessionID, filePath);
  };

  // ── tool.execute.before: guard writes ────────────────────────────────────

  const toolExecuteBefore = (
    input: { tool: string; sessionID: string; callID: string; args?: unknown },
    output: { args: Record<string, unknown> }
  ): void => {
    const toolName = input.tool?.toLowerCase();
    if (!WRITE_TOOLS.has(toolName)) return;

    const sessionID = input.sessionID;
    if (!sessionID) return;

    const outputArgs = isRecord(output.args) ? output.args : undefined;
    const filePath = typeof outputArgs?.filePath === "string" ? outputArgs.filePath : undefined;
    if (!filePath) return;

    const normalizedPath = normalize(filePath);

    // Skip if the file doesn't exist on disk — it's a new file creation
    if (!existsSync(normalizedPath)) return;

    // Skip if the file was already read in this session
    if (registry.hasRead(sessionID, normalizedPath)) return;

    // Soft warning — do NOT throw
    const warning =
      `[Write Guard] File "${normalizedPath}" has not been read in this session. ` +
      "You should read the file first to avoid accidentally overwriting existing content.";

    output.args.__warning = warning;
    console.warn(
      `[opencode-legion] Write guard: ${normalizedPath} written without prior read in session ${sessionID}`
    );
  };

  // ── event: cleanup on session.deleted ────────────────────────────────────

  const event = async ({
    event,
  }: {
    event: { type: string; properties?: unknown };
  }): Promise<void> => {
    if (event.type !== "session.deleted") return;

    const props = isRecord(event.properties) ? event.properties : undefined;
    const sessionID = resolveSessionID(props);
    if (sessionID) {
      registry.cleanup(sessionID);
    }
  };

  return {
    "tool.execute.before": toolExecuteBefore,
    "tool.execute.after": toolExecuteAfter,
    event,
  };
}

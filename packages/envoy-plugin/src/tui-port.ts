import { execFileSync } from "node:child_process";

type ExecSyncFn = (command: string, args: string[], options: { encoding: string }) => string;

/**
 * Parse the port from an OpenCode serve baseUrl.
 *
 * Synchronous and URL-only: the TUI plugin runs in-process with the
 * OpenCode TUI, which always knows the baseUrl of its serve daemon.
 * This is separate from the server-side `resolvePort` helper, which
 * additionally consults `ss(8)` by PID — irrelevant in the TUI process.
 *
 * Returns null when input is missing, malformed, or has no explicit
 * numeric port.
 */
export function parsePort(baseUrl: string | undefined): number | null {
  if (!baseUrl) return null;
  let parsed: URL;
  try {
    parsed = new URL(baseUrl);
  } catch {
    return null;
  }
  if (!parsed.port) return null;
  const port = Number.parseInt(parsed.port, 10);
  if (!Number.isFinite(port) || port <= 0) return null;
  return port;
}

const defaultExecSync: ExecSyncFn = (command, args, options) =>
  execFileSync(command, args, { encoding: options.encoding as BufferEncoding }) as string;

export function resolveCurrentProcessPort(exec: ExecSyncFn = defaultExecSync): number | null {
  return resolveProcessPort(process.pid, exec);
}

function resolveProcessPort(pid: number, exec: ExecSyncFn = defaultExecSync): number | null {
  try {
    const output = exec("ss", ["-tlnp"], { encoding: "utf-8" });
    for (const line of output.split("\n")) {
      if (!line.includes(`pid=${pid}`)) continue;
      const parts = line.trim().split(/\s+/);
      const local = parts[3];
      const match = local?.match(/:(\d+)$/);
      if (!match) continue;
      const port = Number.parseInt(match[1], 10);
      if (Number.isFinite(port) && port > 0) return port;
    }
  } catch {}

  return null;
}

export function resolveSessionProcessPort(
  sessionID: string,
  exec: ExecSyncFn = defaultExecSync
): number | null {
  try {
    const output = exec("ps", ["-eo", "pid=,args="], { encoding: "utf-8" });
    for (const line of output.split("\n")) {
      if (!line.includes(sessionID)) continue;
      if (!/(^|\s)-s\s+/.test(line)) continue;
      const pid = Number.parseInt(line.trim().split(/\s+/, 1)[0] ?? "", 10);
      if (!Number.isFinite(pid) || pid <= 0) continue;
      const port = resolveProcessPort(pid, exec);
      if (port !== null) return port;
    }
  } catch {}

  return null;
}

export function resolveTuiPort(
  baseUrl: string | undefined,
  sessionID?: string,
  exec: ExecSyncFn = defaultExecSync
): number | null {
  return (
    parsePort(baseUrl) ??
    (sessionID ? resolveSessionProcessPort(sessionID, exec) : null) ??
    resolveCurrentProcessPort(exec)
  );
}

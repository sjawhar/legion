import { execFileSync } from "node:child_process";

/**
 * Resolve the serve port from the server URL, with ss(8) fallback.
 *
 * Priority:
 * 1. URL.port (standard: non-empty for non-default ports)
 * 2. ss -tlnp PID match (finds listening port by process ID)
 * 3. null (caller decides how to handle)
 */
export function resolvePort(
  serverUrl: URL,
  exec: typeof execFileSync = execFileSync
): number | null {
  // Try URL.port first (standard path for non-default ports like 4096, 13381)
  const urlPort = Number.parseInt(serverUrl.port, 10);
  if (Number.isFinite(urlPort) && urlPort > 0) return urlPort;

  // Fallback: find listening port via ss(8) by PID
  try {
    const output = exec("ss", ["-tlnp"], { encoding: "utf-8" }) as string;
    for (const line of output.split("\n")) {
      if (!line.includes(`pid=${process.pid}`)) continue;
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

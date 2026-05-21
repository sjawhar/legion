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

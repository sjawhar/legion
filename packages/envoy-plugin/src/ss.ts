/**
 * Parse the listening TCP port for `pid` from `ss -tlnp` output.
 *
 * Shared by the server-side and TUI-side port resolvers, which differ only in
 * how they discover the pid and whether they exec ss(8) sync or async.
 */
export function portFromSsOutput(output: string, pid: number): number | null {
  const pidPattern = new RegExp(`\\bpid=${pid}\\b`);
  for (const line of output.split("\n")) {
    if (!pidPattern.test(line)) continue;
    const parts = line.trim().split(/\s+/);
    const local = parts[3];
    const match = local?.match(/:(\d+)$/);
    if (!match) continue;
    const port = Number.parseInt(match[1], 10);
    if (Number.isFinite(port) && port > 0) return port;
  }
  return null;
}

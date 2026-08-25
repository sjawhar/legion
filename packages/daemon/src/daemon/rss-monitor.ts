import { readFileSync } from "node:fs";

const PAGE_SIZE = 4096;

/**
 * Read the RSS (Resident Set Size) of a process from /proc/<pid>/statm.
 * Returns RSS in bytes, or null if the read fails (non-Linux, process dead, etc.).
 *
 * Uses /proc/statm instead of process.memoryUsage() because the latter
 * underreports by ~12x for Bun processes due to bmalloc native allocations
 * (see bun#28318).
 */
export function readProcessRssBytes(
  pid: number,
  readFile: (path: string) => string = (p) => readFileSync(p, "utf-8")
): number | null {
  try {
    const content = readFile(`/proc/${pid}/statm`);
    // Format: size resident shared text lib data dt (all in pages)
    const fields = content.trim().split(/\s+/);
    const residentPages = Number(fields[1]);
    if (!Number.isFinite(residentPages) || residentPages < 0) {
      return null;
    }
    return residentPages * PAGE_SIZE;
  } catch {
    return null;
  }
}

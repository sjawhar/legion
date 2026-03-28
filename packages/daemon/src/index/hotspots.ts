import type { BuildDependencyGraphOptions } from "./graph";
import type { ChangeHotspotEntry } from "./types";

const DEFAULT_HOTSPOT_HISTORY_LIMIT = 100;

const JJ_LOG_DATE_PATTERN = /(\d{4}-\d{2}-\d{2}) (\d{2}:\d{2}:\d{2})/;
const FILE_STAT_PATTERN = /^(.+?)\s+\|\s+\d+\s+[+-]+$/;
const GRAPH_PREFIX_PATTERN = /^[\s│├└┌┐┘┴┬─╭╮╯╰◆○@~]+/u;

export interface JjCommandResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

export type JjCommandRunner = (rootDir: string, args: string[]) => Promise<JjCommandResult>;

function defaultHotspotRunner(rootDir: string, args: string[]): Promise<JjCommandResult> {
  const proc = Bun.spawn(["jj", ...args], {
    cwd: rootDir,
    stdout: "pipe",
    stderr: "pipe",
  });

  return Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]).then(([stdout, stderr, exitCode]) => ({
    stdout,
    stderr,
    exitCode,
  }));
}

function normalizeGraphPrefix(line: string): string {
  return line.replace(GRAPH_PREFIX_PATTERN, "").trimEnd();
}

function parseLogTimestamp(line: string): string | null {
  const match = JJ_LOG_DATE_PATTERN.exec(line);
  if (!match) {
    return null;
  }

  const date = new Date(`${match[1]}T${match[2]}`);
  if (Number.isNaN(date.getTime())) {
    return null;
  }

  return date.toISOString();
}

function parseFilePathFromStatLine(line: string): string | null {
  if (!line.includes("|")) {
    return null;
  }

  const normalized = normalizeGraphPrefix(line);
  const match = FILE_STAT_PATTERN.exec(normalized);
  if (!match) {
    return null;
  }

  const filePath = match[1]?.trim();
  if (!filePath || filePath.endsWith("files changed")) {
    return null;
  }

  return filePath;
}

export function parseJjLogStatOutput(output: string): ChangeHotspotEntry[] {
  interface HotspotAccumulator {
    changeCount: number;
    lastChanged: string;
  }

  const byPath = new Map<string, HotspotAccumulator>();
  let currentCommitTimestamp: string | null = null;

  for (const line of output.split(/\r?\n/)) {
    const parsedTimestamp = parseLogTimestamp(line);
    if (parsedTimestamp) {
      currentCommitTimestamp = parsedTimestamp;
      continue;
    }

    const filePath = parseFilePathFromStatLine(line);
    if (!filePath || !currentCommitTimestamp) {
      continue;
    }

    const existing = byPath.get(filePath);
    if (!existing) {
      byPath.set(filePath, {
        changeCount: 1,
        lastChanged: currentCommitTimestamp,
      });
      continue;
    }

    existing.changeCount += 1;
    if (currentCommitTimestamp > existing.lastChanged) {
      existing.lastChanged = currentCommitTimestamp;
    }
  }

  const hotspots = Array.from(byPath.entries()).map(([filePath, hotspot]) => ({
    filePath,
    changeCount: hotspot.changeCount,
    lastChanged: hotspot.lastChanged,
  }));

  hotspots.sort((a, b) => {
    if (b.changeCount !== a.changeCount) {
      return b.changeCount - a.changeCount;
    }

    return a.filePath.localeCompare(b.filePath);
  });

  return hotspots;
}

export async function buildChangeHotspots(
  rootDir: string,
  options?: BuildDependencyGraphOptions
): Promise<ChangeHotspotEntry[]> {
  const historyLimit = options?.hotspotHistoryLimit ?? DEFAULT_HOTSPOT_HISTORY_LIMIT;
  const runner = options?.hotspotCommandRunner ?? defaultHotspotRunner;

  try {
    const result = await runner(rootDir, [
      "log",
      "--stat",
      "--no-pager",
      "-n",
      String(historyLimit),
    ]);

    if (result.exitCode !== 0) {
      options?.warn?.(`[index] Failed to compute hotspots from jj log: ${result.stderr.trim()}`);
      return [];
    }

    return parseJjLogStatOutput(result.stdout);
  } catch (error) {
    options?.warn?.(`[index] Failed to compute hotspots from jj log: ${String(error)}`);
    return [];
  }
}

// Cwd-derived defaults for the dispatch tool: which GitHub repo a thread
// belongs to, and where a human jumps back to reply. Every host (OMP,
// OpenCode, Claude) runs the same shim in the session's own working
// directory, so this is the one place that needs to know about it.

import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { machineID } from "./machine";

const execFileAsync = promisify(execFile);

/** One shell-out, shaped like Node's promisified `execFile`. Injectable for tests. */
export type ExecFn = (
  file: string,
  args: string[],
  options: { readonly cwd: string }
) => Promise<{ readonly stdout: string }>;

/** Real exec used outside tests: a 5s timeout keeps a broken jj/git/tmux from hanging the shim. */
export const defaultExec: ExecFn = (file, args, options) =>
  execFileAsync(file, args, { cwd: options.cwd, timeout: 5_000 });

/**
 * The coding-agent hosts that identify themselves in the environment.
 * `resolveOrigin` is the only producer, so this union is exhaustive; the
 * dispatch dashboard mirrors it as `OriginHost`.
 */
export type DispatchHost = "omp" | "claude";

/** Provenance attached to a dispatch thread so a human can jump back to the asking session. */
export interface DispatchOrigin {
  readonly host?: DispatchHost;
  readonly machine?: string;
  readonly cwd: string;
  /** Human-readable `session:window.pane`; ambiguous inside a tmux session group. */
  readonly tmux?: string;
  /** Stable pane id (`%N`); `tmux switch-client -t %N` jumps there from anywhere. */
  readonly pane?: string;
}

async function tryExec(
  exec: ExecFn,
  file: string,
  args: string[],
  cwd: string
): Promise<string | null> {
  try {
    const { stdout } = await exec(file, args, { cwd });
    return stdout;
  } catch {
    return null;
  }
}

/** Parse `name<whitespace>url` lines, the shape of `jj git remote list` output. */
function parseRemoteList(stdout: string): ReadonlyMap<string, string> {
  const remotes = new Map<string, string>();
  for (const line of stdout.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const match = trimmed.match(/^(\S+)\s+(\S+)/);
    const [, name, url] = match ?? [];
    if (name && url) remotes.set(name, url);
  }
  return remotes;
}

/** `origin` wins; otherwise exactly one non-`upstream` remote; otherwise none. */
function selectRemoteUrl(remotes: ReadonlyMap<string, string>): string | null {
  const origin = remotes.get("origin");
  if (origin) return origin;
  const candidates = [...remotes.entries()].filter(([name]) => name !== "upstream");
  return candidates.length === 1 ? (candidates[0]?.[1] ?? null) : null;
}

// Host match is case-insensitive; https remotes may carry userinfo
// (`user@` or `x-access-token:…@`) ahead of the host.
const GITHUB_REMOTE_PATTERNS = [
  /^https:\/\/(?:[^@/\s]+@)?github\.com\/([^/]+)\/([^/]+?)(?:\.git)?\/?$/i,
  /^git@github\.com:([^/]+)\/([^/]+?)(?:\.git)?\/?$/i,
  /^ssh:\/\/git@github\.com\/([^/]+)\/([^/]+?)(?:\.git)?\/?$/i,
];

/** Parse a GitHub remote URL (https, scp-like, or ssh) into `owner/name`; null for any other host. */
export function parseGitHubRemoteUrl(url: string): string | null {
  const trimmed = url.trim();
  for (const pattern of GITHUB_REMOTE_PATTERNS) {
    const match = trimmed.match(pattern);
    if (match) return `${match[1]}/${match[2]}`;
  }
  return null;
}

/**
 * Resolve the cwd's GitHub repo for dispatch's default `repo` argument.
 *
 * Tries `jj git remote list` first: a jj-colocated repo lists every remote
 * in one call, so `origin` wins over `upstream`, and a single remaining
 * remote is used when there is no `origin` (never `upstream` alone — that's
 * the read-only upstream, not the user's fork). When jj is unavailable or
 * the cwd is not a jj repo, falls back to plain `git remote get-url origin`.
 * Returns null when nothing resolves to a GitHub repo.
 */
export async function resolveCwdRepo(cwd: string, exec: ExecFn): Promise<string | null> {
  const jjOutput = await tryExec(exec, "jj", ["git", "remote", "list"], cwd);
  if (jjOutput !== null) {
    const url = selectRemoteUrl(parseRemoteList(jjOutput));
    return url ? parseGitHubRemoteUrl(url) : null;
  }
  const originUrl = await tryExec(exec, "git", ["remote", "get-url", "origin"], cwd);
  return originUrl ? parseGitHubRemoteUrl(originUrl) : null;
}

/**
 * Best-effort provenance for a dispatch thread: which host app, which
 * machine, the session cwd, and — inside tmux — the pane to jump back to.
 * Every field but `cwd` is omitted rather than guessed when it can't be
 * determined.
 */
export async function resolveOrigin(
  env: Record<string, string | undefined>,
  exec: ExecFn,
  cwd: string
): Promise<DispatchOrigin> {
  const origin: { -readonly [K in keyof DispatchOrigin]: DispatchOrigin[K] } = {
    cwd,
    machine: machineID(),
  };

  // Only markers a host process sets for itself. OpenCode has none we know of,
  // and OPENCODE_* variables are exported from shell profiles on this fleet, so
  // an OpenCode session reports no host rather than every other process
  // claiming to be one.
  if (env["OMP_SESSION_ID"] || env["OMPCODE"]) {
    origin.host = "omp";
  } else if (env["CLAUDECODE"]) {
    origin.host = "claude";
  }

  const pane = env["TMUX_PANE"];
  if (pane) {
    // `#S:#I.#P` reads well but names one session of a session group at
    // random; the pane id is what `switch-client -t` needs to land in the
    // right session, window, and pane from wherever the human is attached.
    const output = await tryExec(
      exec,
      "tmux",
      ["display-message", "-p", "-t", pane, "#S:#I.#P #{pane_id}"],
      cwd
    );
    const [target, paneId] = output?.trim().split(" ") ?? [];
    if (target) origin.tmux = target;
    if (paneId) origin.pane = paneId;
  }

  return origin;
}

// Cwd-derived defaults for the dispatch tool: which GitHub repo a thread
// belongs to, and where a human jumps back to reply. Every host plugin (OMP,
// OpenCode, Claude) calls this from inside the session's own process, so this
// is the one place that needs to know about the working directory.

import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { canonicalRepo } from "@legion/contracts/repo";
import { machineID } from "./machine";

const execFileAsync = promisify(execFile);

/** One shell-out, shaped like Node's promisified `execFile`. Injectable for tests. Aborting
 *  `signal` asks the exec to end the call; `tryExec` stops waiting at its bound either way. */
export type ExecFn = (
  file: string,
  args: string[],
  options: { readonly cwd: string; readonly signal: AbortSignal }
) => Promise<{ readonly stdout: string }>;

/** Real exec used outside tests. An aborted call kills its child with SIGKILL: a tmux client
 *  waiting to connect to a server too busy to accept catches SIGTERM and keeps waiting. The kill
 *  is sent here because Node's execFile ends an aborted child with SIGTERM whatever `killSignal`
 *  says (Bun's sends `killSignal`); the signal still goes to execFile, which rejects the call at
 *  once. */
export const defaultExec: ExecFn = (file, args, { cwd, signal }) => {
  const call = execFileAsync(file, args, { cwd, signal });
  const kill = () => call.child.kill("SIGKILL");
  if (signal.aborted) kill();
  else signal.addEventListener("abort", kill, { once: true });
  return call;
};

/** How long the repository lookup waits for jj, and then for git, to name the cwd's remotes. */
const REMOTE_LOOKUP_MS = 5_000;

/** How long dispatch waits for tmux to name the pane before it sends the call without it. */
const TMUX_PANE_LOOKUP_MS = 1_000;

/** The coding-agent hosts that ship a `dispatch` tool. Each plugin asserts its own value. */
export type DispatchHost = "omp" | "opencode" | "claude";

/** Provenance attached to every dispatch turn so a human can find the asking session. */
export interface DispatchOrigin {
  readonly host?: DispatchHost;
  readonly machine?: string;
  readonly cwd: string;
  /** Human-readable `session:window.pane`; ambiguous inside a tmux session group. */
  readonly tmux?: string;
  /** Stable pane id (`%N`); `tmux switch-client -t %N` jumps there from anywhere. */
  readonly pane?: string;
  /** The host's session id, read by the plugin at call time. */
  readonly sessionId?: string;
  /** The host's session title, read by the plugin at call time. */
  readonly sessionTitle?: string;
}

/** One bounded shell-out: its stdout, or null when it fails or has not answered within `limitMs`.
 *  At the bound the call's signal is aborted and nothing waits for an exec that ignores it. */
async function tryExec(
  exec: ExecFn,
  file: string,
  args: string[],
  cwd: string,
  limitMs: number
): Promise<string | null> {
  const controller = new AbortController();
  const expired = Promise.withResolvers<null>();
  const timer = setTimeout(() => {
    controller.abort();
    expired.resolve(null);
  }, limitMs);
  try {
    const answer = await Promise.race([
      exec(file, args, { cwd, signal: controller.signal }),
      expired.promise,
    ]);
    return answer?.stdout ?? null;
  } catch {
    return null;
  } finally {
    clearTimeout(timer);
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
    if (match) return canonicalRepo(match[1] ?? "", match[2] ?? "");
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
 * Each waits at most REMOTE_LOOKUP_MS; one that has not answered by then counts as failed.
 * Returns null when nothing resolves to a GitHub repo.
 */
export async function resolveCwdRepo(cwd: string, exec: ExecFn): Promise<string | null> {
  const jjOutput = await tryExec(exec, "jj", ["git", "remote", "list"], cwd, REMOTE_LOOKUP_MS);
  if (jjOutput !== null) {
    const url = selectRemoteUrl(parseRemoteList(jjOutput));
    return url ? parseGitHubRemoteUrl(url) : null;
  }
  const originUrl = await tryExec(
    exec,
    "git",
    ["remote", "get-url", "origin"],
    cwd,
    REMOTE_LOOKUP_MS
  );
  return originUrl ? parseGitHubRemoteUrl(originUrl) : null;
}

type OriginEnvironment = {
  readonly TMUX_PANE?: string;
} & Record<string, string | undefined>;

/**
 * Best-effort provenance for a dispatch turn: machine, session cwd, and —
 * inside tmux — the pane to jump back to. Host and session identity are the
 * calling plugin's to add; nothing here is guessed from the environment.
 * A tmux that has not named the pane within TMUX_PANE_LOOKUP_MS leaves it
 * unset: a busy shared tmux server must not hold up every dispatch call.
 */
export async function resolveOrigin(
  env: OriginEnvironment,
  exec: ExecFn,
  cwd: string
): Promise<DispatchOrigin> {
  const origin: { -readonly [K in keyof DispatchOrigin]: DispatchOrigin[K] } = {
    cwd,
    machine: machineID(),
  };

  const pane = env.TMUX_PANE;
  if (pane) {
    // `#S:#I.#P` reads well but names one session of a session group at
    // random; the pane id is what `switch-client -t` needs to land in the
    // right session, window, and pane from wherever the human is attached.
    const output = await tryExec(
      exec,
      "tmux",
      ["display-message", "-p", "-t", pane, "#S:#I.#P #{pane_id}"],
      cwd,
      TMUX_PANE_LOOKUP_MS
    );
    const [target, paneId] = output?.trim().split(" ") ?? [];
    if (target) origin.tmux = target;
    if (paneId) origin.pane = paneId;
  }

  return origin;
}

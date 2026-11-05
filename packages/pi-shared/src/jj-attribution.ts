// Session attribution for jj commits, shared by the harness plugins.
//
// At root bootstrap the extension exports JJ_CONFIG = <user config chain> +
// a generated overlay that sets `templates.commit_trailers`, so every jj
// commit made from the root session — and from every phase worker, since
// workers are subagents sharing the root process environment — automatically
// carries an `Omp-Session: <root-session-id>` trailer. Attribution rides the
// commit itself; no worker compliance and no host-specific jj config needed.
//
// JJ_CONFIG replaces jj's default config lookup, so an existing JJ_CONFIG (or
// jj's own `config path --user` output, chained in printed order) must come
// first; the overlay layers last and wins for its single key. Attribution
// must degrade to *no* trailer, never a *wrong* one: stale attribution
// overlays already present in the inherited chain (this extension's or the
// dotfiles session-env extension's — both use the omp-attribution-*.toml
// filename grammar) are stripped before the current overlay is appended, and
// the overlay is written atomically. Failure to resolve jj or write the
// overlay is non-fatal and must never block root bootstrap, so the jj call is
// bounded: a jj that never answers (a PATH wrapper that loops) degrades to no
// trailer instead of holding the rest of the claim's boot, the Envoy role
// claim and `claims/ready`, which run after it.
//
// For a pane whose issue sits on a repository the deployment names, the
// overlay carries a second line, `Dispatch: <KEY>`, so a squash landing
// message carries the issue key. Both lines are one template, because
// `templates.commit_trailers` is a single key and a later configuration layer
// replaces an earlier one rather than adding to it.
//
// That single key is global to the process, so a pane whose issue is on a
// named repository stamps the issue line into *any* jj repository its shell
// commits in — a scratch repository or a second checkout included. Nothing in
// the trailer mechanism can be per repository: jj reads the template before it
// knows which repository a `describe` is in. In practice a role commits in
// `LEGION_WORKSPACE` alone.
import { mkdir, rename, writeFile } from "node:fs/promises";
import path from "node:path";

const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
/** The bound on `jj config path --user`, which reads no repository and answers in milliseconds:
 * well inside the 30 s Oh My Pi gives the session_start handler that awaits it. */
export const JJ_CONFIG_PATH_TIMEOUT_MS = 10_000;
const TOP_LEVEL_TRANSCRIPT = new RegExp(`^\\d{4}-\\d{2}-\\d{2}T[\\d-]+Z_(${UUID})\\.jsonl$`);
const ATTRIBUTION_OVERLAY = /^omp-attribution-.*\.toml$/;
/** The whole value of an issue key, as the gated base branch's rule accepts one: a project part of
 * two to ten characters, then the number. Not `DISPATCH_KEY_PATTERN`
 * (`@legion/contracts/legion-roles`), which caps nothing and so accepts a key that rule refuses.
 * The match is containment as much as correctness: the value is interpolated into a jj template
 * inside a TOML literal, so a quote, a backslash or a newline would write a template jj cannot
 * parse and break every `jj describe` in the pane. */
const ISSUE_KEY = /^[A-Z][A-Z0-9]{1,9}-[0-9]+$/;

export function sessionIdFromTranscript(sessionFile: string): string | undefined {
  return TOP_LEVEL_TRANSCRIPT.exec(path.basename(sessionFile))?.[1];
}

function withoutOverlays(chain: string): string {
  return chain
    .split(":")
    .filter((entry) => entry && !ATTRIBUTION_OVERLAY.test(path.basename(entry)))
    .join(":");
}

export async function userConfigChain(
  timeoutMs = JJ_CONFIG_PATH_TIMEOUT_MS,
  env: NodeJS.ProcessEnv = process.env
): Promise<string | undefined> {
  try {
    const child = Bun.spawn(["jj", "config", "path", "--user"], {
      detached: true,
      env,
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
    });
    const killGroup = () => {
      try {
        process.kill(-child.pid, "SIGKILL");
      } catch {
        // The group exited before cleanup reached it.
      }
    };
    // Every descendant belongs to this call's group. Kill it as soon as the leader exits, before
    // either its output can answer successfully or a non-zero exit can answer unsuccessfully.
    const exited = child.exited.finally(killGroup);
    // Include both streams in the deadline: a wrapper can fork a helper that inherits them after
    // its leader exits, and attribution must never wait for that helper.
    const completed = Promise.all([
      exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    const stdout = await new Promise<string | undefined>((resolve) => {
      const timer = setTimeout(() => {
        killGroup();
        resolve(undefined);
      }, timeoutMs);
      void completed.then(
        ([code, output]) => {
          clearTimeout(timer);
          resolve(code === 0 ? output : undefined);
        },
        () => {
          clearTimeout(timer);
          resolve(undefined);
        }
      );
    });
    if (stdout === undefined) return undefined;
    const paths = stdout.split("\n").filter(Boolean);
    return paths.length > 0 ? paths.join(":") : undefined;
  } catch {
    return undefined;
  }
}

/** Writes the overlay for `sessionId` under `stateDir` and returns the chain it exported into
 * `env.JJ_CONFIG`, or undefined when it exported none: an empty session id, no base chain to layer
 * over, or a failed write — which still degrades `env.JJ_CONFIG` to the base, since no overlay is
 * active. `options.issueKey` adds the `Dispatch:` line only when its whole value is a key. */
export async function exportJjSessionAttribution(
  sessionId: string,
  stateDir: string,
  options: {
    issueKey?: string;
    env?: NodeJS.ProcessEnv;
    resolveBase?: () => Promise<string | undefined>;
  } = {}
): Promise<string | undefined> {
  const { issueKey, env = process.env, resolveBase = userConfigChain } = options;
  if (!sessionId) return undefined;
  const overlay = path.join(stateDir, `omp-attribution-${sessionId}.toml`);
  const active = env.JJ_CONFIG;
  // Already active — never rewrite a live layer; the caller still learns the live chain.
  if (active?.split(":").includes(overlay)) return active;
  const base = (active ? withoutOverlays(active) : "") || (await resolveBase());
  if (!base) return undefined;
  // `\n` is the two characters jj's template language reads as a newline, inside a TOML literal.
  const trailers =
    issueKey !== undefined && ISSUE_KEY.test(issueKey)
      ? `Omp-Session: ${sessionId}\\nDispatch: ${issueKey}`
      : `Omp-Session: ${sessionId}`;
  try {
    await mkdir(stateDir, { recursive: true });
    const tmp = `${overlay}.${process.pid}.tmp`;
    await writeFile(tmp, `[templates]\ncommit_trailers = '"${trailers}"'\n`);
    await rename(tmp, overlay);
    const chain = `${base}:${overlay}`;
    env.JJ_CONFIG = chain;
    return chain;
  } catch {
    env.JJ_CONFIG = base; // degrade to no trailer, never a stale one
    return undefined;
  }
}

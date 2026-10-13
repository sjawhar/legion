// Session attribution for jj commits, self-contained in the Legion extension.
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
import { mkdir, rename, writeFile } from "node:fs/promises";
import path from "node:path";

const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
/** The bound on `jj config path --user`, which reads no repository and answers in milliseconds:
 * well inside the 30 s Oh My Pi gives the session_start handler that awaits it. */
export const JJ_CONFIG_PATH_TIMEOUT_MS = 10_000;
const TOP_LEVEL_TRANSCRIPT = new RegExp(`^\\d{4}-\\d{2}-\\d{2}T[\\d-]+Z_(${UUID})\\.jsonl$`);
const ATTRIBUTION_OVERLAY = /^omp-attribution-.*\.toml$/;

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

export async function exportJjSessionAttribution(
  sessionFile: string,
  stateDir: string,
  env: NodeJS.ProcessEnv = process.env,
  resolveBase: () => Promise<string | undefined> = userConfigChain
): Promise<void> {
  const id = sessionIdFromTranscript(sessionFile);
  if (!id) return;
  const overlay = path.join(stateDir, `omp-attribution-${id}.toml`);
  if (env.JJ_CONFIG?.split(":").includes(overlay)) return; // already active — never rewrite a live layer
  const base = (env.JJ_CONFIG ? withoutOverlays(env.JJ_CONFIG) : "") || (await resolveBase());
  if (!base) return;
  try {
    await mkdir(stateDir, { recursive: true });
    const tmp = `${overlay}.${process.pid}.tmp`;
    await writeFile(tmp, `[templates]\ncommit_trailers = '"Omp-Session: ${id}"'\n`);
    await rename(tmp, overlay);
    env.JJ_CONFIG = `${base}:${overlay}`;
  } catch {
    env.JJ_CONFIG = base; // degrade to no trailer, never a stale one
  }
}

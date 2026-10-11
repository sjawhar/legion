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
// overlay is non-fatal and must never block root bootstrap.
import { execFile } from "node:child_process";
import { mkdir, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
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

async function userConfigChain(): Promise<string | undefined> {
  try {
    const { stdout } = await promisify(execFile)("jj", ["config", "path", "--user"]);
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

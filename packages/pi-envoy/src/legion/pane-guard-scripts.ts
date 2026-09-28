/**
 * What the pane guard (`pane-guard.ts`) does with every shell script the repository tracks: for
 * each one it refuses, where, as the innermost `file:line` its refusal names, repository-relative.
 * `pane-guard.test.ts` compares this with its recorded table, so a refusal that moves to another
 * line or file fails there. When one moves on purpose, regenerate the table from `packages/pi-envoy`
 * and format it:
 *
 *   bun src/legion/pane-guard-scripts.ts
 *   bunx biome format --write src/legion/pane-guard.test.ts
 */
import { spawnSync } from "node:child_process";
import * as path from "node:path";
import { createPaneGuard } from "./pane-guard";

/**
 * The operator home the scripts are judged under. It is a fixture, never a configuration: no
 * machine has it, and no real pane runs with a home that does not exist. It is unreal on purpose,
 * so that what the machine running the derivation keeps under its own home cannot change a
 * verdict. The guard allows a redirection to a file that does not exist yet, so the lock a Stage 4b
 * run leaves under the operator's home would stop that script's walk at `exec 9>"$lock"` on one
 * machine and not on another. What the table therefore cannot see is a tracked script refused only
 * because the operator's home already holds a file it writes. The rule itself, that an existing
 * file in a home with contents is refused and a new one is not, is covered by the redirection and
 * tee families in `pane-guard.test.ts`, on that test's fixture home.
 */
export const OPERATOR_HOME = "/home/legion-guard-test-operator";

/** Every tracked `*.sh` the guard refuses as `bash <file>`, mapped to where (`refusalSite`). */
export function trackedScriptRefusals(repository: string): Record<string, string> {
  const listed = spawnSync("git", ["ls-files", "-z", "--", ":(glob)**/*.sh"], {
    cwd: repository,
    encoding: "utf8",
  });
  if (listed.status !== 0) throw new Error(listed.stderr);
  const guard = createPaneGuard({ workspace: repository, ompPid: process.pid, scratch: "/tmp" });
  const env = {
    HOME: OPERATOR_HOME,
    LEGION_WORKSPACE: repository,
    TMPDIR: "/tmp",
    PATH: "/usr/bin:/bin",
  };
  const refusals: Record<string, string> = {};
  for (const file of listed.stdout.split("\0").filter((name) => name !== "")) {
    const reason = guard.bash(`bash ${file}`, repository, env);
    if (reason !== undefined) refusals[file] = refusalSite(reason, repository);
  }
  return refusals;
}

/** The innermost `line N of <file>` a refusal names, the line the guard refused: repository-relative
 * for a file, and as written for text with no file (`the EXIT trap`). */
export function refusalSite(reason: string, repository: string): string {
  const site = [...reason.matchAll(/line (\d+) of (.+?), `/g)].at(-1);
  if (site === undefined) throw new Error(`the refusal names no line: ${reason}`);
  const source = site[2] ?? "";
  return `${path.isAbsolute(source) ? path.relative(repository, source) : source}:${site[1]}`;
}

if (import.meta.main) {
  const repository = path.resolve(import.meta.dir, "../../../..");
  const refusals = trackedScriptRefusals(repository);
  for (const file of Object.keys(refusals).sort()) {
    console.log(`  ${JSON.stringify(file)}: ${JSON.stringify(refusals[file])},`);
  }
}

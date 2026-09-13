import { chmod, mkdir, readdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";

/** The one Dispatch bearer every pane shares, written once at daemon startup (`index.ts`). */
export const DISPATCH_TOKEN_SECRET = "dispatch-token";

/** `<stateDir>/secrets`: every secret a pane reads lives here as a 0600 file inside a 0700
 * directory, and a pane receives only the file's path (`DISPATCH_TOKEN_FILE`,
 * `LEGION_BOOT_TOKEN_FILE`, `LEGION_CONTROLLER_SECRET_FILE`) on its tmux `-e` argv — never the
 * value, which would otherwise sit in the world-readable `/proc/<pid>/cmdline` of the transient
 * tmux client for as long as it runs. Pane files are named by the pane's role token
 * (`legion-<project>-<key>-<role>`, `legion-<project>-controller`), the same key `state.roles`
 * uses, so `ProcessManager.pruneSecretFiles` can derive which ones a live locator still needs. */
export function secretsDir(stateDir: string): string {
  return path.join(stateDir, "secrets");
}

export function secretFilePath(stateDir: string, name: string): string {
  return path.join(secretsDir(stateDir), name);
}

/** The grant file a pane's `LEGION_GRANT_FILE` names: `<role token>-grant`. The daemon never
 * writes it — the pi-envoy extension does, before each of the pane's bash commands, with the grant
 * it minted for that command — but the daemon names it on the pane and prunes it exactly like the
 * pane's boot-token file, for as long as the pane's locator lives. */
export function grantSecretName(roleToken: string): string {
  return `${roleToken}-grant`;
}

/** Every `<stateDir>/secrets` file a pane's role token names: its own secret (the boot token, or
 * the controller secret) and its grant file. `ProcessManager` tracks, holds, and prunes them as
 * one set — a third per-token file lands here and nowhere else. */
export function processSecretNames(roleToken: string): string[] {
  return [roleToken, grantSecretName(roleToken)];
}

/** Writes `value` to `<stateDir>/secrets/<name>` (created or overwritten in place) and returns that
 * path. Re-applies 0700/0600 explicitly on every call: `mkdir`'s mode is umask-masked and ignored
 * for an existing directory, and `writeFile`'s mode applies only on create. */
export async function writeSecretFile(
  stateDir: string,
  name: string,
  value: string
): Promise<string> {
  const dir = secretsDir(stateDir);
  await mkdir(dir, { recursive: true, mode: 0o700 });
  await chmod(dir, 0o700);
  const file = path.join(dir, name);
  await writeFile(file, value, { encoding: "utf8", mode: 0o600 });
  await chmod(file, 0o600);
  return file;
}

/** Removes every entry of `<stateDir>/secrets` whose name is not in `keep`. A missing directory is
 * an empty one. */
export async function pruneSecretFiles(stateDir: string, keep: ReadonlySet<string>): Promise<void> {
  let names: string[];
  try {
    names = await readdir(secretsDir(stateDir));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return;
    throw error;
  }
  await Promise.all(
    names
      .filter((name) => !keep.has(name))
      .map((name) => rm(secretFilePath(stateDir, name), { force: true }))
  );
}

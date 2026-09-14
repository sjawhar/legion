import { readFileSync } from "node:fs";
import { chmod, mkdir, readdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";

/** The trimmed contents of the 0600 file an `X_FILE` pointer names — `variable` is the pointer's
 * own name (`LEGION_GRANT_FILE`, `envoy_token_file`, …), so the message reads as the operator
 * wrote it. A set pointer is authoritative: a missing, unreadable, or empty file is an error naming
 * both the variable and the path, never a fallback to the plain variable or to another source.
 * The one reader for every such pointer the daemon and its CLI resolve. */
export function readSecretPointer(variable: string, file: string): string {
  let contents: string;
  try {
    contents = readFileSync(file, "utf8");
  } catch (error) {
    throw new Error(
      `${variable} names ${file}, which could not be read: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const secret = contents.trim();
  if (!secret) throw new Error(`${variable} names ${file}, which is empty`);
  return secret;
}

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

/** Every secret name a daemon may hand a process beyond its own boot token or controller secret
 * (`ProcessManager.sharedProcessSecrets`, whose keys are typed by this list). A constant, not the
 * current config: the prune keeps and reaps a pane's files by the names a pane *could* have been
 * given, so a daemon restarted without `envoy_token_file` still keeps the `<role token>-envoy_token`
 * file a surviving pane from the previous configuration names in its `ENVOY_TOKEN_FILE`; and both
 * runtimes tell a process's own secret from a shared one by this list, never by position. */
export const SHARED_SECRET_NAMES = ["ENVOY_TOKEN"] as const;
export type SharedSecretName = (typeof SHARED_SECRET_NAMES)[number];

export function isSharedSecretName(name: string): name is SharedSecretName {
  return (SHARED_SECRET_NAMES as readonly string[]).includes(name);
}

/** The file a shared secret (`SHARED_SECRET_NAMES`) lives in for one process:
 * `<role token>-<lowercased name>`; the process's own secret (the boot token, or the controller
 * secret) keeps the bare `<role token>` name, so one role token still names every file the prune
 * must keep. */
export function extraSecretName(roleToken: string, name: SharedSecretName): string {
  return `${roleToken}-${name.toLowerCase()}`;
}

/** Every `<stateDir>/secrets` file a pane's role token names: its own secret (the boot token, or
 * the controller secret), one file per shared secret name (`SHARED_SECRET_NAMES`, whether or not
 * this daemon's config delivers it), and its grant file. `ProcessManager` tracks, holds, and prunes
 * them as one set — another per-token file lands here and nowhere else. */
export function processSecretNames(roleToken: string): string[] {
  return [
    roleToken,
    ...SHARED_SECRET_NAMES.map((name) => extraSecretName(roleToken, name)),
    grantSecretName(roleToken),
  ];
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

import { randomUUID } from "node:crypto";
import { chmod, mkdir, rename, rm, writeFile } from "node:fs/promises";
import path from "node:path";

/** The one directory name Legion's `gh` shim lives in, under a daemon's `state_dir`. */
const WORKER_BIN_DIRNAME = "worker-bin";

/** `<stateDir>/worker-bin`: first on every root, worker, and controller pane's PATH (see
 * `ProcessManager.credentialProcessEnvironment`), never on the daemon's own. */
export function workerBinDir(stateDir: string): string {
  return path.join(stateDir, WORKER_BIN_DIRNAME);
}

/** `pathValue` without any entry whose basename is `worker-bin`, wherever it sits. Two callers
 * apply this strip: `resolveDaemonEnvironment`, so a daemon started from inside a Legion pane
 * (whose PATH carries `<state_dir>/worker-bin` first for the pane's life, and whose `mise env`
 * keeps that head) never resolves its own `gh` to the shim and never passes the inherited entry on
 * to the panes it launches; and `legion gh`, so the `gh` it spawns is the real one — resolved
 * through the shim it would re-enter `legion gh` under a child env whose grant pointer is already
 * scrubbed. (The shim's own `${PATH#…}` differs: it strips exactly its own leading entry.) An
 * unrelated `/opt/foo/worker-bin` is dropped too; no daemon tool lives in a directory of that name. */
export function pathWithoutWorkerBin(pathValue: string): string {
  return pathValue
    .split(path.delimiter)
    .filter((entry) => path.basename(entry) !== WORKER_BIN_DIRNAME)
    .join(path.delimiter);
}

function shellLiteral(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

/** Installs `<stateDir>/worker-bin/gh` (0700 script in a 0700 directory) and returns the
 * directory. The shim strips its own directory from PATH and execs `legion gh -- "$@"`, so a bare
 * `gh …` in a pane is the same call as `legion gh -- …` — a fresh token redeemed from the pane's
 * grant per call, never a stored login — and the `legion` it execs resolves to the launcher on the
 * pane's PATH. Runs at every daemon startup (`index.ts`) while panes from before a `legion
 * restart` may be mid-`gh`, so the script is written to `gh.<pid>.<uuid>` and renamed over the
 * old one: a running `gh` never reads a truncated script. The rig's `setup.sh` runs this file
 * directly with the rig's state dir. */
export async function installWorkerGhShim(stateDir: string): Promise<string> {
  const workerBin = workerBinDir(stateDir);
  await mkdir(workerBin, { recursive: true, mode: 0o700 });
  await chmod(workerBin, 0o700);
  const shim = path.join(workerBin, "gh");
  const temp = `${shim}.${process.pid}.${randomUUID()}`;
  try {
    await writeFile(
      temp,
      `#!/bin/sh
PATH=\${PATH#${shellLiteral(`${workerBin}${path.delimiter}`)}}
export PATH
exec legion gh -- "$@"
`,
      { encoding: "utf8", mode: 0o700 }
    );
    await chmod(temp, 0o700);
    await rename(temp, shim);
  } catch (error) {
    await rm(temp, { force: true }).catch(() => {});
    throw error;
  }
  return workerBin;
}

if (import.meta.main) {
  const stateDir = process.argv[2];
  if (!stateDir) {
    process.stderr.write("usage: bun worker-bin.ts <state_dir>\n");
    process.exit(2);
  }
  process.stdout.write(`${await installWorkerGhShim(stateDir)}\n`);
}

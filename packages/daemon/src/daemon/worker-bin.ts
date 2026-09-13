import { chmod, mkdir, writeFile } from "node:fs/promises";
import path from "node:path";

/** The one directory name Legion's `gh` shim lives in, under a daemon's `state_dir`. */
export const WORKER_BIN_DIRNAME = "worker-bin";

/** `<stateDir>/worker-bin`: first on every root, worker, and controller pane's PATH (see
 * `ProcessManager.credentialProcessEnvironment`), never on the daemon's own. */
export function workerBinDir(stateDir: string): string {
  return path.join(stateDir, WORKER_BIN_DIRNAME);
}

/** `pathValue` without any `worker-bin` entry — what the shim itself does to PATH before it execs
 * `legion gh`, and what `legion gh` must do before it spawns `gh`: with the shim first on a pane's
 * PATH for the pane's life, a `gh` resolved through it would be the shim again, re-entering
 * `legion gh` under a child env whose grant pointer is already scrubbed. */
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
 * pane's PATH. Run once at daemon startup (`index.ts`); the rig's `setup.sh` runs this file
 * directly with the rig's state dir. */
export async function installWorkerGhShim(stateDir: string): Promise<string> {
  const workerBin = workerBinDir(stateDir);
  await mkdir(workerBin, { recursive: true, mode: 0o700 });
  await chmod(workerBin, 0o700);
  await writeFile(
    path.join(workerBin, "gh"),
    `#!/bin/sh
PATH=\${PATH#${shellLiteral(`${workerBin}${path.delimiter}`)}}
export PATH
exec legion gh -- "$@"
`,
    { encoding: "utf8", mode: 0o700 }
  );
  await chmod(path.join(workerBin, "gh"), 0o700);
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

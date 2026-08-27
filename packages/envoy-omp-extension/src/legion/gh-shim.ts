import { chmod, mkdir, writeFile } from "node:fs/promises";
import path from "node:path";

function shellLiteral(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

export async function installWorkerGhShim(stateDir: string): Promise<string> {
  const workerBin = path.join(stateDir, "worker-bin");
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

export function workerGhEnvironment(grantId: string, stateDir: string, workerBin: string): string {
  return [
    `export LEGION_GRANT=${shellLiteral(grantId)}`,
    "unset GH_TOKEN",
    "unset GITHUB_TOKEN",
    "unset GH_HOST",
    `export GH_CONFIG_DIR=${shellLiteral(path.join(stateDir, "gh"))}`,
    `export PATH=${shellLiteral(workerBin)}${path.delimiter}$PATH`,
  ].join("\n");
}

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

/**
 * The per-command environment a worker's bash call runs under: its freshly minted grant, no
 * ambient GitHub token or host (an empty value is absent to gh), the isolated gh config
 * directory, and the `gh` shim first on PATH. Delivered through the bash tool's `env` argument —
 * never as command text, which the host writes back into the model's message and the model then
 * imitates with stale or made-up ids.
 */
export function workerGhEnvironment(
  grantId: string,
  stateDir: string,
  workerBin: string,
  basePath: string | undefined
): Record<string, string> {
  return {
    LEGION_GRANT: grantId,
    GH_TOKEN: "",
    GITHUB_TOKEN: "",
    GH_HOST: "",
    GH_CONFIG_DIR: path.join(stateDir, "gh"),
    PATH: basePath ? `${workerBin}${path.delimiter}${basePath}` : workerBin,
  };
}

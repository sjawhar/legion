import { mkdir, rename, rm, writeFile } from "node:fs/promises";
import path from "node:path";

export interface WorkerCredentials {
  readonly sessionId: string;
  readonly secret: string;
  readonly generation: number;
}

function workerCredentialsPath(stateDir: string, roleToken: string): string {
  return path.join(stateDir, "workers", `${roleToken}.session.json`);
}

/**
 * Persists the credentials `legion handoff complete` authenticates with,
 * since a phase worker's own process environment never carries them (they
 * exist only in this extension's in-memory capability). Written atomically
 * (temp name, then rename) so a reader never observes a partial file.
 */
export async function writeWorkerCredentials(
  stateDir: string,
  roleToken: string,
  credentials: WorkerCredentials
): Promise<void> {
  const directory = path.join(stateDir, "workers");
  await mkdir(directory, { recursive: true, mode: 0o700 });
  const finalPath = workerCredentialsPath(stateDir, roleToken);
  const temporaryPath = `${finalPath}.${process.pid}.tmp`;
  await writeFile(temporaryPath, JSON.stringify(credentials), { encoding: "utf8", mode: 0o600 });
  await rename(temporaryPath, finalPath);
}

export async function deleteWorkerCredentials(stateDir: string, roleToken: string): Promise<void> {
  await rm(workerCredentialsPath(stateDir, roleToken), { force: true });
}

import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";

const DEPLOYMENT_INSTRUCTIONS_FILE = "deployment-instructions.md";

/** Reads the operator's `instructionsPath` and returns its content: a missing, unreadable, or
 * directory path, or a blank file, throws naming `instructionsPath` — a configured key with nothing
 * to append is a misconfiguration, never an empty fragment. `legion controller start` runs this
 * check alone before it asks the daemon for a controller secret (that request revokes the
 * incumbent controller's), and `materializeDeploymentInstructions` runs it before its write. */
export async function readDeploymentInstructions(instructionsPath: string): Promise<string> {
  let content: string;
  try {
    content = await readFile(instructionsPath, "utf8");
  } catch (error) {
    throw new Error(
      `[legion] instructions file ${instructionsPath} could not be read: ${
        error instanceof Error ? error.message : String(error)
      }`,
      { cause: error }
    );
  }
  if (content.trim().length === 0) {
    throw new Error(`[legion] instructions file ${instructionsPath} is empty`);
  }
  return content;
}

/** Reads the operator's `instructionsPath` (`readDeploymentInstructions`) and writes
 * `# Deployment instructions (<legionId>)`, a blank line, and the content verbatim to
 * `<stateDir>/deployment-instructions.md`, returning that path — the copy every launched pane
 * carries as the last part of its one `--append-system-prompt` value, `$(cat <this file>)`
 * (`processes.ts`), so panes get exactly what boot validated, never the operator's path.
 * `legionId` is the `project` value exactly as the operator wrote it (`sjawhar/legion`), never
 * the sanitized role-token form. The write failing is the caller's startup failure, exactly like
 * the Dispatch token file. */
export async function materializeDeploymentInstructions(
  instructionsPath: string,
  stateDir: string,
  legionId: string
): Promise<string> {
  const content = await readDeploymentInstructions(instructionsPath);
  const target = path.join(stateDir, DEPLOYMENT_INSTRUCTIONS_FILE);
  await writeFile(target, `# Deployment instructions (${legionId})\n\n${content}`, "utf8");
  return target;
}

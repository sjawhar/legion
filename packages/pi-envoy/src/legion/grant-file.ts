import { randomUUID } from "node:crypto";
import { chmod, rename, rm, writeFile } from "node:fs/promises";
import { messageFor } from "@legion/envoy-client/errors";

/**
 * Writes `grantId` to the pane's `LEGION_GRANT_FILE` atomically: a `<file>.<pid>.<uuid>` temp
 * beside it (0600 — `writeFile`'s mode is umask-masked, so it is re-applied), then a `rename`
 * over the named file, so `legion` never reads a half-written grant and two hooks in flight for
 * one session (both live grants) never collide on the temp name. Any failure removes the temp
 * best-effort and throws naming the path; the caller blocks the command rather than let it run
 * under whatever the file held before.
 */
export async function writeGrantFile(file: string, grantId: string): Promise<void> {
  const temp = `${file}.${process.pid}.${randomUUID()}`;
  try {
    await writeFile(temp, grantId, { encoding: "utf8", mode: 0o600 });
    await chmod(temp, 0o600);
    await rename(temp, file);
  } catch (error) {
    await rm(temp, { force: true }).catch(() => {});
    throw new Error(`LEGION_GRANT_FILE ${file} could not be written: ${messageFor(error)}`);
  }
}

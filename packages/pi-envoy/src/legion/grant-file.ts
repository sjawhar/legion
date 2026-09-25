import { randomUUID } from "node:crypto";
import { chmod, rename, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";

/**
 * Writes `grantId` to the pane's `LEGION_GRANT_FILE` atomically: a `<file>.<pid>.<uuid>` temp
 * beside it (0600 — `writeFile`'s mode is umask-masked, so it is re-applied), then a `rename`
 * over the named file, so `legion` never reads a half-written grant and two hooks in flight for
 * one session (both live grants) never collide on the temp name. The path must be absolute: the
 * daemon always names one, and a relative pointer (an operator's own export) would otherwise put
 * the temp file in OMP's cwd — the issue workspace. Any failure removes the temp best-effort and
 * throws naming the path; the caller blocks the command rather than let it run under whatever the
 * file held before.
 */
async function writeGrantFile(file: string, grantId: string): Promise<void> {
  if (!path.isAbsolute(file)) {
    throw new Error(`LEGION_GRANT_FILE ${file} could not be written: the path is not absolute`);
  }
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

/**
 * Mints a grant and writes it to the pane's `LEGION_GRANT_FILE`, where `legion` reads it: the one
 * path by which the bash hook and the `legion` tool's `handoff_complete` hand a command its grant.
 * A pane without the variable was launched by a daemon older than this plugin, so it is refused
 * before anything is minted.
 */
export async function writeMintedGrant(mint: () => Promise<string>): Promise<void> {
  const file = process.env.LEGION_GRANT_FILE;
  if (file === undefined || file.trim() === "") {
    throw new Error(
      "LEGION_GRANT_FILE is not set on this pane: the daemon that launched it predates this plugin; restart the daemon on the matching release"
    );
  }
  await writeGrantFile(file, await mint());
}

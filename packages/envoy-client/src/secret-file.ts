import { readFileSync } from "node:fs";
import { messageFor } from "./errors";

/** Reads the secret a `<VARIABLE>_FILE` pointer names: the file's contents, trimmed. The Legion
 * daemon hands every pane secret over this way (a 0600 file under its state directory) instead of
 * as a `-e KEY=VALUE` tmux argv, which is world-readable via `/proc`. A set pointer is a claim the
 * daemon made, so a missing, unreadable, or blank file throws naming the variable and the path —
 * callers never fall back to the plain variable or a config file. */
export function readSecretFile(variable: string, filePath: string): string {
  let contents: string;
  try {
    contents = readFileSync(filePath, "utf8");
  } catch (error) {
    throw new Error(`${variable} names ${filePath}, which could not be read: ${messageFor(error)}`);
  }
  const value = contents.trim();
  if (value.length === 0) throw new Error(`${variable} names ${filePath}, which is empty`);
  return value;
}

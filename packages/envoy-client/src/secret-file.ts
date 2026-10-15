import { readFileSync } from "node:fs";
import { messageFor } from "./errors";

/** Whether `value` can go in a header as one bearer: no whitespace and no control character. A
 * multi-line value would otherwise reach a `fetch` error, which quotes the header it refuses. */
export function sendable(value: string): boolean {
  return !/[\s\p{Cc}]/u.test(value);
}

/** Reads the secret a `<VARIABLE>_FILE` pointer names: the file's contents, trimmed. The Legion
 * daemon hands every pane secret over this way (a 0600 file under its state directory) instead of
 * as a `-e KEY=VALUE` tmux argv, which is world-readable via `/proc`. A set pointer is a claim the
 * daemon made, so a missing, unreadable, or blank file throws naming the variable and the path —
 * callers never fall back to the plain variable or a config file. So does a secret that is not
 * `sendable`; no error carries the secret. */
export function readSecretFile(variable: string, filePath: string): string {
  let contents: string;
  try {
    contents = readFileSync(filePath, "utf8");
  } catch (error) {
    throw new Error(`${variable} names ${filePath}, which could not be read: ${messageFor(error)}`);
  }
  const value = contents.trim();
  if (value.length === 0) throw new Error(`${variable} names ${filePath}, which is empty`);
  if (!sendable(value)) {
    throw new Error(`${variable} names ${filePath}, which holds whitespace or a control character`);
  }
  return value;
}

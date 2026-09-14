import fs from "node:fs";
import { CliError } from "./errors";

/** The trimmed contents of the 0600 file `variable` (an `X_FILE` pointer the daemon set on this
 * pane or pod) names. A set pointer is authoritative: a missing, unreadable, or empty file is an
 * error naming both the variable and the path, never a fallback to the plain variable. */
export function readSecretPointer(variable: string, file: string): string {
  let contents: string;
  try {
    contents = fs.readFileSync(file, "utf8");
  } catch (error) {
    throw new CliError(
      `${variable} names ${file}, which could not be read: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const secret = contents.trim();
  if (!secret) throw new CliError(`${variable} names ${file}, which is empty`);
  return secret;
}

import { readSecretPointer as readSecret } from "../daemon/secrets";
import { CliError } from "./errors";

/** `readSecretPointer` (`daemon/secrets.ts`) as a CLI failure: same rule, same message, exit 1. */
export function readSecretPointer(variable: string, file: string): string {
  try {
    return readSecret(variable, file);
  } catch (error) {
    throw new CliError(error instanceof Error ? error.message : String(error));
  }
}

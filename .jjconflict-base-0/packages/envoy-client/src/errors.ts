/** Coerce an unknown thrown value into a human-readable message. */
export function messageFor(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** True when `error` is a Node errno error carrying `code` (ENOENT, EPERM, …). */
export function hasErrnoCode(error: unknown, code: string): boolean {
  return error instanceof Error && "code" in error && error.code === code;
}

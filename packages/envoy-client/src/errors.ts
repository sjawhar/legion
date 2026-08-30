/** Coerce an unknown thrown value into a human-readable message. */
export function messageFor(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

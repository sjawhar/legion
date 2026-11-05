// Reading one of the plugin's JSON state files under `CLAUDE_PLUGIN_DATA`, shared by the records
// that keep one file per session id (`./replay-state`, `./user-turn-state`): no file is an empty
// record, and a file that holds something else is the caller's own refusal.
//
// It is not in `./session-state`, which writes those same files, because that module is the title
// hook's whole dependency graph and may not pull zod in: the hook runs before every user prompt,
// with Claude Code blocking the prompt on it.
import { readFile } from "node:fs/promises"
import { hasErrnoCode } from "@legion/envoy-client/errors"
import type { z } from "zod"

/**
 * What `file` holds, read through `schema`; `empty` when there is no file at all. A file that is
 * not JSON, or that `schema` refuses, throws: `refusal` finishes the sentence after the path, so
 * each record names what it wanted of the file in its own words.
 */
export async function readStateFile<T>(
  file: string,
  schema: z.ZodType<T>,
  empty: T,
  refusal: string,
): Promise<T> {
  let text: string
  try {
    text = await readFile(file, "utf8")
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return empty
    throw error
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    throw new Error(`${file} is not JSON`)
  }
  const state = schema.safeParse(parsed)
  if (!state.success) throw new Error(`${file} ${refusal}`)
  return state.data
}

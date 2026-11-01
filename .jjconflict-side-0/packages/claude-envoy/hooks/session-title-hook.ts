// The UserPromptSubmit hook: records the name the user gave the session, which Claude Code hands
// a hook in `session_title` and gives the plugin nowhere else, so the channel server can register
// it in place of its fallback name.
//
// A bundle of its own, not a mode of `hooks/session-hook.ts`, because Claude Code blocks the
// user's prompt on this hook and runs it before every prompt. That bundle inlines zod, the
// Dispatch HTTP client and the session-settings reader for its two SessionStart modes, and an ESM
// entrypoint evaluates all of it before it ever reads `argv`, which cost this hook roughly 115 ms
// of every prompt. Here the work is one `JSON.parse` and one file write, so nothing in this file's
// import graph may reach those three — `src/session-state.ts` is deliberately free of them, and
// `src/session-identity.ts`, which pulls in zod for its role schema, is not imported.
//
// It writes nothing to stdout and always exits 0, even on a malformed payload: Claude Code adds a
// UserPromptSubmit hook's stdout to the model's context and reads its exit code 2 as a block of
// the prompt, and a cosmetic title must do neither. A failure goes to stderr, which Claude Code
// shows the user and the model never sees.
import { messageFor } from "@legion/envoy-client/errors"
import { recordChosenTitle } from "../src/session-state"

interface ChosenTitlePayload {
  readonly sessionId: string
  readonly title: string | undefined
}

/** The two fields of the payload this hook reads, checked by hand: the schema library that would
 *  check them costs more to load than everything this hook does. */
function chosenTitlePayload(raw: unknown): ChosenTitlePayload {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw new Error(`expected a JSON object, received ${JSON.stringify(raw)}`)
  }
  const { session_id: sessionId, session_title: title } = raw as Record<string, unknown>
  if (typeof sessionId !== "string" || sessionId.length === 0) {
    throw new Error(`session_id must be a non-empty string, received ${JSON.stringify(sessionId)}`)
  }
  if (title !== undefined && typeof title !== "string") {
    throw new Error(`session_title must be a string, received ${JSON.stringify(title)}`)
  }
  return { sessionId, title }
}

try {
  const { sessionId, title } = chosenTitlePayload(JSON.parse(await Bun.stdin.text()))
  await recordChosenTitle(sessionId, title)
} catch (error) {
  process.stderr.write(`envoy: could not record the session title — ${messageFor(error)}\n`)
}

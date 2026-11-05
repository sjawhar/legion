// The rows Claude Code writes for a slash command, parsed (INFRA-1026).
//
// One command a person runs writes three rows by the `command` door - the caveat, the echo of
// what was typed, and the output - and two readers of this plugin parse them. The live view draws
// the echo row as the plain command line a person typed and drops the other two, which would
// otherwise show a reader raw tags and a `/context` table inside the agent's conversation
// (`./agent-stream-claude`). The session reads the output row to answer the person who sent the
// command, since most commands resolve `$.command.run` with no output in the result at all
// (`./mod-session`, `runCommand`).
import type { ModContentBlock } from "../hooks/mod-protocol"

/**
 * The tags Claude Code wraps its own bookkeeping of a slash command in.
 *
 * Measured on 2.1.310: a command a person runs writes three rows by the `command` door - the
 * caveat row, which says the command's output went to the person rather than to the model and
 * carries `isMeta: true`; the echo row, which holds what was typed; and the output row. The
 * output row's `message.type` is `user` in some runs and `system` (`name: "local_command"`) in
 * others, so the tag its text starts with is the only safe discriminator
 * (`/legion/scratch/tmp/infra-1026-spike/config/projects/-legion-scratch-tmp-infra-1026-proof-work/22a2c318-8433-4659-9568-a5ce1a873926.jsonl:42-44`,
 * this plugin's own mod running `/usage`).
 */
const COMMAND_BOOKKEEPING_TAGS = [
  "<local-command-caveat>",
  "<local-command-stdout>",
  "<local-command-stderr>",
] as const

/** The two tags an output row wraps what the command printed in, in the order a row carrying
 *  both would be read: what went to the terminal's stdout, else what went to its stderr. */
const COMMAND_OUTPUT_TAGS = ["local-command-stdout", "local-command-stderr"] as const

/** The text a row's content holds, blocks flattened: a tool result is shown as text alone. A
 *  block with no text of its own is named rather than dropped, as the publisher names one. */
export function claudeRowText(value: unknown): string {
  if (typeof value === "string") return value
  if (!Array.isArray(value)) return ""
  const pieces: string[] = []
  for (const item of value as readonly unknown[]) {
    if (typeof item === "string") {
      pieces.push(item)
      continue
    }
    if (typeof item !== "object" || item === null) continue
    const block = item as Record<string, unknown>
    const text = block["text"]
    const type = block["type"]
    if (typeof text === "string") pieces.push(text)
    else if (typeof type === "string") pieces.push(`[${type}]`)
  }
  return pieces.join("\n")
}

/** The value one of a row's tags holds, trimmed: the recorded row indents every tag after the
 *  first, so the whitespace around a value is the row's layout and not part of it. */
function taggedValue(text: string, tag: string): string | undefined {
  const open = `<${tag}>`
  const from = text.indexOf(open)
  if (from === -1) return undefined
  const to = text.indexOf(`</${tag}>`, from + open.length)
  if (to === -1) return undefined
  return text.slice(from + open.length, to).trim()
}

/**
 * The plain command line a row of the `command` door echoes, or `undefined` where the row is not
 * the echo of a command a person ran.
 *
 * Measured on 2.1.310: the echo row holds one text block naming `<command-name>/usage</command-
 * name>`, `<command-message>usage</command-message>` and `<command-args></command-args>`,
 * newline-separated, each tag on its own line (same transcript as
 * `COMMAND_BOOKKEEPING_TAGS`). The command line is the name, and the arguments after a space
 * where there are any - `/clear`, `/effort low` - which is what the person typed and all of the
 * command the live view shows.
 */
export function claudeEchoedCommandLine(content: readonly ModContentBlock[]): string | undefined {
  const text = claudeRowText(content)
  const name = taggedValue(text, "command-name")
  if (name === undefined || name === "") return undefined
  const args = taggedValue(text, "command-args") ?? ""
  return args === "" ? name : `${name} ${args}`
}

/**
 * What the output row of a slash command printed, or `undefined` where the row is not an output
 * row. A row that is one and holds nothing answers `""`: the command printed nothing, which is
 * itself the answer the person who sent it gets.
 *
 * Measured on 2.1.310: the row holds one text block wrapped in
 * `<local-command-stdout>…</local-command-stdout>` (`<local-command-stderr>` where the command
 * wrote to stderr), and most commands put their whole output there and nowhere else -
 * `/effort low` resolves `$.command.run` with `{}` and writes
 * `<local-command-stdout>Set effort level to low …</local-command-stdout>` 158 ms later
 * (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q2-310.jsonl:98-102`).
 */
export function claudeCommandOutput(content: readonly ModContentBlock[]): string | undefined {
  const text = claudeRowText(content)
  for (const tag of COMMAND_OUTPUT_TAGS) {
    const value = taggedValue(text, tag)
    if (value !== undefined) return value
  }
  return undefined
}

/** Whether a row of the `command` door is Claude Code's bookkeeping of the command rather than
 *  anything a person wrote: the caveat it records before the command runs, and the command's own
 *  output, which published shows the person raw tags and a `/context` table in the agent's
 *  conversation (INFRA-1026). */
export function isCommandBookkeeping(content: readonly ModContentBlock[]): boolean {
  const text = claudeRowText(content)
  return COMMAND_BOOKKEEPING_TAGS.some((tag) => text.startsWith(tag))
}

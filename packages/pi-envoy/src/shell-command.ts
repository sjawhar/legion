/**
 * Which shell commands a Legion architect may run, and where a `dispatch` command's own text ends,
 * for `extensions/envoy.ts` and `extensions/legion.ts`, which bundle separately. A conservative
 * character scan, not a shell parser: whatever it cannot reason about it refuses.
 */

/** One `legion ...` invocation, with no chaining outside a quoted argument. It rejects some
 * legitimate quoting it can't reason about (nested quotes, escapes) rather than risk letting a
 * chained command through. */
export function isSingleLegionCommand(command: unknown): boolean {
  if (typeof command !== "string") return false;
  const trimmed = command.trim();
  if (trimmed.length === 0) return false;
  let quote: '"' | "'" | undefined;
  for (const char of trimmed) {
    if (quote !== undefined) {
      if (char === quote) quote = undefined;
      continue;
    }
    if (char === '"' || char === "'") {
      quote = char;
      continue;
    }
    if (char === "\n" || char === ";" || char === "&" || char === "|") return false;
  }
  if (quote !== undefined) return false;
  return trimmed.split(/\s+/, 1)[0] === "legion";
}

/** The one here-document a command's first line may end with, opened with a quoted delimiter so
 * the shell expands nothing in its body. */
const HEREDOC_OPENING = /^(.*?)\s*<<-?\s*'([A-Za-z_][A-Za-z0-9_]*)'\s*$/;

/** Outside quotes, a `dispatch` head refuses whatever would end the command, start another, expand
 * something, redirect, start a comment (bash reads the rest of the line as one, a here-document
 * opener included) or escape a character (the shell's to interpret, so never modelled here). */
const REFUSED_UNQUOTED = "\n;&|$`<>()#\\";

/** Inside double quotes bash still expands `$` and backticks and interprets `\`. */
const REFUSED_DOUBLE_QUOTED = "$`\\";

/** One `legion` or `dispatch` command, optionally fed one quoted here-document on stdin. */
export function isSingleArchitectCommand(command: unknown): boolean {
  return architectCommandHead(command) !== undefined;
}

/** A `dispatch` command's first line, without its here-document opener, when the command passes
 * `isSingleArchitectCommand`; undefined for any other command. */
export function dispatchCommandHead(command: unknown): string | undefined {
  const head = architectCommandHead(command);
  return head?.trim().split(/\s+/, 1)[0] === "dispatch" ? head : undefined;
}

function architectCommandHead(command: unknown): string | undefined {
  if (typeof command !== "string" || hasControlCharacter(command)) return undefined;
  const lines = command.trim().split("\n");
  let head = lines[0] ?? "";
  if (lines.length > 1) {
    const opening = HEREDOC_OPENING.exec(head);
    if (opening === null) return undefined;
    const delimiter = opening[2];
    const end = lines.findIndex(
      (line, index) => index > 0 && line.replace(/^\t+/, "") === delimiter
    );
    // The first line equal to the delimiter ends the here-document in the shell, so it must be
    // the last line: anything after it would run as a command.
    if (end !== lines.length - 1) return undefined;
    head = opening[1] ?? "";
  }
  const word = head.trim().split(/\s+/, 1)[0];
  if (word === "dispatch") return headScan(head) ? head : undefined;
  return word === "legion" && isSingleLegionCommand(head) ? head : undefined;
}

/** Any control character but newline and tab: a `\r` before a line end would make the delimiter
 * this scan finds differ from the one bash finds (bash ends a here-document at `EOF\r`, not `EOF`). */
function hasControlCharacter(text: string): boolean {
  for (let index = 0; index < text.length; index += 1) {
    const code = text.charCodeAt(index);
    if ((code < 0x20 && code !== 0x0a && code !== 0x09) || code === 0x7f) return true;
  }
  return false;
}

/** Whether a `dispatch` head is one command whose words the shell takes as written: single quotes
 * make every character literal, and an unterminated quote is refused. */
function headScan(head: string): boolean {
  let quote: '"' | "'" | undefined;
  for (const char of head) {
    if (quote === "'") {
      if (char === "'") quote = undefined;
      continue;
    }
    if (quote === '"') {
      if (char === '"') quote = undefined;
      else if (REFUSED_DOUBLE_QUOTED.includes(char)) return false;
      continue;
    }
    if (char === "'" || char === '"') quote = char;
    else if (REFUSED_UNQUOTED.includes(char)) return false;
  }
  return quote === undefined;
}

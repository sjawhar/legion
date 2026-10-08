/**
 * Which shell commands a Legion architect may run, and where a `dispatch` command's own text ends,
 * for `extensions/envoy.ts` and `extensions/legion.ts`, which bundle separately. A conservative
 * character scan, not a shell parser: whatever it cannot reason about it refuses. Whitespace is
 * bash's blanks, space and tab, never JavaScript's `\s`, which also takes a no-break space that
 * bash reads as part of a word.
 */

/** The one here-document a command's first line may end with, opened with a quoted delimiter so
 * the shell expands nothing in its body. The second group is `-` for `<<-`, the only form under
 * which bash strips leading tabs from the delimiter line. */
const HEREDOC_OPENING = /^(.*?)[ \t]*<<(-?)[ \t]*'([A-Za-z_][A-Za-z0-9_]*)'[ \t]*$/;

/** A line of nothing but blanks, which bash runs as no command. */
const BLANK_LINE = /^[ \t]*$/;

/** Outside quotes, a `legion` or `dispatch` head refuses whatever would end the command, start
 * another, expand something, redirect, start a comment (bash reads the rest of the line as one, a
 * here-document opener included) or escape a character (the shell's to interpret, so never
 * modelled here). */
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
  return head !== undefined && firstWord(head) === "dispatch" ? head : undefined;
}

function architectCommandHead(command: unknown): string | undefined {
  if (typeof command !== "string" || hasControlCharacter(command)) return undefined;
  // Blank lines before the head and after the command's last line run as no command, so they are
  // dropped whole. No other line is trimmed: bash ends a here-document only at a line that is
  // exactly its delimiter (with leading tabs stripped under `<<-`), whatever blanks the line
  // carries before or after it.
  const all = command.split("\n");
  const first = all.findIndex((line) => !BLANK_LINE.test(line));
  if (first === -1) return undefined;
  const last = all.findLastIndex((line) => !BLANK_LINE.test(line));
  const lines = all.slice(first, last + 1);
  let head = lines[0] ?? "";
  if (lines.length > 1) {
    const opening = HEREDOC_OPENING.exec(head);
    if (opening === null) return undefined;
    const stripTabs = opening[2] === "-";
    const delimiter = opening[3];
    const end = lines.findIndex(
      (line, index) => index > 0 && (stripTabs ? line.replace(/^\t+/, "") : line) === delimiter
    );
    // The first line equal to the delimiter ends the here-document in the shell, so it must be
    // the last line: anything after it would run as a command.
    if (end !== lines.length - 1) return undefined;
    head = opening[1] ?? "";
  }
  head = head.replace(/^[ \t]+|[ \t]+$/g, "");
  const word = firstWord(head);
  return (word === "dispatch" || word === "legion") && headScan(head) ? head : undefined;
}

/** The trimmed head's first word as bash splits it, at a space or a tab. */
function firstWord(head: string): string {
  return head.split(/[ \t]/, 1)[0] ?? "";
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

/** Whether a `legion` or `dispatch` head is one command whose words the shell takes as written:
 * single quotes make every character literal, and an unterminated quote is refused. */
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

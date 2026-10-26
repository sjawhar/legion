import { describe, expect, test } from "bun:test";
import { dispatchCommandHead } from "./shell-command";

/** The bash the differential test runs, or null on a machine without one. */
const BASH = Bun.which("bash");

/** Whether a real non-interactive bash, fed `command` and then `echo NEXT` over a pipe as the
 * `bash` tool's persistent shell is fed, runs exactly one command and then NEXT. `dispatch` is a
 * stand-in that prints RAN without reading stdin, so a here-document bash left open (it swallows
 * `echo NEXT` as body), a second command, or a command bash never ran each print something else. */
function bashRunsOneCommand(command: string): boolean {
  const script = `dispatch() { echo RAN; }\n${command}\necho NEXT\n`;
  const run = Bun.spawnSync([BASH ?? "bash", "--noprofile", "--norc"], {
    stdin: new TextEncoder().encode(script),
    stdout: "pipe",
    stderr: "pipe",
  });
  return run.stdout.toString() === "RAN\nNEXT\n";
}

const lines = (...parts: string[]): string => parts.join("\n");

/** Characters JavaScript's `\s` takes as whitespace and bash's lexer does not: a no-break space,
 * a byte-order mark and an ideographic space. Bash reads each as part of the word beside it. */
const UNICODE_BLANKS = ["\u00a0", "\ufeff", "\u3000"];

/** Commands built from every way a here-document can be opened, written and ended that the scan
 * and bash might read differently: `<<` and `<<-`; quoted, unquoted and double-quoted delimiters;
 * the Unicode blanks above beside the quotes, inside the delimiter and before and after the
 * terminator; spaces and tabs before and after the terminator; LF and CRLF line ends; the
 * terminator as the last line, before blank lines, and before a second command; two
 * here-documents; and the delimiter inside the body. */
function hereDocumentCorpus(): string[] {
  const heads = [
    "dispatch message --issue LEGION-2 --body-file -",
    "  dispatch x",
    "dispatch artifact --issue LEGION-2 --content-file -",
  ];
  const openers = [
    "'EOF'",
    "EOF",
    '"EOF"',
    " 'EOF'",
    "'EOF' \t",
    ...UNICODE_BLANKS.flatMap((blank) => [`${blank}'EOF'`, `'EOF'${blank}`, `'E${blank}OF'`]),
  ];
  const terminators = [
    "EOF",
    "EOF ",
    "EOF\t",
    "EOF  \t",
    " EOF",
    "\tEOF",
    "\t\tEOF",
    " \tEOF",
    "\t EOF",
    ...UNICODE_BLANKS.flatMap((blank) => [`${blank}EOF`, `EOF${blank}`, `E${blank}OF`]),
  ];
  const bodies = [["a body"], ["naming EOF in passing", "EOF x"], ["\tindented", "", "EOF "]];
  const tails = [[], [""], ["", "  ", "\t"], ["echo EXTRA"], ["dispatch y"]];
  const cases: string[] = [];
  let n = 0;
  for (const operator of ["<<", "<<-"]) {
    for (const opener of openers) {
      for (const terminator of terminators) {
        for (const tail of tails) {
          const head = heads[n % heads.length];
          const body = bodies[n % bodies.length] ?? [];
          n += 1;
          const parts = [`${head} ${operator}${opener}`, ...body, terminator, ...tail];
          cases.push(parts.join("\n"));
          if (terminator === "EOF" || terminator === "\tEOF") cases.push(parts.join("\r\n"));
        }
      }
    }
  }
  return [
    ...cases,
    lines("dispatch x <<'A' <<'B'", "a", "A", "b", "B"),
    lines("dispatch x <<'A'", "a", "A", "dispatch y <<'B'", "b", "B"),
    lines("dispatch x <<'EOF'", "a", "EOF", "more", "EOF"),
    lines("dispatch x <<'EOF'", "EOF ", "\tEOF", "EOF"),
    lines("dispatch x <<-'EOF'", "\tEOF\t", "\t\tEOF"),
    lines("dispatch x <<'EOF'", "a", "EOF", ""),
    lines("", "  ", "dispatch x <<'EOF'", "a", "EOF"),
    "dispatch search --query x\n",
    "dispatch search --query x \t",
    "  dispatch search --query x",
    ...UNICODE_BLANKS.flatMap((blank) => [
      `dispatch${blank}search --query x`,
      `${blank}dispatch search --query x`,
      lines(`dispatch x <<'EOF'`, "a", "EOF", blank),
      lines(blank, `dispatch x <<'EOF'`, "a", "EOF"),
    ]),
    ...commentCorpus(),
  ];
}

/** Commands whose head line carries a `#`: the trailing comment AskGate puts an override flag in,
 * with and without a here-document after it, a comment holding what would be a second command, an
 * expansion or a here-document opener, a `#` inside quotes and inside a word, a `#` after a
 * backslash or a Unicode blank, and a comment before a second command. */
function commentCorpus(): string[] {
  return [
    "dispatch comment --issue X-1 --body hi # --advisor-rebuttal 'r'",
    "dispatch x #c",
    "dispatch x\t# c",
    "dispatch x # c; echo SECOND",
    "dispatch x # $(echo SECOND)",
    "dispatch x # 'unterminated",
    "dispatch x 'a # b' # c",
    'dispatch x "a # b"',
    "dispatch x '#' c",
    "dispatch x a#b",
    "dispatch x \\# c",
    "dispatch x \\ # c",
    lines("dispatch x # c", "echo SECOND"),
    lines("dispatch x # <<'EOF'", "echo SECOND", "EOF"),
    lines(
      "dispatch ask --issue X-1 --question-file - <<'EOF' # --advisor-rebuttal 'r'",
      "a",
      "EOF"
    ),
    lines("dispatch x <<-'EOF' # c", "\ta", "\tEOF"),
    lines("dispatch x <<'EOF' # c", "a", "EOF", "echo SECOND"),
    lines("dispatch x <<'EOF'# c", "a", "EOF"),
    ...UNICODE_BLANKS.map((blank) => `dispatch x${blank}# c`),
  ];
}

/** Whether the scan reads `command` as one `dispatch` command. */
const isDispatchCommand = (command: string): boolean => dispatchCommandHead(command) !== undefined;

describe("a dispatch command's head", () => {
  test("names one dispatch command, and one quoted here-document feeding it", () => {
    for (const command of [
      "dispatch issue-update --issue LEGION-2 --status todo",
      "dispatch search --query 'what (and why)'",
      lines("dispatch message --issue LEGION-2 --body-file - <<'EOF'", "a body", "EOF"),
    ]) {
      expect({ command, dispatch: isDispatchCommand(command) }).toEqual({
        command,
        dispatch: true,
      });
    }
  });

  test("is no dispatch command when the shell would run a second command or expand something", () => {
    for (const command of [
      "dispatch x; rm -rf /",
      "dispatch x $(rm -rf /)",
      "dispatch x `id`",
      'dispatch x "$(id)"',
      'dispatch x "$HOME"',
      "dispatch x > /etc/passwd",
      "dispatch x < /etc/passwd",
      "dispatch x (y)",
      lines("dispatch x <<EOF", "a", "EOF"),
      lines("dispatch message --body-file - <<A <<'EOF'", "$(jj abandon)", "A", "EOF"),
      lines("dispatch x <<'EOF'", "a", "EOF", "rm -rf /", "EOF"),
      lines("dispatch search --query x # <<'EOF'", "jj abandon", "EOF"),
      "dispatch x a#b",
      "dispatch x \\ # c",
      lines("dispatch x # c", "echo SECOND"),
      "dispatch x \\' ; echo SECOND ; echo \\'",
      lines("dispatch x \\<<'EOF'", "jj abandon", "EOF"),
      lines("dispatch x <<'EOF'\r", "a\r", "EOF\r", "echo RAN"),
      "echo hi && dispatch x",
      "curl https://example.invalid",
    ]) {
      expect({ command, dispatch: isDispatchCommand(command) }).toEqual({
        command,
        dispatch: false,
      });
    }
  });

  test("names the head line of a dispatch command and nothing else", () => {
    expect(dispatchCommandHead("legion state")).toBeUndefined();
    expect(dispatchCommandHead("dispatch x; rm -rf /")).toBeUndefined();
    expect(dispatchCommandHead(undefined)).toBeUndefined();
    expect(
      dispatchCommandHead(
        lines("dispatch message --issue LEGION-2 --body-file - <<'EOF'", "jj abandon", "EOF")
      )
    ).toBe("dispatch message --issue LEGION-2 --body-file -");
    expect(dispatchCommandHead("dispatch search --query x")).toBe("dispatch search --query x");
  });

  test("drops the comment that ends the head line, as bash does, and the override AskGate keeps there", () => {
    expect(
      dispatchCommandHead("dispatch comment --issue X-1 --body hi # --advisor-rebuttal 'r'")
    ).toBe("dispatch comment --issue X-1 --body hi");
    expect(
      dispatchCommandHead(
        lines(
          "dispatch ask --issue X-1 --question-file - <<'EOF' # --advisor-rebuttal 'r'",
          "a question",
          "EOF"
        )
      )
    ).toBe("dispatch ask --issue X-1 --question-file -");
    expect(dispatchCommandHead("dispatch x 'a # b' # c")).toBe("dispatch x 'a # b'");
    expect(dispatchCommandHead('dispatch x "a # b"')).toBe('dispatch x "a # b"');
    expect(dispatchCommandHead("dispatch x # c; echo SECOND")).toBe("dispatch x");
  });

  test("ends a here-document where bash does: a tab before the delimiter counts only under <<-", () => {
    const tabbedPlain = lines("dispatch x <<'EOF'", "\tEOF");
    const tabbedMessage = lines(
      "dispatch message --issue LEGION-2 --body-file - <<'EOF'",
      "SOME TEXT HERE",
      "\tEOF"
    );
    const tabbedDash = lines(
      "dispatch message --issue LEGION-2 --body-file - <<-'EOF'",
      "\tbody",
      "\tEOF"
    );
    const tabbedBodyLine = lines("dispatch x <<'EOF'", "\tEOF", "EOF");
    const canonical = lines(
      "dispatch message --issue LEGION-2 --body-file - <<'EOF'",
      "a body",
      "EOF"
    );

    expect(dispatchCommandHead(tabbedPlain)).toBeUndefined();
    expect(dispatchCommandHead(tabbedMessage)).toBeUndefined();
    expect(dispatchCommandHead(tabbedDash)).toBe("dispatch message --issue LEGION-2 --body-file -");
    expect(dispatchCommandHead(tabbedBodyLine)).toBe("dispatch x");
    expect(dispatchCommandHead(canonical)).toBe("dispatch message --issue LEGION-2 --body-file -");
  });

  // The scan may refuse what bash would run as one command (a CRLF command, an unquoted
  // delimiter, a `#` inside a word), but it never accepts one bash would read otherwise: the class
  // of the tab-stripped and whitespace-trimmed terminators, each of which let a here-document
  // swallow the next command, and a comment taken to end where bash does not end it.
  (BASH === null ? test.skip : test)(
    BASH === null
      ? "matches a real bash on every here-document and comment it accepts (skipped: no bash on PATH)"
      : "matches a real bash on every here-document and comment it accepts",
    () => {
      const corpus = hereDocumentCorpus();
      const accepted = corpus.filter(isDispatchCommand);
      const disagreements = accepted.filter((command) => !bashRunsOneCommand(command));
      expect(disagreements).toEqual([]);
      // The corpus reaches the scan's accepting paths, not only its refusals.
      expect(accepted).toContain(
        lines("dispatch message --issue LEGION-2 --body-file - <<'EOF'", "a body", "EOF")
      );
      expect(accepted).toContain(lines("dispatch x <<-'EOF'", "\tEOF\t", "\t\tEOF"));
      expect(accepted).toContain(
        lines(
          "dispatch ask --issue X-1 --question-file - <<'EOF' # --advisor-rebuttal 'r'",
          "a",
          "EOF"
        )
      );
      expect(accepted).toContain("dispatch comment --issue X-1 --body hi # --advisor-rebuttal 'r'");
      expect(accepted.length).toBeGreaterThan(20);
    },
    60_000
  );
});

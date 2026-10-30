---
title: "Text put back into a shell command an agent runs goes where the parser ends the line, and is read back before it is kept"
category: integration-patterns
tags:
  - shell
  - quoting
  - untrusted-input
  - command-rewrite
date: 2026-10-30
status: active
module: packages/pi-envoy
related_issues:
  - "INFRA-168"
  - "sjawhar/legion#2161"
---

# Text put back into a shell command an agent runs goes where the parser ends the line, and is read back before it is kept

Extends docs/solutions/daemon/scrub-tmux-environment-tables-by-parsing-show-environment-s.md.

When code writes text that an agent or person chose (an override's reason, a note) into a shell command, and the agent then runs that command:

- **Write it at one position, which your command parser computes: where bash ends the command's line.** That is after the last word, any `2>&1`, any here-document opener and any pipe stage the grammar accepts. It is never the first newline, which can sit inside a quoted value or before a line continuation. Writer and reader take that position from one function. A second scanner that decides "is this past a newline" disagrees with it at line continuations.
- **Quote every word, the flag name included, and do not rely on the quoting for safety.** Inside an open single-quoted value the quotes pair up so that the inserted text ends up outside them. Only the position makes the text inert. Refuse a value that no literal word can carry (a newline, or a `'` together with one of `"` `$` `` ` `` `\`) rather than escaping it harder.
- **Parse the rewritten command back with the same grammar before you hand it over,** and keep it only when it reads back as the same command and the same inserted value. Otherwise hand back the command without the text, and say that the text was removed. That read-back proves that your grammar takes back what it wrote. It does not prove that bash agrees with your tokenizer, so prove that separately.
- **Prove it in real bash:** a table that runs each rewritten command against a stand-in program and a sentinel file, with `$(...)`, backticks and quotes in the text, across the command shapes the grammar accepts. Expect the exact argv and stdin of the command without the text, and no sentinel. Run the same table on the code before the fix, so the table is shown to catch the defect.

## Evidence

- At a43bfa38 the override comment was put at the first newline. Review threads of that round showed that it cut a multi-line quoted value, missed a piped command, and let a `$(...)` in the rebuttal run. In the deep review's bash run, `--advisor-rebuttal 'as asked: $(touch .../PWNED)'` created the file.
- e361a909 moved the comment to `lineEnd` and quoted each word. On bd250c48, the code before it, 18 of the 30 real-bash rows fail and the `$(...)` row creates the sentinel.
- Thread `PRRT_kwDORFy7ds6xz0kR`: a second scanner (`newlineOutsideQuotes`) refused, across a `cd … \⏎ &&` continuation, a comment that the writer had put at `lineEnd`. 08ff93e8 deleted it, so both sides read the one `lineEnd` from `dispatchCommandHead`. Thread `…kS` corrected the claim that quoting alone made the text inert.
- The landed shape: `packages/pi-envoy/extensions/askgate/askgate-core.ts:2059` (`literalWord`), `:2071` (`overrideWords`), `:2097` (`keepOverrideVisible`, which reads its output back). Tester round 16 ran nine continuation shapes through real bash in both modes: the comment was kept in all 18 runs, and no canary appeared.

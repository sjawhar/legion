---
title: "A hit on a removed identifier is judged on the matched text, never on a truncated line or as an unrelated word"
category: legion
tags:
  - census
  - grep
  - forward-merge
  - agent-read-text
date: 2026-10-26
status: active
module: skills, docs
related_issues:
  - "INFRA-80"
  - "sjawhar/legion#2173"
---

# A hit on a removed identifier is judged on the matched text, never on a truncated line or as an unrelated word

Extends docs/solutions/legion/a-record-is-true-when-written-and-read-later-so-pin-each-claim-to-a-command-or-delete-it-INFRA-83.md.

That document covers a read whose renderer cut a line and so reported a sentence absent. This is
the case after the search has worked: the grep *found* the line, and the agent dismissed the hit
because the part it printed did not show the match.

- **Print the match, not the line.** Use `grep -o '.\{0,80\}<name>.\{0,80\}'` for the match and its
  context, or `grep -l` to list files you then read. Never cut each output line to a display width
  (`cut -c1-300`) when sorting hits. Skill and prompt files write a whole step on one line of 700+
  characters, and the name you removed sits at its end.
- **A removed identifier has no innocent hit.** A file name, a variable or a symbol your change
  deleted means nothing in ordinary prose. Give every hit a reason with its `file:line`: history (a
  changelog, a contract entry, `docs/solutions`), a test asserting absence, or a fix. "Unrelated
  word" is never one of them.
- **When a conflict resolves to main's whole file, grep main's version for what your hunk took
  out before you write that the merge restores nothing.** Main's rewrite can carry the old
  statement into a different sentence of the same file. The unchanged-diff fingerprint cannot see
  this, since the line is main's own text and not your diff's.

## Evidence (INFRA-80, sjawhar/legion#2173)

Main's INFRA-353 rewrite of the Legion skills (`af460a46bb16`) conflicted with this change's hunk
in `skills/legion-worker/SKILL.md`, which had removed the instruction to read
`.legion/<issue>/workspace-recovered.json`, a marker this change stops writing. The merge
`8867bd5d67a1` took main's file. The merge comment (issuecomment-6408160931) said "Main deleted
that whole paragraph, so the merge takes main's file unchanged and restores nothing". But main
had moved the instruction to the end of step 1, line 14, a line of about 770 characters. The
implementer's census `grep -rnEi 'workspace-recovered|…' skills/ | cut -c1-300` matched that line,
printed its first 300 characters, and the handoff filed the hit under "unrelated words". The
tester's red test (`skills-guard.test.ts:84-102`, commit `585076d4e409`, 11 pass and 1 fail
naming that file) caught it, and `4bee7ec164de` fixed it. The fingerprint with the skill paths
excluded was `032f4c81…` both before and after the merge, as it had to be.

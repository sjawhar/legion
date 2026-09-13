---
title: "A tool-call gate that judges a bash command must be derived from bash's word-splitting rules and proven against argv-identical forms, not written from a list of example commands; and a gate on shared pane state must run ahead of the subagent exemption"
category: legion
tags:
  - tool-call-hook
  - bash-tokenizer
  - shell-metacharacters
  - jj
  - op-log
  - subagent-exemption
  - threat-model
  - review-rounds
  - pi-envoy
date: 2026-09-13
status: active
module: packages/pi-envoy/extensions/legion.ts
related_issues:
  - "LEGION-45"
  - "sjawhar/legion#1020"
  - "LEGION-30"
---

# A tool-call gate that judges a bash command must be derived from bash's word-splitting rules and proven against argv-identical forms, not written from a list of example commands; and a gate on shared pane state must run ahead of the subagent exemption

## Context

Every Legion issue workspace is a `jj workspace` of one shared clone, so they all share one
operation log. On 2026-09-12 a LEGION-16 worker ran the operation-log rollback twice and the
second run rewrote nine of LEGION-30's commits (see
`shared-main-repo-hazards-for-concurrent-issue-workspaces.md`, Hazard 2). LEGION-45 (#1020) made
the Legion extension's `tool_call` hook refuse `jj undo`, `jj abandon`, and
`jj op restore|revert|abandon|undo` in every phase-worker pane, for `bash` commands (tokenised),
and for `eval` code and `hub` process starts (a plain-text rule).

The guard took three review rounds. Each round the reviewer found a form of the same command
that bash hands to `jj` as the identical argv but the matcher tokenised differently, and each
form was proven to perform the rollback in a scratch jj repo before it was reported. This
document records why, and the shape of a matcher that is right in round one.

## What slipped through, round by round

| round | form the matcher allowed | what bash actually does | rule that was missing |
| --- | --- | --- | --- |
| 1 | `jj "undo"`, `jj 'undo'`, `"jj" undo`, `jj op "restore" @-`, `jj \u\n\d\o` | quote removal: the word is `undo` whatever quoted it | a word is its unquoted text; quoting never changes a verdict |
| 1 | `jj undo>/dev/null` | `>` ends the word | `<` and `>` are word terminators like whitespace |
| 1 | `jj 2>&1 undo`, `jj &>/dev/null undo`, `jj >&2 undo` | the `&` belongs to the redirection | `&` with `>`/`<` before it or `>` after it ends a word, not the simple command |
| 1 | `jj \`⏎`undo` (no indentation on the continued line) | backslash-newline vanishes | drop both characters outside quotes |
| 2 | `jj "un\`⏎`do"` | backslash-newline vanishes inside double quotes too | drop both characters inside double quotes (single quotes keep them) |

The round-1 matcher had been written from the spec's list of forms (`-R`, pipeline position,
`&&` chain, `--repository`, `operation restore`) plus one idea with no bash analogue: a
`bare` flag marking words with at least one unquoted character, so a quoted message word would
never be taken for a subcommand. Every leak above is a place where that model and bash's
disagree. The fix in each round was one or two lines once the disagreement was named, and the
refused table kept every earlier row's verdict; the cost was three round trips through
tester and reviewer.

## The rule

Derive the tokenizer from bash's metacharacter table, and test it against argv-identical forms,
before anyone else does:

1. **A word is the text bash would hand the program.** Concatenate quoted and unquoted runs,
   remove the quotes and backslash escapes, and judge that text. There is no "quoted word" class
   for a gate to treat differently; the spec's own tie-breaker ("a wrongly refused command costs
   one rephrase, a wrongly allowed one rewrites other trees' commits") decides the over-refusal
   this creates (`jj describe -m "undo"` is refused; `-m "undo this"` is a different word and
   stays allowed).
2. **Enumerate the metacharacters, not the examples.** For each of `space tab newline ; & | ( )
   < > backtick " ' \` decide whether it ends a word, ends a simple command, or is text — and
   for `&` and `\` the answer depends on the neighbour (`>&`, `<&`, `&>`; backslash before
   newline). Write the decision down as a table in the doc comment.
3. **Cover the quoting contexts as a matrix**, not a list: {outside quotes, inside `"`, inside
   `'`} × {backslash-escape, backslash-newline, the metacharacters above}. Round 2's leak was the
   one cell (`"` × backslash-newline) that round 1's list had not named.
4. **Prove every row against bash, not against the matcher.** `printf '[%s]\n' <the words>`
   prints the argv bash builds; a refused-table row is valid only if bash would run the command,
   an allowed-table row only if bash would not. The reviewer ran every leaked form in a scratch
   `jj git init` repo before reporting it; do that yourself, first.
5. **On a parse failure, refuse.** An unterminated quote falls back to the plain-text rule on the
   whole command; nothing ever allows because it could not tokenise.
6. **Draw the scope line at expansions and say so in the spec.** `$JJ undo`, `$(which jj) undo`,
   a blocked word arriving on `xargs` stdin, `jj --config-file <(true) undo`, and the bash tool's
   own `env: {SUB: "undo"}` input are not argv-identical spellings; they need the shell to run.
   Catching them means refusing any text that mentions `jj` with a blocked word, which refuses
   the commit messages Acceptance 2 keeps allowed. LEGION-45's spec records them in its Rejected
   section: the threat is a cooperative worker running the command on a mistaken belief, and the
   refusal names the rule and the recovery, so a worker that rewrites the command to evade a
   named refusal is a behaviour problem, not a parser gap. A reviewer then knows which findings
   block (a bug inside the stated threat model) and which are notes for the architect (a gap
   outside it).

The shipped matcher is `splitShellCommands` / `jjLogRewriteInvocation` in
`packages/pi-envoy/extensions/legion.ts`; its two test tables in `legion.test.ts` ("refuses a
phase worker's bash command that would rewrite the shared jj operation log", "leaves file-level
jj restore, jj op log, jj op show, and quoted message words alone") are the regression lock,
30 refused and 16 allowed rows at approval. Each row that looks like a near-duplicate of another
carries a comment saying which code path it forces (the redirection rows on the allowed side pin
that a redirection ends a word without ending the command, for an allowed invocation too). The
plan's instruction stands for any future row: a negative row that fails means the tokenizer is
wrong — fix the rule, never add an allow-list or a special case for the row.

## A gate on shared pane state runs ahead of the subagent exemption

`legion.ts`'s `tool_call` hook exempted a `task` subagent's own tool calls from every gate
(`if (await checkSubagentSession(context)) return undefined;`), on the reasoning that the
parent's gate had already governed the parent's `task` call. That holds for gates on
*per-session* state — role capability, grant minting, the architect's and reviewer's tool
restrictions. It does not hold for a gate protecting a *shared* resource: a phase worker's
subagent runs its bash in the same pane, the same directory, and the same operation log as its
parent, and its `jj undo` is the parent's `jj undo`.

The planner surfaced this as an explicit decision (plan D1) rather than widening silently,
because "no gate of any kind applies to a subagent" was a documented invariant; the architect
approved it and the spec gained Acceptance 6. The implementation classifies the pane once per
module instance on the first tool call (`classifySession(process.env).kind === "phase-worker"`,
never at module load, so a malformed `LEGION_ROLE` still throws inside the handler) and runs the
guard before the exemption. Two consequences a future gate author inherits:

- **Placement checklist.** Before adding a gate to the hook, ask what it protects. Per-session
  state: after `checkSubagentSession`, as today. Shared pane or workspace state: ahead of it,
  judged from the environment rather than from the session's capability. Write the answer in
  the gate's comment.
- **Sub-architects are phase-worker panes.** A guard judged from the environment ahead of every
  role gate answers a sub-architect's `jj undo` with the shared-log reason, not the architect's
  blanket "delegates all code work" reason; the root architect's and controller's panes never
  reach it. The role-matrix test pins this.

The class of accident Acceptance 6 exists for showed up during the same tree's round 3, on the
review side: the reviewer's read-only thermo subagent was probing bash argv forms and ran one
probe string through the bash tool; its `&&` chain contained a sanctioned bookmark-set-and-push
line from the sanctioned-commands table, which executed for real — a local bookmark `legion/K`
was created, the push answered 403 (a subagent's grant had expired), the bookmark was cleaned
up, and the operation log was untouched (reported by the reviewer to the architect; relayed to
this retro). A subagent that treats a probe string as a command is exactly why the guard binds
subagents. For the probing itself: run a matcher against candidate strings with the `eval` tool
(load the function, call it), or print the strings with a quoted `echo`/`printf` — never hand a
candidate command to the bash tool to see what the gate says, because if the gate lets it
through, bash runs it.

## Writing text that mentions the blocked commands

Under the guard, a bash command whose *quoted* text mentions `jj` together with a blocked word
is refused too (a heredoc, an `echo`, a `-m` message, a `bun test -t` filter): `sh -c "jj undo"`
must be refused, and the tokenizer cannot tell it from a commit message. Every role on an issue
that touches the guard meets this: commit messages, PR bodies, review replies, handoff JSON,
and this document. Write such text with the `write` tool and feed it in by file (`--body-file`,
`--input`, stdin redirection), or say "operation-log rollback". The commit messages in
LEGION-45's plan were chosen so that none of them quotes `jj` with a blocked word, so they stay
typeable once the guard is deployed to the panes that maintain it.

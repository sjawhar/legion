---
title: "A value an agent wrote is a measurement only when the transcript holds it in a tool result"
category: legion
tags:
  - transcript
  - provenance
  - agent-records
  - fabricated-values
  - handoffs
  - acceptance-check
date: 2026-10-31
status: active
module: packages/daemon/internal/prompts
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# A value an agent wrote is a measurement only when the transcript holds it in a tool result

Extends docs/solutions/legion/a-record-is-true-when-written-and-read-later-so-pin-each-claim-to-a-command-or-delete-it-INFRA-83.md.
That note's rule is the policy — pin each claim to a command, or delete it. This is the instrument
that decides whether a given value obeyed it.

- **To tell a measured value from an invented one, search the session's own transcript for the
  string and read which kind of entry holds it.** A value in assistant entries alone came out of
  the model. A value that is also in a `toolResult` entry was read off something the session ran.

  ```sh
  jq -rc 'select((tostring|test("<the value>"))) | {type, role: (.message.role // null)}' <session>.jsonl
  ```

  A headless run writes that file when `OMP_SESSION_STORAGE=file` is set and
  `OMP_SESSION_SQL_DSN_FILE` is not; `--session-dir <dir>` says where.

- **Use it as the acceptance check for any instruction of the form "write only what you
  measured".** It is binary and cheap, and it applies to every class of value a later reader
  trusts: times, commit SHAs, counts, run ids, version strings, durations.

- **Write the instruction so this audit can decide it.** Name the admissible sources — a command,
  a task result or a tool result printed in this session, or the text of the task — rather than
  the property "correct". A reader cannot check "true"; a transcript can show "printed here".

- **An agent asked for a value it cannot read writes a plausible one. It does not refuse.** Expect
  a round number: the top of the hour, midnight, noon. Treat a suspiciously round value in an
  agent's record as unmeasured until the transcript says otherwise.

## Evidence

INFRA-336 removed `completed`, a time every Legion phase worker typed into its handoff and nothing
filled in. The proof ran the same three tasks on the branch's prompt and on the prompt `main`
still carried, composed from each checkout's own daemon module.

On `main`'s prompt, two of three sessions wrote `"completed":"2026-10-31T00:00:00Z"` and
`"completed":"2026-10-31T12:00:00Z"` — midnight, and noon ten hours in the future — in sessions
that ran at 01:30Z. Each string sat in exactly one assistant message of its transcript and in no
tool result: the sessions' only tool call was the `write` of the handoff itself.

On the branch's prompt no handoff carried a time at all, and the one RFC 3339 string any of those
sessions wrote — a pull-request comment recording when its proof ran — was in a `toolResult`
entry, the output of the `date -u +%FT%TZ` the session ran before it wrote the comment.

The sentence that now sits in `packages/daemon/internal/prompts/go/worker-common.md`,
`go/architect-common.md` and `roles/controller-root.md` states the sources rather than the
property, which is what makes the transcript check decide it.

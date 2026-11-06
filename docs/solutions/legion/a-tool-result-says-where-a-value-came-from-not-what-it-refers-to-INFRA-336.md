---
title: "A tool result says where a value came from, not what it refers to"
category: legion
tags:
  - transcript
  - provenance
  - agent-records
  - fabricated-values
  - acceptance-check
date: 2026-10-31
status: active
module: packages/daemon/internal/prompts
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# A tool result says where a value came from, not what it refers to

Extends docs/solutions/legion/a-value-an-agent-wrote-is-a-measurement-only-when-the-transcript-holds-it-in-a-tool-result-INFRA-336.md,
which calls the transcript check binary. It is binary about one question only, and the second
cycle of that same pull request found the hole.

- **The transcript check decides provenance, not reference.** A value passes it whenever the
  session ran *a* command that printed it. It cannot tell a reading of the right thing from a
  reading of the wrong thing.

- **Ask the second question too: did the command that printed it answer the question the field
  asks?** Run at the time, the head, the host or the subject the field names. A clock read now,
  written into a field that asks when something ran an hour ago, is a measured string and a false
  record. Only provenance and reference together make a value a measurement.

- **An instruction that forbids an unmeasured value must say what to write instead**, or it costs
  you the artifact that needed the value. An agent that cannot measure what the field asks for,
  and is given no alternative, writes nothing.

- **Whether an agent invents or refuses depends on what its task admits.** The same prompt rule
  produced a refusal when the task named no admissible source, and a correct measured value when
  the task said which command to run for it. Design the task, not only the rule.

## Evidence

INFRA-336's prompt sentence names the admissible sources of a time: a command, a task result or a
tool result printed in this session, or the text of the task.

In the tester's round at `4389e477`, three sessions on `main`'s prompt wrote `completed` times;
the tester's and the planner's read `2026-10-31T00:00:00Z` — midnight — in sessions that ran at
05:06Z, each in an assistant entry and in no tool result, which the transcript check catches. The
third did worse and passed the check: it **ran `date`** and wrote the output, `05:06:47Z`, as the
time its proof had run — a measured clock reading standing for a time nobody measured.

On the branch's prompt the same task produced a refusal: the implementer session wrote no comment
and replied that nothing in the session had printed that time, that `date -u +%FT%TZ` gives the
time now and not when the proof ran, and that writing it would be a fabricated record on the pull
request. That corrects the extended note's last bullet, which says an agent asked for a value it
cannot read writes a plausible one and does not refuse: under an instruction that names the
admissible sources, it did refuse — and the artifact that wanted a time was not written at all.

The same task rerun at retro, with `date` named in the task as the source for the comment's time,
got the comment and the correct time, whose string the transcript holds in a `toolResult` entry.

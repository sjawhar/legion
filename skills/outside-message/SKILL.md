---
name: outside-message
description: "Use before any text reaches a person outside the company: a comment on an issue or pull request of a repository the company does not own, an email, a chat message to another company, a release note, an enrollment reply, a bug report, a disclosure. Use it also before you open an ask that asks the operator to approve such text. Not for a message to the operator, to another agent, or inside the company's own organization."
---

# A message to someone outside the company

The operator approves every message that leaves the company, and four steps come before you ask.
The rules below are the operator's own, from the answers that set them; this skill is where they
are written down.

## The exclusions

An exclusion exists only where the operator has stated it. Never infer one from the shape of a
message.

**A project the operator has excluded by name** goes out without approval. Only the operator's own
answer names such a project, and your stream's brief is where that answer reaches you. With no such
answer for the project in front of you, there is no exclusion.

**A pull request to a project that has already enrolled**, with its description and its comments to
that project's maintainers. The operator does not want to be asked for approval every time for an
enrolled project, and ceremony around a pull request must not stop the work. The four steps still
apply. The approval ask does not. The work does not wait for the pull request either: prepare and
sign the changes locally, and send them when the work they belong to ends.

What counts as enrolled is the enrolling project's own record, and its stream's brief says where
that record is. Do not decide enrollment from a message's tone, or from a maintainer having replied
to the company before.

Everything else waits for the operator: a first contact with a project that has not enrolled, a bug
report, a disclosure, and a reply to a maintainer who wrote first.

## The four steps

1. **Research the audience.** Read the repository's contributing guide, its issue and pull-request
   templates, its code of conduct, and its last ten issues or pull requests. Read how the
   maintainers write to each other. Search for what has already been discussed about your subject; a
   question they answered last month, asked again, costs you the reader.
2. **Write for that audience, in their conventions.** Use their template where they have one, their
   terms, their level of detail. Run `skill://asd-ste100` and `skill://plain-words` over the text.
   Neither removes AI writing tells; where your session has a pass for those, run it as well.
3. **Follow their conventions in form as well as words.** A pull request on their repository uses
   their pull-request template. An issue uses their issue template. A thread reply matches the
   thread.
4. **A fresh reviewer reads it and cuts.** Dispatch a subagent that has not seen the drafting, give
   it the message and the research, and tell it to remove everything that is not needed to
   understand the core message. A reviewer that only approves has not done step 4. A subagent with
   no model of its own runs on yours. Where your session can give the reviewer a different model,
   do: a reader of the same model as the writer is the weaker check, though no measurement here
   separates the two.

**Step 4 ends with a list of what it removed, and that list goes in the ask.** Measured on
2026-10-30 over 18 drafts of three real messages: drafts written with this skill met the bar 9
times in 9, drafts with no guidance 8 times in 9, so the first three steps changed almost nothing
— the research teaches them. The step that changed the message was the cut, and it is the step
agents skip: 2 of 9 with the skill and 3 of 9 without had no cut at all. Only the cut drafts lost
a promise about a future release, and they lost half their length. An instruction alone did not
make the cut happen, so the list is what proves it did: three lines naming what came out, or the
sentence "the reviewer removed nothing", which is itself an answer.

The form of the text is not in this skill. Where the stream that owns the relationship keeps its own
rules for writing to maintainers — in its project's `AGENTS.md`, in its skills, or in its brief —
those rules decide how the message reads, and some of them are owned by people through that
repository's code owners. Read them before you draft, and send a change to the form there rather
than into this file.

This skill covers when approval is needed and what comes before the ask. The project's own rules
cover how the text reads.

## Then the ask

**Put the full message text in the ask itself.** Not a link to a document holding it, not a summary
of it, not "the draft is at …". The operator has to open another document to read the message in
almost every one of these asks, and cannot judge the message from the ask.

An ask for an outside message has, in this order:

- who the reader is, and one line on what they already know about this;
- **the message, in full, as it would be sent**;
- **what the reviewer removed in step 4**, in three lines or fewer, or "the reviewer removed
  nothing";
- what changes if the answer is no.

Link the research, not the message. An ask with no removal line has not had step 4, whatever it
says.

## Never write these

Delete the empty verb phrases and say the literal thing instead. These shapes:

| Never | Write instead |
|---|---|
| "to name the problem" | say the problem |
| "land the change" | merge it, release it, deploy it — whichever it is |
| "carry the finding to X" | tell X, send X, put it on the issue |
| "this shows the gap" | the gap is … |
| "that pays for itself" | it costs $X and returns $Y |

The test: delete the verb phrase and ask what literally happens. If you cannot say what happens, you
did not know, and the phrase hid that from both of you. Write what happens.

The same rule runs over asks, specs, messages, commit bodies and pull-request descriptions, not only
outside messages. `skill://plain-words` has the full list and the rewrites.

## What this skill does not cover

- Text written in a person's own name, which that person signs off.
- A message to another agent or to the operator: write it plainly and send it; no approval step.
- Posting a release the pipeline publishes from approved template text: the template was approved
  once; the release needs no new ask unless its text changes.

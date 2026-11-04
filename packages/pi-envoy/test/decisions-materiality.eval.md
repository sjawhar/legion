# Decisions materiality behavior check

This is a text-output check for `skills/dispatch-writing-specs/SKILL.md`, not a
model call in CI. The co-located skill guards check the packaged rule; this check
asks a fresh agent to use it. It covers the failure where a spec writer gave each
design detail its own decision section.

## Run

Freeze the task and pass bar below before running. Give each fresh agent the task
only, with the full skill text as its instructions. Give no diagnosis or grading
criteria. Collect the actual document text, not a description of planned writes.
No tools or live Dispatch access are needed.

Compare three arms: the unchanged skill, the proposed skill, and no skill text.
Run each arm three times, interleaved. If arms differ by only one or two passes,
run three more times per arm. Keep outputs under random IDs, separate from the
arm key. Have a fresh scorer read each output with the pass bar but without the
arm key. Report passes/trials, a 95% Wilson interval, the model, and a decisive
quote from each output. Keep the full outputs with the PR's verification record.
Do not tune the text on this case and then report it as held-out evidence.

## Task

```text
Prepare spec.md and decisions.md for ACME-42, which recovers an agent's task after
a provider error. Return both complete Markdown documents, labeled by filename.
This is an offline draft: do not call tools, request approval or post anything.
The research and source checks are complete. Use only the facts below.

The existing worker stops after a provider error, leaving the assigned task idle.
The operator wants it to resume without losing uncommitted work. The team has
settled these choices; write the documents that record the current design.

- Build on the existing recovery change rather than copy its claim state machine.
  Two implementations would conflict and give the task two owners.
- Retry errors without an attempt limit, waiting 30 seconds and doubling to a
  maximum of 10 minutes. Stopping after a fixed count strands work in an outage.
- After three consecutive provider refusals, start a new session; other provider
  errors retry in the same session. This preserves context for transient errors.
  The operator chose this in the answered ask
  dispatch://ACME-42/ask/11111111-1111-4111-8111-111111111111.
- Before replacing a session, save uncommitted work to a rescue branch. Replacing
  its pod can otherwise destroy that work.
- Put a blank line between paragraphs in the rendered error explanation.
- Use “Provider error” rather than “Provider failure” as that explanation's title.
  These last two presentation choices change no behavior or operator action.
  The operator chose the title in the answered ask
  dispatch://ACME-42/ask/22222222-2222-4222-8222-222222222222.

The spec's existing Design block IDs are b-reuse, b-retry, b-session and b-rescue
for the first four choices, respectively. There are no open questions. Acceptance
must check retry after an error, reset after three refusals, and work recovery.
```

## Pass bar

All conditions must hold:

- `decisions.md` has exactly four active `##` sections: reuse the existing state
  machine, retry policy, session reset policy, and preserving uncommitted work.
  Equivalent headings and order are fine; there is no word-count target.
- Neither presentation choice has a decision section or is bundled into another
  decision as an extra rationale. Each remains a single line in the spec, even
  though the title came from an answered ask.
- Each decision gives its reason and links the supplied spec block. The session
  policy also links its answered ask; the title's ask remains in its spec line.
- There are no invented choices, open asks or approval requests, and the spec
  still includes the three requested acceptance checks.

An empty decisions document, a plan to write one, or removing the presentation
choices entirely fails. A provider or harness error is not a scored trial.

## Held-out transfer task

Freeze this case before testing a revised instruction. It transfers the same
failure to a different design; it is not another observed production incident.
Use the same run protocol and pass bar, with exactly three material sections and
the three supplied export checks instead of the recovery sections and checks.

```text
Prepare spec.md and decisions.md for ACME-43, an audit-log export. Return both
complete Markdown documents, labeled by filename. This is an offline draft:
do not call tools, request approval or post anything. Research and source checks
are complete. Use only the facts below; there are no open questions.

Operators currently copy log rows by hand, which loses rows and can expose
message text. The export must give them a complete file without message bodies.
The team has settled these choices:

- Export CSV rather than JSON because operators import the file into a spreadsheet.
- Exclude message bodies, retaining only event time, actor and event type.
  Including bodies could expose private conversation text.
- Export the rows in a snapshot taken when the operator clicks Export rather than
  collecting new rows while the file is generated; the snapshot prevents missed
  or duplicated rows during concurrent writes. The operator chose this in
  dispatch://ACME-43/ask/33333333-3333-4333-8333-333333333333.
- Separate paragraphs in the export confirmation with a blank line.
- Title that confirmation “Export complete” rather than “Export finished”.
  These last two choices change no behavior or operator action. The operator
  chose the title in
  dispatch://ACME-43/ask/44444444-4444-4444-8444-444444444444.

The spec's existing Design block IDs are b-format, b-privacy and b-snapshot for
the first three choices. Acceptance must check spreadsheet-compatible CSV,
absence of message bodies, and a stable snapshot during concurrent writes.
```

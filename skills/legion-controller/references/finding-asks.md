# Finding asks and clear defects

Read this when a clear defect or a finding ask's reference reaches you on the controller topic,
when a finding ask you follow is answered, and at every start. What each is and where it goes is
`skill://legion-worker/references/findings.md`; what you may file is the skill's "Findings, and
the issues you file" section.

## File a clear defect

A role sends a clear defect as a message on the controller topic. The message holds its role
token, the defect, the issue it came from, where the fix belongs, the fix, and its evidence
comment on the report issue. That comment's first line is `Clear defect from <role>`. A message
can carry the evidence itself when the report issue did not exist or was closed. Then create or
reopen the report issue as the skill's "Daily report" says, and post that evidence with
`dispatch comment`, first line `Clear defect from <role>`. A clear defect you find yourself gets
the same comment, with `controller` as the role. A role that found no controller sends none: it
asks instead, the question opening `Clear defect from <role>` (`findings.md`, "When no controller
takes it"). Such an ask needs no answer: file it as below, the ask its evidence, then reply under
it and resolve it (`dispatch resolve-ask --kind resolved`).

1. Check that it is clear: the fix needs no choice a person must make, and it belongs in a
   repository your addressing line lists. Otherwise it is a finding that needs a decision: open an
   ask for it, as `findings.md` says.
2. Search first, as `skill://dispatch-first` says. When an open issue already tracks it, comment
   the evidence there, reply `Filed dispatch://<that KEY>` under the evidence comment, and file
   nothing.
3. File it in the project the repository maps to, on that repository:

   ```text
   dispatch issue --project <KEY> --repo <owner/name> --title '<the defect, as a title>' --label legion --spec-file -
   ```

   `--repo` is the repository where the fix belongs, as the message names it. Without it Dispatch
   records the project's default repository, and the daemon admits the issue there: in a project
   with two repositories, a defect in the other one starts work in the wrong workspace. The spec
   states the defect, where the fix belongs and the fix, cites the evidence by its `dispatch://`
   reference, and ends with the line `Filed for Legion from <role> on <issue>: <evidence>`.
   `<role>` is the sending role's token, or `controller` for one you found. `<issue>` is the
   `dispatch://<KEY>` it came from. `<evidence>` is the evidence comment's reference, or the ask's
   for a clear defect a role asked. The issue opens in `triage` with the label, so its triage wake
   brings it back to you (the skill's "New issue triage").
4. Reply under the evidence comment (or the ask: `--reply-to-ask '<ask id>'` on the ask's issue),
   its first line exactly `Filed dispatch://<NEW KEY>`:

   ```text
   dispatch comment --issue <REPORT KEY> --reply-to '<comment id>' --body-file -
   ```

   That reply is the record a later session of yours reads at triage and at every start.

## Follow it

A finding ask another role opened reaches you as its reference, `dispatch://<KEY>/ask/<id>`, on
the controller topic. Follow it, so that its answer reaches you:

```text
dispatch follow --ask 'dispatch://<KEY>/ask/<id>' --action follow
```

When `dispatch read --ref` shows it answered already, act on its answer now.

## Act on its answer

Once it is answered (an ask you follow brings its answer to you), read it with
`dispatch read --ref 'dispatch://<KEY>/ask/<id>'` and act on the newest answer, its typed text over
the selected option when the two disagree:

- `Do not file`: file nothing.
- `File an issue`: `dispatch issue --project <PROJECT> --title '<the problem, as a title>'
  --spec-file -` in the project where the fix belongs, as the answer or the ask names it. The spec
  states the problem and where the fix belongs, cites the ask and its evidence by their
  `dispatch://` references, and carries the line `Filed for <the answer's By:> at their request:
  dispatch://<REPORT KEY>/ask/<id>` (the skill's "Findings, and the issues you file"). No `legion`
  label.
- `File it for Legion`: the same, with `--label legion`. It opens in `triage` with the label, so
  its triage wake brings it back to you (the skill's "New issue triage").

An ask that re-files an escalated child also takes the steps of the skill's "Architect escalation".

Then reply under the ask with what you did, its first line exactly `Filed dispatch://<NEW KEY>` or
`Not filed: <reason>`. The ask's issue is the report issue, or the role's own issue for an ask no
controller took:

```text
dispatch comment --issue <the ask's issue> --reply-to-ask '<ask id>' --body-file -
```

Dispatch refuses to resolve an answered ask, so that reply is its record. An open finding ask that
turns moot (an issue now tracks the problem, or the problem is gone) you close with
`dispatch resolve-ask --kind resolved`, naming why.

## Catch up at every start

An answer that arrives while no controller runs never wakes you, a reference or clear defect
published then reaches nobody, and a new session follows none of the asks an earlier one
followed. So at every start, catch up on the report issue (with none yet, there is nothing to
catch up), and on the finding asks roles opened on their own issues while no controller ran:

1. Read it with `dispatch read`. Reopen it when its `Status:` is `done`, and assign it when its
   `Assignee:` says `unassigned` and a person is known, both as the skill's "Daily report" says.
2. Take each open ask: `dispatch open-asks --project <PROJECT>` lists every unanswered ask of the
   project, with no cap, each line naming its issue. It leaves out an ask on a closed issue
   (`asks.go`'s `listOpenAsks` takes `i.closed_at is null`), which is why an architect holds a
   held issue open for one. Follow those on the report issue, and file at once each whose question
   opens `Clear defect from `. One on another issue whose question opens `Finding from ` or
   `Clear defect from ` is a role's own-issue ask, which its architect holds acceptance on. File a
   clear defect. Move a finding that needs a decision: ask it again on the report issue, the same
   question, options and `--ref`, then reply `Moved dispatch://<REPORT KEY>/ask/<new id>` under the
   old ask. Resolve the old ask either way.
3. List the answered asks still waiting on you, which no `dispatch` command lists:
   `curl -sS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<REPORT KEY>/asks?state=answered&thread=1" | jq -c '[.[] | select(any(.options[]; .label == "Do not file" or .label == "Admit it")) | .answer.at as $t | select([.thread.replies[]? | select(.author.kind == "session" and .created_at > $t and (.body | test("^(Filed dispatch://|Admitted dispatch://|Not filed:|Not admitted:)")))] | length == 0) | {id, question, answer}]'`.
   Run the same for each issue in `needs_review`, whose architect holds its acceptance on a
   finding ask's answer. Take the list as one snapshot before you act on any issue in it:
   `curl -sS --fail-with-body -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues?project=<PROJECT>&status=needs_review" | jq -r 'if type == "array" then .[].key else error("Dispatch did not answer the listing") end'`.
   With neither `limit` nor `offset`, Dispatch answers every matching issue in one array
   (`api/issue_page.go`'s `parseIssuePage`). Do not page it. Pages by `offset` skip or repeat an
   issue whose status changes between reads, and the acceptance your reply releases moves an issue
   out of `needs_review`. `dispatch issues` always sends a page, so it cannot take this read.
   Read the exit status, never the empty output: a plain pipeline reports `jq`'s status alone, so
   an unset variable or a 401 would look like an empty list. `--fail-with-body` hands a refusal to
   `jq`, which `error` turns into a non-zero exit, and a connection failure prints curl's own
   message on stderr. No output with a zero exit is the one case that means nothing is waiting.
   Act on each as above, a triage ask as the skill's "New issue triage" says. Your reply under it
   releases that acceptance. An issue that enters `needs_review` after the snapshot is read at
   your next start.
4. List the clear defects nobody filed yet. Each is an evidence comment whose first line starts
   `Clear defect from `, with no session reply under it that starts `Filed dispatch://`, `Not
   filed:` or `Asked dispatch://` (a role that asked instead, which step 2 takes):
   `curl -sS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<REPORT KEY>/comments" | jq -c '. as $all | [.[] | select(.reply_to == null and (.body | startswith("Clear defect from "))) | .id as $id | select([$all[] | select(.reply_to == $id and .author.kind == "session" and (.body | test("^(Filed dispatch://|Not filed:|Asked dispatch://)")))] | length == 0) | {id, body}]'`.
   File each as [File a clear defect](#file-a-clear-defect) says. When one is not clear, reply
   `Not filed: <reason>` under it and open an ask for it instead.

A read that fails, and an assignment Dispatch refuses, are handled as the skill's "Findings, and
the issues you file" says.

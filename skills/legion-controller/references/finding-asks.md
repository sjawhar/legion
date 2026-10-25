# Finding asks and clear defects

Read this when a clear defect or a finding ask's reference reaches you on the controller topic,
when a finding ask you follow is answered, and at every start. What each is and where it goes is
`skill://legion-worker/references/findings.md`; what you may file is the skill's "Findings, and
the issues you file" section.

## File a clear defect

A role sends a clear defect as a message on the controller topic: the defect, the issue it came
from, where the fix belongs, the fix, and its evidence comment on the report issue. You file one
you find yourself the same way.

1. Check that it is clear: the fix needs no choice a person must make, and it belongs in a
   repository your addressing line lists. Otherwise it is a finding that needs a decision: open an
   ask for it, as `findings.md` says.
2. Search first, as `skill://dispatch-first` says. When an open issue already tracks it, comment
   the evidence there, reply under the evidence comment with that issue's reference, and file
   nothing.
3. File it in the project the repository maps to:

   ```text
   dispatch issue --project <KEY> --title '<the defect, as a title>' --label legion --spec-file -
   ```

   The spec states the defect, where the fix belongs and the fix, cites the evidence by its
   `dispatch://` reference, and ends with the line `Filed for Legion from <role> on <issue>:
   <evidence>`. `<role>` is the sending role's token, or `controller` for one you found.
   `<issue>` is the `dispatch://<KEY>` it came from. `<evidence>` is the evidence comment's
   reference, or `envoy message` when the evidence came in the message. The issue opens in
   `triage` with the label, so its triage wake brings it back to you (the skill's "New issue
   triage").
4. Reply under the evidence comment, its first line exactly `Filed dispatch://<NEW KEY>`:

   ```text
   dispatch comment --issue <REPORT KEY> --reply-to '<comment id>' --body-file -
   ```

   When the evidence came in the message, there is no comment to reply under: the `Filed for`
   line and this session's own filing are the record.

## Follow it

A finding ask another role opened reaches you as its reference, `dispatch://<KEY>/ask/<id>`, on
the controller topic. Follow it, so that its answer reaches you:

```text
dispatch follow --ask 'dispatch://<KEY>/ask/<id>' --action follow
```

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
`Not filed: <reason>`:

```text
dispatch comment --issue <REPORT KEY> --reply-to-ask '<ask id>' --body-file -
```

Dispatch refuses to resolve an answered ask, so that reply is its record. An open finding ask that
turns moot (an issue now tracks the problem, or the problem is gone) you close with
`dispatch resolve-ask --kind resolved`, naming why.

## Catch up at every start

An answer that arrives while no controller runs never wakes you, a reference published then
reaches nobody, and a new session follows none of the asks an earlier one followed. So at every
start, catch up on the report issue (with none yet, there is nothing to catch up):

1. Read it with `dispatch read`. Reopen it when its `Status:` is `done`, and assign it when its
   `Assignee:` says `unassigned` and a person is known, both as the skill's "Daily report" says.
2. Follow each of its open asks: `dispatch open-asks --project <PROJECT>` lists every unanswered
   ask of the project, with no cap, each line naming its issue; follow those on the report issue.
3. List the answered asks still waiting on you, which no `dispatch` command lists:
   `curl -sS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<REPORT KEY>/asks?state=answered&thread=1" | jq -c '[.[] | select(any(.options[]; .label == "Do not file" or .label == "Admit it")) | .answer.at as $t | select([.thread.replies[]? | select(.author.kind == "session" and .created_at > $t and (.body | test("^(Filed dispatch://|Admitted dispatch://|Not filed:|Not admitted:)")))] | length == 0) | {id, question, answer}]'`.
   Act on each as above, a triage ask as the skill's "New issue triage" says.

A read that fails, and an assignment Dispatch refuses, are handled as the skill's "Findings, and
the issues you file" says.

# Finding asks

Read this when a finding ask's reference reaches you on the controller topic, when one you follow
is answered, when you file an issue a person asked for, and at every start. What a finding ask is
and where it goes is `skill://legion-worker/references/findings.md`; what you may file is the
skill's "Findings, and the issues you file" section.

## The `Filed for` line

Every issue you file for a person carries one line in its spec, `Filed for <person> at their
request: <request>`, because triage reads it to tell your filing from anyone else's label
(`skill://legion-controller/references/triage.md`). The request, and the person it names, are:

- an answer to an ask: `dispatch://<REPORT KEY>/ask/<id>`, for the person who answered it;
- a Slack request: `Slack <channel_id> message <ts>`, for the mention's `<@user_id>`;
- a direct message: `direct message <id>`, with the id its frame's `reply_with` names after
  `--in-reply-to`, for the frame's `actor`. A message that arrives as your own user turn shows no
  id: write `direct message to session <your session id>`, for the person by the name they gave,
  else `the person who messaged this session`.

## Follow it

A finding ask another role opened reaches you as its reference, `dispatch://<KEY>/ask/<id>`, on
the controller topic. Follow it, so that its answer reaches you:

```text
dispatch follow --ask 'dispatch://<KEY>/ask/<id>' --action follow
```

## Act on its answer

Once it is answered (an ask you follow brings its answer to you), read it with
`dispatch read --ref 'dispatch://<KEY>/ask/<id>'` and act on the newest answer. When its typed
text asks for a different one of the three actions than the option selected, do what the text
asks; when the text asks for something that is none of the three, reply under the ask with your
question and file nothing:

- `Do not file`: file nothing.
- `File an issue`: `dispatch issue --project <PROJECT> --title '<the problem, as a title>'
  --spec-file -` in the project where the fix belongs, as the answer or the ask names it; when
  neither names one, in this project. The spec
  states the problem and where the fix belongs, cites the ask and its evidence by their
  `dispatch://` references, and carries the line `Filed for <the answer's By:> at their request:
  dispatch://<REPORT KEY>/ask/<id>` (above). No `legion` label.
- `File it for Legion`: the same, with `--label legion`. It opens in `triage` with the label, so
  its triage wake brings it back to you (`skill://legion-controller/references/triage.md`).

An ask that re-files an escalated child also takes the steps of
`skill://legion-controller/references/escalation.md`.

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
   `Assignee:` says `unassigned` and a person is known, both as
   `skill://legion-controller/references/daily-report.md` "Keep it answerable" says.
2. Follow each of its open asks: `dispatch open-asks --project <PROJECT>` lists every unanswered
   ask of the project, with no cap, each line naming its issue; follow those on the report issue.
3. List the answered asks still waiting on you, which no `dispatch` command lists:
   `curl -sS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<REPORT KEY>/asks?state=answered&thread=1" | jq -c '[.[] | select(any(.options[]; .label == "Do not file" or .label == "Admit it")) | .answer.at as $t | select([.thread.replies[]? | select(.author.kind == "session" and .created_at > $t and (.body | test("^(Filed dispatch://|Admitted dispatch://|Not filed:|Not admitted:)")))] | length == 0) | {id, question, answer}]'`.
   Act on each as above, a triage ask as `skill://legion-controller/references/triage.md` says.

A read that fails, and an assignment Dispatch refuses, are handled as
`skill://legion-controller/SKILL.md#rules` and
`skill://legion-controller/references/daily-report.md` say.

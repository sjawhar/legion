# Finding asks

Read this when a clear defect or a finding ask's reference reaches you on the controller topic,
when one you follow is answered, when you file an issue a person asked for, and at every start.
What a finding ask is and where it goes is `skill://legion-worker/references/findings.md`; what
you may file is the skill's "Findings, and the issues you file" section.

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

For a clear defect the line is `Filed for Legion from <role> on <issue>: <evidence>`
([File a clear defect](#file-a-clear-defect)).

## File a clear defect

A role sends a clear defect on the controller topic: its role token, the defect, the issue it came
from, where the fix belongs, the fix, and its evidence comment on the report issue, whose first line
is `Clear defect from <role>`. When the report issue was missing or closed, the message carries the
evidence: create or reopen the report issue (`skill://legion-controller/references/daily-report.md`)
and post it there with that first line. A clear defect you find yourself gets the same comment, with
`controller` as the role. A clear-defect ask (`findings.md`, "When no controller takes it") needs
no answer: file it as below, the ask its evidence, then reply under it and resolve it
(`dispatch resolve-ask --kind resolved`).

1. Check that it is clear: the fix needs no choice a person must make, and it belongs in a
   repository your addressing line lists. Otherwise open an ask for it, as `findings.md` says.
2. Search first (`skill://dispatch-first`). When an open issue tracks it, comment the evidence
   there, reply `Filed dispatch://<that KEY>` under the evidence, and file nothing.
3. File it on the repository where the fix belongs, in the project that repository maps to:

   ```text
   dispatch issue --project <KEY> --repo <owner/name> --title '<the defect, as a title>' --label legion --spec-file -
   ```

   Without `--repo`, Dispatch records the project's default repository, and the daemon admits the
   issue there. The spec states the defect, where the fix belongs and the fix, and ends with the
   `Filed for Legion from` line: the sending role's token (or `controller`), the `dispatch://<KEY>`
   it came from, and the evidence comment's or the ask's reference. It opens in `triage` with the
   label, and triage admits it (`skill://legion-controller/references/triage.md`).
4. Reply under the evidence comment (`--reply-to '<comment id>'`) or the ask
   (`--reply-to-ask '<ask id>'`, on the ask's issue), its first line exactly
   `Filed dispatch://<NEW KEY>`. A later session reads that reply at triage and at every start.

## Follow it

A finding ask another role opened reaches you as its reference, `dispatch://<KEY>/ask/<id>`, on
the controller topic. Follow it, so that its answer reaches you:

```text
dispatch follow --ask 'dispatch://<KEY>/ask/<id>' --action follow
```

When `dispatch read --ref` shows it answered already, act on its answer now.

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
`Not filed: <reason>`. The ask's issue is the report issue, or the role's own issue for an ask no
controller took:

```text
dispatch comment --issue <the ask's issue> --reply-to-ask '<ask id>' --body-file -
```

Dispatch refuses to resolve an answered ask, so that reply is its record. An open finding ask that
turns moot (an issue now tracks the problem, or the problem is gone) you close with
`dispatch resolve-ask --kind resolved`, naming why.

## Catch up at every start

An answer that arrives while no controller runs never wakes you, a reference published then
reaches nobody, and a new session follows none of the asks an earlier one followed. So at every
start, catch up on the report issue (with none yet, there is nothing to catch up), and on the
finding asks roles opened on their own issues while no controller ran:

1. Read it with `dispatch read`. Reopen it when its `Status:` is `done`, and assign it when its
   `Assignee:` says `unassigned` and a person is known, both as
   `skill://legion-controller/references/daily-report.md` "Keep it answerable" says.
2. Take each open ask: `dispatch open-asks --project <PROJECT>` lists every unanswered ask of the
   project, with no cap, each line naming its issue. It leaves out an ask on a closed issue
   (`asks.go`'s `listOpenAsks` takes `i.closed_at is null`), which is why an architect holds its
   issue open for one. Follow those on the report issue, and file at once each whose question opens
   `Clear defect from `. One on another issue whose question opens `Finding from ` or
   `Clear defect from ` is a role's own-issue ask. File a clear defect. Move a finding that needs a
   decision: ask it again on the report issue with the same question, options and `--ref`, then
   reply `Moved dispatch://<REPORT KEY>/ask/<new id>` under the old ask. Resolve the old ask either
   way.
3. List the answered asks still waiting on you, which no `dispatch` command lists:
   `curl -sS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<REPORT KEY>/asks?state=answered&thread=1" | jq -c '[.[] | select(any(.options[]; .label == "Do not file" or .label == "Admit it")) | .answer.at as $t | select([.thread.replies[]? | select(.author.kind == "session" and .created_at > $t and (.body | test("^(Filed dispatch://|Admitted dispatch://|Not filed:|Not admitted:)")))] | length == 0) | {id, question, answer}]'`.
   Act on each as above, a triage ask as `skill://legion-controller/references/triage.md` says.
   Run the same for each issue in `needs_review`, whose architect holds its acceptance on a finding
   ask's answer. Take that list as one snapshot before you act on any issue in it:

   ```text
   rows=$(curl -sS --fail-with-body -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues?project=<PROJECT>&status=needs_review") || { echo "Dispatch read failed" >&2; exit 1; }
   printf '%s' "$rows" | jq -r 'if type == "array" then .[].key else error("Dispatch did not answer the listing") end'
   ```

   With neither `limit` nor `offset`, Dispatch answers every match in one array
   (`api/issue_page.go`'s `parseIssuePage`). `dispatch issues` always sends a page, so it cannot
   take this read. Never page it: pages by `offset` skip or repeat an issue whose status changes
   between reads, and your reply moves issues out of `needs_review`. The read lands in `rows` first,
   because a pipeline reports only the status of `jq`, which succeeds on a failed read's empty
   input. Exit 0 with keys is work. Exit 0 with no output means nothing waits. Anything else is a
   failed read and admits nothing. An issue that enters `needs_review` after the snapshot is read
   at your next start.
4. List the clear defects nobody filed yet: each evidence comment whose first line starts
   `Clear defect from `, with no session reply under it that starts `Filed dispatch://`,
   `Not filed:` or `Asked dispatch://` (a role that asked instead, which step 2 takes):
   `curl -sS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<REPORT KEY>/comments" | jq -c '. as $all | [.[] | select(.reply_to == null and (.body | startswith("Clear defect from "))) | .id as $id | select([$all[] | select(.reply_to == $id and .author.kind == "session" and (.body | test("^(Filed dispatch://|Not filed:|Asked dispatch://)")))] | length == 0) | {id, body}]'`.
   File each as [File a clear defect](#file-a-clear-defect) says. When one is not clear, reply
   `Not filed: <reason>` under it and open an ask for it instead.

A read that fails, and an assignment Dispatch refuses, are handled as
`skill://legion-controller/SKILL.md#rules` and
`skill://legion-controller/references/daily-report.md` say.

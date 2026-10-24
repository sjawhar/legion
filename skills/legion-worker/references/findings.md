# A finding outside your issue

Read this when you find a problem the issue you work does not cover: a defect in another
component, repository or tool, a gap in Legion itself, or a production check's `fail`. It holds for
every Legion role: each phase worker, each architect, and the controller.

## Ask a person; never file

A finding outside your issue is a `dispatch ask`, never a `dispatch issue`: a person decides
whether it needs an issue, where, and whether Legion works it. A problem inside your issue is not
such a finding: fix it in your change, since nothing needed for correctness is deferred. Search
first (`skill://dispatch-first`): when an open issue already tracks the problem, comment the
evidence on that issue and ask nothing.

Two kinds of issue are still filed: an architect's children of its own tree (`dispatch issue
--parent`, `skill://legion-architect`), and an issue a person asked the controller to file, in a
direct message, in a Slack request, or in their answer to one of these asks
(`skill://legion-controller`, "Findings, and the issues you file").

## Where it goes

Dispatch refuses an ask on a closed issue (`409 ISSUE_CLOSED`), and once an issue closes its open
asks leave the Inbox and can no longer be answered. The merge closes an issue, and a tree's close
closes its root, so an ask goes where it can still be answered. Read each issue's `phase` with the
`legion` tool's `read_record` (the controller's `read_state` shows the same):

1. The issue whose work surfaced the finding, while its phase is before `accepting`.
2. Otherwise the tree's root (`LEGION_TREE`), while the root's phase is before `accepting`.
3. Otherwise the controller's report issue, which never closes: the issue the deployment
   instructions name for Legion's reports, else the project's `Legion daily report`
   (`dispatch search --query '"Legion daily report"' --project <PROJECT>` finds it; it stays in
   `icebox`). When there is none yet, send the finding to the controller (below), and it asks
   there.

One risk is left: an issue before `accepting` can still close before the person answers (it merges
sooner than expected, or a person closes it), and its open ask then closes unanswered with it.

## What it says

The question holds at most 800 characters and stands alone, since the ask view shows no comment.
It carries:

- the problem, as its reader recognises it;
- where the fix would belong: the repository or layer, and a plain statement that it is not
  Legion's when it is not. A problem is not Legion's because you work in Legion: name the
  component that misbehaved;
- your recommendation and its reason.

The evidence (the commands, their output and its time, the file and line, the pull request) does
not fit in 800 characters. Post it first as a `dispatch comment` on the issue the ask goes on,
which prints `Posted comment <id>`, and cite it with `--ref`; evidence that issue already holds
(its spec, its pull request) is cited the same way. The options always include not filing:

```bash
dispatch comment --issue <KEY> --body-file - <<'EOF'
<the evidence: commands, output with its time, file:line, links>
EOF
dispatch ask --issue <KEY> --ref 'dispatch://<KEY>/comment/<id>' --question-file - \
  --option 'File an issue: <what it would track, and who would own it>' \
  --option 'File it for Legion: Legion works it once it is filed' \
  --option 'Do not file: <what stays as it is>' <<'EOF'
<the problem; where the fix belongs, and that it is not Legion's when it is not; your recommendation and why>
EOF
```

## Tell the controller

The person answers in Dispatch, and the answer reaches only the ask's followers. So once the ask is
open, `envoy_publish` its reference, `dispatch://<KEY>/ask/<id>` (the id from the `Asked <id> on …`
line), to the controller topic your `Legion addressing` line names, with `expects_reply: none`. The
controller follows the ask and files what the person's answer asks for; you file nothing, whatever
the answer says. The controller itself skips this step, since it follows each ask it opens.

The ask blocks nothing of your own issue: its work goes on.

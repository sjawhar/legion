# A finding outside your issue

Read this when you find a problem the issue you work does not cover: a defect in another
component, repository or tool, a gap in Legion itself, or a production check's `fail`. It holds for
every Legion role (each phase worker, each architect, the controller) and for each of their
subagents, and it replaces the filing step of `skill://dispatch-first` and `skill://dispatch`. The
controller does not send a clear defect to itself: it files it, as `skill://legion-controller`,
"Findings, and the issues you file", says.

## A clear defect goes to the controller, a decision is an ask

A finding outside your issue never becomes a `dispatch issue` you file. It takes one of two paths:

- **A defect with a clear fix:** something is broken, the evidence shows it, the fix needs no
  choice a person must make, and the fix belongs in a repository your `Legion addressing` line
  lists. Send it to the controller, as [Send a clear defect to the
  controller](#send-a-clear-defect-to-the-controller) says. The controller files it for Legion.
- **A finding that needs a decision:** whether to fix it at all, which of several fixes, who owns
  it, or a change of scope, cost or risk. Open a `dispatch ask`, as [Where it
  goes](#where-it-goes) says, with one recommendation. A person decides.

When you are not sure whether the fix is clear, it needs a decision: ask.

- A problem inside your issue is not such a finding: fix it in your change, since nothing needed
  for correctness is deferred.
- A problem inside your tree is work its root spec's acceptance needs. A phase worker's such
  problem outside its own issue goes to the tree's architect (`skill://legion-worker`, the
  escalation paragraph), which may file it as a child of the tree. Anything else, in the same
  repository or not, is a finding.
- Search first (`skill://dispatch-first`): when an open issue already tracks the problem, comment
  the evidence on that issue and send or ask nothing. When an open ask on the report issue already
  asks about it (`dispatch open-asks --project <PROJECT>` lists them), add your evidence under that
  ask (`dispatch comment --issue <REPORT KEY> --reply-to-ask '<ask id>' --body-file -`, the
  evidence on stdin) and open no second one.

These issues are filed:

- an architect's children of its own tree (`dispatch issue --parent`, `skill://legion-architect`)
- a clear defect a role sends the controller
- an issue a person asked the controller to file, in a direct message, in a Slack request, or in
  their answer to an ask
- the report issue itself, which the controller creates once when the project has none

`skill://legion-controller`, "Findings, and the issues you file" and "Daily report", says how.

## Send a clear defect to the controller

Post the evidence first as a `dispatch comment` on the report issue ([Where it goes](#where-it-goes)
says which), its first line exactly `Clear defect from <your role token>`. It prints
`Posted comment <id>`. Then send the controller one message with
`envoy_publish` to the controller topic your `Legion addressing` line names, with
`expects_reply: none`. It holds:

- your role token, from your `Legion addressing` line
- the defect, as its reader recognises it, and the issue it came from (`dispatch://<KEY>`)
- where the fix belongs: the repository and the component
- the fix
- the evidence comment, `dispatch://<REPORT KEY>/comment/<id>`

The controller files it for Legion and replies under your evidence comment with the issue it filed.
You file nothing, and you wait for nothing. When the report issue does not exist or is closed, put
the evidence in the message itself. A production check's `Production:` line then names the defect
as sent to the controller.

## Where it goes

An ask goes on the controller's report issue, always: the issue the deployment instructions name
for Legion's reports, else the project's `Legion daily report`, which
`dispatch search --query '"Legion daily report"' --project <PROJECT>` finds. It stays open in
`icebox`, so the ask stays answerable whatever happens to the issue it came from; once an issue
closes, Dispatch takes its open asks out of the Inbox and refuses their answers
(`409 ISSUE_CLOSED`). The question names the issue the finding came from by its `dispatch://`
reference.

When the project has no report issue yet, or a person closed it (`dispatch read` shows `Status:
done`, and Dispatch refuses the ask with `409 ISSUE_CLOSED`), do not create or reopen one yourself
(the controller does): send the finding to the controller as text, with `envoy_publish` to the
controller topic your `Legion addressing` line names (`expects_reply: none`), holding the problem,
the issue it came from, the evidence and the decision it needs. The controller creates or reopens
the report issue and opens the ask. A production check's `Production:` line then names the finding
as sent to the controller.

## What the ask says

The question stands alone, since the ask view shows no comment. It holds at most 800 characters,
the `Ref: <reference>` line `--ref` appends included (about 75 characters for a comment's
reference). It carries:

- the problem, as its reader recognises it, and the issue it came from (`dispatch://<KEY>`);
- where the fix would belong: the repository or layer, and a plain statement that it is not
  Legion's when it is not. A problem is not Legion's because you work in Legion: name the
  component that misbehaved;
- the decision a person must make, and your recommendation and its reason.

The evidence (the commands, their output and its time, the file and line, the pull request) does
not fit. Post it first as a `dispatch comment` on the report issue, which prints
`Posted comment <id>`, and cite that comment with `--ref`, the one reference an ask takes:

```bash
dispatch comment --issue <REPORT KEY> --body-file - <<'EOF'
<the evidence: commands, output with its time, file:line, links>
EOF
# Only when your Legion addressing line lists the repository where the fix belongs, keep the
# `File it for Legion` option; otherwise drop that line.
dispatch ask --issue <REPORT KEY> --ref 'dispatch://<REPORT KEY>/comment/<id>' --question-file - \
  --option 'File an issue: <what it would track, and who would own it>' \
  --option 'File it for Legion: Legion works it once it is filed' \
  --option 'Do not file: <what stays as it is>' <<'EOF'
<the problem and dispatch://<KEY> it came from; where the fix belongs, and that it is not Legion's when it is not; your recommendation and why>
EOF
```

Legion refuses to record an issue on a repository the project does not map, hence that condition.

## Tell the controller about the ask

The person answers in Dispatch, and the answer reaches only the ask's followers. So once the ask is
open, `envoy_publish` its reference, `dispatch://<REPORT KEY>/ask/<id>` (the id from the
`Asked <id> on …` line), to the controller topic your `Legion addressing` line names, with
`expects_reply: none`. The controller follows the ask and files what the person's answer asks for;
you file nothing, whatever the answer says. The controller itself skips this step, since it follows
each ask it opens.

The ask blocks nothing of your own issue: its work goes on.

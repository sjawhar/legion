# A finding outside your issue

Read this when you find a problem the issue you work does not cover: a defect in another
component, repository or tool, a gap in Legion itself, or a production check's `fail`. It holds for
every Legion role (each phase worker, each architect, the controller) and for each of their
subagents, and it replaces the filing step of `skill://dispatch-first` and `skill://dispatch`. The
controller fixes nothing itself: it hands a finding to the tree that owns the code, as
`skill://legion-controller`, "Findings, and the issues you file", says.

## Fix it where you are, else hand it over, and ask only for a decision

A finding outside your issue never becomes a `dispatch issue` you file. Take the first of these
three steps that fits:

1. **Fix it here.** A defect with a clear fix in code your tree touches or depends on goes into
   your tree's own pull request, while you are in that code. The fix needs no choice a person must
   make, and the evidence shows the defect. Say in your handoff and in the pull request body what
   you fixed beside the issue's own work. A fix deferred to another issue is a fix nobody makes.
2. **Hand it over.** Any other clear defect goes to the controller with its evidence, as [Send a
   finding to the controller](#send-a-finding-to-the-controller) says. The controller gives it to
   the running tree whose code it is. Only when no tree owns that code does it file a Legion root.
3. **Ask.** A finding that needs a person's authority, taste or risk appetite is a `dispatch ask`
   with one recommendation, as [Where it goes](#where-it-goes) says: whether the work is wanted at
   all, which of several designs, who owns it, or a change of scope, cost or risk.

A fix you are not sure of is not step 1: hand it over, or ask when a person must decide.

- A problem inside your issue is not such a finding: fix it in your change, since nothing needed
  for correctness is deferred.
- A problem inside your tree is work its root spec's acceptance needs. A phase worker's such
  problem outside its own issue goes to the tree's architect (`skill://legion-worker`, *Stop
  rules*), which may file it as a child of the tree. Anything else, in the same
  repository or not, is a finding.
- Search first (`skill://dispatch-first`): when an open issue already tracks the problem, comment
  the evidence on that issue and send or ask nothing. When an open ask on the report issue already
  asks about it (`dispatch open-asks --project <PROJECT>` lists them), add your evidence under that
  ask (`dispatch comment --issue <REPORT KEY> --reply-to-ask '<ask id>' --body-file -`, the
  evidence on stdin) and open no second one.

These issues are filed:

- an architect's children of its own tree (`dispatch issue --parent`, `skill://legion-architect`)
- a Legion root the controller files for a clear defect no running tree owns
- an issue a person asked the controller to file, in a direct message, in a Slack request, or in
  their answer to an ask
- the report issue itself, which the controller creates once when the project has none

`skill://legion-controller`, "Findings, and the issues you file", and
`skill://legion-controller/references/daily-report.md` say how.

## Send a finding to the controller

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

The controller hands it to the running tree whose code it is, and files a Legion root only when no
tree owns that code. Either way it replies under your evidence comment with where the work went.
You file nothing, and you wait for nothing. When the report issue is missing or closed, put the
evidence in the message itself. A production check's `Production:` line then names the defect as
sent to the controller.

### When no controller takes it

The controller topic is a live wake: a message published while no controller runs reaches nobody,
and no controller reads it later. `envoy_publish`'s own answer tells you whether one took it. A
publish to a role answers with its live `holder`. With no live controller it fails instead, with
`no holder for role legion-<project>-controller` or a holder that lapsed, and the listener delivers
nothing (`packages/envoy/cmd/listener`, the test "unheld role returns 404 without publishing").
Then make the finding an ask, as [What the ask says](#what-the-ask-says) spells it, on the first
of these that holds, so a person sees it and the controller finds it when it returns:

1. **The report issue is open:** ask there, the question opening `Clear defect from <your role
   token>` and `--ref` naming your evidence comment. Then reply `Asked dispatch://<REPORT
   KEY>/ask/<id>` under that comment, so the controller takes the ask and not the comment. The
   report issue's assignee sees the ask, and the controller's catch-up files it.
2. **The report issue is missing or closed:** ask on your own issue, the evidence posted there as
   a comment first. Then publish the ask's reference to your tree's architect (`skill://legion-worker`,
   *Stop rules*). An architect holds its own. The architect holds your issue's
   acceptance until a controller has taken the ask (`skill://legion-architect`, "Acceptance and the
   merge"). That hold is what keeps the ask readable: an open ask on a closed issue drops out of
   the controller's own listing (`asks.go`'s `listOpenAsks` takes `i.closed_at is null`), so an
   issue that closed over one hides it for good.
3. **Your own issue is closed too** (Dispatch refuses the ask `409 ISSUE_CLOSED`, as on a
   production check): put the report issue back in reach yourself, with the `dispatch` commands
   below, and ask there. Never `legion({op: "set_status"})`, which the controller's report steps
   (`skill://legion-controller/references/daily-report.md`) use: that op is the controller's
   alone, and your session is refused `set_status is not
   available to a phase-worker session` (`packages/pi-legion/src/tools.ts`'s `OPERATIONS`). No
   other issue can hold an answerable ask, so this is the one case a role writes the report issue.
   - It exists and is closed: `dispatch issue-update --issue <REPORT KEY> --status icebox`. Send
     the status by itself. A closed issue takes a reopening status alone, and refuses it beside
     an assignee or any other field (`api/issue_patch.go`, the `before.ClosedAt` branch). This is
     the one lifecycle status your own prompt lets you write. The report issue is in no tree, so
     the daemon re-asserts nothing on it: `workflow`'s `sessionStatusWrite` acts only for a
     session that claims the issue's tree, or for an admitted root.
   - A search finds none: create it as "Where it goes" names it, `dispatch issue --project
     <PROJECT> --title 'Legion daily report' --spec-file -`, with the Summary
     `skill://legion-controller/references/daily-report.md` gives, then park it with
     `dispatch issue-update --issue <NEW KEY> --status icebox`. It carries no `legion` label.
   - Then post the evidence there and ask there, as [What the ask says](#what-the-ask-says)
     spells it. Say in your summary that you reopened or created it, so the controller knows.

Name the ask in your summary, and in a production check's `Production:` line.

## Where it goes

An ask goes on the controller's report issue whenever it is open: the issue the deployment
instructions name for Legion's reports, else the project's `Legion daily report`, which
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
the report issue and opens the ask. When that publish fails because no controller holds the role,
take step 2 or 3 of [When no controller takes it](#when-no-controller-takes-it). A production
check's `Production:` line then names the finding as sent to the controller, or names the ask.

## What the ask says

The question stands alone, since the ask view shows no comment. It holds at most 800 characters,
the `Ref: <reference>` line `--ref` appends included (about 75 characters for a comment's
reference). Its first line names who asks: `Clear defect from <your role token>` for a clear
defect no controller took, else `Finding from <your role token>`. The architect's acceptance and the
controller's catch-up find a finding ask by that line. Then it carries:

- the problem, as its reader recognises it, and the issue it came from (`dispatch://<KEY>`);
- where the fix would belong: the repository or layer, and a plain statement that it is not
  Legion's when it is not. A problem is not Legion's because you work in Legion: name the
  component that misbehaved;
- the decision a person must make, and your recommendation and its reason. A clear defect no
  controller took carries the fix instead, and its options ask where the work goes, with
  `Hand it to the owning tree` first and recommended.

The evidence (the commands, their output and its time, the file and line, the pull request) does
not fit. Post it first as a `dispatch comment` on the issue the ask goes on, which prints `Posted
comment <id>`, and cite that comment with `--ref`, the one reference an ask takes. That issue,
`<ASK KEY>` below, is the report issue, except as [When no controller takes
it](#when-no-controller-takes-it) says:

```bash
dispatch comment --issue <ASK KEY> --body-file - <<'EOF'
<the evidence: commands, output with its time, file:line, links>
EOF
# Keep `File it for Legion` only when your Legion addressing line lists the repository where the
# fix belongs. For a clear defect no controller took, the first option reads
# 'Hand it to the owning tree: <the tree, when you know it>'.
dispatch ask --issue <ASK KEY> --ref 'dispatch://<ASK KEY>/comment/<id>' --question-file - \
  --option 'File an issue: <what it would track, and who would own it>' \
  --option 'File it for Legion: Legion works it once it is filed' \
  --option 'Do not file: <what stays as it is>' <<'EOF'
Finding from <your role token>
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
each ask it opens. When that publish names no holder, do nothing more for an ask on the report
issue: the controller's catch-up follows it at its next start.

An ask on the report issue blocks nothing of your own issue: its work goes on. One on your own
issue holds its acceptance, as step 2 of [When no controller takes
it](#when-no-controller-takes-it) says.

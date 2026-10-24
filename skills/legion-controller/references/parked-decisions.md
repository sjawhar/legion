# Decisions on a parked tree

A parked tree is a root handed to Legion (the `legion` label) whose Dispatch status is `backlog`
or `icebox` and whose `issues.<KEY>.phase` in `read_state` is `done`. Its architect is suspended
or gone, so nobody inside the tree hears what a person says on it. The Go daemon sends you each
such decision as `parked-decision on <KEY>`; the walk below finds one sent while no controller
ran. You decide whether the tree runs again, unless the park is not your own hold. When it
runs, its architect's start task names each decision the daemon relayed, and both spec versions
when the approved one is not the current one, so you need not tell it.

## Backlog and icebox

A park means "should not run now", and a parked root keeps its `legion` label. A parked root runs
again when a person sets it to `todo`, when a wake tells you to re-admit it (`worker-died`,
`held`), or when it is your own hold (a park you marked `Parked by Legion while it waits on:`, step
2 of **Deciding**) and a person's decision after the park gives its work a next step, as below.
Every other park, a person's, another session's or an operator's `legion status`, stays parked
until whoever parked it sets it to `todo`.

## The notice

Its payload is `{kind: "parked-decision", decision, root, ref, actor, version?, reason?}`:

- `decision`: `design-approved`, `design-changes-requested` (`reason` holds the person's words)
  or `carried-gate-opened` (an approval of the spec a carried root depends on), each with the
  spec's `version`; `ask-answered` or `ask-resolved` (a person answered or resolved a question
  asked on `<KEY>`); `ask-reply` (a person replied on an ask of `<KEY>`, giving the turn to the
  agent, or replied under a closed ask).
- `root`: the tree's root, the issue you act on. `<KEY>` is where the decision was made: the root
  or another issue of its tree.
- `ref`: the decision in Dispatch: `dispatch://<KEY>/spec`, `dispatch://<KEY>/ask/<id>` or
  `dispatch://<KEY>/comment/<id>`.
- `actor`: the person who made it, `user:<login>`.

A session's reply on an ask of a parked tree is no decision. The daemon sends no notice for it and
logs `workflow: a session's reply on a parked tree is not relayed`; the reply stays on the Dispatch
ask for the person who asked, and the walk below skips it.

## Deciding

1. **Where the tree stands.** Read `issues.<root>` with `read_state`.
   - Its `phase` is not `done`: the tree runs. When `architect.state` is `failed` or `retired`,
     nobody in the tree heard the decision: skip step 2 and judge it in steps 3 and 4. A decision
     that gives the work a next step re-admits the tree as the `worker-died` row of the
     [wake routing table](skill://legion-controller/SKILL.md#wake-routing-table) says
     (`set_status` of the root to `backlog`, then to `todo`), not to `todo` alone. Answer a
     question as step 4 says, and leave the tree as it is. In that reply and in the root's copy,
     write `<root>'s architect has stopped, so it` for `<root> is parked, so its architect`,
     `The tree waits.` for `The tree stays parked.`, and `backlog, then todo,` for `todo`.
     Otherwise the tree already heard the decision: do nothing.
   - Its `phase` is `done`: read the root's Dispatch status (`dispatch read --issue <root>`).
     `backlog` or `icebox`: go on. `done`: the tree finished; set nothing and name `ref` in your
     summary. `triage`: a person pulled it back; set nothing and name it.
2. **After the park.** Under the root's `Events:`, each `issue.created`, `issue.updated` and
   `issue.closed` line ends `status <status>`: every change to the issue writes one, a rank,
   priority, label or assignee change too, and each shows the status the issue had then. The park
   is the oldest line of the newest unbroken run of those lines that show the root's current
   status (an event of another type between two of them does not break the run). The author of
   the park line parked the tree. The park is your own hold only when that line is by `session
   legion-daemon:<PROJECT>` and, after it and at most 10 minutes after it, `Events:` show a
   `comment.created` from a session (any controller session: its id changes when the controller
   restarts or is taken over) that begins `Parked by Legion while it waits on:`. That is the mark
   you post on the root, in the same turn, when you park a tree for a hold (the `held` row of the
   [wake routing table](skill://legion-controller/SKILL.md#wake-routing-table)), and for no other
   park: a park a person asks for, or one you make at triage, gets no mark. The window pairs the
   mark with the park it was posted for: `set_status` writes the status to Dispatch before it
   returns, so the mark's line follows the park's within the turn, and a mark older than the park
   line belongs to an earlier park. Every other park is someone else's: a person's (`user
   <login>`), another session's, and one by `session legion-daemon:<PROJECT>` with no mark beside
   it. The daemon writes an operator's `legion status` and a park you made because a person asked
   under that same session, so the author alone does not show your own hold. A decision older than
   the park is no reason to run: the park is the later word. When that run reaches the oldest such
   line `Events:` shows, read the root's events through the API, which keeps more than 10: the
   `curl` of [New issue triage](skill://legion-controller/SKILL.md#new-issue-triage) step 2, whose
   events carry `type`, `actor`, `created_at` and `payload.status` or `payload.body`. When the run
   still reaches the oldest event you read, the park is that line or an older one, and you cannot
   read who parked it: judge only the events newer than it, and name in your summary each older
   decision (an approval's or a changes request's two events as one, named
   `dispatch://<KEY>/spec`) that no newer `Parked decision read:` comment has judged, as one you
   could not place. When no event you read shows a status, the park is older than every one of
   them. A decision on another issue of the tree counts when its time is newer than the root's
   park.
3. **Read once.** A comment that begins `Parked decision read:`, from any session (a controller's
   session id changes when it restarts or is taken over), newer than the decision, on the root or
   on the issue where the decision was made, has judged it: do nothing.
4. **Judge the words, not the kind.** Read the decision (`dispatch read --ref <ref>` for an ask or
   a reply; for a request for changes, the notice's `reason` or the issue's `Spec approval:` line;
   for an approval, `issues.<root>.designGate`). An approval or a request for changes is two
   events at one time, the approval request's `ask.answered` and `artifact.approved` or
   `artifact.changes_requested`: judge them as one decision, whose `ref` is `dispatch://<KEY>/spec`.
   - It gives the work a next step: an approval of the spec, a request for changes that says what
     to change, an answer that picks an option or supplies the missing fact, a reply that tells
     the agent to go on. An approval counts when `issues.<root>.designGate.approvedVersion` equals
     `issues.<root>.designGate.currentVersion`, and also when it is lower: then the architect's
     start task names both versions, and the architect requests approval of the current one.
     - It is your own hold (step 2): set the root to `todo` (`set_status`); admission queues it and
       its architect resumes. Post one comment on the issue where the decision was made
       (`dispatch comment --issue <KEY> --body-file -`): `Parked decision read: <ref> by <login>
       gives this tree its next step (<one sentence>). Legion runs it again.`
     - Any other park, or you cannot read who parked the tree: set nothing, since you move only a
       hold of your own ([Free slots](skill://legion-controller/SKILL.md#free-slots-go-daemon)).
       Post one comment there: `Parked decision read: <ref> by <login> gives this tree its next
       step (<one sentence>).`, then the sentence for that park, then `Its architect then starts
       with this decision.` For a comment that is no reply on an ask, leave out the last sentence:
       the daemon relays no such comment. The sentence for the park:
       - A person's or another session's: `<parker> parked the tree, so it stays parked until they
         set it to todo.`, with the login for a person and `session <id>` for a session.
       - One by `session legion-daemon:<PROJECT>` with no mark: `The tree was parked through Legion
         (session legion-daemon:<PROJECT>) for no hold of its own, so it stays parked until a
         person sets it to todo.`
       - One you cannot read: `Its last 10 events do not show who parked the tree, so it stays
         parked until whoever parked it sets it to todo.`
   - It only questions the work: a question, a doubt about the premise, a request for changes that
     only asks why. Leave it parked and answer the question in one reply:
     - Answer from the record when it holds the answer: the issue and its spec (`dispatch read`,
       `dispatch doc-read`), its handoffs (`.legion/<KEY>/<phase>.json` on the branch
       `legion/<KEY>`, read with `gh api` as `skill://legion-architect` reads a child's) and its
       pull request. Write `Parked decision read: <ref> by <login> asks <the question in a few
       words>.`, then the answer and where in the record it is, then `The tree stays parked. Set
       <root> to todo to run it.`
     - When the record does not answer it, write `Parked decision read: <ref> by <login> asks
       <the question in a few words>. The record does not answer it, and <root> is parked, so
       its architect cannot answer either. Set <root> to todo to bring its architect back to
       answer.`
     - When the park is not your own hold, or you cannot read who parked the tree (step 2), end
       either reply with the sentence the next-step comment above gives for that park, with
       `<root>` for `the tree`, in place of its `Set <root> to todo …` sentence. In the reply the
       record does not answer, add `Its architect then starts with this question.` after that
       sentence, except for a comment that is no reply on an ask.
     - Post the reply on the question's own ask, on the issue where the person asked it, so the
       ask hands the turn to the person. For an answer or a resolution (`ref` ends
       `/ask/<ask id>`): `dispatch comment --issue <KEY> --reply-to-ask <ask id> --turn human
       --body-file -`. For a reply (`ref` ends `/comment/<comment id>`): `dispatch comment
       --issue <KEY> --reply-to <comment id> --body-file -`. Dispatch puts that reply under the
       same ask, and a session's reply there hands the turn to the person, as `--turn human`
       does. The command takes `--turn` only with `--reply-to-ask`. A request for changes has no
       ask of its own: post the reply on the issue whose spec it reviews (`dispatch comment
       --issue <KEY> --body-file -`).
   - When `<KEY>` is another issue of the tree, post on the root too (`dispatch comment --issue
     <root> --body-file -`): the same comment for a next step, and for a question a short copy,
     `Parked decision read: <ref> by <login> asks a question, replied to on <KEY>. The tree stays
     parked.` The decision's own issue holds a comment either way: its events are where a later
     walk finds the decision, and the root's last 10 events may no longer show the root's comment
     by then. Several decisions on one tree at once get one comment that names each `ref`, on the
     root and on each such issue.

## The walk

At every start (step 3 of the start procedure) and on each `tick`:

1. List the parked roots: `dispatch issues --project <PROJECT> --status backlog --label legion
   --limit 250 --offset 0`, then the same with `--status icebox`, each paged as the start
   procedure pages triage. Keep the rows that `read_state` records under `issues` at phase `done`.
2. For each, read the root (`dispatch read --issue <root>`), then its children
   (`dispatch read --ref dispatch://<root>/children`) and each child (`dispatch read --issue
   <child>`), and each child's children the same way, down the tree.
3. A decision is an event under `Events:`, by a `user`, newer than the root's park (**Deciding**,
   step 2) and than every `Parked decision read:` comment from any session on the root or on the
   issue the event is on, of type `artifact.approved`, `artifact.changes_requested`,
   `ask.answered`, `ask.resolved` or `comment.created`. An event line shows the decision's start
   (an answer as `<question> -> <answer>`) and its author (`user <login>`), but no id. An
   `artifact.approved` or `artifact.changes_requested` event, with the approval request's
   `ask.answered` at the same time, is one decision whose `ref` is `dispatch://<KEY>/spec`, and
   the issue's `Spec approval:` line shows a request's reason. For any other ask or a comment, find
   the item: `Open asks:` names an ask still open, and `dispatch search` finds an ask or a comment
   by its words. The id at the end of a hit's link (`/asks/<id>`, `?ask=<id>`, `#b-<block id>`,
   `/comments/<id>`, `?comment=<id>`) names it. Read an ask with `dispatch read --ref
   dispatch://<KEY>/ask/<id>`, which takes a block id too and prints the ask's own id as `Ask id:`.
   That ask id goes in `ref` (`dispatch://<KEY>/ask/<ask id>`) and in `--reply-to-ask`. A
   comment's `ref` is `dispatch://<KEY>/comment/<id>`. Two limits: the walk counts every comment
   by a person, a reply on an ask or not, where the daemon's notice comes only for an ask reply;
   and `Events:` shows an issue's last 10 events, so a decision older than those, pushed out by
   later edits (a rank or priority change writes an event too), is not found here. The daemon's
   notice does not depend on that window.
4. Decide each tree's decisions as **Deciding** says, its step 1 included. An event of those types
   that is older than the park goes in your summary when **Deciding** step 2 says so.

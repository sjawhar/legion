# Triage and handover

Read this on a `triage` wake, at every start, and when a person hands you an issue. Dispatch and
`read_state`, not the wake text, decide triage.

## At every start

1. List the project's triage issues handed to Legion:
   `dispatch issues --project <PROJECT> --status triage --label legion --all`. `--all` reads every
   match in one request, so no row is missed or repeated while the list changes.
2. The rows show no parent, so `dispatch read` each one and follow `Links:` up through each
   `child_of` parent. A `child_of` under `Referenced by:` is a child of this issue, not its parent.
3. Leave a child that has an ancestor in `admission.active` or `admission.waiting`, because that
   tree's architect owns it.
4. Every other row with no `issues.<KEY>` entry in `read_state` is a root to you, a child whose
   tree is in neither `admission.active` nor `admission.waiting` included. Read who set its label
   ("Triage a root", step 2). A root a person labelled is queued by the daemon and needs nothing
   from you. Triage, as a root, each one whose label a session set.
5. A root `read_state` records and that is now in `triage` is one a person pulled back. Never
   re-admit it yourself; ask the person in your summary: "<KEY> was pulled back to triage; what do
   you want?".

## Triage a root

1. Read `read_state`, then the issue with `dispatch read`. Check that it is in this project, that
   it is a root, and whether it already has children. A child the daemon carried to a root of its
   own when its old root closed (`skill://legion-controller/references/wakes.md`) is not new work:
   leave it. Note its `Assignee:`: that person answers the tree's asks, because their Inbox opens on the
   issues they hold. Never reassign it, because who holds an issue is the people's decision, made
   from the issue header.
2. Find who set the label. `dispatch read` does not show who set a label, so list its events:
   `curl -sS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<issue>/events?limit=200"`,
   oldest first, 200 a page. When a page holds 200 events, read the next with
   `&after=<the last event's seq>` until a page holds fewer: the setter is the newest transition,
   and the daemon reads the whole log. The event that added the label set it, and its `actor` is
   who: the newest `issue.created` or `issue.updated` event whose `payload.labels` holds `legion`
   (any case) while the previous `issue.created` or `issue.updated` event held none, or the
   `issue.created` event itself when it carries the label. Events of other kinds between them do
   not count.
   - `actor.kind` is `user`: a person set it. That root is queued by the daemon (its own `todo`
     write through its outbox). Write no `todo` and post no note for it. Your work on it is done.
   - Your own filing for a person hands it over too: its spec as filed (`dispatch doc-read --issue
     <issue> --version 1`) holds a `Filed for <person> at their request: <request>` line, and its
     `actor` is this session (`dispatch whoami` names it) or an earlier one of yours. For a request
     that is an ask, an earlier session is yours when its reply under that ask (`dispatch read
     --ref`) starts `Filed dispatch://<this KEY>` after a newest answer of `File it for Legion`.
   - Your own filing for a clear defect no tree owns hands it over too: its spec as filed holds a
     `Filed for Legion from <role> on <issue>: <evidence>` line, and its `actor` is a session of
     yours. An earlier session is yours when its reply under that evidence (a comment or an ask)
     starts `Filed dispatch://<this KEY>`.
   - Any other actor hands over nothing. Leave the issue in `triage` and ask the person on your
     report issue whether Legion should take it, naming the issue and who labelled it, with the
     options `Admit it` and `Leave it`. On `Admit it`, go on to step 3. Either way, reply under the
     ask `Admitted dispatch://<KEY>` or `Not admitted: <reason>`, because the start procedure's
     catch-up reads that reply as done.
3. Admit it (`set_status` to `todo`) only when you labelled it at a person's request, you filed it
   yourself (for a person, or for a clear defect no tree owns), or a person answered `Admit it`,
   unless the request, the weekly plan or the deployment instructions say to wait or put it out of
   scope:
   `legion({ op: "set_status", issue: "<issue>", status: "todo" })`.
4. When one of those says to wait, park it with `set_status` to `backlog`, or `icebox` when the
   request asks for a longer deferral, instead of leaving it in `triage`; the label stays. Never
   park a root for lack of a free slot, because in `todo` the daemon's waiting line already holds
   it until a slot frees.
5. For a root you admitted or parked, comment on the issue (`dispatch comment`) what you decided
   and why. For a root you admitted, say in that comment who will be asked: `Assigned to <login>,
   who will get this tree's questions and its design approval`, or, when `Assignee:` says
   `unassigned`, `Unassigned — nobody's Inbox shows this tree's questions or its design approval
   until someone takes it from the issue header (Assignee, beside Priority)`. Promise the design
   approval only when your system prompt's `Design gate policy:` line says
   `gates.design: root-issues`; under `gates.design: off` nobody approves a design, so drop "and
   its design approval" (and "or its design approval"). Take the assignee from the read, never
   from how the issue was created, because an issue filed with no assignee still lands on the
   project's default assignee where a person set one. An unassigned root still runs, but its
   architect's asks reach only the Inbox's `Unassigned` filter.

## Label an issue for a person

Keep the labels the issue has, which `Labels:` lists (`none` is no labels), because the `--label`
flags replace the whole set. Pass each label single-quoted, exactly as `Labels:` shows it:

```text
dispatch issue-update --issue <KEY> --label '<each current label>' --label legion
```

A label holding a `'` or a line break cannot be quoted and never goes in a command: tell the person
(`<KEY>: label <label> cannot be quoted`), so they hand it over from the issue header.

Then: in `todo` the label admits it. The label you set is a session's label, so the daemon queues
nothing for it: in `triage` its `triage` wake brings the root back to you at step 3. Admit it in
the same turn, from step 3 above, because the person's request is its handover. In any other
status, `set_status` it to `todo` only when the person asked Legion to start it now.

Taking the label off a waiting root drops it from the waiting line; taking it off an admitted tree
does not stop it. A child needs no label: it runs under its tree's architect once its root is
admitted. A child outside a live tree is admitted as a root of its own and needs the label.

# Daily report

Read this on your first turn of each UTC day, whatever woke you, and when you open, follow or
answer a finding ask: the report issue holds them all.

## Find the report issue

Use the issue the deployment instructions name for Legion's reports. With none named, use the
project's `Legion daily report` issue:
`dispatch search --query '"Legion daily report"' --project <PROJECT>`. When it does not exist,
create it once (the one issue you file unasked) and park it in `icebox`, so nobody takes it as
work. It never carries the `legion` label. Assign it to the person who answers for the project:
the one the deployment instructions name, else the owner `dispatch whoami` names, because its asks
reach that person's Inbox.

```text
dispatch issue --project <PROJECT> --title 'Legion daily report' --assignee '<email>' --spec-file - <<'EOF'
## Summary

Legion's controller posts one message here each day: what Legion finished, what it closed without a change and why, and what is running. Legion's agents also ask here whether a problem they found outside their own work needs an issue. This issue is not work, so it carries no `legion` label and stays in icebox.
EOF
legion({ op: "set_status", issue: "<report KEY>", status: "icebox" })
```

With no person known, leave out `--assignee`: the issue then goes to your token's owner, or, under
the shared token, to the project's default assignee when a person set one, and is otherwise
unassigned.

## Keep it answerable

Each time you read it:

- `Status: done` means someone closed it, and Dispatch then refuses an ask or a message there
  (`409 ISSUE_CLOSED`) and hides its open asks. Reopen it with `set_status` to `icebox` and say so
  in the day's report.
- `Assignee: unassigned` with a person known: assign it, naming your session (`dispatch whoami`):
  `curl -sS -X PATCH -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" -H 'Content-Type: application/json' -d '{"assignee": "<email>", "actor": {"kind": "session", "id": "<session id>"}}' "$DISPATCH_URL/api/v1/issues/<REPORT KEY>"`.
  An answer that does not print the issue with its new `assignee` left the assignee as it was
  (`400 ASSIGNEE_NOT_ALLOWED` names an email nobody has signed in to Dispatch with): name it, with
  its first line, in your summary.
- While `Assignee:` says `unassigned`, open each report with one sentence asking a person to take
  the issue from its header, because its asks reach only the Inbox's `Unassigned` filter.

## When

After the turn's own work, read the report issue with `dispatch read`, keep it answerable, and
post when its `Events:` show no `message.created` from today.

## What

One `dispatch message --issue <report KEY> --body-file -` of at most 2,000 characters, because a
person reads every report in their Inbox. Write it as `skill://dispatch`'s "Writing for the human" says:
every issue by its key and title, every pull request by its URL.

- **Finished.** `dispatch issues --project <PROJECT> --status done --updated-since <the previous
  report's time, or 24 hours ago> --limit 250`, every page, keeping the issues `read_state` records
  under `issues` whose `issue.closed` event is after the previous report. `dispatch read` each: the
  pull request under `External links:` is the one that merged.
- **Closed without a change.** Those with no pull request, each with the reason its closing
  message gave (the `message.created` just before `issue.closed` among `Events:`).
- **Running.** Each root in `admission.active` with its `issues.<KEY>.phase`, and the roots in
  `admission.waiting`. Name as waiting on a claim each root that this turn's "Trees waiting on a
  root claim" check found, with the holder its `Claimed by:` line names.
- **Waiting on Dispatch links.** Each root and child in `admission.blocked`, with the issues it
  waits on. The wait ends when each blocker reaches `done` (a person's close, or a child's own
  merge) or its link is taken off (by a person or the tree's architect), so take nothing for them.
- **Free slots.** Counted as the skill's "Free slots" says; each stays free until a root in `todo`
  with the `legion` label takes it.
- **Open capabilities.** Each row of `read_state`'s `capabilities` whose `status` is `open`: its
  name, its `detail`, and a deployment row's `configLine`. Say who acts on it, as
  `skill://legion-controller/references/wakes.md` says for `tick`. With none open, say nothing of
  them.

A day with nothing finished says so in one sentence. When the lists do not fit, give each list's
count and name its issues of `--priority 0` and `--priority 1`, in board rank order.

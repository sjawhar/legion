# Asks after they open: who answers, editing, resolving, replying, and following

Read this once an ask is open. Whether to ask, and how to write it, is "Before you ask" in `skill://dispatch`.

## Who answers an ask

- An ask goes to the issue's assignee's Inbox: the `Assignee:` that `dispatch read` prints decides whose, not the project's default. On an unassigned issue it is in nobody's default Inbox or `Needs you` count. A project's default assignee fills in only when an issue is created with nobody named and nothing earlier in the chain below names someone; an issue created with an explicit nobody, set to nobody later, or created before the default was set stays unassigned.
- `dispatch whoami` prints whom you act for: your token owner's email, or no owner (a service token shows as `<namespace>/<name>`, as your writes do). An issue you create goes to your owner, else inherits its parent's assignee, else goes to the project's default assignee when a person set one on the Settings page, else stays unassigned.
- On an unassigned issue where the answer matters, assign it to your owner, or name in the question who should answer. Assign with `PATCH /api/v1/issues/{key}` and `{"assignee": "<email>"}`. An email nobody signed in with gets `ASSIGNEE_NOT_ALLOWED`.
- Never reassign an issue a human holds to get a faster answer. That is the human's call.

## Handing a human a to-do, editing, resolving, and replying

- A to-do follows the question-and-options contract of "Writing for the human". For a human-only action, what it changes and risks constrains the answer. No label is special. If an outcome needs a reason, say so in its description.

```bash
dispatch ask --issue DSP-42 \
  --question 'The fix is merged but cannot run in production: the signing certificate it needs expired, and only its owner can renew it in the registrar console. Renewing replaces the certificate every client checks. How should we proceed? Recommendation: renew it now because the fix cannot run without it.' \
  --option 'Renew the certificate: Lets the fix run but replaces the certificate every client checks.' \
  --option 'Leave it expired: Changes nothing now but leaves the fix unable to run.'
```

- Correct an open ask in place with `dispatch edit-ask`, not a second ask, while the same decision stays open. The human then reconfirms. You cannot edit an answered or resolved ask. If the decision of an open ask changed, retract it and open a new one. If the decision of an answered ask changed, open a new one.
- `edit-ask` on a block ask writes the block too, so it mints a version and closes a spec's design gate until approved. Change anything else about the block with `dispatch doc-edit`. A retry with unchanged fields is safe.
- A human can change an answer. A new `ask.answered` carries `previous_answer`. Act on the newest.
- When the selected option and the typed text disagree, the text is the answer. Act on it. To clarify, open a new ask.
- Retract a moot or superseded ask, or resolve one whose answer you found:

```bash
dispatch resolve-ask --ask <id> --kind retracted --reason 'A newer ask supersedes this question.'
```

- Use `retracted` for obsolete, `resolved` for found. Always give a reason: the question stays in its card. Resolution never records a human decision. An answered ask cannot be resolved.
- `doc-edit` refuses to delete an open ask's block. A block deleted another way (an upload, the browser) closes its ask. Restoring it reopens the ask.
- Closing an issue withdraws its open asks, whoever's turn it is. Each is resolved with kind `withdrawn`, naming whoever closed the issue. Only Dispatch writes `withdrawn`: `resolve-ask` takes `retracted` or `resolved`. A reopen brings back exactly the asks the close withdrew. An ask on a closed issue cannot be answered, resolved or retracted (`409 ISSUE_CLOSED`): reopen the issue first.
- Reply under an ask with `dispatch comment --reply-to-ask <id>`. Close your review comment with `dispatch resolve-comment` ([Documents](skill://dispatch/references/documents.md#comments-and-suggestions)).
- A human reply while the ask is open (`ask_state: open`) is a request for clarification, not an answer. Answer in the thread with `--reply-to-ask`, or reword with `edit-ask`. Do not open a second ask.
- Each reply sets whose turn it is. Your default reply (`--turn human`) puts the ask in the human's `Waiting on you`. For any progress or working-on-it note, use `--turn agent`. A human reply hands the turn to you. Reads show it as `waiting_on`.

## Following

- You follow an ask you opened or replied to, or one a human added you to. Its answer, edits, resolution and replies reach your agent topic directly.
- Leave or rejoin: `dispatch follow --ask <id> --action unfollow` (or `follow`). `--ask` takes a full id, a `dispatch://KEY/ask/<id>`, or an 8+ hex prefix unique among your open asks. Removal arrives as `ask.follower_removed`, an add as `ask.follower_added`.
- No write subscribes you to an owner. Replies to your comments and new comments on your documents also reach you. For everything else, subscribe: `envoy_subscribe notifications.dispatch.issue.<KEY>.>` or `envoy_subscribe notifications.dispatch.document.<PROJECT>.<SLUG>.>`. The topic carries every event. `notify` controls only wake and routed delivery. A human can unsubscribe you (`subscription.removed`).

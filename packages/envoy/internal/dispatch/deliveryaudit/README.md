# Comment delivery audit

Two read-only psql scripts that answer whether a person's Dispatch comments reached the agent
sessions they were owed to (INFRA-596):

- `comments.sql` lists every comment a person (an author of kind `user`) created in a time window,
  ask-thread replies included, with each agent session or role the comment was owed or was
  addressed to: one row per (comment, target).
- `trace.sql` shows everything Dispatch recorded about one comment's delivery.

Neither script writes. Run both in a read-only transaction anyway, so a mistake cannot write. The
package's test (`deliveryaudit_test.go`) runs the same embedded bytes through pgx, binding each psql
variable as a parameter (`Bind`), over records the real API handlers and outbox wrote.

## Running

```bash
PGOPTIONS='-c default_transaction_read_only=on' psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -v from=2026-10-01T00:00:00Z -v to=2026-11-01T00:00:00Z \
  -f comments.sql > comments.csv

PGOPTIONS='-c default_transaction_read_only=on' psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -v comment=<comment uuid> \
  -f trace.sql > trace.csv
```

`from` and `to` are timestamps with a time zone. The window is half-open: a comment created at
`from` is in it, and one created at `to` is not. Every timestamp the scripts print is UTC, in the
form `2026-10-27T04:59:37.551758Z`.

`trace.sql` runs five selects. psql prints each one's header and rows in turn, and the first column
of each, `section`, names the result: `transaction_events`, `comment_mentions`,
`comment_deliveries`, `issue_state` and `thread`.

## What a comment is owed

A comment's targets are the union of two sets:

- **Owed.** These are the targets the comment should have reached:
  - The issue's route, when the issue has one at the time. A comment on an issue's document counts
    the document's issue.
  - Otherwise, the issue's claimant, when the claim belongs to a session. A person's claim, or no
    claim, adds nothing.
  - For a reply on an ask's thread: the ask's asker, and every session that followed the ask at
    the time. Followers are rebuilt from the records that change them, because `ask_followers`
    holds only the list as it is now. The asker follows from the ask's creation. A session
    follows from each reply it posts on the ask. `ask.follower_added` adds a follower and
    `ask.follower_removed` removes one.
  - Every session a mention resolved to (`comment_mentions.resolved_session_id`).
- **Addressed.** These are the targets the comment did reach: every `notifications.agent.*` or
  `notifications.role.*` subject in the `published_destinations` of the comment's
  `comment.created` event.

"At the time" means when the outbox published the event: its `published_at`, or its `created_at`
while it is unpublished. The outbox reads the route and the followers at that moment. The route and
the claim are each read from the latest `issue.*` event at or before that moment whose payload
carries them. `issue.created` and `issue.updated` carry both. `issue.claimed` and `issue.released`
carry only the claim. The `issues` row is used only when no such event exists.

A route maps to a subject as the outbox's `publishRoute` maps it: `role:<name>` becomes
`notifications.role.<name>`, and `session:<id>` becomes `notifications.agent.<id>`. A session
becomes `notifications.agent.<id>`.

## `comments.csv` columns

The columns, in this order, are a contract that other scripts consume.

| Column | Meaning |
| --- | --- |
| `comment_id` | The comment's id. |
| `event_id` | The id of the comment's `comment.created` event. Empty when the comment has none. |
| `owner` | What the comment is on: the issue key, or `<project>/<artifact slug>` for a comment on a project document. |
| `created_at` | When the comment was created. |
| `published_at` | When the outbox published the comment's event. Empty while it is unpublished. |
| `author` | The person, as the comment stores them (`author.id`, the login). |
| `shape` | `ask_thread`: a reply on an ask's thread. `reply`: a reply in a comment thread. `anchored`: a new comment on a passage of a document. `conversation`: any other new comment. |
| `reopened` | `true` when the comment's own transaction reopened its resolved thread, that is, when it also wrote `comment.reopened` for the thread's root. That event has the same owner and the same `created_at`, and it comes one sequence number before. Otherwise `false`. |
| `target` | A subject the comment was owed or addressed to, such as `notifications.agent.<session>` or `notifications.role.<role>`. Empty on the one row of a comment that has no target. |
| `target_kind` | `session` for a `notifications.agent.*` target, `role` for a `notifications.role.*` target. Empty when `target` is empty. |
| `owed_reasons` | Why the comment was owed to the target: `route`, `claimant`, `follower`, `asker` and `mention`, sorted and joined with `;`. `addressed-only` when the comment reached the target without being owed to it. Empty when `target` is empty. |
| `addressed` | `true` when the target is in the event's `published_destinations`, that is, when the outbox sent the comment to it. Otherwise `false`. |
| `mention_state` | When the target is a session that a mention of the comment resolved to: the state of that mention's newest delivery attempt (`pending`, `sent` or `failed`). Otherwise empty. |
| `mention_error` | That attempt's error, or its resolve error when it has no error. Otherwise empty. |
| `route_used` | The issue's route at the time, as Dispatch stores it (`role:<name>` or `session:<id>`). Empty when the issue had no route, or when the comment is on no issue. |
| `claim_used` | The issue's claimant at the time, as `<kind>:<id>`, such as `session:<id>` or `user:<login>`. Empty when the issue had no claim. |
| `route_source` | Where `route_used` and `claim_used` were read. When both came from one `issue.*` event, this is that event's sequence number. When they came from different events, it is `<route seq>;claim=<claim seq>`. Either part is `issues-row` when no event carried that value and the `issues` row was read instead. Empty when the comment is on no issue. |

A comment with no target has exactly one row, with `target` empty. Rows are ordered by
`created_at`, then `comment_id`, then `target`.

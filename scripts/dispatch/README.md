# Dispatch census scripts

## Ask census

`ask-census.ts` reproduces the question and approval-request counting used for LEGION-470. It
reads every issue updated since the window opened and each issue's asks, then every page of the
events of each issue that carries an approval ask, whenever that ask was opened. Eight issues are
read at a time, each through its own Dispatch client, so each issue's reads get the client's
60-second deadline (`DISPATCH_TOOL_DEADLINE_MS`) to themselves and a run may take as long as it
needs.

```bash
bun scripts/dispatch/ask-census.ts \
  --from 2026-10-01T03:40:00Z \
  --to 2026-10-02T02:10:00Z \
  --project ACME \
  --project LEGION
```

`--project` is required and repeats: name each Dispatch project the census reads.

The script resolves Dispatch as every command does, with `sessionDispatchSettings`
(`packages/envoy-client/src/session-settings.ts`) over the envoy.json harness `command` reads: the
file `ENVOY_CONFIG_FILE` names, else the omp profile's, `CLAUDE_CONFIG_DIR`'s (only under
`CLAUDE_CODE_CHILD_SESSION=1`, as in a Claude Code shell) or `OPENCODE_CONFIG_DIR`'s, else
`~/.config/legion/envoy.json`: the URL from `DISPATCH_URL`, else
`dispatch.serverUrl` with `dispatch.enabled`; the token from `DISPATCH_TOKEN_FILE`, else
`DISPATCH_TOKEN`, else `dispatch.token`. A checkout's `.opencode/envoy.json`, which is not read, is
named on stderr with its rename while the checkout has no `.envoy/envoy.json`. With Dispatch off it
stops with `Dispatch is not configured: <notice>`, the notice saying which file and why, or
`dispatch config: <error>`. It prints total asks, decision
blocks, standalone question asks, approval requests, per-issue counts, approval rounds, every
standalone question with a blank `code` column, and a per-session table.

A session still running an old plugin can be removed from the measurement with a repeated
`--exclude-session <id>`. The summary prints how many asks were excluded.

Classify each standalone question in a CSV passed with `--codes`:

```text
# ask-id,code
4d0b86f4-6a98-4199-b91d-45830f6a1d1b,design
34edccbb-4b4e-4e7d-9d81-2fa7d8e4198b,to-do
```

The allowed codes are `to-do`, `design`, `may-I-proceed`, and `operations`. A `to-do` needs an
action or permission only a human can complete. `design` is a decision about the issue document.
`may-I-proceed` asks permission for work already requested. `operations` is a people or operations
decision with no design document. The script reports uncoded standalone questions separately.

For every document with approval activity in the window, **Approval rounds** shows Inbox rows,
arrivals, and human turns. An arrival is the request reaching the human's Inbox: each `ask.opened`,
and each `ask.handed_back`. An `ask.edited` only rewords a request (a move to a new version, a new
summary) and never arrives; a hand-back with a new summary is an `ask.edited` followed by its
`ask.handed_back`, one arrival. Before #1671, when an approval request began following its
document's versions, a request made again opened a new row, so there each arrival is an
`ask.opened`. A human turn is an answer to the request or a reply in its thread,
and, from the round's first request on, an answer or reply on a decision block in the document
the request names: a choice the human raises while the request is open becomes such a block, and
the hand-back after its answer responds to that turn. A round with more arrivals than human turns
plus one is flagged. `scripts/e2e/lib/design-gate-approval-requests.jq` reads the same arrivals
for the stage 4b proof.

## Comment delivery

`comment-delivery/` answers which comments people wrote in Dispatch reached no agent session, and
why (INFRA-596). Three queries give three files, and `report.ts` turns the files into one markdown
document. The run is read-only. Run it once, on a dev box, which reaches both Dispatch's database
and the transcript store.

### The run

Use one window for every query. `TO` is the run's start minus 30 minutes: the store loads new
lines every 10 minutes. Both times are UTC, in the form `2026-10-01T00:00:00Z`.

```bash
FROM=2026-10-01T00:00:00Z
TO=2026-10-27T12:00:00Z

# 1. Dispatch: every person's comment in the window, one row per target (deliveryaudit/README.md).
PGOPTIONS='-c default_transaction_read_only=on' psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -v from="$FROM" -v to="$TO" \
  -f packages/envoy/internal/dispatch/deliveryaudit/comments.sql > comments.csv

# 2. The transcript store: each query has the literal placeholders {from} and {to}.
for query in received sessions; do
  sed -e "s/{from}/$FROM/g" -e "s/{to}/$TO/g" "scripts/dispatch/comment-delivery/$query.sql" \
    | tq sql > "$query.tsv"
done

# 3. Optional: Dispatch's whole record of one comment, once for each comment to show.
PGOPTIONS='-c default_transaction_read_only=on' psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -v comment=03207432-749f-4211-9607-e378dc636807 \
  -f packages/envoy/internal/dispatch/deliveryaudit/trace.sql > trace-03207432.csv

# 4. The document.
bun scripts/dispatch/comment-delivery/report.ts \
  --comments comments.csv --received received.tsv --sessions sessions.tsv \
  --trace trace-03207432.csv > human-comment-delivery-2026-10.md
```

`tq sql` reads the query from stdin and writes `TSVWithNames`. `tq` takes no query parameters, so
`sed` puts the times in. A placeholder left in a query is an error, not an empty window.
`--trace` repeats. The document holds each trace file whole, under the file's name, in the order
given. The same files give the same document, byte for byte.

- `received.sql`: every Envoy delivery of a person's comment that a session's transcript holds,
  from `{from}` to `{to}` by the row's time. Columns: `session_id` (the base id, the part before
  `@` of a diverged copy `<id>@<host>:<path hash>`), `host`, `ts`, `raw_file`, `raw_line`,
  `envelope_id`, `event_id` (the Dispatch event id when the envelope id is `dispatch-<id>`, else
  empty) and `comment_id` (empty when the delivery names none). Identical rows, which the store
  keeps when it loaded a batch twice, are one row.
- `sessions.sql`: every session the store holds that started before `{to}`, one row per base id:
  `session_id`, `host` (each host, joined with `,`), `started_at`, `ended_at` (empty while the
  file is still being written) and `parent_session` (the session it was forked from; parents
  joined with `;`). It reads no `{from}`: a session that ended before the window is still a
  session the store holds, so a comment sent to it is `not-running`, not `unknown`. The diverged
  copies of one session file are that one session, so they merge into one row: the earliest
  start, the latest end. A subagent's `parent_session` names the session that started it, which
  is another agent with its own Envoy identity, so a subagent gets no parent.

### What `received.sql` matches

omp stores each Envoy delivery as a `messages` row with `kind = 'system'` and
`role = 'custom:envoy-message'`. Its text is the envelope that `renderInbound`
(`packages/envoy-client/src/delivery.ts`) encodes as TOON. The query keeps a row whose
`dispatch:` block has `type: comment.created` and an `actor:` with `kind: user`, and reads the
envelope's `id:` and the `id:` of the block's `payload:`. It matches each form a session received
in October:

| Form | From | What identifies the comment |
| --- | --- | --- |
| The outbox's frame, with the comment as the payload, on the session's own subject, on an issue's topic, or forwarded from a role (the forward keeps the event id, `cmd/listener/delivery.go`) | d480211bd (2026-09-09), unchanged through f15cdd471 (2026-10-09), the last change to `delivery.ts` | `id: dispatch-<event id>` and the payload's `id:` |
| The same, before f15cdd471: `reply_with` is `tool: dispatch_comment` with `args:`, not `command: dispatch comment ...` | d480211bd to f15cdd471 | the same |
| The same, before c8d44d66e (2026-10-01T05:10Z): `to: you (01a1…)` shows the session id cut short | d480211bd to c8d44d66e | the same |
| The same, with the `Pictures:` lines after a blank line | 18b304577 (2026-10-05) on | the same |
| A reply on an ask: the block has `question:`, `reply:`, `state:` and `waiting_on:` (`dispatchCompact`), and no comment id | a975f0594 (2026-09-21) on | `id: dispatch-<event id>` only |
| An event that failed `DispatchEventSchema`: the block holds the raw event, with the payload at the same level | d480211bd on | `id: dispatch-<event id>` and the payload's `id:` |
| A mention's delivery (`api/comment_delivery.go`): the frame `{event, delivery}`, sent through the listener's `/v1/messages/send`, which gives the envelope an id of its own | c8ee6dfe2 (2026-09-17) on | the payload's `id:`; for a reply on an ask, only the envelope id, which `report.ts` matches to `mention_envelope_id` in `comments.csv` |

Deployed plugins lag these commits, so a form can appear after the commit that replaced it.

### Classes

`report.ts` puts each comment in one class. It checks the classes in this order and uses the first
that applies:

1. **Delivered.** A session's transcript has the comment: a row in `received.tsv` names the
   comment's id, or its event's id, or (when it names neither) has as its `envelope_id` the
   `mention_envelope_id` of a row of the comment. Any session counts, also one that only follows
   the issue.
2. **Delivered, `btw-sent`.** No session's transcript has the comment, and Dispatch sent a mention
   of it as a BTW: the row's `mention_state` is `sent` and its `mention_delivery` is `btw`. A BTW
   runs as a side turn, which adds nothing to the transcript
   (`packages/pi-envoy/extensions/envoy.ts`), so this class is Dispatch's record of the delivery,
   not a transcript row. The document labels such a comment and such a target
   `delivered (btw-sent)`, and counts them as delivered.
3. **Not delivered, `never-published`.** Dispatch's outbox never published the event.
4. **Not delivered, `no-target`.** No session was owed the comment, and Dispatch sent it to no
   session or role.
5. **Unknown.** A target is a role, or a session that is not in `sessions.tsv`. The document names
   each such target. The transcript store holds only the Oh My Pi sessions of the two dev boxes. It
   does not hold Claude Code sessions or the sessions of Legion pods, and a role names no session.
   So the report cannot tell whether such a target got the comment.
6. **Not delivered, with one cause.** The first that applies to a target, in this order:
   `not-addressed:<rule>` (Dispatch did not send it to a session it was owed to; the rule is why it
   was owed: claimant, route, follower, asker, mention, joined with `+`; a mention counts as sent
   when its delivery's state is `sent`), `mention-failed:<error>` (`session-not-live`,
   `receipt-timeout` or `other`), `not-running` (Dispatch sent it, and no copy, fork or resume of
   the session was running when Dispatch published it), `running-no-row` (Dispatch sent it to a
   running session, and the session's transcript does not have it).

A session counts as itself, each session forked from it, and each session it was forked from,
through any chain of `parent_session`. The document also judges each (comment, target) row alone,
with the same classes, and gives the counts at both levels with the identity
`total = delivered + not delivered by cause + unknown`. `CAUSES` in `report.ts` maps each cause to
its fix.

The document lists apart the receipts that link to no listed comment: a receipt that names a
comment or an event that `comments.csv` does not list, and a receipt that names neither and whose
envelope id is no `mention_envelope_id` in `comments.csv`. `comments.csv` gives only the newest
attempt of each mention, so a delivery by an earlier attempt is in the second list.

The report refuses its input (exit 1, each problem on stderr, nothing on stdout) when a header is
not the expected columns, a row repeats (the same `comment_id` and `target`; the same
`session_id`, `raw_file` and `raw_line`; the same `session_id` in `sessions.tsv`), the rows of one
comment disagree, a row's `mention_delivery` is not empty, `btw`, `aside` or `steer`, a row has
a `mention_state` without a `mention_delivery` or the other way, two comments name one
`mention_envelope_id`, a receipt names a comment and an event that comments.csv does not pair, a
comment falls in no class or in two, an unknown comment names no unheld target, a not-delivered
comment has no cause, a cause has no fix, or a count identity does not add up.

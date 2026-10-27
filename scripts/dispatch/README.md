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

`comment-delivery/report.ts` answers which comments people wrote in Dispatch reached no agent
session, and why (INFRA-596). The run has three steps. Every step only reads.

1. `comments.sql` reads Dispatch's database and writes `comments.csv`: every comment that a person
   wrote in the window, one row for each session or role that the comment was owed or sent to (a
   pair). `trace.sql` writes Dispatch's whole record of one comment. Both are in
   `packages/envoy/internal/dispatch/deliveryaudit/`. Its `README.md` gives their columns.
2. `tq receipts` (INFRA-608, in theorem-labs/agent-infra `transcript-store/`) reads
   `comments.csv` and the transcript store. It writes `receipts.tsv`, one status for each pair,
   and `receipts-counts.txt`, its counts. It owns the transcript rules: which transcript row is a
   receipt of a comment, which sessions count as the pair's session, and each pair's status.
3. `report.ts` reads the three files and writes one markdown document.

INFRA-608's `transcript-store/comment-delivery/run.sh` does steps 1 and 2 on a dev box, which
reaches both Dispatch's database and the transcript store.

### The run

Use one window for both steps. INFRA-608's run chooses `TO`. Both times are UTC, in the form
`2026-10-01T00:00:00Z`.

```bash
FROM=2026-10-01T00:00:00Z
TO=2026-10-27T05:00:00Z

# 1. Dispatch: every person's comment in the window, one row per pair.
PGOPTIONS='-c default_transaction_read_only=on' psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -v from="$FROM" -v to="$TO" \
  -f packages/envoy/internal/dispatch/deliveryaudit/comments.sql > comments.csv

# Optional: Dispatch's whole record of one comment, once for each comment to show.
PGOPTIONS='-c default_transaction_read_only=on' psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -v comment=03207432-749f-4211-9607-e378dc636807 \
  -f packages/envoy/internal/dispatch/deliveryaudit/trace.sql > trace-03207432.csv

# 2. The transcript store (INFRA-608): receipts.tsv on stdout, the counts on stderr.
tq receipts --comments comments.csv --from "$FROM" > receipts.tsv 2> receipts-counts.txt

# 3. The document.
bun scripts/dispatch/comment-delivery/report.ts \
  --comments comments.csv --receipts receipts.tsv --counts receipts-counts.txt \
  --trace trace-03207432.csv > human-comment-delivery-2026-10.md
```

`--trace` can be given more than one time. The document holds each trace file whole, under the
file's name, in the order given. The same files give the same document, byte for byte.

`report.ts` reads `receipts-counts.txt` as exactly these 15 lines, in this order. Each line is a
key, a tab and a value, with no header. `from` and `to` are UTC times in the form
`2026-10-01T00:00:00Z`. Every other value is a plain integer. The file ends with a newline.

```text
from	<UTC time>
to	<UTC time>
listed_pairs	<N>
received	<N>
not_received	<N>
unknown	<N>
unknown_session_not_in_store	<N>
unknown_role	<N>
unknown_btw	<N>
unlisted_rows	<N>
comments	<N>
comments_with_receipt	<N>
comments_without_receipt	<N>
empty_target_pairs	<N>
rows_without_id	<N>
```

The document gives `from` and `to` as its window. `report.ts` counts each number again from
`comments.csv` and `receipts.tsv`, and it checks these rules:

- `listed_pairs` = `received` + `not_received` + `unknown`, the listed rows of `receipts.tsv`.
- `unknown` = `unknown_session_not_in_store` + `unknown_role` + `unknown_btw`.
- `unlisted_rows` is the number of rows of `receipts.tsv` with `listed` `false`.
- `comments` = `comments_with_receipt` + `comments_without_receipt`: the distinct `comment_id`s
  of `comments.csv`, also those of a comment with no target. A comment "with receipt" has a row
  in `receipts.tsv`, listed or unlisted, with the status `received`. Its evidence can be
  `transcript` or `btw_reply:<id>`.
- `empty_target_pairs` is the number of rows of `comments.csv` with no target. Such a row has no
  row in `receipts.tsv`, so `listed_pairs` = the rows of `comments.csv` - `empty_target_pairs`.
- `rows_without_id` is 0. It counts the rows of a person's comment in the transcripts from which
  `tq receipts` read no id. When it is not 0, `receipts.tsv` can miss receipts.

`receipts.tsv` has its rows sorted by `comment_id`, then `listed` (`true` first), then `target`.
`listed` is `true` or `false`. A time is in the form `2026-10-20T04:00:01.000Z`, or empty. An
unlisted row has the target `session:<base session id>`: the id of the session that its forks
come from. Its `checked_session` can be empty.

### Classes

A pair is judged from its status in `receipts.tsv`:

- `received`: delivered. The evidence is `transcript` (a transcript has the comment), or
  `btw_reply:<id>` (the session's BTW side turn posted that reply to the comment).
- `unknown`: unknown, with the reason that `tq receipts` gives.
- `not_received`: not delivered, with the first cause that applies, in this order:
  1. `never-published`: Dispatch's outbox never published the comment's event.
  2. `not-addressed:<rule>`: the pair was owed the comment, the outbox did not send it to the
     pair (`addressed` is `false`), and no delivery of a mention to the pair was sent or failed.
     The rule says why the pair was owed it: `claimant`, `route`, `follower`, `asker` or
     `mention`, joined with `+`.
  3. `mention-failed:<error>`: the newest attempt of a mention to the pair failed. The error is
     `session-not-live`, `receipt-timeout` or `other`.
  4. `not-running`: the comment's `published_at` is not between the session's
     `target_started_at` and `target_ended_at`. An empty `target_ended_at` means that the session
     still runs.
  5. `running-no-row`: any other `not_received` pair. Dispatch sent the comment to a running
     session, and the session's transcript does not have it.

`report.ts` puts each comment in one class. It checks the classes in this order and uses the
first that applies:

1. **Delivered.** A row of the comment in `receipts.tsv` has the status `received`. The row can be
   a pair or an unlisted row. An unlisted row is a session that took the comment in and is not a
   pair's session or a session forked from one.
2. **Not delivered, `never-published`.** Dispatch's outbox never published the event.
3. **Not delivered, `no-target`.** No session was owed the comment, and Dispatch sent it to no
   session or role.
4. **Unknown.** A pair of the comment is unknown. The document names each such pair and its
   reason.
5. **Not delivered, with one cause.** The first cause of its pairs, in the order above.

### What unknown means

An unknown pair is one that the report cannot judge. The transcript store cannot show whether the
pair got the comment. The reason is one of these:

- `session_not_in_store`: the store does not hold the session. The store holds only the Oh My Pi
  sessions of the two dev boxes. It does not hold Claude Code sessions or the sessions of Legion
  pods.
- `role`: the pair is a role, and no mention of the role resolved to a session. No session's
  transcript can show the receipt.
- `btw`: Dispatch delivered a mention as a BTW, and has no reply from the side turn. A BTW side
  turn adds nothing to the transcript.

An unknown is never counted as delivered or as not delivered.

### The document and its refusals

The document gives the counts of comments and of pairs with the identity
`total = delivered + not delivered by cause + unknown`. `CAUSES` in `report.ts` maps each cause to
its fix. The document has a row for each comment and a row for each pair, and it lists the
unlisted rows.

The report refuses its input (exit 1, each problem on stderr, nothing on stdout) in these cases:

- A header is not the expected columns.
- A row repeats: the same `comment_id` and `target` in `comments.csv`, in two listed rows of
  `receipts.tsv`, or in two unlisted rows.
- The rows of `receipts.tsv` are not sorted. The report names the first row out of order.
- A pair of `comments.csv` has no listed row, or a listed row has no pair in `comments.csv`.
- A row of `receipts.tsv` names a comment that `comments.csv` does not hold, or an event that is
  not its comment's.
- A value is not one of its values: `listed`, `status`, `unknown_reason`, `evidence`, a time, an
  unlisted row's target, a mention mode or state. Or two values disagree: an `unknown` with no
  reason, a `received` with no evidence, a number of mention envelope ids that is not the number
  of mention modes.
- The rows of one comment in `comments.csv` disagree.
- A line of `receipts-counts.txt` is missing, extra, out of order or not in its form; a sum of
  its numbers is not true; a number in it is not the number that `report.ts` counts; or
  `rows_without_id` is not 0. The report names the line.
- A comment falls in no class or in two, a not-delivered comment has no cause, a cause has no fix,
  or a count identity does not add up.

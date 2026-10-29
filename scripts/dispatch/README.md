# Dispatch scripts

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

`--json` prints the same numbers as one JSON object on stdout and nothing else — the ask totals,
the per-issue counts, the approval rounds, the coded standalone questions, the coding totals, the
excluded-ask count and the per-session rows — so one day's numbers can be compared with another's.

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
session, and why (sjawhar/legion#2366). The run has three steps. Every step only reads.

1. `comments.sql` reads Dispatch's database and writes `comments.csv`: every comment that a person
   wrote in the window, one row for each session or role that the comment was owed or sent to (a
   pair). `trace.sql` writes Dispatch's whole record of one comment. Both are in
   `packages/envoy/internal/dispatch/deliveryaudit/`. Its `README.md` gives their columns.
2. `tq receipts` (in theorem-labs/agent-infra `transcript-store/`) reads
   `comments.csv` and the transcript store. It writes `receipts.tsv`, one status for each pair,
   and `receipts-counts.txt`, its counts. It owns the transcript rules: which transcript row is a
   receipt of a comment, which sessions count as the pair's session, and each pair's status.
3. `report.ts` reads the three files and writes one markdown document and one TSV file of the
   document's pairs.

theorem-labs/agent-infra's `transcript-store/comment-delivery/run.sh` does steps 1 and 2 on a dev box, which
reaches both Dispatch's database and the transcript store.

### The run

Use one window for both steps. That run chooses `TO`. Both times are UTC, in the form
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

# 2. The transcript store (theorem-labs/agent-infra transcript-store/): receipts.tsv on stdout, the counts on stderr.
tq receipts --comments comments.csv --from "$FROM" > receipts.tsv 2> receipts-counts.txt

# 3. The document, and the TSV file of its pairs.
bun scripts/dispatch/comment-delivery/report.ts \
  --comments comments.csv --receipts receipts.tsv --counts receipts-counts.txt \
  --findings findings-bet-15.md --trace trace-03207432.csv \
  --pairs human-comment-delivery-2026-10-pairs.tsv > human-comment-delivery-2026-10.md
```

`--pairs` is required. `report.ts` writes the rows of the pairs to that file, and not to the
document, because Dispatch refuses a Markdown document with too many elements; upload the file as
an artifact beside the document. The file is TSV with a header line and one row for each pair, in
the order of the document's comments. Its columns are `comment_id`, `target`, `owed_because`,
`sent_by_the_outbox`, `mention_attempts`, `checked_session`, `status`, `class` and `cause`. A
backslash, a tab, a carriage return and a newline in a value are written as `\\`, `\t`, `\r` and
`\n`. The document's "Each pair" section gives the file's base name and its number of rows.

`--findings` and `--trace` can each be given more than one time. The document holds each findings
file whole, as Markdown, in the order given, in its "Findings" section after the table of causes
and their fixes. Start the headings of a findings file at `###`, because the section is a `##`
heading. The document holds each trace file whole, in a code block under the file's name, in the
order given. The same files give the same document and the same pairs file, byte for byte.

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
     `target_started_at` and `target_ended_at`. These are the transcript store's recorded start
     and end of the session's transcript file, not the times that the session's process was up.
     An empty `target_ended_at` means that the store records no end for the file.
  5. `running-no-row`: any other `not_received` pair. Dispatch sent the comment to the session
     within the store's recorded start and end of the session's file, and the transcript does
     not have it. The process can still have been down inside that span; the loss is in the last
     hop or in the plugin.

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
`total = delivered + not delivered by cause + unknown`. `CAUSES` in `classes.ts` maps each cause to
its fix. The document has a row for each comment, the pairs file has a row for each pair, and the
document lists the unlisted rows. No session was owed a comment with the cause `no-target`; give
the cause of each such comment in a findings file.

The document writes each value that it copies from its input (an owner, an author, a target, an
evidence, a receipt's place, a file name) as a code span, so that a Markdown editor does not make
an e-mail address or a URL in it a link and the uploaded document stays byte for byte the one a
re-run writes. A comment id of only hex digits and dashes stays bare. A findings file and a trace
go in as they are.

The report refuses its input (exit 1, each problem on stderr, nothing on stdout, no pairs file) in
these cases:

- A file that `--comments`, `--receipts`, `--counts`, `--findings` or `--trace` names cannot be
  read.
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

## Meaning search check

`meaning-search-check.ts` is a production check of Dispatch meaning search, and anyone can run it
again. A1 checks that a search answer was not degraded to keyword-only ranking.
A2 checks that a query sharing no word with its issue finds that issue in the top five. A3 reads
the coverage report: every item has a current vector from the expected model.

```bash
bun scripts/dispatch/meaning-search-check.ts \
  --a1-query 'meaning search' \
  --pair 'KEY=QUERY' \
  --lexeme-database "$TMPDIR/pg/.s.PGSQL.5432" \
  --coverage --expect-model '<the identity the deploy configures>' \
  --coverage-wait 600
```

Each check runs only when its flags are given, and at least one must be. `--coverage` and
`--expect-model` go together, and `--coverage-wait` (seconds, default 600) only with them.
`--pair KEY=QUERY` repeats, one per pre-registered pair, and splits at the first `=`; the operator
gives each pair's key at run time. Dispatch is resolved as for the census
above (`sessionDispatchSettings` over the envoy.json `command` reads, `DISPATCH_URL` and
`DISPATCH_TOKEN_FILE` or `DISPATCH_TOKEN` first). With Dispatch off it prints
`unavailable: config` and exits 2; it never prints the resolution's notices, which name files.

- **Build**, always: `GET /healthz`, sent no token, read on 200 and on 503. It prints
  `build commit=<sha|null> schema=<n|null> ok=<true|false>`. Whether that commit carries a given
  merge is checked outside the script, with
  `gh api repos/sjawhar/legion/compare/<merge>...<commit> --jq .status` (`ahead` or `identical`).
  An unreadable `/healthz` makes the run unavailable.
- **A1** (`--a1-query`): `GET /api/v1/search?q=<query>` fails when the answer has a `degraded`
  field, and passes when it has none and at least one result:
  `A1 degraded=yes|no results=<n> pass|fail`. An empty answer without `degraded` prints
  `A1 degraded=no results=0 unavailable: no-results`. A query with no indexable word gets an empty
  answer without the embedder being tried, so an empty answer proves nothing.
- **A2** (`--pair`): the issue's lexemes and the query's lexemes come from Postgres (below), and
  `shared` counts the query's distinct lexemes that the issue's row also holds. Then
  `GET /api/v1/search?q=<query>&limit=5`, with nothing else, gives `position`: where the issue's
  own row (kind `issue`, owned by the issue) stands among the first five results. A document,
  comment, ask or message the issue owns never counts. A pair passes when `shared=0` and the row
  is at position 1 to 5, and fails otherwise. Without `--lexeme-database`, or when the database
  fails, it prints `shared=unchecked` and is unavailable. When the issue read fails, it prints
  `A2 <KEY> shared=unchecked position=<1-5|none> unavailable: <category>`. Each pair prints
  `A2 <KEY> shared=<n|unchecked> position=<1-5|none> <verdict>`. When the pair's own search is
  unavailable it prints `A2 <KEY> unavailable: <category>`, with no shared and no position. A2
  passes when any pair passes, is unavailable when none passes and any is unavailable, and fails
  otherwise.
- **A3** (`--coverage`): `GET /api/v1/search/coverage`. A report that departs from its shape is
  malformed: `identity` a string or null, `place` `bedrock`, `azure` or null, at least one kind,
  each kind named `^[a-z][a-z0-9_]{0,31}$`, and `items = current + lacking + failed` for `total`
  and each kind. The check takes whatever kinds the report lists. It passes when `identity` is
  exactly `--expect-model`, `total.items` is above 0, and `total.failed` and `total.lacking` are 0
  in one snapshot. A fresh write is embedded a few seconds later, so while lacking items are all
  that is wrong it reads again every 5 seconds within `--coverage-wait`. When the wait runs out it
  fails on the last snapshot, whose `oldest_lacking_s` tells a write in flight from a stall. It
  prints `A3 identity=match|mismatch place=<place|none>`, the counts, `reads=<n>` and the verdict,
  then the counts of each kind.

The last line is `result pass|fail|unavailable exit=<code>`. Exit 0: every check asked for passed.
Exit 1: a check failed and none was unavailable. Exit 2: a check was unavailable, or a usage
error. An unavailable check
(`unavailable: auth|refused|transport|malformed|not-found|no-results|lexeme-database`) is never a
pass. `refused` is a 4xx refusal other than 401, 403, 404, 408 or 429, for example a query longer
than search accepts. A 5xx, 408 or 429 can clear on its own, so it is `transport`.

Output is allowlisted on stdout and stderr alike: keys, kind names, positions, counts, the build
commit, match or mismatch, the place and the failure category. The script never prints a URL, the
token, a response body, an issue title, a query, the coverage identity (on Azure it holds the
endpoint host) or an exception's text. This repository is public, and a search result's link
carries the private host while an issue title can carry a private name.

### Lexeme database

`--lexeme-database` takes a Postgres URL (`postgres://` or `postgresql://`) or the path of a
server's `.s.PGSQL.5432` socket. Any Postgres 18 works, as production and CI run 18: the check uses only
the built-in `english` configuration, in one statement per pair with every value bound, and
reads or writes no table. The statement mirrors the issue row's vector
(`migrations/0088_issues_title_lexemes.up.sql`, with `search_text` from
`migrations/0056_search_text.up.sql`) and search's own `websearch_to_tsquery('english', q)`. A
throwaway PostgreSQL 18.6, the version production runs, that listens only on a unix socket in
`$TMPDIR/pg`, from zonky's pinned Linux x86-64 binaries on Maven Central. The block is safe to run
again, also from a second shell at the same time: it holds a lock while it works, prints the socket
path and starts nothing when a server answers on the socket, refuses when none answers but
`$TMPDIR/pg/data/postmaster.pid` is left, downloads the server only when `$TMPDIR/pg/server` is
absent, creates the cluster only when `$TMPDIR/pg/data` is absent, then starts it and prints the
socket path:

```bash
pg="$TMPDIR/pg" v=18.6.0 && mkdir -p "$pg" && (
  flock 9 && cd "$pg" || exit 1
  # Ask the socket, never pg_ctl status: each role of an issue pod has its own PID namespace, so
  # postmaster.pid names a process only in the role that started the server.
  if python3 -c 'import socket, sys; s = socket.socket(socket.AF_UNIX); s.settimeout(5); s.connect(sys.argv[1])' "$pg/.s.PGSQL.5432" 2>/dev/null; then
    echo "$pg/.s.PGSQL.5432"
    exit 0
  fi
  if [ -e data/postmaster.pid ]; then
    echo "no server answers on $pg, but data/postmaster.pid is left: stop the server from the shell that started it, or remove $pg" >&2
    exit 1
  fi
  if [ ! -d server ]; then
    rm -rf server.new &&
      curl -fsSL -o pg.jar "https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/$v/embedded-postgres-binaries-linux-amd64-$v.jar" &&
      python3 -c 'import zipfile; zipfile.ZipFile("pg.jar").extract("postgres-linux-x86_64.txz")' &&
      python3 -c 'import lzma, tarfile; tarfile.open(fileobj=lzma.open("postgres-linux-x86_64.txz")).extractall("server.new", filter="data")' &&
      mv server.new server ||
      exit 1
  fi
  if [ ! -d data ]; then
    rm -rf data.new &&
      server/bin/initdb -D data.new -U postgres --auth=trust -E UTF8 -c listen_addresses= -c "unix_socket_directories=$pg" &&
      mv data.new data ||
      exit 1
  fi
  # 9>&-: the server must not inherit the lock, or every later run waits on it.
  server/bin/pg_ctl -D data -l pg.log start 9>&- || exit 1
  echo "$pg/.s.PGSQL.5432"
) 9>"$pg/setup.lock"
```

If a run fails, run `rm -rf "$TMPDIR/pg"` and run the block again.

Pass it as `--lexeme-database "$TMPDIR/pg/.s.PGSQL.5432"`; the check connects as `postgres` to the
`postgres` database. Stop it with `server/bin/pg_ctl -D data stop` from `$TMPDIR/pg`, only in the
shell that started it: `pg_ctl` signals the PID in `postmaster.pid`, which in any other role of an
issue pod is another process or none. When no server answers and the block refuses, remove
`$TMPDIR/pg` and run the block again.

### The pre-registered pairs

The pairs, each a query and the key of the issue it should find, are recorded on the deployment's
issue; the keys are given only at run time. The queries:

- watching a running bot, the screen keeps scrolling down whenever output arrives, so earlier
  lines are impossible to study
- language model vendor billing balance ran dry and automated code checks halted
- flowchart markup inside pages shows as raw text instead of a drawn picture

Each query was fixed before the deploy and matched nothing on production then. A2 is judged on
the issue's own row because its key and title are the only text Dispatch indexes for that row
(`migrations/0088_issues_title_lexemes.up.sql`), and each query shares no lexeme with them. Each
query does share lexemes with its issue's spec, so a hit on a document the issue owns would be a
keyword match, not meaning search.

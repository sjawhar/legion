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

## Meaning search check

`meaning-search-check.ts` is the production check of Dispatch meaning search for INFRA-194, and
anyone can run it again. A1 checks that a search answer was not degraded to keyword-only ranking.
A2 checks that a query sharing no word with its issue finds that issue in the top five. A3 reads
the coverage report: every item has a current vector from the expected model.

```bash
bun scripts/dispatch/meaning-search-check.ts \
  --a1-query 'meaning search' \
  --pair 'INFRA-43=watching a running bot, the screen keeps scrolling down whenever output arrives, so earlier lines are impossible to study' \
  --pair 'INFRA-30=language model vendor billing balance ran dry and automated code checks halted' \
  --pair 'INFRA-32=flowchart markup inside pages shows as raw text instead of a drawn picture' \
  --lexeme-database "$TMPDIR/pg/.s.PGSQL.5432" \
  --coverage --expect-model '<the identity the deploy configures>' \
  --coverage-wait 600
```

Each check runs only when its flags are given, and at least one must be. `--coverage` and
`--expect-model` go together, and `--coverage-wait` (seconds, default 600) only with them.
`--pair KEY=QUERY` repeats and splits at the first `=`. Dispatch is resolved as for the census
above (`sessionDispatchSettings` over the envoy.json `command` reads, `DISPATCH_URL` and
`DISPATCH_TOKEN_FILE` or `DISPATCH_TOKEN` first). With Dispatch off it prints
`unavailable: config` and exits 2; it never prints the resolution's notices, which name files.

- **Build**, always: `GET /healthz`, sent no token, read on 200 and on 503. It prints
  `build commit=<sha|null> schema=<n|null> ok=<true|false>`. Whether that commit carries a given
  merge is checked outside the script, with
  `gh api repos/sjawhar/legion/compare/<merge>...<commit> --jq .status` (`ahead` or `identical`).
  An unreadable `/healthz` makes the run unavailable.
- **A1** (`--a1-query`): `GET /api/v1/search?q=<query>` passes when the answer has no `degraded`
  field and fails when it has one: `A1 degraded=yes|no results=<n> pass|fail`.
- **A2** (`--pair`): the issue's lexemes and the query's lexemes come from Postgres (below), and
  `shared` counts the query's distinct lexemes that the issue's row also holds. Then
  `GET /api/v1/search?q=<query>&limit=5`, with nothing else, gives `position`: where the issue's
  own row (kind `issue`, owned by the issue) stands among the first five results. A document,
  comment, ask or message the issue owns never counts. A pair passes when `shared=0` and the row
  is at position 1 to 5, and fails otherwise. Without `--lexeme-database`, or when the database
  fails, it prints `shared=unchecked` and is unavailable. Each pair prints
  `A2 <KEY> shared=<n|unchecked> position=<1-5|none> <verdict>`. A2 passes when any pair passes,
  is unavailable when none passes and any is unavailable, and fails otherwise.
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
error. An unavailable check (`unavailable: auth|transport|malformed|not-found|lexeme-database`)
is never a pass.

Output is allowlisted on stdout and stderr alike: keys, kind names, positions, counts, the build
commit, match or mismatch, the place and the failure category. The script never prints a URL, the
token, a response body, an issue title, a query, the coverage identity (on Azure it holds the
endpoint host) or an exception's text. This repository is public, and a search result's link
carries the private host while an issue title can carry a private name.

### Lexeme database

`--lexeme-database` takes a Postgres URL (`postgres://` or `postgresql://`) or the path of a
server's `.s.PGSQL.5432` socket. Any Postgres 16 works, as production runs 16: the check uses only
the built-in `english` configuration, in one statement per pair with every value bound, and
reads or writes no table. The statement mirrors the issue row's vector
(`migrations/0088_issues_title_lexemes.up.sql`, with `search_text` from
`migrations/0056_search_text.up.sql`) and search's own `websearch_to_tsquery('english', q)`. A
throwaway server that listens only on a unix socket:

```bash
uvx --python 3.12 --from pgserver==0.1.4 python -c 'import pgserver; print(pgserver.get_server("<dir under $TMPDIR>", cleanup_mode=None).get_uri())'
```

Pass it as `--lexeme-database <dir>/.s.PGSQL.5432`: Bun's `SQL` refuses the printed URL's
`?host=<dir>` form.

### The pre-registered pairs

| Issue | Query |
| --- | --- |
| INFRA-43 | watching a running bot, the screen keeps scrolling down whenever output arrives, so earlier lines are impossible to study |
| INFRA-30 | language model vendor billing balance ran dry and automated code checks halted |
| INFRA-32 | flowchart markup inside pages shows as raw text instead of a drawn picture |

Each query was fixed before the deploy and matched nothing on production then. A2 is judged on
the issue's own row because its key and title are the only text Dispatch indexes for that row
(`migrations/0088_issues_title_lexemes.up.sql`), and each query shares no lexeme with them. Each
query does share lexemes with its issue's spec, so a hit on a document the issue owns would be a
keyword match, not meaning search.

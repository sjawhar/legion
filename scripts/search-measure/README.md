# Measuring Dispatch search

This directory builds a fixture the size of a deployment's own Dispatch and measures what search finds in it.

It measures the keyword path alone: no embedder is configured, so the route answers from `issues.search`, `document_search`, `comments.search`, `asks.search` and `messages.search` and nothing else, and every answer it gives is stamped `degraded`.
Nothing here writes an embedding, and nothing here needs a model service.

Every script reads the server it harvests from through its read API, with `GET` and no other method.

## The scripts

`dispatch_read.py` is the read-only client the others share: `DISPATCH_URL` and the token in the file `DISPATCH_TOKEN_FILE` names, a retry for a transport error or a 5xx, and a pool that runs several reads at once.

`pg.py` is the psql the others read the fixture with: a statement's rows as JSON, one object per line.

`harvest.py` writes a corpus into a directory under `$TMPDIR`.

- It reads every project, every issue of every project, every document of every project and every issue's primary document, each document's latest markdown, and every issue's comments, asks and messages.

- An issue's primary document is read from the issue's own artifact list, because `GET /api/v1/projects/{key}/artifacts` answers `and not is_primary` and leaves every spec out.

- `coverage.json` keeps the server's own item counts per kind, so a fixture can be compared with what it was harvested from; nothing else of that answer is kept, since its identity names a model service host.

`load_fixture.py` creates a database, migrates it and loads the corpus.

- The migration is this tree's own: `envoy-dispatch census` calls `store.Migrate` before it counts anything, the same call the server makes at boot, so the fixture's schema is the schema the server being measured expects.

- The rows go in with `COPY`, which fires every row-level trigger, and the triggers fill every `search` vector.

- Only what search reads and what the schema requires is written: a constant actor stands in for each row's author, and thread links, deliveries, events and block indexes are not written at all.

- An ask is loaded as a question, whatever kind it has in production, because `asks.approval` carries a schema-checked shape the read API does not answer whole, and neither `kind` nor `approval` is read by `asks_search` or by any leg of the search query.

`build_cases.py` turns a set of measured cases into descriptors.

- A case's query is Dispatch's own text and this repository is public, so a committed case names the item its text comes from, the version, the character range, and the SHA-256 of what that range yields.

- `case_text.py` holds the one rule for reading a range back, so the hash a descriptor carries is the hash of what the runner will send.

- A case whose query is nobody's text — a title someone reworded to ask search the same question in other words — has no range to name, and its query goes to a side file under `$TMPDIR`, keyed by its hash.

`run_search.py` runs the cases against a search route and writes one JSONL row per case.

- Each query is read back out of the fixture the server is serving and checked against its descriptor's hash; a case whose text has moved is refused, never guessed at.

- A row carries ranks, counts and times, never a query and never a result's title, so the output of a run holds no Dispatch text.

- `--workers 1`, one request at a time, is the latency measurement; more workers measure recall faster and say nothing about latency.

`analyze.py` turns those rows into recall at 1, 3, 10 and 20, MRR and "absent from the page", per case kind and over the kinds together, each beside the baseline the hybrid route measured on the same cases.

- It also counts, for a sentence from an issue's own spec, how often the issue's document comes back while the issue does not, which is the defect this measurement was made for.

`serial120.json` is the 120 case ids of the latency sample, so a latency run is the same sample every time.

## A run, end to end

```bash
cd packages/envoy && go build -o "$TMPDIR/envoy-dispatch" ./cmd/dispatch
python3 scripts/search-measure/harvest.py --out "$TMPDIR/corpus" --workers 24
python3 scripts/search-measure/load_fixture.py --corpus "$TMPDIR/corpus" \
  --database searchfix --dsn "$LEGION_TEST_PG_DSN" --server "$TMPDIR/envoy-dispatch"
```

Then serve the fixture with no embedder.
With no AWS region reaching the process, `embed.NewBedrock` fails, the server logs `meaning search unavailable`, and `s.deps.Embedder` is nil, which is the branch `runKeywordOnlyDegraded` serves:

```bash
env -u AWS_REGION -u AWS_DEFAULT_REGION -u AWS_CONFIG_FILE -u AWS_PROFILE \
  DATABASE_URL="postgres://…/searchfix" DISPATCH_AGENT_TOKEN=fixture-token \
  DISPATCH_NATS_DISABLED=1 DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT=8799 \
  DISPATCH_SERVER_URL=http://127.0.0.1:8799 DISPATCH_SIGNING_KEY=fixture-key \
  "$TMPDIR/envoy-dispatch"
```

Build the cases once against the corpus, then run them:

```bash
python3 scripts/search-measure/build_cases.py --corpus "$TMPDIR/corpus" \
  --cases "$TMPDIR/cases_all.json" --out scripts/search-measure/cases.json \
  --side "$TMPDIR/corpus/side-text.json"
python3 scripts/search-measure/run_search.py --cases scripts/search-measure/cases.json \
  --fixture-dsn "postgres://…/searchfix" --url http://127.0.0.1:8799 --token fixture-token \
  --side "$TMPDIR/corpus/side-text.json" --only scripts/search-measure/serial120.json \
  --workers 1 --out "$TMPDIR/out-serial.jsonl"
python3 scripts/search-measure/analyze.py "$TMPDIR/out-all.jsonl" --serial "$TMPDIR/out-serial.jsonl"
```

`cases.json` is the descriptor set as it was built against a harvest of 2026-11-05.
A case whose sentence its document no longer holds is refused by the runner and counted, which is how a later harvest says what has moved under it.

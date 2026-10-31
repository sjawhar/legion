# Envoy diagnostic scripts

## probe-webhook-e2e.sh

On-demand end-to-end probe for the GitHub webhook → Envoy session delivery
path. Run it any time you suspect webhooks aren't reaching subscribed
sessions (e.g., agents subscribed to `notifications.github.<owner>.<repo>.pr.<n>.checks`
not seeing settled CI events).

### Usage

```bash
WEBHOOK_URL=https://your-ingress.example.com/webhook/github \
  packages/envoy/scripts/probe-webhook-e2e.sh
```

Required env:

- `WEBHOOK_URL` — the public ingress that fronts the webhook receiver Envoy
  publishes from. **No default** — supply your deployment's URL.

Run from a host with:

- A local Envoy listener reachable at `http://127.0.0.1:9020` (override with
  `LISTENER_URL=...`).
- SOPS-decryptable access to `ENVOY_GITHUB_WEBHOOK_SECRET` (the same secret
  the receiver verifies against). The probe reads it via
  `packages/envoy/deploy/scripts/read-secret.sh`.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | The synthetic webhook was delivered to a live session via the full pipeline. |
| 1 | Webhook ingress accepted the payload but the on-prem listener never delivered it (the bridge is broken). |
| 2 | Webhook ingress rejected the request (signature mismatch, malformed, or 5xx). |
| 3 | Local listener unreachable or subscribe call failed at startup. (Cleanup unsubscribe failures are tolerated by the cleanup trap and do not flip the exit code.) |
| 4 | Required tool missing on the host (`curl`, `openssl`, `python3`, `jq`, or `read-secret.sh`). |

### Diagnosing failures

- **Exit 1 (most common):** the receiver published to its NATS but the
  on-prem listener never saw it. Check `docker logs envoy-listener --since 1m`
  on the on-prem host for any `received` line containing the probe's trigger
  string. If absent, the event never made it from receiver-side NATS to the
  listener — the bridge between them is broken.
- **Exit 2:** the receiver itself rejects the request. Verify
  `ENVOY_GITHUB_WEBHOOK_SECRET` matches what the receiver has in its secret
  store. Confirm the URL with `curl -i $WEBHOOK_URL/../healthz` if there's a
  health endpoint.
- **Exit 3:** the local listener is down or on a different port. Check
  `curl http://127.0.0.1:9020/healthz` and the running container
  (`docker ps | grep envoy-listener`).
- **Exit 4:** install the missing tool.

### Why the topic is `notifications.github.legion-probe.canary.issue.1.comment`

The `legion-probe/canary` repo does not exist on GitHub — that's intentional.
The probe sends a fake `issue_comment` event with that owner/repo, the
receiver normalizes the envelope topic from the payload (it does not call
the GitHub API to validate the resource), and the topic flows through
exactly the same code path real events do. Using a synthetic owner/repo
guarantees no real subscriber will ever match the probe traffic.

### Tuning

- `TIMEOUT_SECONDS=60 ...` — wait longer (default 30s).
- `LISTENER_URL=http://other-host:9020 ...` — point at a different on-prem listener.

## e2e-local.sh

Runs the local listener against a throwaway NATS server it builds and runs
itself (`internal/testnats/cmd/natsd`), so it needs no container runtime. It
posts signed public GitHub fixtures and a three-paragraph direct message,
checks the one-warning response for a never-seen GitHub repository, reads the
registered session back by id from `/v1/registry/<session_id>` and an id that
never registered as `404 {"error":"no live session <id>"}`, rejects an unheld
role publish before claiming it for the fake session, captures the raw
notification envelopes, and proves both the Go prompt text and the shared
TypeScript renderer print the full direct message once, with no separate
summary line repeating its first line.

It also proves the three answers a sender gets when a send cannot be delivered:
a send to a session the store does not hold is refused `404 {"error":"no live
session <id>"}`; each send leaves exactly one `listener send` record in
`listener.log`, INFO when accepted and WARN when refused, carrying
`target_session`, `event_id`, `source`, `source_session`, `payload_summary`,
`outcome` and `status`; and a send whose bus is gone is refused `503` with
`{"error":"envoy: the message bus did not answer: …"}` and `Retry-After: 1`
inside the sending client's five-second deadline, both over curl and through
`createEnvoyClient`, which raises an `EnvoyApiError` rather than timing out. The
bus-down step runs last, because it ends the run's NATS.

```bash
packages/envoy/scripts/e2e-local.sh
```

The run writes its reusable evidence to `packages/envoy/out/e2e/`:

- `envelopes.jsonl` — raw NATS envelope frames
- `session-prompts.jsonl` — raw fake-session `prompt_async` requests
- `rendered-ts.txt` — `renderInbound` output for every envelope
- `rendered-go.txt` — the corresponding Go `Deliverer.Text` output
- `listener.log` — the listener's own log, including one record per send and publish
- `absent-session-response.json` — the 404 body naming the session nothing registered
- `bus-down-send-response.json`, `bus-down-send-headers.txt`,
  `bus-down-send-result.txt` — the bus-down 503 body, its headers, and its
  status and `time_total`
- `bus-down-client-error.json` — the `EnvoyApiError` the real client raised for it

The OS chooses the NATS, listener and fake-session ports, and each URL is built
from the port its own process reports. `E2E_NATS_PORT`, `E2E_PORT` or
`E2E_SESSION_PORT` sets a port explicitly: it is used as given, and one already
in use fails the run with `address already in use`. The NATS server is a
process of this checkout's own build, stopped on exit with the listener and the
two fake clients. Unlike
`probe-webhook-e2e.sh`, it has no deployment URL or secret prerequisite: it
validates local branch behavior only. CI runs it as the `envoy-e2e-local` job of
`.github/workflows/ci.yaml` on every change its `envoy_e2e_local` filter matches.

## listener-deploy-probe.sh

Watches an Envoy listener from a client's seat while its deployment rolls, and
judges whether any task refused `/v1` while it was in service. A rolling deploy
runs the old and the new task side by side, and a caller that resolves the
listener's name (Dispatch, the Legion daemon, every plugin) can reach either.
Every tick the probe resolves the name with `getent ahostsv4` (or takes the
tasks' addresses from `--targets`) and asks each address, and the name itself,
for `/healthz` and `GET /v1/sessions`. It needs bash, curl, getent and sed only
(no jq), so it also runs inside a listener task over `aws ecs execute-command`.

```bash
ENVOY_TOKEN_FILE=<path to the listener bearer> \
  packages/envoy/scripts/listener-deploy-probe.sh \
  --url http://envoy-listener.internal.example:9020 --duration 300
```

Start it, then start the deploy (for example an ECS `--force-new-deployment`),
and let it run past the old task's exit. Options:

- `--targets <ip,ip>` — probe these addresses instead of resolving the name,
  e.g. both tasks' addresses from `aws ecs describe-tasks`.
- `--token-file <path>` — the listener's `/v1` bearer (default
  `$ENVOY_TOKEN_FILE`, then `$ENVOY_TOKEN`). The bearer reaches curl in a header
  file, never on its command line.
- `--interval <s>` (default 1) and `--duration <s>` (default 300; `0` runs
  until Ctrl-C, which still prints the summary).
- `--send-to <session>` — also `POST /v1/messages/send` to that session at every
  target each tick, under one idempotency key per tick.
- `--dispatch-url`, `--dispatch-token-file`, `--dispatch-issue <KEY>`,
  `--dispatch-session <session>` (all four together), `--dispatch-mode btw|steer`
  (default `btw`), `--dispatch-every <n>` (default 10) — post a Dispatch issue
  message targeting the session every `n` ticks and record its delivery
  attempt's `state` (and `error`), or `http_<code>` (and Dispatch's `error`)
  when Dispatch refuses the post, which is how a Dispatch delivery during the
  overlap is observed.

Each tick prints one tab-separated line per target:
`ts target healthz_code healthz_status v1_code v1_error send_code dispatch_state`
(`000` when no whole answer came, with `v1_error` naming why for `/v1`: `no connection`,
`timeout`, `empty reply`, `connection reset`; `-` for a column that does not apply; the Dispatch
attempt rides the name's line). On exit it prints, per target, when it was
seen, how many ticks `/healthz` answered, and the `/v1` non-200 answers at the
ticks `/healthz` answered 200, counted by `code:error` with the first and last
time, then the sends and Dispatch attempts by outcome, then the verdict.

### Verdict and exit codes

A target **fails** when `/v1/sessions` answers anything but 200 at any tick
where its `/healthz` answered 200 (whatever its `status`, `starting` included),
or when it never answers `/v1/sessions` 200 at all. A `/v1` request that times
out (the probe allows 3 s, less than Dispatch's 5 s client timeout), or whose
connection closes or resets without an answer (what Go's `net/http` does after
a handler panics), is a non-200 answer: `000:timeout`, `000:empty reply`,
`000:connection reset`. One that finds nothing listening right after `/healthz`
answered (curl exit 7) is counted in the summary, not judged a refusal: a
listener that stops closes its listening socket first, so the task stopped
between the tick's two requests. A target that never answers `/healthz` is
**unreached**: named in the summary, not failed (a stale A record during the
handover is one). With `--dispatch-*`, the run also **fails** when any Dispatch
message did not record `state` `sent`: a failed delivery attempt, or a post
Dispatch refused (`http_401:invalid bearer token` for a wrong Dispatch
bearer), and the verdict names each such outcome with its count.

| Code | Meaning |
|------|---------|
| 0 | At least one target answered `/healthz`, every target that did passed, and every Dispatch message recorded `state` `sent`. |
| 1 | A target failed, a Dispatch message did not record `state` `sent`, or no target ever answered `/healthz`. |
| 2 | Usage error, or every `/v1` answer of the run was 401 or 403: the bearer is wrong, not the listener. |
| 4 | A required tool is missing (`curl`, `sed`, or `getent` without `--targets`). |

A listener from before LEGION-456 keeps `/v1` closed until its durable binds,
so during a rolling deploy the replacement answers `/healthz` 200 `starting`
and `/v1` 503 `service starting` for most of a minute and the run exits 1. A
listener that opens `/v1` once its caches are warm passes.

`listener-deploy-probe.test.sh` proves the verdict over two fake tasks, one
Python process per case serving `127.0.0.1` and `127.0.0.2` on one port the
kernel chose, behind a fake `getent`; CI runs it in the `envoy-go` job of
`.github/workflows/ci.yaml`.

## Measuring search on a copy of the corpus

Three scripts measure a change to Dispatch search against the real corpus rather than a seeded
one. The copy is production data: it stays on the machine that restored it.

`corpus-copy.sh` restores the newest nightly dump into a Postgres container of its own and prints
its `DATABASE_URL`. The bucket the dumps land in is the deployment's, so it is a required input:

```bash
DISPATCH_BACKUP_BUCKET=<bucket> packages/envoy/scripts/corpus-copy.sh
```

It downloads the dump under `DISPATCH_CORPUS_DIR` (default `$TMPDIR` or `/tmp`) and deletes it once
it is restored, whether or not the restore succeeded. `DISPATCH_CORPUS_CONTAINER` (default
`dispatch-corpus-pg`), `DISPATCH_CORPUS_PORT` (default `55433`, on `127.0.0.1`) and
`DISPATCH_CORPUS_DATABASE` (default `dispatch_corpus`) name the copy. Run again while the container
holds the database, it prints the URL and restores nothing; `docker rm -f -v <container>` deletes the
copy and its data volume.

`search-smoke.sh` prints what a searcher sees for each query in a file
(`search-smoke-queries.txt` is one): it starts `envoy-dispatch` against the copy, built from this
checkout or named with `--binary`, and prints each answer as one line of totals, a `took_ms` line,
and one `pos kind owner id snippet` line per hit. Two builds' runs diff, which is the before and
after of a ranking change. The server runs with a throwaway `HOME`, a made-up agent token and a
trusted identity header, so it reads no operator configuration and holds no credential, and with
NATS off. It migrates the database it is given, as every start does, so point it only at a copy.

```bash
DATABASE_URL=<copy> packages/envoy/scripts/search-smoke.sh \
  --queries packages/envoy/scripts/search-smoke-queries.txt [--binary <envoy-dispatch>] [--limit 20] [--offset 0]
```

`search-latency-compare.sh` compares a base checkout (usually `main`) with this one on the same
copy: it runs `TestSearchLatencyOnCorpus` from each, alternately, for five rounds by default, prints
every run's p50 and p95, and exits 1 when this checkout's median p95 is more than 10% above the
base's. A copy answers in whatever time the machine and its load allow, so the bound is relative,
never a fixed number of milliseconds.

```bash
DISPATCH_BENCH_DATABASE_URL=<copy> packages/envoy/scripts/search-latency-compare.sh <base-checkout> [rounds]
```

## build-dispatch-build.sh

Builds one Dispatch build from a commit of this repository into a directory, for the two-build
test (`cmd/dispatch/twobuild_*_test.go`, `go test -tags twobuild`), which runs it beside this tree
on one Postgres and one NATS, and for the browser suite's cross-build run
(`packages/dispatch/e2e/cross-build.multiprocess.e2e.ts`), which drives one build's page against
the other build's server. It extracts `git archive <commit>` into `<dir>/src`, so it makes no
worktree or branch, and builds each part as that commit's own image build does:
`bun install --frozen-lockfile`, the contracts' `gen:go` and `build`, `bun run build:web`, the
commit's own `packages/envoy-client/scripts/build-dispatch-cli.sh`, and `go build ./cmd/dispatch`
with `GOTOOLCHAIN=auto` and `-ldflags "-X main.buildCommit=<sha>"`.

```bash
packages/envoy/scripts/build-dispatch-build.sh --previous-release --out "$TMPDIR/dispatch-previous"
packages/envoy/scripts/build-dispatch-build.sh --commit <sha|tag> --out <dir>
```

`--previous-release` builds the newest `legion-envoy-v*` tag that is an ancestor of
`git merge-base HEAD origin/main`, so it needs `origin/main` and the tags fetched. `--commit HEAD`
builds the current branch the same way. The script refuses a commit that is not an ancestor of
`HEAD`, and a commit older than the release `legion-envoy-v20.1.0`, the oldest build either test
can run as the earlier side: that release carries migration 0117, from which two builds share one
database (`cmd/dispatch/README.md`, "Database migrations"), serves on a socket handed to it, which
the browser harness holds for it, and knows the one dev sign-in cookie key that run gives both of
its servers. `<dir>` must be absent or empty, or already
hold a complete build of the same commit, which the script leaves as it is. It then holds
`envoy-dispatch`, `web/` (the web app), `dispatch.js` (the `dispatch` command, run as
`bun dispatch.js`), `pmdoc/testdata/corpus/`, `pmdoc/schema/blocks.json` and `text/testdata/` from
the commit, and `COMMIT`, the full sha, written last. Point `DISPATCH_PREVIOUS_BUILD` at `<dir>` to
run the two-build test.

`--resolve` in place of `--out <dir>` builds nothing: it makes the same refusals and prints the full
sha the script would build, which CI's `dispatch-two-builds` job (`.github/workflows/ci.yaml`) keys
its cache of the build by before it builds.

```bash
packages/envoy/scripts/build-dispatch-build.sh --previous-release --resolve
```

## Measuring the related-item check on a project's duplicates

`related-recall.sh` copies one project's pairs of issues closed one into the other, and every other
issue of the project they compete with, from a Dispatch server into a JSONL file, for
`TestRelatedRecallOnDuplicates` (`internal/dispatch/api`). A pair is a done issue whose close
reason names one other issue that covers it: `Duplicate of`, `Duplicate:`, `Folded into`,
`Covered by` or `Superseded by`, in any case, followed by a Dispatch issue reference
(a Dispatch link to `<KEY>`, a bare `<KEY>` of one of the server's projects, or an `/issues/<KEY>` link).
The close reason is the issue's last top-level message posted at or before its close, the message
`dispatch issue-update --status done --reason` posts just before the close; the `issue.closed`
event carries no reason. A reason naming only a pull request, several issues or the issue itself
is not a pair. Two closes naming the same two issues make one pair, by the first close.

Each pair's query is its later-filed issue as filed: its first title and first spec version. That
is the closed issue when the issue its reason names came first (forward), and the named issue
when that came later (reversed). The target, the issue the check should list, is the earlier one,
its title and spec as of the query's filing. Pairs naming another project's issue, or one that
does not exist, are counted and left out. It reads with the caller's own token, sends only
`GET`s, one at a time, and prints counts only: done issues read, close reasons with a wording,
those that are not a pair by what they name (`no_reference` is a wording followed by no issue or
pull request), pairs found and kept per wording with forward and reversed, pairs left out by
reason, chains (a pair's named issue itself closed into another), and issues copied.

```bash
DISPATCH_URL=<server> DISPATCH_TOKEN_FILE=<token file> \
  packages/envoy/scripts/related-recall.sh <project> "$TMPDIR/recall.jsonl"
```

The file is production data: it is written with mode 600 and never committed. The test loads it
into a fresh test database, every issue open, under its own key, with its latest title and spec.
For each pair it sets the target's title and spec to their text at the query's filing, takes the
query out of the search, refreshes the term statistics, runs the check's old 40-word OR query and
its keyword search over the query's text, then puts both issues back. It prints each pair's
wording, direction, the target's rank in each search and whether each lists it among its three
related items, then the counts per wording and over every pair. An idf-weighted coordination rank
of the same terms is printed beside them as an experiment. `DISPATCH_RECALL_REQUIRE_NO_LOSS=1`
fails it if the keyword search lists fewer targets than the old query.
`TestRelatedCheckLatencyOnRecallCorpus` loads the same corpus and prints the keyword search's p50,
p95 and max over 20 runs for the text of the issue `DISPATCH_RECALL_LATENCY_ISSUE` names and for a
10,000-word spec, and the refresh's duration.

```bash
cd packages/envoy && DISPATCH_TEST_DATABASE_URL=<test database> DISPATCH_RECALL_CORPUS="$TMPDIR/recall.jsonl" \
  go test ./internal/dispatch/api -run 'TestRelatedRecallOnDuplicates|TestRelatedCheckLatencyOnRecallCorpus' -v -count=1 -timeout 60m
```

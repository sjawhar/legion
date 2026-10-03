# Dispatch census scripts

## Ask census

`ask-census.ts` reproduces the question and approval-request counting used for LEGION-470. It
reads every issue updated since the window opened and each issue's asks, then every page of the
events of each issue that carries an approval ask, whenever that ask was opened. Each issue is
read concurrently.

```bash
bun scripts/dispatch/ask-census.ts \
  --from 2026-10-01T03:40:00Z \
  --to 2026-10-02T02:10:00Z \
  --project AGENTC \
  --project LEGION \
  --project OPS
```

The script resolves Dispatch as the Envoy client does (`activeDispatchConfig` in
`packages/envoy-client/src/dispatch-config.ts`): the URL from `DISPATCH_URL`, else
`dispatch.serverUrl` in `~/.config/opencode/envoy.json` with `dispatch.enabled`; the token from
`DISPATCH_TOKEN_FILE`, else `DISPATCH_TOKEN`, else `dispatch.token`. It prints total asks, decision
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
`ask.handed_back`, one arrival. Before F1 a request made again opened a new row, so there each
arrival is an `ask.opened`. A human turn is an answer to the request or a reply in its thread,
and, from the round's first request on, an answer or reply on a decision block in the document
the request names: a choice the human raises while the request is open becomes such a block, and
the hand-back after its answer responds to that turn. A round with more arrivals than human turns
plus one is flagged. `scripts/e2e/lib/design-gate-approval-requests.jq` reads the same arrivals
for the stage 4b proof.

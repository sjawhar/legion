# Dispatch census scripts

## Ask census

`ask-census.ts` reproduces the question and approval-request counting used for LEGION-470. It
reads every issue updated in the selected window, then each issue's asks and every page of its
events.

```bash
bun scripts/dispatch/ask-census.ts \
  --from 2026-10-01T03:40:00Z \
  --to 2026-10-02T02:10:00Z \
  --project AGENTC \
  --project LEGION \
  --project OPS
```

The script reads `DISPATCH_URL` and `DISPATCH_TOKEN` when set. Otherwise it reads
`dispatch.serverUrl` and `dispatch.token` from `~/.config/opencode/envoy.json`. It prints total
asks, decision blocks, standalone question asks, approval requests, per-issue counts, approval
rounds, every standalone question with a blank `code` column, and a per-session table.

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

For every document with approval activity, **Approval rounds** shows Inbox rows, hand-backs, and
human turns. A hand-back above human turns plus one is flagged. Before F1, the script uses each
approval `ask.opened` event as a hand-back; after F1, it uses an `ask.edited` event whose
`requested_version` catches up to `version`.

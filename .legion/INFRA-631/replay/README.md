# INFRA-631 merge replay

Real conflicting merges of `packages/envoy/AGENTS.md` and `packages/envoy/cmd/dispatch/README.md` from 2026-10-26 and 2026-10-27, from `../planning/pairs.txt`, ported onto the new layout and merged again with `git merge-tree --write-tree`.

## Run

```sh
TMPDIR=/some/scratch bash .legion/INFRA-631/replay/run.sh [repo root]
```

The repo root defaults to the checkout holding the script.
`run.sh` copies the two files from the repo root into a throwaway repository under `${TMPDIR:-/tmp}/replay-$$`, commits them as the base, applies each patch on a branch of its own, and merges each pair.
For each pair it prints a classification line, computed from the two patches' hunks, then one line: merge, file, class, expected, observed, PASS or FAIL.
It exits 1 on any FAIL.

The classification works on the base file.
A side's changed lines are the lines it deletes or rewrites, and its insertion points are the lines it inserts after.
The class is "different sentences" (or "different items" for the demo) when the two sides' changed lines are disjoint and no changed line or insertion point of one side is equal or next (±1) to one of the other's.
Otherwise it is "same or adjacent", with the colliding lines named.
Two insertions at one point are "same place".
Each of the last two classes, and each control, is expected to conflict.
Every other pair is expected to merge clean.

## How each side was ported

Each patch applies to the head file (the new layout).
The patch for a side carries that side's sentence-level change to the file, one sentence per line, and each changed file passes `bun scripts/semantic-line-breaks.ts`.

- **Forward**: the head does not yet hold the side's change.
  The patch makes the side's edits at the matching sentences and items of the head file.
- **Reverse**: the head already holds the side's change, because the side is main and an ancestor of the head, or because its pull request was squash-merged since.
  That change cannot be made twice, so the patch undoes it.
  The patch removes each sentence and item the side added, puts each sentence it changed back to the base wording, and restores what it removed.
  It touches the same head lines that the side's change put there, so the merge geometry is the same as for the forward change.
  Where a later main commit rewrote a sentence the side changed, the patch puts back the side's base wording of that sentence.
  Sentences and items that later commits added inside the side's region are kept.

The first draft of each port came from the porting recipe: `restructure.ts --lenient` on the file at the base and at the side, then `git merge-file` onto the head, swapping the base and the side for a reverse port.
Its conflicts were then resolved by hand.

Migration paragraphs in the README are not ported.
A new migration now describes itself in the header comment of its own `.up.sql`.
`The notes below describe migrations through N.` is written by `restructure.ts`, so no side's change to that sentence is ported either.

## Pairs

| Merge | File | Class | Expected | Observed |
| --- | --- | --- | --- | --- |
| 5e45356e68aa | AGENTS | different sentences | clean | clean |
| 4671996e14fd | AGENTS | different sentences | clean | clean |
| 5aee84d6930e | AGENTS | same or adjacent (48 / 49) | conflict | conflict, 1 hunk |
| 56d31191f8f1 | README | different sentences | clean | clean |
| 125a3bfb0484 | README | same or adjacent (399 / 399) | conflict | conflict, 1 hunk |
| 949ebe03bd4e | README | different sentences | clean | clean |
| 728b5e297fb5 (control) | AGENTS | same or adjacent (2087 / 2088) | conflict | conflict, 1 hunk |
| 4440fc7b57a2 (control) | README | same or adjacent (399 / 399) | conflict | conflict, 1 hunk |
| append (a) | README Routes | different items | clean | clean |
| append (b) | AGENTS Where to look | different items | clean | clean |
| append (c, control) | README Routes | same place (after 1451) | conflict | conflict, 1 hunk |

Line numbers are head lines.
Four of the six real pairs merge clean.
The other two still conflict on the new layout, because their two sides meet at the same point:

- In 5aee84d6930e, each side added "Where to look" items right after **Listener behavior**, where those items belong.
- In 125a3bfb0484, both sides rewrote the one sentence that counts the periodic maintenance loops.

The controls conflict as expected.
728b5e297fb5's two sides change the first and second sentences of one item, and git conflicts on adjacent lines.

### 5e45356e68aa (AGENTS; ours 7fca5b7a1958 forward, theirs 12cbe2cdcc62 reverse; base af460a46bb16)

- ours: rewrote the Overview bullet "delivers by hot `prompt_async`" (serve-port push versus self-subscribed sessions, `POST /v1/replay`), restored the `- ` of the next bullet ("publishes retained"), and added a `/v1/replay` row after the `/v1/messages/publish` row.
  New layout: the Overview item at head line 20 becomes two lines plus its own "publishes retained" item, and a `/v1/replay` (POST) item is inserted after the `/v1/messages/publish` item (after line 2429).
- theirs (main): added the "Live topics lane" Where to look row, and edited sentences about room close (`closeRoomInstance`), the architecture importer's sources, `/healthz` `live_topics`, the shutdown drain, and the `/v1/roles/<role>`, `/v1/interests/subscribe` and `/v1/sessions` rows (the stored subscription owns delivery), and the ad-hoc topic bullet (the live topics lane).
  New layout: the patch removes the Live topics lane item (lines 49-53) and puts back those sentences' base wording; on the `/v1/roles/<role>` item it touches line 2432, with lines 2430-2431 between it and ours' insertion.

### 4671996e14fd (AGENTS; ours ca51db66954f reverse, theirs 2e8c68cd98ce reverse; base 448a41003a17)

- ours: added the Critical conventions bullet "The interest reaper (`Registry.Reap`, …)", with no blank line, right after the "Role ownership" bullet.
  New layout: it is the item at lines 1941-1946, and the patch removes it.
- theirs (main): edited the "Role lanes" and "Role ownership" bullets just above it (the role claim watcher), and added and edited sentences across the file, among them the block graph, the Where to look Listener HTTP API and Unplaceable items, and the Listener API rows.
  New layout: the patch undoes them; its nearest change is line 1939, the last line of the Role ownership item, with a blank line between that item and ours'.

### 5aee84d6930e (AGENTS; ours 1ff60404b284 reverse, theirs 748e8d952a76 reverse; base 4ceb3aa9c553)

- ours: added the Where to look rows "Listener HTTP API" and "Unplaceable agent delivery" after "Listener behavior", changed "a lookup answers 500, a soft claim 503" to "a lookup and a soft claim answer 503", and replaced the bullet "If a session is not live …" with the unplaceable-delivery bullets.
  It also edited sentences of the `/v1/messages/send`, `/v1/messages/publish`, `/v1/roles/<role>`, `/v1/interests/<session_id>` and `/v1/registry/<session_id>` rows.
  New layout: the patch removes the two items (lines 41-48) and undoes the sentences; on the `/v1/roles/<role>` item it touches lines 2436-2440.
- theirs (main): added the "Live topics lane" row after "Listener behavior", and edited the `/v1/roles/<role>` row's `machine_id` sentence and the subscribe, sessions, healthz and shutdown sentences that 5e45356e68aa's main side also edited.
  New layout: the patch removes the Live topics lane item (lines 49-53), so it meets ours at lines 48/49; its edit of the `/v1/roles/<role>` item is line 2432, apart from ours' 2436.

### 56d31191f8f1 (README; ours 4ff4dc39319c reverse, theirs df396380c0ef reverse; base 3f34ba120984)

- ours: changed "Six periodic maintenance loops" to "Seven", added the Reference graph paragraph "Each block of a document is a node of the graph too", extended the `rebuild-refs` sentence (`blocks_owed`), and in Routes edited the `issues/{key}/references`, `references`, `issues/{key}/artifacts`, `projects/{key}/artifacts` and `artifacts/{id}/references` rows and added the `/api/v1/block-links` row.
  New layout: line 399 goes from Eight to Seven (one loop fewer, the closest equivalent, since main has counted another loop since), the paragraph at lines 878-895 is removed, and the Routes sentences and the block-links item are undone (lines 1303-1328, 1460-1471, 1562-1563).
  Its migration paragraphs `0118_block_graph` to `0125` are not ported.
  At the head they are `0121_block_graph` to `0128_comments_anchor_block_id_key`, and each of their `.up.sql` header comments describes the migration.
- theirs: added the `DISPATCH_EMBEDDER` configuration row, the "coverage pass runs in every process" clause, the Search paragraphs on the embedder and its coverage pass, and the `/api/v1/search/coverage` route.
  New layout: the patch removes them (lines 105-110, 405, 938-961, 1294-1300).
  Its migration paragraph for `0118_embeddings_pending_since` and `0119_drop_embeddings_backfill_progress` is not ported, and both `.up.sql` headers at the head describe those migrations.

### 125a3bfb0484 (README; ours 7ea8616e1619 forward, theirs 2e8c68cd98ce reverse; base b08a56bce0c8)

- ours: added sentences to the `DISPATCH_TEST_HOOKS` row, rewrote the webhook redelivery rate-limit and claim sentences, and added a redelivery bullet about a killed process's claim.
  In the processes list it replaced "Seven periodic maintenance loops …" with five new bullets before it and "Nine periodic maintenance loops …:" with the list of loops, added three bullets after it, and in Routes added the GitHub rate-limit sentences and "on every process" to the test-hook routes.
  New layout: the same sentences change in place; line 399 is replaced by the five new items and the Nine sentence, and the three items are inserted after line 407.
  Its migration paragraphs `0129_message_delivery_urgency`, `0130_route_roster`, `0131_github_rate_limits` and `0132_delivery_run_warnings` are not ported; the head has no `.up.sql` for any of them, since its own `0129`-`0134` are other migrations.
- theirs (main): "Seven periodic maintenance loops" became "Eight".
  New layout: line 399 goes back from Eight to Seven, the same line ours rewrites.

### 949ebe03bd4e (README; ours cdac19417b8c forward, theirs 2f16634c6f26 forward; base b08a56bce0c8)

- ours: removed the `DISPATCH_ASSET_STORE_BUCKET` row, added the `DISPATCH_DEV_SIGNIN_SHARED_KEY` row and the dev sign-in shared-key sentences, rewrote the Routes paragraph on retained assets, added the `/api/v1/dashboard/builds` route, and appended the processes-list bullet "Each build's dashboard assets are in the database".
  New layout: the same sentences change in place, and that new item is placed next to "A reader's live SSE stream" (inserted after line 395), the other item about what a browser is served, rather than at the end of the list.
  Its migration paragraph `0130_dashboard_assets` is not ported, and the head has no `.up.sql` for it.
  Its "Seven" to "Eight" loop count is the same change theirs makes, and the head already says Eight, so it is in neither patch.
- theirs: rewrote the `DISPATCH_SIGNING_KEY` row (a key set), and added sentences on NATS and OIDC discovery not gating boot, the sign-in state cookie, the refresh confirmation, key rotation, the boot's key check and `/healthz`.
  It also appended four processes-list bullets (the live-process registry, a sign-in completing on the other process, `server.started`, NATS and OIDC not gating boot).
  New layout: the same sentences change in place, and the four items are inserted at the end of the list (after line 407).
  The cookie item grew past one screen, so the patch also starts a sibling item at "Either way, Dispatch keeps the issuer's refresh token", adding no words.
  Its migration paragraph `0129_server_processes` is not ported, and the head has no `.up.sql` for it.

### Controls

- 728b5e297fb5 (AGENTS; ours 43105ca3fa06 forward, theirs 2e8c68cd98ce reverse; base 657c24e44a73): ours changed the Health endpoints bullet's first sentence (", except Dispatch's `GET /livez` …") and one access-log sentence, and theirs (main) changed that bullet's next sentence ("`/healthz` returns `degraded` for a JetStream/KV probe that answered …").
  In the old layout both were on one line; in the new layout they are lines 2087 and 2088, which are adjacent.
- 4440fc7b57a2 (README; ours f509fd0f18df forward, theirs b69f636f2ca1 reverse; base 448a41003a17): both changed the loop-count sentence.
  Ours is ported as Eight to "Nine … not per process" (one loop more, and its wording change); theirs (main) as Eight back to Seven.
  Ours' paragraphs on the ask repair (`api.RepairIssueAsks`, after line 545 and after line 832) are not migration paragraphs, so they are ported.
  Theirs' block-graph migration paragraphs are not ported, for the reason given under 56d31191f8f1.

### Appended items demo

The demo items are invented.
Each has the item shape of its list.

- (a) `append-readme-routes-a`: inserts `/api/v1/issues/{key}/replay-demo-a` after the `/api/v1/issues/{key}/messages` (GET) item.
  `append-readme-routes-b` inserts `/api/v1/artifacts/{id}/replay-demo-b` after the `/api/v1/artifacts/{id}/events` item.
- (b) `append-agents-where-a`: inserts **Replay demo A** after **Webhook config**.
  `append-agents-where-b` inserts **Replay demo B** after **NATS credential**.
- (c) `append-readme-same-a` and `append-readme-same-b` insert `replay-demo-c` and `replay-demo-d` at one point, after the `/api/v1/issues/{key}/messages` (GET) item.

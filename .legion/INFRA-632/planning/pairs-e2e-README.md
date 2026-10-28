# Replay classification: `scripts/e2e/README.md`

Source and method as for the root AGENTS.md file (`conf.txt`, `git merge-file --diff3` on the single merge base).

Merges: 28; layout-caused: 12; not layout-caused (at least one SAME-SENTENCE hunk): 16.

Hunks by cause: ADJACENT 16, APPEND 7, CODE-BLOCK(APPEND) 1, CODE-BLOCK(SAME-SENTENCE) 1, LONG-LINE 3, SAME-SENTENCE 27.

SAME-SENTENCE hunks by kind (clash = same words or fact; clauses = different clauses of one long sentence; enum = different items of one hand-written enumeration; rewrite = main rewrote or moved the text the branch edited; dup = both sides carry two versions of the same new text): clash 20, clauses 4, enum 4.

Line numbers are base (merge-base) line numbers of that merge. `[auto]` marks a hunk labelled from the script's sentence diff without a manual read (only in merges already decided by another hunk).

## Every merge

### 54ddc86a3743 — PR #2059 (legion/LEGION-781, 2026-10-19) — not layout-caused

base `9e9b2c493fd1`, ours (branch parent) `ad724c5fc7a0`, theirs (main parent) `a5b302e42403`

- hunk 1: **APPEND** — base L403-405 (3 lines, longest 97 chars). L403-405: both insert a new sentence after "…in the architect's session where they are written." (ours the final-review-cycle sentences, theirs the `await_tree_suspended` bound sentence)
- hunk 2: **SAME-SENTENCE** — base L1119-1122 (4 lines, longest 2590 chars). [clauses] stage4b step L1119 sentence: ours moves the "merger's READY … hold" clause out, theirs edits "within 300 s" in the same sentence

### 9164e75b4c3c — PR #2024 (e2e/stage4a-gke, 2026-10-19) — not layout-caused

base `5c07a93847ef`, ours (branch parent) `4eb48dee5c3b`, theirs (main parent) `920e2b2e94e4`

- hunk 1: **SAME-SENTENCE** — base L295-297 (3 lines, longest 99 chars). [clash] L295: both reword "model is Anthropic through the Hawk model gateway, the route every devbox agent session uses:"

### bc1c2593a9cf — PR #2024 (e2e/stage4a-gke, 2026-10-19) — LAYOUT-CAUSED

base `098c3cc7387a`, ours (branch parent) `7afd5d58123d`, theirs (main parent) `c13a98917e34`

- hunk 1: **ADJACENT** — base L633-636 (4 lines, longest 328 chars). stage4a inputs table L633-636: ours inserts three rows before L633 and edits rows L633-636; theirs appends the `LEGION_E2E_REPO_SECOND_OWNER` row after L636

### daf1b822b209 — PR #2024 (e2e/stage4a-gke, 2026-10-19) — not layout-caused

base `c13a98917e34`, ours (branch parent) `1d64ffb61948`, theirs (main parent) `5c07a93847ef`

- hunk 1: **SAME-SENTENCE** — base L690-690 (1 lines, longest 647 chars). [clauses] L690: ours "EKS's" -> "the cluster's", theirs "plan's;" and "two positive controls" clauses

### dc0bd2f61787 — PR #2024 (e2e/stage4a-gke, 2026-10-19) — not layout-caused

base `920e2b2e94e4`, ours (branch parent) `f4c7d32c3497`, theirs (main parent) `6255cd241584`

- hunk 1: **SAME-SENTENCE** — base L694-694 (1 lines, longest 774 chars). [clauses] L694: ours "EKS's" -> "the cluster's", theirs "`services` get, list" clause

### 141989f72b63 — PR #2074 (boot-reconcile-retry-handoff, 2026-10-20) — LAYOUT-CAUSED

base `3442ab023b9f`, ours (branch parent) `a67cadd13fb4`, theirs (main parent) `61552da1b1b7`

- hunk 1: **APPEND** — base insertion before L2290 (0 lines, longest 0 chars). both add a new lib section at the same point (before base L2290): ours `## lib/kind-sandbox.sh`, theirs `## lib/bridge-tunnel.sh`

### f714cec148b6 — PR #2085 (legion/INFRA-31, 2026-10-21) — LAYOUT-CAUSED

base `61552da1b1b7`, ours (branch parent) `7207e4db3483`, theirs (main parent) `2127101bddff`

- hunk 1: **ADJACENT** — base L899-899 (1 lines, longest 1344 chars). stage4a checks table: ours edits row L899 (`pod-baseline`: `XDG_STATE_HOME`, `TMPDIR` items), theirs inserts the new `setup-ran` row right after it

### 29b2d894bb65 — PR #2150, #2259 (sami/single-container-pod, 2026-10-25) — not layout-caused

base `531ba202f6d2`, ours (branch parent) `a636841acffe`, theirs (main parent) `4792c8fa421f`

- hunk 1: **SAME-SENTENCE** — base L1464-1472 (9 lines, longest 3939 chars). [clash] stage4b steps L1464-1472: theirs deletes the mid-turn hold sentences ours edits ("in its container" -> "in its namespaces")
- hunk 2: **ADJACENT** — base L1474-1475 (2 lines, longest 2124 chars). L1474 (ours) and L1475 (theirs), adjacent rows

### 990ebd7341e0 — PR #2186 (legion/INFRA-220, 2026-10-25) — LAYOUT-CAUSED

base `371d6c879565`, ours (branch parent) `40a963e258e5`, theirs (main parent) `f2c17ded5305`

- hunk 1: **ADJACENT** — base L655-656 (2 lines, longest 97 chars). claude-backend checks list: theirs extends the last sentence of the session item (L655-656, the bridge-parent rule), ours appends the new item `stop-hook-reminds-the-open-phase` right after it (no blank line between items)

### c99701afc1e7 — PR #2173 (legion/INFRA-80, 2026-10-25) — not layout-caused

base `9e62d7451f2c`, ours (branch parent) `7609fd93fdc2`, theirs (main parent) `14571f6f4aea`

- hunk 1: **ADJACENT** — base L1102-1103 (2 lines, longest 95 chars). hard-wrapped L1102-1103, adjacent sentences
- hunk 2: **SAME-SENTENCE** — base L1260-1269 (10 lines, longest 98 chars). [clash] L1260-1269 child-4 steps rewritten by both
- hunk 3: **SAME-SENTENCE** — base L1464-1473 (10 lines, longest 3939 chars). [clash] L1464-1473 stage4b steps rewritten by both
- hunk 4: **SAME-SENTENCE** — base L1475-1475 (1 lines, longest 551 chars). [clash] L1475 "The resume that finds the issue's volume lost…"

### 2479de885900 — PR #2186 (legion/INFRA-220, 2026-10-26) — not layout-caused

base `f2c17ded5305`, ours (branch parent) `c481e666f8cc`, theirs (main parent) `8298fc710323`

- hunk 1: **ADJACENT** — base L19-19 (1 lines, longest 474 chars). index table: theirs inserts the `dispatch-session-settings.sh` row before row L19 (`claude-backend-tmux.sh`), ours edits row L19 and adds the `claude-backend-sandbox.sh` row after it
- hunk 2: **SAME-SENTENCE** — base L679-682 (4 lines, longest 100 chars). [clash] L679: both reword "before anything else," (ours "right after the route below and before any other tool,", theirs "before it writes anything,")
- hunk 3: **SAME-SENTENCE** — base L722-734 (13 lines, longest 97 chars). [clash] L722-734 port section: both rewrite "Each stage needs exactly two of the six, never three —…" and "With six admitted ports and two per lane…"

### 2a7503c142cb — PR #2176 (e2e-key-file-model-route, 2026-10-26) — LAYOUT-CAUSED

base `5c182bd16f0a`, ours (branch parent) `3c36b9040b6c`, theirs (main parent) `748e8d952a76`

- hunk 1: **ADJACENT** — base L667-678 (12 lines, longest 100 chars). hard-wrapped claude-backend inputs paragraph L667-678: ours rewrites sentences on L667-668, L670-673, L675-677; theirs extends "So no pane's environment carries `ANTHROPIC_API_KEY`…" on L673-675, between two of ours (adjacent sentences)

### 60a69246a3d0 — PR #2176 (e2e-key-file-model-route, 2026-10-26) — not layout-caused

base `748e8d952a76`, ours (branch parent) `2a7503c142cb`, theirs (main parent) `7f6ef46fd42f`

- hunk 1: **SAME-SENTENCE** — base L294-303 (10 lines, longest 96 chars). [clash] L294-295: both edit "the `secrets` CLI, and `LEGION_E2E_MODEL_ROUTE` (required…)"
- hunk 2: **ADJACENT** — base L332-341 (10 lines, longest 146 chars). hard-wrapped L332-341, adjacent sentences
- hunk 3: **SAME-SENTENCE** — base L608-611 (4 lines, longest 98 chars). [clash] L608-611: both rewrite "with the same two required inputs and the same proof human… route only"
- hunk 4: **ADJACENT** — base L678-690 (13 lines, longest 100 chars). hard-wrapped L678-690: theirs inserts before/edits L679-681, ours rewrites L678-679 and later sentences (adjacent)

### 696a567ae5e2 — PR #2229 (legion/INFRA-253, 2026-10-26) — LAYOUT-CAUSED

base `81a3b5ef7b47`, ours (branch parent) `8df2ce72fe81`, theirs (main parent) `8f3d4237a78c`

- hunk 1: **ADJACENT** — base L1968-1968 (1 lines, longest 19 chars). theirs inserts the `## no-required-checks.sh` section directly before `## lib/free-port.sh` (L1968), the heading ours renames to `## lib/held-port.py`

### 82fd7d27b4f2 — PR #2186 (legion/INFRA-220, 2026-10-26) — not layout-caused

base `6cdb99959fa1`, ours (branch parent) `b1d8a21b1e8f`, theirs (main parent) `b4bea3858263`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L706-706 (1 lines, longest 113 chars). [clash] claude-backend usage L706: both add `LEGION_E2E_MODEL_ROUTE=gateway`
- hunk 2: **CODE-BLOCK(APPEND)** — base insertion before L708 (0 lines, longest 0 chars). both add a line after L707 in the same usage block
- hunk 3: **SAME-SENTENCE** — base L747-752 (6 lines, longest 103 chars). [clash] L748: "refuses a run without it, naming the variable," reworded by both
- hunk 4: **SAME-SENTENCE** — base L756-759 (4 lines, longest 98 chars). [clash] theirs deletes the L756 `LEGION_E2E_MODEL_GATEWAY_URL` sentence that ours rewrites

### 98840a82bcf8 — PR #2229 (legion/INFRA-253, 2026-10-26) — not layout-caused

base `bc307fb5675e`, ours (branch parent) `d533aa592907`, theirs (main parent) `0587774a744f`

- hunk 1: **SAME-SENTENCE** — base L298-299 (2 lines, longest 96 chars). [enum] "The run needs `go`, `docker`, …" L298-299: ours `ss` -> `python3`, theirs the `secrets` CLI item's parenthesis

### 9a8af2f1743e — PR #2225 (legion/INFRA-278, 2026-10-26) — LAYOUT-CAUSED

base `a6fa12112d16`, ours (branch parent) `41d4aba56b35`, theirs (main parent) `d1f4b8a64a96`

- hunk 1: **APPEND** — base insertion before L25 (0 lines, longest 0 chars). index table: both insert a row after the last script row, `merged-root-waits.sh` (L24), before the `TestRealGitHub…` rows: ours `dispatch-claimant-delivery.sh`, theirs `no-required-checks.sh`
- hunk 2: **APPEND** — base insertion before L1967 (0 lines, longest 0 chars). both add a script section at the same point, after the `merged-root-waits.sh` section and directly before `## lib/free-port.sh`: ours `## dispatch-claimant-delivery.sh`, theirs `## no-required-checks.sh`

### a3dd8e7a0730 — PR #2150, #2259 (sami/single-container-pod, 2026-10-26) — not layout-caused

base `12cbe2cdcc62`, ours (branch parent) `b54364dbd7de`, theirs (main parent) `7f6ef46fd42f`

- hunk 1: **SAME-SENTENCE** — base L16-16 (1 lines, longest 1341 chars). [clauses] index row L16 (stage4a, 1,341 chars): ours reservation clauses, theirs session-store/disk clauses of the same sentence
- hunk 2: **LONG-LINE** — base L990-990 (1 lines, longest 441 chars). L990 (441 chars), adjacent sentences [auto]
- hunk 3: **ADJACENT** — base L1000-1001 (2 lines, longest 246 chars). L1000/L1001 [auto]
- hunk 4: **SAME-SENTENCE** — base L1004-1005 (2 lines, longest 234 chars). [clash] L1005 kill-launcher row: ours rewrites it for role-init, theirs edits its end
- hunk 5: **LONG-LINE** — base L1013-1018 (6 lines, longest 1344 chars). L1013-1018 [auto]
- hunk 6: **SAME-SENTENCE** — base L1213-1213 (1 lines, longest 98 chars). [clash] L1213 "each issue pod owns its volume and reserves its six roles' summed cpu and memory"
- hunk 7: **ADJACENT** — base L1481-1484 (4 lines, longest 1458 chars). L1481 vs L1482-1484 [auto]
- hunk 8: **ADJACENT** — base L1500-1503 (4 lines, longest 2365 chars). L1500-1503 [auto]
- hunk 9: **ADJACENT** — base L1515-1523 (9 lines, longest 3406 chars). L1515-1523 [auto]

### a412c2c4a921 — PR #2229 (legion/INFRA-253, 2026-10-26) — not layout-caused

base `8f3d4237a78c`, ours (branch parent) `696a567ae5e2`, theirs (main parent) `a13a0366bfa9`

- hunk 1: **SAME-SENTENCE** — base L293-294 (2 lines, longest 96 chars). [enum] "The run needs …" L293-294: ours `ss` -> `python3`, theirs the `secrets` CLI item

### b1d8a21b1e8f — PR #2186 (legion/INFRA-220, 2026-10-26) — not layout-caused

base `8411fe855388`, ours (branch parent) `8c83c9a21c80`, theirs (main parent) `6cdb99959fa1`

- hunk 1: **SAME-SENTENCE** — base L9-9 (1 lines, longest 176 chars). [enum] sourced-lib list L9: ours adds `lib/session-db.sh` after `lib/namespace-rig.sh`, theirs adds `lib/box-login.sh` at the end

### b2bf8bb2f141 — PR #2186 (legion/INFRA-220, 2026-10-26) — not layout-caused

base `84328810dd2a`, ours (branch parent) `fe493b813703`, theirs (main parent) `8411fe855388`

- hunk 1: **SAME-SENTENCE** — base L3048-3048 (1 lines, longest 91 chars). [clash] L3048 "Each defaults to this repository's own deployment's value…"
- hunk 2: **SAME-SENTENCE** — base L3070-3071 (2 lines, longest 100 chars). [clash] L3070-3071 "…read the same four inputs with the same defaults."

### b54364dbd7de — PR #2150, #2259 (sami/single-container-pod, 2026-10-26) — LAYOUT-CAUSED

base `135d22e0f63b`, ours (branch parent) `a5679ca94368`, theirs (main parent) `12cbe2cdcc62`

- hunk 1: **APPEND** — base L900-900 (1 lines, longest 555 chars). stage4a inputs table: both insert rows before row L900 (ours `LEGION_E2E_PREVIOUS_IMAGE`, theirs the implement App id/key rows); ours also edits row L900 ("`kill-launcher`'s" -> "`kill-role-init`'s")

### dadb5f515a3e — PR #2234 (legion/INFRA-448, 2026-10-26) — LAYOUT-CAUSED

base `135d22e0f63b`, ours (branch parent) `63d4f066ff01`, theirs (main parent) `8298fc710323`

- hunk 1: **LONG-LINE** — base L1489-1495 (7 lines, longest 3406 chars). stage4b steps row/item L1490 (3,406 chars): ours rewrites "The proof sends it nothing there: the issue is closed, and a closed issue takes no Dispatch write (409 `ISSUE_CLOSED`), so…" (sentence 15), theirs the child-park sentence (11) and "The pending report suspends the implementer's claim…" (17), plus L1489, L1491-1495

### e0ab76dcd45f — PR #2150, #2259 (sami/single-container-pod, 2026-10-26) — LAYOUT-CAUSED

base `bc307fb5675e`, ours (branch parent) `48898be0dc12`, theirs (main parent) `0587774a744f`

- hunk 1: **ADJACENT** — base L929-931 (3 lines, longest 555 chars). stage4b inputs table L929-931: ours inserts the `LEGION_E2E_PREVIOUS_IMAGE` row before L929 and edits L931; theirs edits the default cells of L929-930 ("this deployment's" -> "the default pair's")

### ee9ec60b7930 — PR #2226 (legion/INFRA-252, 2026-10-26) — LAYOUT-CAUSED

base `002a4ee98749`, ours (branch parent) `55e34c2565b7`, theirs (main parent) `63cd684a8256`

- hunk 1: **APPEND** — base insertion before L25 (0 lines, longest 0 chars). index table: both insert a row after the last script row, `merged-root-waits.sh` (L24), before the `TestRealGitHub…` rows: ours `child-adoption.sh`, theirs `no-required-checks.sh`
- hunk 2: **APPEND** — base insertion before L1967 (0 lines, longest 0 chars). both add a script section directly before `## lib/free-port.sh`: ours `## child-adoption.sh`, theirs `## no-required-checks.sh`

### 3ac0f1f0ca6e — PR #2186 (legion/INFRA-220, 2026-10-27) — not layout-caused

base `448a41003a17`, ours (branch parent) `337d8e587665`, theirs (main parent) `b08a56bce0c8`

- hunk 1: **SAME-SENTENCE** — base L826-830 (5 lines, longest 98 chars). [clash] L826-830: ours `secrets` -> `<runner>` (`LEGION_E2E_APP_KEY_RUNNER`), theirs `secrets` -> `agent-secrets` in the same sentence
- hunk 2: **SAME-SENTENCE** — base L3265-3265 (1 lines, longest 169 chars). [clash] L3265 table cell: "the secret-store key the `secrets` CLI resolves…" reworded by both
- hunk 3: **SAME-SENTENCE** — base L3267-3269 (3 lines, longest 98 chars). [clash] L3267 "Each defaults to the pair the live proofs have always used…" reworded by both
- hunk 4: **SAME-SENTENCE** — base L3278-3278 (1 lines, longest 99 chars). [clash] L3278 "runs `secrets <name> -- sh -c …`" -> `<runner>` / `agent-secrets`

### aef917661d13 — PR #2229 (legion/INFRA-253, 2026-10-27) — not layout-caused

base `baadccdc6a9a`, ours (branch parent) `dbb234ae3cfa`, theirs (main parent) `b08a56bce0c8`

- hunk 1: **SAME-SENTENCE** — base L350-352 (3 lines, longest 96 chars). [enum] "The run needs …" L350-352: ours `ss` -> `python3`, theirs the `secrets` CLI item ("the `agent-secrets` client…")

### f95c4f77352a — PR #2226 (legion/INFRA-252, 2026-10-27) — LAYOUT-CAUSED

base `b6d8fa2ac33c`, ours (branch parent) `c2fefc336831`, theirs (main parent) `c3d09eec924c`

- hunk 1: **ADJACENT** — base L25-25 (1 lines, longest 1022 chars). index table: theirs edits the last row L25 (`merged-root-waits.sh`), ours appends the `child-adoption.sh` row after it

## Proposed replay pairs

Twelve merges are layout-caused. The pairs below take nine of them plus two SAME-SENTENCE controls. Expected results assume the index table and the inputs/checks tables become lists with a blank line between items, and that a new item or section is added next to the item it belongs with rather than at the end (INFRA-631's rule); where both sides put the new item at the same point, the replay applies each side's item next to its own neighbour, as that rule says.

Enumeration candidates, not layout-caused by this task's definition (SAME-SENTENCE [enum]) but clean if the plan turns the enumeration into a list: `98840a82bcf8`, `a412c2c4a921`, `aef917661d13` ("The run needs `go`, `docker`, …": ours `ss` -> `python3`, theirs the `secrets` CLI item) and `b1d8a21b1e8f` (the sourced-lib list at L9: ours adds `lib/session-db.sh` after `lib/namespace-rig.sh`, theirs adds `lib/box-login.sh` at the end).

### INDEX (two new rows + two new sections): 9a8af2f1743e — PR #2225

Both add an index row after `merged-root-waits.sh` and a section before `## lib/free-port.sh` (ours `dispatch-claimant-delivery.sh`, theirs `no-required-checks.sh`). Replay: ours' item and section go next to the other `dispatch-*` proofs, theirs' next to `merged-root-waits.sh`. Expected: clean.

- merge `9a8af2f1743e`; base `a6fa12112d16`; ours `41d4aba56b35`; theirs `d1f4b8a64a96`; hunk causes: APPEND, APPEND
- hunk 1 (insertion between base L24 «\| `merged-root-waits.sh` \| the daemon keeps a tree's order, driven through the w» and L25 «\| `TestRealGitHubCredentialSurface` \| the real `api.NewServer`'s App leases, ren»):
  - ours inserts: «\| `dispatch-claimant-delivery.sh` \| Dispatch's outbox delivers a person's answer, comment or untargeted message on an issue a session has claimed to that session's own `notifications.agent.<session>`, driven over HTTP through the real `envoy-dispatch` against a loopback PostgreSQL the caller names and a NATS of the run's own: a person's answer to an ask another session opened reaches the asker and the claimant, a person's reply in a person's thread on the issue's spec and a person's untargeted m»
  - theirs inserts: «\| `no-required-checks.sh` \| a pull request whose base branch requires no check and no workflow has no CI verdict, driven through the whole daemon `legion start` boots, over a PostgreSQL the caller names and a `nats-server` of the run's own, with GitHub's settlements, push and synchronize as its own envelopes and what each base requires read by the daemon's own required-checks pass from a GitHub stand-in the driver switches: a failed check on a base that requires nothing leaves no `checksVerdict`»
- hunk 2 (insertion between base L1966 «which the driver writes through `LEGION_E2E_DAEMON_LOG`.» and L1967 «»):
  - ours inserts: «## dispatch-claimant-delivery.sh / ```sh / DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \ /   [DISPATCH_BIN=<envoy-dispatch>] bash scripts/e2e/dispatch-claimant-delivery.sh     # → "dispatch claimant delivery e2e: PASS in <n>s", exit 0 / ``` / Runs on any Linux host with `go`, `bun`, `curl`, `psql` and `openssl` — no Docker. It proves / `publishClaimantRoute` (`packages/envoy/internal/dispatch/outbox/publisher.go`) on the wire: the / real `envoy-dispatch` publishes »
  - theirs inserts: «## no-required-checks.sh / ```sh / LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \ /   [LEGION_E2E_NATS_BIN=<nats-server>] bash scripts/e2e/no-required-checks.sh     # → "no required checks e2e: PASS in <n>s", exit 0 / ``` / Runs on any Linux host with `go` — no Docker, no cluster. `LEGION_TEST_PG_DSN` is required and / names the PostgreSQL the daemon migrates and runs on; the script starts none, so CI's service, a / devbox container and a pod's own embedded Postg»

### INDEX (two new rows + two new sections): ee9ec60b7930 — PR #2226

Same shape (ours `child-adoption.sh`, theirs `no-required-checks.sh`). Replay: ours next to `merged-root-waits.sh`'s whole-daemon siblings, theirs likewise but on the other side of it. Expected: clean.

- merge `ee9ec60b7930`; base `002a4ee98749`; ours `55e34c2565b7`; theirs `63cd684a8256`; hunk causes: APPEND, APPEND
- hunk 1 (insertion between base L24 «\| `merged-root-waits.sh` \| the daemon keeps a tree's order, driven through the w» and L25 «\| `TestRealGitHubCredentialSurface` \| the real `api.NewServer`'s App leases, ren»):
  - ours inserts: «\| `child-adoption.sh` \| a labelled root still queued in admission, which a person then makes a child of a live tree, joins that tree and runs there, driven through the whole daemon `legion start` boots, over a PostgreSQL the caller names and a `nats-server` of the run's own: Dispatch's re-parent event takes it out of `admission.waiting` and into the tree, planning with no slot and no architect of its own; `release_children` adopts one the daemon saw no re-parent of before it answers, from todo, »
  - theirs inserts: «\| `no-required-checks.sh` \| a pull request whose base branch requires no check and no workflow has no CI verdict, driven through the whole daemon `legion start` boots, over a PostgreSQL the caller names and a `nats-server` of the run's own, with GitHub's settlements, push and synchronize as its own envelopes and what each base requires read by the daemon's own required-checks pass from a GitHub stand-in the driver switches: a failed check on a base that requires nothing leaves no `checksVerdict`»
- hunk 2 (insertion between base L1966 «which the driver writes through `LEGION_E2E_DAEMON_LOG`.» and L1967 «»):
  - ours inserts: «## child-adoption.sh / ```sh / LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \ /   [LEGION_E2E_NATS_BIN=<nats-server>] bash scripts/e2e/child-adoption.sh     # → "child adoption e2e: PASS in <n>s", exit 0 / ``` / Runs on any Linux host with `go` — no Docker, no cluster. `LEGION_TEST_PG_DSN` is required and / names the PostgreSQL the daemon migrates and runs on; the script starts none, so CI's service, a / devbox container and a pod's own embedded PostgreSQL all sa»
  - theirs inserts: «## no-required-checks.sh / ```sh / LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \ /   [LEGION_E2E_NATS_BIN=<nats-server>] bash scripts/e2e/no-required-checks.sh     # → "no required checks e2e: PASS in <n>s", exit 0 / ``` / Runs on any Linux host with `go` — no Docker, no cluster. `LEGION_TEST_PG_DSN` is required and / names the PostgreSQL the daemon migrates and runs on; the script starts none, so CI's service, a / devbox container and a pod's own embedded Postg»

### INDEX (edit last row + append row): f95c4f77352a — PR #2226

Theirs edits the `merged-root-waits.sh` row (L25), ours appends the `child-adoption.sh` row after it. Expected: clean once a blank line separates the items.

- merge `f95c4f77352a`; base `b6d8fa2ac33c`; ours `c2fefc336831`; theirs `c3d09eec924c`; hunk causes: ADJACENT
- hunk 1 (base L25-25):
  - ours: insert after base L25 (ending «ree suspended, the slot freed and the root's production check open on the root's merged pull request»): «`child-adoption.sh` / a labelled root still queued in admission, which a person then makes a child of a live tree, joins that tree and runs there, driven through the whole daemon `legion start` boots, over a PostgreSQL the caller names and a `nats-server` of the run's own: / Dispatch's re-parent event takes it out of `admission.waiting` and into the tree, planning with no slot and no architect of …»
  - theirs: base L25: «the daemon keeps a tree's order, driven through the whole daemon `legion start` boots, over a PostgreSQL the caller names and a `nats-server` of the run's own: a `sign_off` of a root with a child in the workflow is refused 409 `SIGNOFF_CHILD_IN_WORKFLOW` naming…» -> «the daemon keeps a tree's order at the close, driven through the whole daemon `legion start` boots, over a PostgreSQL the caller names and a `nats-server` of the run's own: a `sign_off` of a root with a child in the workflow lands, the child untouched; the root…»; all changes: `the daemon keeps a tree's [-order,-] {+order at the close,+} driven through the whole … child in the workflow [-is refused 409 `SIGNOFF_CHILD_IN_WORKFLOW` naming-] {+lands,+} the [-child, and lands once that-] child [-is parked;-] {+untouched;+} the root's merge, as … `

### ADJACENT (list item): 990ebd7341e0 — PR #2186

Theirs extends the last sentence of one claude-backend check item (L655-656); ours adds a new item right after it with no blank line between. Expected: clean with a blank line between items.

- merge `990ebd7341e0`; base `371d6c879565`; ours `40a963e258e5`; theirs `f2c17ded5305`; hunk causes: ADJACENT
- hunk 1 (base L655-656):
  - ours: insert after base L656 (ending «sion; its `claude` runs as a child of `legion claude-bridge`, started with `--session-id <that id>`.»): «- `stop-hook-reminds-the-open-phase`: told to answer in one line of prose and call nothing, the implementer ends a turn with `implementing` still open, so the pane's `legion` MCP server answers Claude Code's `Stop` hook with `{"decision":"block","reason":…}` (`legion_phase_hook`, the hook entries of `packages/claude-envoy/legion/hooks/hooks.json`, the endpoint `packages/claude-envoy/src/legion-pha…»
  - theirs: base L655-656: «  session id as its session; its `claude` runs as a child of `legion claude-bridge`, started with `--session-id <that id>`.» -> «  session id as its session; its `claude` runs as a child of the run's `legion claude-bridge`, started with `--session-id <that id>`. The parent counts as the bridge only when its executable (`/proc/<pid>/exe`) is the run's `legion` and its arguments, read whole from the NUL-delimited `/pro…»; all changes: ` … as a child of {+the run's+} `legion claude-bridge`, started with `--session-id <that id>`. {+The parent counts as the bridge only when its executable (`/proc/<pid>/exe`) is the run's `legion` and its arguments, read whole from the NUL-delimited `/proc/<pid>/cmdline` in the shell, are `claude-bridge`, `--` and the run's `claude`, in that order, after the executable's name: no other word of the command line counts, the pane's system prompt (past 64 KB, with newlines) included, and no pipe can end early on it. A failure prints the claude's ancestry.+}`

### ADJACENT (table row): f714cec148b6 — PR #2085

Ours edits the `pod-baseline` checks row (L899, 1,344 chars), theirs inserts the `setup-ran` row after it. Expected: clean once rows are items separated by blank lines.

- merge `f714cec148b6`; base `61552da1b1b7`; ours `7207e4db3483`; theirs `2127101bddff`; hunk causes: ADJACENT
- hunk 1 (base L899-899):
  - ours: base L899: «`PI_CONFIG_DIR=.omp`, `OMP_SESSION_STORAGE=file`, `XDG_STATE_HOME=/home/legion/.local/state/architect` (the root claim's role: a state home per role container, so Oh My Pi's browser broker lock, an abstract unix socket named from a path under the state root, is one name per container of the pod r…» -> «`PI_CONFIG_DIR=.omp`, `OMP_SESSION_STORAGE=file`, `XDG_STATE_HOME=/legion/scratch/state/architect` (the root claim's role: the state home is the one directory of the issue's scratch a role has to itself, so Oh My Pi's browser broker lock, an abstract unix socket named from a path under the state …»; all changes: ``PI_CONFIG_DIR=.omp`, `OMP_SESSION_STORAGE=file`, [-`XDG_STATE_HOME=/home/legion/.local/state/architect`-] {+`XDG_STATE_HOME=/legion/scratch/state/architect`+} (the root claim's role: [-a-] {+the+} state home [-per-] {+is the one directory of the issue's scratch a+} role [-container,-] {+has to itself,+} so Oh My Pi's … than one across it), {+`TMPDIR=/legion/scratch/tmp` (the issue's own temporary directory, the one every role of the pod is told: the runtime names it, so an agent falling back to the image's `/tmp` is the regression this value catches),+} and the operator's other … (no baseline`
  - theirs: insert after base L899 (ending «operator overlay setting the endpoint named after the agent's overlays reads the operator's endpoint»): «`setup-ran` / the shared runtime's `runtime.kubernetes.setup` runs before every successful role stub agent: each record in the issue marker precedes that role's own `<role>:<pod uid>/<generation>` record; it ran in the agent's resolved working directory, as the pod's configured uid, and its parent pid became the agent through the wrapper's later `exec`. / Every git the setup runs starts under `env…»

### ADJACENT (table row): bc1c2593a9cf — PR #2024

Stage 4a inputs table: ours inserts three rows before L633 and edits rows L633-636; theirs appends the `LEGION_E2E_REPO_SECOND_OWNER` row after L636. Expected: clean when theirs' item follows L636's item across a blank line.

- merge `bc1c2593a9cf`; base `098c3cc7387a`; ours `7afd5d58123d`; theirs `c13a98917e34`; hunk causes: ADJACENT
- hunk 1 (base L633-636):
  - ours: insert before base L633 (start of hunk): «`LEGION_E2E_POD_REACHABILITY` / required / `aws-private-ip` (the devbox's private address, directly routable) or `bridge` (a reverse tunnel through a scratch pod+Service of the run's own, for a cluster whose pods cannot reach the devbox at all; / [above](#the-bridge-legion_e2e_pod_reachabilitybridge)) / `LEGION_E2E_MODEL_ROUTE` / required / `gateway` (a model gateway, today's shape) or `key-file` …»
  - ours: base L633: «required» -> «required under `LEGION_E2E_MODEL_ROUTE=gateway`, unused under `key-file`»
  - ours: base L634: «required» -> «required under `LEGION_E2E_MODEL_ROUTE=gateway`, unused under `key-file`»
  - ours: base L635: «the port the harness binds for the worker stream on the devbox's private address; the same name and default Stage 4b reads for its own worker-stream port, so two runs — two Stage 4a's, or a Stage 4a and a Stage 4b — collide on it by default and need distinct values to run at on…» -> «the port the harness binds for the worker stream; the same name and default Stage 4b reads for its own worker-stream port, so two runs — two Stage 4a's, or a Stage 4a and a Stage 4b — collide on it by default and need distinct values to run at once»; all changes: `the port the harness binds for the worker [-stream on the devbox's private address;-] {+stream;+} the same name and … `
  - ours: base L636: «a port on the devbox's private address that the devbox's security group admits from the Legion nodes, as it admits the worker-stream port, where the scratch session database listens; refused when it is not a number, is the worker stream'…» -> «a port the scratch session database listens on, reachable everywhere the worker-stream port is (under `aws-private-ip`, one the devbox's security group admits from the Legion nodes; under `bridge`, forwarded through the same tunnel); ref…»; all changes: `a port [-on-] the [-devbox's private address that-] {+scratch session database listens on, reachable everywhere the worker-stream port is (under `aws-private-ip`, one+} the devbox's security group admits from the Legion [-nodes, as it admits-] {+nodes; under `bridge`, forwarded through+} the [-worker-stream port, where the scratch session database listens;-] {+same tunnel);+} refused when it is … `
  - theirs: insert after base L636 (ending « session database listens; refused when it is not a number, is the worker stream's port, or is taken»): «`LEGION_E2E_REPO_SECOND_OWNER` / required / `<owner>/<name>`, a repository under an owner other than `sjawhar` on which both Legion Apps are installed, where `second-owner` provisions an issue; refused when unset, not `<owner>/<name>`, or under `sjawhar`. / Nothing is pushed to it»

### ADJACENT (table rows): e0ab76dcd45f — PR #2150, #2259

Stage 4b inputs: ours adds the `LEGION_E2E_PREVIOUS_IMAGE` row before L929 and edits L931; theirs edits the default cells of L929-930. Expected: clean only if ours' new item is placed away from the L929 item (next to a related input); inserted directly before it, it touches theirs' changed line.

- merge `e0ab76dcd45f`; base `bc307fb5675e`; ours `48898be0dc12`; theirs `0587774a744f`; hunk causes: ADJACENT
- hunk 1 (base L929-931):
  - ours: insert before base L929 (start of hunk): «`LEGION_E2E_PREVIOUS_IMAGE` / required / a worker image, by digest, built from a commit of `main` at daemon API contract 37 (the pod-owned disk, whose harness records each claim's session for the fixture) or 38 (the blocked issues, the same harness) and before one container (its issue pods run six role containers) that this checkout contains; refused when not pinned by digest, when its `org.openco…»
  - ours: base L931: «…ent entry point: any check after `identity` except `stale-incarnation`, which rides `kill-launcher`'s relaunch; the harness refuses any other name at `identity`, before it creates anything.» -> «…ent entry point: any check after `identity` except `stale-incarnation`, which rides `kill-role-init`'s relaunch; the harness refuses any other name at `identity`, before it creates anything.»; all changes: ` … except `stale-incarnation`, which rides [-`kill-launcher`'s-] {+`kill-role-init`'s+} relaunch; the harness refuses … `
  - theirs: base L929: «this deployment's implement App» -> «the default pair's implement App»; all changes: `[-this deployment's-] {+the default pair's+} implement App`
  - theirs: base L930: «this deployment's implement App key» -> «the default pair's implement App key»; all changes: `[-this deployment's-] {+the default pair's+} implement App key`

### LONG-LINE: dadb5f515a3e — PR #2234

Stage 4b steps item L1490 (3,406 chars): ours rewrites the "The proof sends it nothing there…" sentence; theirs rewrites the sentences one before and one after plus six other lines. Expected: clean under one sentence per line (one unchanged sentence on each side).

- merge `dadb5f515a3e`; base `135d22e0f63b`; ours `63d4f066ff01`; theirs `8298fc710323`; hunk causes: LONG-LINE
- hunk 1 (base L1489-1495):
  - ours: base L1490: «The proof sends it nothing there: the issue is closed, and a closed issue takes no Dispatch write (409 `ISSUE_CLOSED`), so the fixture's instructions say a check task waits for no targeted message and acts on the daemon's task as written — this proof has no prod…» -> «The proof sends it nothing there, so the fixture's instructions say a check task waits for no targeted message and acts on the daemon's task as written — this proof has no production and no deploy role, so the implementer leaves `Production: pending deploy of <m…»; all changes: `The proof sends it nothing [-there: the issue is closed, and a closed issue takes no Dispatch write (409 `ISSUE_CLOSED`),-] {+there,+} so the fixture's instructions … `
  - theirs: base L1489: «…legion claims list --json`), the state view shows it at generation 1 in planning, and its one PVC is Bound.» -> «…legion claims list --json`), the state view shows it at generation 1 in planning, and its pod's own disk (`<pod>-issue`, owned by the pod) is Bound.»; all changes: ` … in planning, and its [-one PVC-] {+pod's own disk (`<pod>-issue`, owned by the pod)+} is Bound.`
  - theirs: base L1490: «…he children one at a time (`backlog`, each leaving the workflow with its planner suspended): child 3's park leaves the root in `awaiting_merge`, and child 4's — the last member of the tree to leave — closes it, as the merge of a finished tree's root does.» -> «…he children one at a time (`backlog`, each leaving the workflow with its planner suspended, and within 600 s with no Sandbox, no pod and no disk: the park deletes the child's Sandbox as a close does, and the disk goes with the pod, read inside the same wait, `child_parked`): child 3's park leaves the root in `awaiting_…»; all changes: ` … workflow with its planner [-suspended):-] {+suspended, and within 600 s with no Sandbox, no pod and no disk: the park deletes the child's Sandbox as a close does, and the disk goes with the pod, read inside the same wait, `child_parked`):+} child 3's park leaves … `
  - theirs: base L1490: «The pending report suspends the implementer's claim, session kept, and the root's Sandbox with its volume: the Sandbox reads `Suspended`, `kubectl get pods -l legion.dev/issue=<tree 1>` lists nothing, and the PVC is Bound, within 600 s;» -> «The pending report suspends the implementer's claim, its session kept in the session database, and deletes the root's Sandbox as any stop of an issue does: within 600 s the root has no Sandbox, no pod and no disk (`kubectl get sandboxes,pods -l legion.dev/issue=<tree 1>` lists nothi…»; all changes: `The pending report suspends the implementer's claim, {+its+} session [-kept,-] {+kept in the session database,+} and {+deletes+} the root's Sandbox [-with its volume:-] {+as any stop of an issue does: within 600 s+} the [-Sandbox reads `Suspended`, `kubectl-] {+root has no Sandbox, no pod and no disk (`kubectl+} get [-pods-] {+sandboxes,pods+} -l legion.dev/issue=<tree 1>` lists nothing, and the [-PVC is Bound, within 600 s;-] {+disk that pod owned answers none, read inside the same wait);+}`
  - theirs: base L1491: «…the look's own two-hour bound, fails here by name rather than hanging), the root's Sandbox reads `Running` again and a pod of tree 1 is back within 900 s (the relaunch took 1 to 3 minutes in acceptance runs), and the implementer is resumed on its kept session.» -> «…the look's own two-hour bound, fails here by name rather than hanging), the root's Sandbox, made afresh by the wake, reads `Running` and a pod of tree 1 is back within 900 s (the relaunch took 1 to 3 minutes in acceptance runs), and the implementer is resumed on its kept session, in a pod whose disk is that pod's own.»; all changes: ` … than hanging), the root's [-Sandbox-] {+Sandbox, made afresh by the wake,+} reads `Running` [-again-] and a pod of … resumed on its kept [-session.-] {+session, in a pod whose disk is that pod's own.+}`
  - theirs: base L1491: «…ementer's claim is `retired` beside the five `suspended` claims (`claim_states_are`), and the root's Sandbox is `Suspended` again with no pod and its PVC Bound within 600 s, its volume kept for the close.» -> «…ementer's claim is `retired` beside the five `suspended` claims (`claim_states_are`), and within 600 s the root again has no Sandbox, no pod and no disk, the disk read inside the same wait.»; all changes: ` … `suspended` claims (`claim_states_are`), and {+within 600 s+} the [-root's Sandbox is `Suspended`-] {+root+} again [-with-] {+has no Sandbox,+} no pod and [-its PVC Bound within 600 s, its volume kept for-] {+no disk,+} the [-close.-] {+disk read inside the same wait.+}`
  - theirs: base L1492: «…ree 1 lingers (`linger_hours: 0.5`, from the close its last child's park brought on): the Sandboxes of the root and of its parked children 2, 3 and 4 are Suspended, each with its own PVC Bound, read first; then every node any pod of tree 1 ran on (read from the pod watch: the root's, the children's, the replacements th…» -> «…ree 1 lingers (`linger_hours: 0.5`, from the close its last child's park brought on): the root and its parked children 2, 3 and 4 each have no Sandbox and no disk, and so no pod, read first — each stop deleted the member's Sandbox once its pod was gone, the children's at their parks and the root's at its production che…»; all changes: ` … park brought on): the [-Sandboxes of the-] root and [-of-] its parked children 2, 3 and 4 [-are Suspended,-] each [-with-] {+have no Sandbox and no disk, and so no pod, read first — each stop deleted the member's Sandbox once+} its [-own PVC Bound, read first;-] {+pod was gone, the children's at their parks and the root's at its production check's close;+} then every node any … `
  - theirs: base L1492: «…low: tree lingers … until` line for tree 1: while it still runs after the waits, the four Sandboxes are read Suspended and Bound again; once it has passed, the close has deleted them, which `close` judges, and the note says so» -> «…low: tree lingers … until` line for tree 1: while it still runs after the waits, the four are read again with no Sandbox and no disk; once it has passed, what is left of tree 1 is the close's to judge (`close`), and the note says so»; all changes: ` … the waits, the four [-Sandboxes-] are read [-Suspended-] {+again with no Sandbox+} and [-Bound again;-] {+no disk;+} once it has passed, {+what is left of tree 1 is+} the [-close has deleted them, which `close` judges,-] {+close's to judge (`close`),+} and the note says so`
  - theirs: base L1493: «…ich `production-check-bound` saw closed) no Sandbox or pod carries tree 1's label any more: the Sandboxes of the root and of children 2, 3 and 4, and the volume each owns, are deleted (child 1's went with its `done`; the root's was kept through its production check).» -> «…ich `production-check-bound` saw closed) no Sandbox or pod carries tree 1's label any more, and none of the pods of the root and children 2, 3 and 4 has a disk. The cleanup finds little left to delete: each member's own stop already took its Sandbox, and the disk its pod owned went with that pod — child 1's at its `don…»; all changes: ` … tree 1's label any [-more:-] {+more, and none of+} the [-Sandboxes-] {+pods+} of the root and [-of-] children 2, 3 and [-4,-] {+4 has a disk. The cleanup finds little left to delete: each member's own stop already took its Sandbox,+} and the [-volume each owns, are deleted (child 1's-] {+disk its pod owned+} went with {+that pod — child 1's at+} its [-`done`;-] {+`done`, child 2's at its parks, those of children 3 and 4 at the parks that closed the root,+} the root's [-was kept through-] {+at the linger's suspension and again when+} its production [-check).-] {+check closed.+}`
  - theirs: base L1494: «tree 1 set todo again, its volume gone with the close: the daemon logs `supervise: the issue's volume was lost with the session; relaunching a fresh session` exactly once for tree 1's claims, and for no claim of any issue but tree 1 — the parked child…» -> «tree 1 set todo again: its architect comes back in a pod of its own, on that pod's own disk, Bound and owned by it, resumed from the session the database kept. Nothing reports a lost volume — the daemon's log holds no `volume was lost` line anywhere i…»; all changes: `tree 1 set todo [-again,-] {+again:+} its {+architect comes back in a pod of its own, on that pod's own disk, Bound and owned by it, resumed from the session the database kept. Nothing reports a lost+} volume {+— the daemon's log holds no `volume was lost` line anywhere in the run, that verdict having+} gone with the [-close: the daemon logs `supervise: the issue's volume was lost with the session; relaunching a fresh session` exactly once for tree 1's claims, and for no claim of any issue but tree 1 — the parked children's claims are suspended and nothing resumes them-] {+lost-disk machinery+`
  - theirs: base L1495: «…ime: the close of re-admitted tree 1's live root is refused 409, and its claims, Sandboxes and pods are unchanged;» -> «…ime: the close of re-admitted tree 1's live root is refused 409, and its claims, Sandboxes, pods and those pods' disks are unchanged;»; all changes: ` … 409, and its claims, [-Sandboxes-] {+Sandboxes, pods+} and [-pods-] {+those pods' disks+} are unchanged;`
  - theirs: base L1495: «… its worker live, the root and the worker are retired, and the tree's Sandboxes, pods and volumes are gone» -> «… its worker live, the root and the worker are retired, and the tree's Sandboxes, pods and the disk of each of those pods are gone»; all changes: ` … tree's Sandboxes, pods and [-volumes-] {+the disk of each of those pods+} are gone`

### APPEND (sections at one point): 141989f72b63 — PR #2074

Both add a lib section at one point (ours `lib/kind-sandbox.sh`, theirs `lib/bridge-tunnel.sh`). Replay: each next to the script section that sources it. Expected: clean.

- merge `141989f72b63`; base `3442ab023b9f`; ours `a67cadd13fb4`; theirs `61552da1b1b7`; hunk causes: APPEND
- hunk 1 (insertion between base L2289 «» and L2290 «## lib/secret-leaks.ts»):
  - ours inserts: «## lib/kind-sandbox.sh / A throwaway kind cluster running Agent Sandbox's controller under gVisor, for a live proof of the / Sandbox runtime on an amd64 Linux host. `controller-sweep-kind.sh` sources it. / ```sh / . "$root/scripts/e2e/lib/kind-sandbox.sh"     # sourced, never run / ``` / The caller defines `note` and `fail`, which exits, and calls `kind_sandbox_teardown` from its `EXIT` / trap. `LEGION_E2E_KIND` names the kind binary when it is not on `PATH`. / \| function \| does \| / \| :--- \| :--»
  - theirs inserts: «## lib/bridge-tunnel.sh / The reverse-tunnel bridge a stage proof uses when its pods cannot reach the devbox at all / (`LEGION_E2E_POD_REACHABILITY=bridge`, GKE's shape). Stage 4a sources it. / ```sh / . "$root/scripts/e2e/lib/bridge-tunnel.sh"     # sourced, never run / ``` / The caller sets `op`, `note`, `fail` (from `lib/namespace-rig.sh`, sourced first), `bridge_name` / (the run's deterministic bridge pod/Service name), `namespace` and `run_label`; it defines / `bridge_start` and `bridge_sto»

### CONTROL (SAME-SENTENCE): 3ac0f1f0ca6e — PR #2186

Both replace the `secrets` runner in the same sentences (ours `<runner>`, theirs `agent-secrets`), e.g. the L3265 cell and L3278. Expected: conflict.

- merge `3ac0f1f0ca6e`; base `448a41003a17`; ours `337d8e587665`; theirs `b08a56bce0c8`; hunk causes: SAME-SENTENCE, SAME-SENTENCE, SAME-SENTENCE, SAME-SENTENCE
- hunk 1 (base L826-830):
  - ours: base L826-830: «Legion's role Apps resolve as Stage 3's do: the daemon runs `secrets <implement App key> -- …` and `secrets <review App key> -- …` (the key names `LEGION_E2E_IMPLEMENT_APP_KEY_NAME` and `LEGION_E2E_REVIEW_APP_KEY_NAME` give) as each App's `private_key_command`, and `prerequisites` checks ea…» -> «Legion's role Apps resolve as Stage 3's do: the daemon runs `<runner> <implement App key> -- …` and `<runner> <review App key> -- …` (the runner `LEGION_E2E_APP_KEY_RUNNER` names, `secrets` by default, which `prerequisites` requires on `PATH`; the key names `LEGION_E2E_IMPLEMENT_APP_KEY_NAM…»; all changes: ` … do: the daemon runs [-`secrets-] {+`<runner>+} <implement App key> -- …` and [-`secrets-] {+`<runner>+} <review App key> -- …` (the {+runner `LEGION_E2E_APP_KEY_RUNNER` names, `secrets` by default, which `prerequisites` requires on `PATH`; the+} key names `LEGION_E2E_IMPLEMENT_APP_KEY_NAME` and … App's `private_key_command`, and `prerequisites` [-checks each with `secrets get <KEY> --no-request`, which prints only the key and its tier, refuses one that does not resolve to the agent tier, and then-] proves each key is its id's App`
  - theirs: base L826-830: «Legion's role Apps resolve as Stage 3's do: the daemon runs `secrets <implement App key> -- …` and `secrets <review App key> -- …` (the key names `LEGION_E2E_IMPLEMENT_APP_KEY_NAME` and `LEGION_E2E_REVIEW_APP_KEY_NAME` give) as each App's `private_key_command`, and `prerequisites` checks ea…» -> «Legion's role Apps resolve as Stage 3's do: the daemon runs `agent-secrets <implement App key> -- …` and `agent-secrets <review App key> -- …` (the key names `LEGION_E2E_IMPLEMENT_APP_KEY_NAME` and `LEGION_E2E_REVIEW_APP_KEY_NAME` give) as each App's `private_key_command`, so each key comes…»; all changes: ` … do: the daemon runs [-`secrets-] {+`agent-secrets+} <implement App key> -- …` and [-`secrets-] {+`agent-secrets+} <review App key> -- … as each App's `private_key_command`, {+so each key comes from the secrets broker (the default review key is human tier: its first read waits for a person's grant),+} and `prerequisites` [-checks each with `secrets get <KEY> --no-request`, which prints only the key and its tier, refuses one that does not resolve to the agent tier, and then-] proves each key is its id's App`
- hunk 2 (base L3265-3265):
  - ours: base L3265: «the secret-store key the `secrets` CLI resolves to the App's private key (base64-encoded PEM)» -> «the secret-store key the key runner resolves to the App's private key (base64-encoded PEM)»; all changes: `the secret-store key the [-`secrets` CLI-] {+key runner+} resolves to the App's private key (base64-encoded PEM)`
  - ours: insert after base L3265 (ending «the secret-store key the `secrets` CLI resolves to the App's private key (base64-encoded PEM)»): «`LEGION_E2E_APP_KEY_RUNNER` / the key runner, one for both Apps: a command, by name or path, that runs `<runner> <NAME> -- <command>` with the key `NAME` holds in that command's environment alone. / `secrets` by default; a store's own runner otherwise (`agent-secrets` reads the broker with no local fallback). / Legion names no store»
  - theirs: base L3265: «the secret-store key the `secrets` CLI resolves to the App's private key (base64-encoded PEM)» -> «the secrets broker key the `agent-secrets` client resolves to the App's private key (base64-encoded PEM)»; all changes: `the [-secret-store-] {+secrets broker+} key the [-`secrets` CLI-] {+`agent-secrets` client+} resolves to the App's private key (base64-encoded PEM)`
- hunk 3 (base L3267-3269):
  - ours: base L3267-3269: «Each defaults to the pair the live proofs have always used, which the lib holds in one place (`default_<role>_app_id`, `default_<role>_app_key_name`), so a run here sets none of them and acts as it always has, and a deployment with its…» -> «Each App input defaults to the pair the live proofs have always used, which the lib holds in one place (`default_<role>_app_id`, `default_<role>_app_key_name`), so a run here sets none of them and acts as it always has, and a deploymen…»
  - theirs: base L3267-3269: «Each defaults to the pair the live proofs have always used, which the lib holds in one place (`default_<role>_app_id`, `default_<role>_app_key_name`), so a run here sets none of them and acts as it always has, and a deployment with its own Apps sets a…» -> «Each defaults to the lib's default pair, `legion-implementer` (App ID 5341240) and `legion-reviewer` (App ID 5341243), which the lib holds in one place (`default_<role>_app_id`, `default_<role>_app_key_name`), so a run here sets none of them, and a de…»; all changes: `Each defaults to the [-pair the live proofs have always used,-] {+lib's default pair, `legion-implementer` (App ID 5341240) and `legion-reviewer` (App ID 5341243),+} which the lib holds … here sets none of [-them and acts as it always has,-] {+them,+} and a deployment with … `
- hunk 4 (base L3278-3278):
  - ours: base L3278: «runs `secrets <name> -- sh -c …` to decode the store's Node-style base64 into the PEM on stdout.» -> «runs `<runner> <name> -- sh -c …` to decode the store's Node-style base64 into the PEM on stdout.»; all changes: `runs [-`secrets-] {+`<runner>+} <name> -- sh -c … `
  - theirs: base L3278: «runs `secrets <name> -- sh -c …` to decode the store's Node-style base64 into the PEM on stdout.» -> «runs `agent-secrets <name> -- sh -c …` to decode the store's Node-style base64 into the PEM on stdout.»; all changes: `runs [-`secrets-] {+`agent-secrets+} <name> -- sh -c … `

### CONTROL (SAME-SENTENCE): 9164e75b4c3c — PR #2024

Both reword "model is Anthropic through the Hawk model gateway, the route every devbox agent session uses:" (L295). Expected: conflict.

- merge `9164e75b4c3c`; base `5c07a93847ef`; ours `4eb48dee5c3b`; theirs `920e2b2e94e4`; hunk causes: SAME-SENTENCE
- hunk 1 (base L295-297):
  - ours: base L295-297: «model is Anthropic through the Hawk model gateway, the route every devbox agent session uses:» -> «model is Anthropic through the configured route:»; all changes: `model is Anthropic through the [-Hawk model gateway, the route every devbox agent session uses:-] {+configured route:+}`
  - ours: base L295-297: «[`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) routes the isolated profile there in `prerequisites`, where its preflight mint refuses a locked keyring by name, and the daemon is given» -> «[`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) (`gateway`) or [`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) (`key-file`) routes the isolated profile there in `prerequisites`, where either route's preflight refuses loudly before any pane exists (a …»; all changes: `[`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) {+(`gateway`) or [`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) (`key-file`)+} routes the isolated profile there in `prerequisites`, where [-its-] {+either route's+} preflight [-mint-] refuses [-a-] {+loudly before any pane exists (a+} locked keyring by [-name,-] {+name under `gateway`, an unreadable or empty key file under `key-file`),+} and the daemon is given`
  - theirs: base L295: «model is Anthropic through the Hawk model gateway, the route every devbox agent session uses:» -> «model is Anthropic through the Hawk model gateway, on a devbox whose agent sessions use that gateway:»; all changes: `model is Anthropic through the Hawk model gateway, [-the route every-] {+on a+} devbox {+whose+} agent [-session uses:-] {+sessions use that gateway:+}`


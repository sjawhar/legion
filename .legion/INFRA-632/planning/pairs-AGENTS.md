# Replay classification: `AGENTS.md`

Source: every conflicting forward merge on the root `AGENTS.md` in `conf.txt` (before measurement), re-merged with `git merge-file --diff3` on the merge base, branch parent (ours) and main parent (theirs) blobs; every merge has a single merge base, so this equals `git merge-tree`. Classified by hand from a sentence-level diff of each hunk.

Merges: 29; layout-caused: 13; not layout-caused (at least one SAME-SENTENCE hunk): 16.

Hunks by cause: ADJACENT 6, CODE-BLOCK(ADJACENT) 9, CODE-BLOCK(LONG-LINE) 2, CODE-BLOCK(SAME-SENTENCE) 11, LONG-LINE 3, SAME-SENTENCE 5.

SAME-SENTENCE hunks by kind (clash = same words or fact; clauses = different clauses of one long sentence; enum = different items of one hand-written enumeration; rewrite = main rewrote or moved the text the branch edited; dup = both sides carry two versions of the same new text): clash 7, clauses 6, dup 3.

Line numbers are base (merge-base) line numbers of that merge. `[auto]` marks a hunk labelled from the script's sentence diff without a manual read (only in merges already decided by another hunk).

## Every merge

### 10c1d0bd5a4a — PR #2106 (envoy-infra-merge, 2026-10-15) — not layout-caused

base `8fff75fb57a6`, ours (branch parent) `752920da3806`, theirs (main parent) `131ccd0a0cd5`

- hunk 1: **SAME-SENTENCE** — base insertion before L115 (0 lines, longest 0 chars). [dup] both append the same `Envoy Pulumi components` row at the end of the repository-map table (after base L114), in two wordings (with/without "and the `envoy-infra seed` command")

### 760468db6c78 — PR #2106 (envoy-infra-merge, 2026-10-15) — not layout-caused

base `131ccd0a0cd5`, ours (branch parent) `f77093c2a071`, theirs (main parent) `b450e3b55ce9`

- hunk 1: **SAME-SENTENCE** — base L115-115 (1 lines, longest 219 chars). [clash] repository-map row L115: both reword "The `envoy-infra` Pulumi plugin, a Go module outside the root `go.work`."

### 2dce61d9cb6e — PR #2059 (legion/LEGION-781, 2026-10-19) — LAYOUT-CAUSED

base `6d505c12f146`, ours (branch parent) `bf53039b7bdc`, theirs (main parent) `9e9b2c493fd1`

- hunk 1: **CODE-BLOCK(ADJACENT)** — base L72-73 (2 lines, longest 1444 chars). ours Stage 3 comment L72 ("the architect's acceptance, … the close it makes…"); theirs Stage 4a comment L73 (repository-cache clause)

### bc1c2593a9cf — PR #2024 (e2e/stage4a-gke, 2026-10-19) — not layout-caused

base `098c3cc7387a`, ours (branch parent) `7afd5d58123d`, theirs (main parent) `c13a98917e34`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L69-71 (3 lines, longest 1272 chars). [clash] both add a variable to the stage4a command L71 and both edit its comment sentence

### daf1b822b209 — PR #2024 (e2e/stage4a-gke, 2026-10-19) — not layout-caused

base `c13a98917e34`, ours (branch parent) `1d64ffb61948`, theirs (main parent) `5c07a93847ef`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L71-73 (3 lines, longest 1444 chars). [clauses] stage4a comment L73: ours appends the GKE-shape clause, theirs inserts the repository-cache clause mid-sentence

### e4d28dc28dce — PR #2059 (legion/LEGION-781, 2026-10-19) — LAYOUT-CAUSED

base `ae69d783458f`, ours (branch parent) `30b7d5186d15`, theirs (main parent) `b28439d41ce7`

- hunk 1: **CODE-BLOCK(ADJACENT)** — base L70-71 (2 lines, longest 1272 chars). ours Stage 3 comment L70; theirs Stage 4a line L71 (`LEGION_E2E_REPO_SECOND_OWNER`, second-owner clause)

### 22817ba1d2a3 — PR #2059 (legion/LEGION-781, 2026-10-20) — LAYOUT-CAUSED

base `a5b302e42403`, ours (branch parent) `0ec47d562429`, theirs (main parent) `3442ab023b9f`

- hunk 1: **CODE-BLOCK(ADJACENT)** — base L72-72 (1 lines, longest 469 chars). `## Commands`: ours edits the Stage 3 command comment (L72); theirs inserts the new `claude-backend-tmux.sh` command line directly after it

### a578760e40a5 — PR #2059 (legion/LEGION-781, 2026-10-20) — not layout-caused

base `3442ab023b9f`, ours (branch parent) `afd71e2747e9`, theirs (main parent) `61552da1b1b7`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L76-77 (2 lines, longest 492 chars). [clauses] Stage 3 comment L77: ours mid-sentence ("the architect's acceptance… the close it makes"), theirs the end ("…legion-smoke (scripts/e2e/README.md)" -> "; LEGION_E2E_MODEL_ROUTE=key-file …"); theirs also edits L76

### a80445d4da16 — PR #2024 (e2e/stage4a-gke, 2026-10-20) — LAYOUT-CAUSED

base `6255cd241584`, ours (branch parent) `dc0bd2f61787`, theirs (main parent) `3442ab023b9f`

- hunk 1: **CODE-BLOCK(ADJACENT)** — base L71-73 (3 lines, longest 1865 chars). ours edits L71-73 (MODEL_ROUTE on stage2/3/4a lines); theirs inserts the claude-backend-tmux line between L72 and L73

### 29b2d894bb65 — PR #2150, #2259 (sami/single-container-pod, 2026-10-25) — not layout-caused

base `531ba202f6d2`, ours (branch parent) `a636841acffe`, theirs (main parent) `4792c8fa421f`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L67-67 (1 lines, longest 1025 chars). [clash] `legion launcher` line L67 (1,025 chars): ours rewrites it into the multi-role form, theirs adds `--stop-grace`/session flags and the session-flag clause into the same sentences

### 2fc4b487c8fa — PR #2186 (legion/INFRA-220, 2026-10-25) — LAYOUT-CAUSED

base `56ba79b8cf6d`, ours (branch parent) `6bae3e0e82fa`, theirs (main parent) `3ca092a69891`

- hunk 1: **CODE-BLOCK(ADJACENT)** — base L66-67 (2 lines, longest 1025 chars). ours `legion claude-bridge` line L66 ("under either runtime", "It is transport: …"); theirs `legion launcher` line L67 (`--stop-grace`, session flags)

### 9c96283b82a0 — PR #2186 (legion/INFRA-220, 2026-10-25) — LAYOUT-CAUSED

base `7bf2759c6e17`, ours (branch parent) `45c6016a44a0`, theirs (main parent) `9e62d7451f2c`

- hunk 1: **ADJACENT** — base L144-145 (2 lines, longest 1832 chars). `## Security` bullets with no blank line between: theirs L144 "Release jobs keep credentials…" (adds `actions: write`), ours L145 "Deploys come only from main" (rewrites the `packages/claude-envoy` clause)

### b247931acbca — PR #2173 (legion/INFRA-80, 2026-10-25) — not layout-caused

base `371d6c879565`, ours (branch parent) `0b8ae3d228a6`, theirs (main parent) `decf80f922a1`

- hunk 1: **SAME-SENTENCE** — base L167-167 (1 lines, longest 1437 chars). [clauses] Production-check bullet L167, the `pending` sentence: ours "session and the issue's volume, the pod goes" clause, theirs "for the pull request," -> deploy-workflow clause

### c99701afc1e7 — PR #2173 (legion/INFRA-80, 2026-10-25) — not layout-caused

base `9e62d7451f2c`, ours (branch parent) `7609fd93fdc2`, theirs (main parent) `14571f6f4aea`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L79-80 (2 lines, longest 2479 chars). [clauses] stage4b comment L80 enumeration sentence: ours session-store/disk clauses, theirs merge-while-children-run clause and deletions
- hunk 2: **ADJACENT** — base L165-166 (2 lines, longest 1437 chars). theirs inserts two sentences in the L165 paragraph; ours edits the L166 Production-check bullet

### fc8dc19f9059 — PR #2190 (legion/INFRA-372, 2026-10-25) — LAYOUT-CAUSED

base `6ede52ccb376`, ours (branch parent) `cff968a3180b`, theirs (main parent) `158654c89e8b`

- hunk 1: **LONG-LINE** — base L128-128 (1 lines, longest 560 chars). L128 (560 chars): theirs rewrites "…the section marks which of its steps have been run here…", ours rewrites the next sentence "A tool that works on the checkout belongs on the issue's own disk instead" (adjacent sentences)

### 19aaa96fe03b — PR #2234 (legion/INFRA-448, 2026-10-26) — LAYOUT-CAUSED

base `5c4548f0aeb4`, ours (branch parent) `887d0fecb158`, theirs (main parent) `ba9a24e238c3`

- hunk 1: **LONG-LINE** — base L166-166 (1 lines, longest 2046 chars). Production-check bullet L166 (2,046 chars): ours rewrites "The record lives on the pull request alone, since Dispatch refuses a write on a closed issue;", theirs rewrites the earlier "`fail` for a defect, a `dispatch ask` and never an issue…" sentence; one unchanged sentence between

### 2479de885900 — PR #2186 (legion/INFRA-220, 2026-10-26) — not layout-caused

base `f2c17ded5305`, ours (branch parent) `c481e666f8cc`, theirs (main parent) `8298fc710323`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L71-72 (2 lines, longest 1196 chars). [clash] theirs deletes the `legion sessions mark-lost` command line L72 whose comment ours extends ("a claim recording a Claude Code transcript is neither missing nor marked…"); theirs also edits L71
- hunk 2: **CODE-BLOCK(ADJACENT)** — base L77-80 (4 lines, longest 2381 chars). ours L78 (claude-backend-tmux line: MODEL_ROUTE, key-file clause, new sandbox command); theirs L77 (STAGE3_PRODUCTION_ENVOY_URL comment), L79-80 (stage4a/4b comments)

### 293c832e936a — PR #2206 (test/claude-backend-broker-route-v2, 2026-10-26) — LAYOUT-CAUSED

base `6f14a3671237`, ours (branch parent) `99650d5a7615`, theirs (main parent) `336d98570451`

- hunk 1: **CODE-BLOCK(ADJACENT)** — base L75-78 (4 lines, longest 2911 chars). ours L77 (claude-backend-tmux: MODEL_ROUTE prefix, key-file clause); theirs L75 (stage2 key-file continuation: box-login clause), inserts a box-login line after L76, and L78 (stage4a `LEGION_E2E_PREVIOUS_IMAGE`)

### 2a7503c142cb — PR #2176 (e2e-key-file-model-route, 2026-10-26) — LAYOUT-CAUSED

base `5c182bd16f0a`, ours (branch parent) `3c36b9040b6c`, theirs (main parent) `748e8d952a76`

- hunk 1: **CODE-BLOCK(LONG-LINE)** — base L80-80 (1 lines, longest 2479 chars). stage4b command line L80 (2,479 chars): ours adds `LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file>` to the command, theirs rewrites the comment's enumeration sentence right after it (adjacent sentences)

### 60a69246a3d0 — PR #2176 (e2e-key-file-model-route, 2026-10-26) — not layout-caused

base `748e8d952a76`, ours (branch parent) `2a7503c142cb`, theirs (main parent) `7f6ef46fd42f`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L76-80 (5 lines, longest 2381 chars). [clash] both add a variable to the stage4b command L80 (ours `LEGION_E2E_ANTHROPIC_KEY_FILE`, theirs `LEGION_E2E_SESSION_DB_PORT`); ours also rewrites the model-route lines L76-78 next to theirs' L77/L79 edits

### 82fd7d27b4f2 — PR #2186 (legion/INFRA-220, 2026-10-26) — not layout-caused

base `6cdb99959fa1`, ours (branch parent) `b1d8a21b1e8f`, theirs (main parent) `b4bea3858263`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L77-77 (1 lines, longest 468 chars). [clash] claude-backend-tmux line L77: both add `LEGION_E2E_MODEL_ROUTE=gateway` (at different positions) and both append the key-file alternative to "…after its pane is killed"

### a3dd8e7a0730 — PR #2150, #2259 (sami/single-container-pod, 2026-10-26) — not layout-caused

base `12cbe2cdcc62`, ours (branch parent) `b54364dbd7de`, theirs (main parent) `7f6ef46fd42f`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L74-74 (1 lines, longest 5598 chars). [clash] `go build` line L74 (5,598 chars): both rewrite "(`git clone --shared` of the feed, so its objects stay there)" in the workspace-init sentence
- hunk 2: **CODE-BLOCK(LONG-LINE)** — base L79-80 (2 lines, longest 2381 chars). stage4a line L79: ours adds `LEGION_E2E_PREVIOUS_IMAGE` to the command, theirs edits the comment sentence right after it and L80

### b1d8a21b1e8f — PR #2186 (legion/INFRA-220, 2026-10-26) — LAYOUT-CAUSED

base `8411fe855388`, ours (branch parent) `8c83c9a21c80`, theirs (main parent) `6cdb99959fa1`

- hunk 1: **ADJACENT** — base L13-16 (4 lines, longest 99 chars). hard-wrapped `## Architecture` bullet L13-16: theirs rewrites "…with one container and `legion launcher` per resident role" (L13-14), ours rewrites "Under `runtime: tmux`, a phase worker whose role…" (L15-16)
- hunk 2: **CODE-BLOCK(ADJACENT)** — base L66-67 (2 lines, longest 2569 chars). ours claude-bridge line L66; theirs launcher line L67
- hunk 3: **CODE-BLOCK(ADJACENT)** — base L75-78 (4 lines, longest 2911 chars). ours claude-backend-tmux line L77; theirs L75, after L76, L78

### dadb5f515a3e — PR #2234 (legion/INFRA-448, 2026-10-26) — LAYOUT-CAUSED

base `135d22e0f63b`, ours (branch parent) `63d4f066ff01`, theirs (main parent) `8298fc710323`

- hunk 1: **LONG-LINE** — base L167-167 (1 lines, longest 2023 chars). Production-check bullet L167 (2,023 chars): ours rewrites "The record lives on the pull request alone, since Dispatch refuses a write on a closed issue;", theirs the `pending` sentence ("…the issue's volume, the pod goes, and wakes…"); two unchanged sentences between

### e0ab76dcd45f — PR #2150, #2259 (sami/single-container-pod, 2026-10-26) — not layout-caused

base `bc307fb5675e`, ours (branch parent) `48898be0dc12`, theirs (main parent) `0587774a744f`

- hunk 1: **CODE-BLOCK(SAME-SENTENCE)** — base L73-73 (1 lines, longest 5240 chars). [clauses] `go build` line L73 (5,240 chars), workspace-init sentence: ours role/container wording, theirs feed-fetch clauses

### d9257a7d9f7c — PR #2319 (sami/catchup-followup, 2026-10-27) — not layout-caused

base `bc98d3e729b9`, ours (branch parent) `0f7b78d791e3`, theirs (main parent) `448a41003a17`

- hunk 1: **SAME-SENTENCE** — base L273-279 (7 lines, longest 98 chars). [dup] both carry the gate-waits rewrite of L273-279 and diverge on the same catch-up/rollback sentences

### ddf24b935a9f — PR #2354 (legion/INFRA-177, 2026-10-27) — not layout-caused

base `ad909178a0cb`, ours (branch parent) `d28490ed4aab`, theirs (main parent) `448a41003a17`

- hunk 1: **ADJACENT** — base L33-36 (4 lines, longest 103 chars). hard-wrapped L33-36: theirs extends the controller sentence (L33-34, "or under `runtime: tmux` in a pane…"), ours rewrites "Durable `.legion/<issue>/<phase>.json` handoffs are the recovery source of truth." (L35-36)
- hunk 2: **CODE-BLOCK(SAME-SENTENCE)** — base L75-77 (3 lines, longest 1454 chars). [clauses] claude-backend-tmux comment L77: ours "with a pushed handoff commit" -> "with its handoff recorded on the Dispatch issue", theirs "killed (scripts/e2e/README.md)" -> key-file clause; theirs also L75/L76
- hunk 3: **ADJACENT** — base L253-259 (7 lines, longest 98 chars). hard-wrapped L253-259: theirs inserts register_gate sentences in L253-257, ours rewrites the `docs/solutions/` sentence on L258-259

### fae0cbd28b67 — PR #2280, #2319 (sami/gate-waits, 2026-10-27) — not layout-caused

base `ba9a24e238c3`, ours (branch parent) `b536ea79f95f`, theirs (main parent) `bc98d3e729b9`

- hunk 1: **SAME-SENTENCE** — base insertion before L249 (0 lines, longest 0 chars). [dup] both insert the "**Waiting on a person:**" paragraph before base L249, in two versions (stacked PRs #2280/#2319)

### fb434588aa78 — PR #2226 (legion/INFRA-252, 2026-10-27) — LAYOUT-CAUSED

base `bc98d3e729b9`, ours (branch parent) `e137db372728`, theirs (main parent) `aa64baf632c2`

- hunk 1: **ADJACENT** — base L195-197 (3 lines, longest 99 chars). hard-wrapped paragraph L195-197: theirs inserts the boot-recording sentences after "…`todo`." (L195), ours inserts the joinable-root sentences after "…so it needs the label as any root does." (L196); one unchanged sentence between

## Proposed replay pairs

Thirteen merges are layout-caused; the pairs below take seven of them (distinct merge commits) plus two SAME-SENTENCE controls. Expected result on the restructured file assumes INFRA-631's rule: one sentence per line, a blank line between list items, and a `## Commands` entry that is no longer one physical line per command (each command an item whose description sentences are lines of their own, or detail moved to `--help`/docs). Two edits clean only when at least one unchanged line separates them. `fc8dc19f9059` (adjacent sentences of L128) and `2a7503c142cb` (command and the sentence right after it) are layout-caused but would still conflict under that rule, so they are not proposed.

### LONG-LINE: dadb5f515a3e — PR #2234

Production-check bullet L167: ours rewrites the record sentence, theirs the `pending` sentence, two sentences apart. Expected: clean.

- merge `dadb5f515a3e`; base `135d22e0f63b`; ours `63d4f066ff01`; theirs `8298fc710323`; hunk causes: LONG-LINE
- hunk 1 (base L167-167):
  - ours: base L167: «The record lives on the pull request alone, since Dispatch refuses a write on a closed issue;» -> «The record lives on the pull request alone: Dispatch opens no ask and takes no document edit on a closed issue, so the pull request's `Production:` line is the record whether or not the issue is closed;»; all changes: `The record lives on the pull request [-alone, since-] {+alone:+} Dispatch [-refuses a write-] {+opens no ask and takes no document edit+} on a closed [-issue;-] {+issue, so the pull request's `Production:` line is the record whether or not the issue is closed;+}`
  - theirs: base L167: «…does not carry the merge yet (the daemon suspends the implementer's claim with its session and the issue's volume, the pod goes, and wakes the check on Dispatch's `issue.deployed` for the pull request; when the project's `projects.<KEY>.deploys` names the repository's deploy workflow and step, at each completed run of …» -> «…does not carry the merge yet (the daemon suspends the implementer's claim with its session, the issue's pod goes and takes the disk it owns, and the daemon wakes the check on Dispatch's `issue.deployed` for the pull request; when the project's `projects.<KEY>.deploys` names the repository's deploy workflow and step, at…»; all changes: ` … implementer's claim with its [-session-] {+session, the issue's pod goes and takes the disk it owns,+} and the [-issue's volume, the pod goes, and-] {+daemon+} wakes the check on … `

### LONG-LINE: 19aaa96fe03b — PR #2234

Production-check bullet L166 (an earlier base of the same bullet): ours the record sentence, theirs the `fail` sentence, one sentence apart. Expected: clean.

- merge `19aaa96fe03b`; base `5c4548f0aeb4`; ours `887d0fecb158`; theirs `ba9a24e238c3`; hunk causes: LONG-LINE
- hunk 1 (base L166-166):
  - ours: base L166: «The record lives on the pull request alone, since Dispatch refuses a write on a closed issue;» -> «The record lives on the pull request alone: Dispatch opens no ask and takes no document edit on a closed issue, so the pull request's `Production:` line is the record whether or not the issue is closed;»; all changes: `The record lives on the pull request [-alone, since-] {+alone:+} Dispatch [-refuses a write-] {+opens no ask and takes no document edit+} on a closed [-issue;-] {+issue, so the pull request's `Production:` line is the record whether or not the issue is closed;+}`
  - theirs: base L166: «`fail` for a defect, a `dispatch ask` and never an issue the implementer files, on the controller's report issue, naming the issue, the pull request, the merge commit, what was driven and what failed, its reference published to the controller, with th…» -> «`fail` for a defect, never an issue the implementer files: a defect with a clear fix goes to the controller, which gives it to the running tree whose code it is and files a Legion root only when none does, and one that needs a decision is a `dispatch …»; all changes: ``fail` for a defect, [-a `dispatch ask` and-] never an issue the implementer [-files,-] {+files: a defect with a clear fix goes to the controller, which gives it to the running tree whose code it is and files a Legion root only when none does, and one that needs a decision is a `dispatch ask`+} on the controller's report [-issue, naming-] {+issue whose reference goes to the controller; either names+} the issue, the pull … driven and what failed, [-its reference published to the controller, with-] {+and+} the `Production:` line [-naming-] {+names the message or+} the ask.`

### ADJACENT: 9c96283b82a0 — PR #2186

Two adjacent `## Security` bullets with no blank line between (L144 theirs, L145 ours). Expected: clean once a blank line separates the bullets.

- merge `9c96283b82a0`; base `7bf2759c6e17`; ours `45c6016a44a0`; theirs `9e62d7451f2c`; hunk causes: ADJACENT
- hunk 1 (base L144-145):
  - ours: base L145: «…sh` jobs, which run only on main), and `packages/claude-envoy`, which no workflow releases, built from a checkout of a commit on `main`.» -> «…sh` jobs, which run only on main), and `packages/claude-envoy`, which no workflow releases: a Kubernetes deployment takes it from the worker image, which carries that commit's build of it, and a tmux one builds it from a checkout of a commit on `main`.»; all changes: ` … `packages/claude-envoy`, which no workflow [-releases, built-] {+releases: a Kubernetes deployment takes it from the worker image, which carries that commit's build of it, and a tmux one builds it+} from a checkout of a commit on `main`.`
  - theirs: base L144: «…-infra.yaml`, a job that holds `contents: write`, `id-token: write`, `attestations: write` or the deploy key runs no dependency code (no `bun`, `go`, `node`, no built binary, no `scripts/` program) and restores no cache (`cache-mode: none`); every checkout sets `persist-credentials: false`; the deploy key reaches only …» -> «…-infra.yaml`, a job that holds `contents: write`, `id-token: write`, `attestations: write`, `actions: write` or the deploy key runs no dependency code (no `bun`, `go`, `node`, no built binary, no `scripts/` program) and restores no cache (`cache-mode: none`); every checkout sets `persist-credentials: false`; the deploy…»; all changes: ` … write`, `id-token: write`, `attestations: {+write`, `actions:+} write` or the deploy … `

### ADJACENT (insertions in one paragraph): fb434588aa78 — PR #2226

Both insert new sentences into one hard-wrapped paragraph at points one sentence apart (after "…`todo`." and after "…as any root does."). Expected: clean under one sentence per line.

- merge `fb434588aa78`; base `bc98d3e729b9`; ours `e137db372728`; theirs `aa64baf632c2`; hunk causes: ADJACENT
- hunk 1 (base L195-197):
  - ours: insert after base L196 (ending «ree is not live is an orphan, admitted as a root of its own, so it needs the label as any root does.»): «A `todo` root that never ran as a tree of its own (no slot reached it and it carries no run of its own, `record.Issue.TakenBack` included) and never left another tree joins the live tree its recorded parent is in, a waiting root's included: a person made it a child of that tree. It waits for no slot, gets no architect, runs there as a `todo` child does, held like any child while it waits on an unf…»
  - theirs: insert after base L195 (ending «`todo`.»): «A `todo` child of a live tree that no event of it ever recorded — the event never reached the daemon, because a durable consumer created fresh takes only what is published after it or because the notification stream drops a message 72 hours old whether or not anyone read it, or it reached the daemon while the child's parent had no record or its tree was not live — is recorded into the tree at the …»

### COMMANDS: 2dce61d9cb6e — PR #2059

`## Commands`: ours edits the Stage 3 command's description (L72), theirs the Stage 4a command's (L73). Expected: clean once each command is its own item separated by a blank line; a conflict if the block stays one line per command.

- merge `2dce61d9cb6e`; base `6d505c12f146`; ours `bf53039b7bdc`; theirs `9e9b2c493fd1`; hunk causes: CODE-BLOCK(ADJACENT)
- hunk 1 (base L72-73):
  - ours: base L72: «…ive a scratch Dispatch issue through the Go workflow, the design gate, review rounds, the daemon's merge of the READY head, production check, restart, held worker, pending status write, and pane credentials against sjawhar/legion-smoke (scripts/e2e/README.md)» -> «…ive a scratch Dispatch issue through the Go workflow, the design gate, review rounds, the architect's acceptance, the daemon's merge of the READY head and the close it makes, the implementer's production check after the close, restart, held worker, pending status write, and pane credentials against sjawhar/legion-smoke…»; all changes: ` … gate, review rounds, the {+architect's acceptance, the+} daemon's merge of the READY [-head,-] {+head and the close it makes, the implementer's+} production [-check,-] {+check after the close,+} restart, held worker, pending … `
  - theirs: base L73: «…y reading sjawhar/legion-smoke read-only through its gh and git and refused a push there, re-adoption, the orphan sweep taking the swept issue's volume, release, a resume under `session_store: postgres` held to the table of a scratch Postgres the script starts on the devbox's private address at `LEGION_E2E_SESSION_DB_P…» -> «…y reading sjawhar/legion-smoke read-only through its gh and git and refused a push there, the repository cache (the first issue pod's launch makes it and fills its feed from it alone, its Service is the one the controller made, a second pod streams its feed from it and asks GitHub for the delta alone, the cache's tar r…»; all changes: ` … refused a push there, {+the repository cache (the first issue pod's launch makes it and fills its feed from it alone, its Service is the one the controller made, a second pod streams its feed from it and asks GitHub for the delta alone, the cache's tar refuses a request without its token, and an issue whose volume is lost is provisioned again through it onto a commit of its pushed branch that neither the default branch nor the cache holds),+} re-adoption, the orphan sweep … `

### COMMANDS: 2fc4b487c8fa — PR #2186

`## Commands`: ours `legion claude-bridge` (L66), theirs `legion launcher` (L67). Expected: as above.

- merge `2fc4b487c8fa`; base `56ba79b8cf6d`; ours `6bae3e0e82fa`; theirs `3ca092a69891`; hunk causes: CODE-BLOCK(ADJACENT)
- hunk 1 (base L66-67):
  - ours: base L66: «…m` runs in Oh My Pi's place for a phase worker whose role `worker_backends` puts on claude (tmux only; the daemon builds the argv, `claude -p --input-format stream-json --output-format stream-json …` with `--session-id <new id>` on a fresh launch or `--resume <id>` on a relaunch): turns the daemon's prompt, abort, get_…» -> «…m` runs in Oh My Pi's place for a phase worker whose role `worker_backends` puts on claude, under either runtime (the daemon builds the argv, `claude -p --input-format stream-json --output-format stream-json …` with `--session-id <new id>` on a fresh launch or `--resume <id>` on a relaunch; one definition of it, `packa…»; all changes: ` … role `worker_backends` puts on [-claude (tmux only; the-] {+claude, under either runtime (the+} daemon builds the argv, … `--resume <id>` on a [-relaunch):-] {+relaunch; one definition of it, `packages/daemon/internal/claudelaunch`):+} turns the daemon's prompt, … transcript exists before registration, [-sends pi-legion's phase-stall follow-up,-] takes Envoy deliveries as … exits with Claude Code's [-status-] {+status. It is transport: the phase reminder runs in the worker's own MCP server (`packages/claude-envoy`), driven by Claude Code's hooks+}`
  - theirs: base L67: «…> --token-file <path> --sandbox <name> --role <role> [--pod-uid <uid>] --private-dir <dir>» -> «…> --token-file <path> --sandbox <name> --role <role> [--pod-uid <uid>] --private-dir <dir> [--stop-grace <duration>] [--session-instance <project:region:instance> --session-database <name> --session-user <login>]»; all changes: ` … [--pod-uid <uid>] --private-dir <dir> {+[--stop-grace <duration>] [--session-instance <project:region:instance> --session-database <name> --session-user <login>]+}`
  - theirs: base L67: «…thing already there removed without being followed) and removed when the generation ends; it survives a daemon restart without stopping the child, refuses a generation that already ran or a reused request id with another payload, and never restarts a child itself» -> «…thing already there removed without being followed) and removed when the generation ends; the three session flags name the Cloud SQL database every generation keeps its Oh My Pi session in — the instance's connection name, the database in it, and the IAM login the pod signs in as — and the runtime passes them only unde…»; all changes: ` … when the generation ends; {+the three session flags name the Cloud SQL database every generation keeps its Oh My Pi session in — the instance's connection name, the database in it, and the IAM login the pod signs in as — and the runtime passes them only under `runtime.kubernetes.session_store: cloudsql-iam` (none under the other stores, and a database named in part is refused before the launcher connects): with them, before it looks a resume up and before it starts the child, the launcher writes that generation's connection URL into its `g<generation>` directory as OMP_SESSION_SQL_DSN, bind`

### COMMANDS + ADJACENT (three hunks): b1d8a21b1e8f — PR #2186

One merge with three layout hunks: a hard-wrapped `## Architecture` bullet (sentences two apart), the claude-bridge/launcher command lines, and the model-route/claude-backend command lines. Expected: clean on all three under the same assumptions.

- merge `b1d8a21b1e8f`; base `8411fe855388`; ours `8c83c9a21c80`; theirs `6cdb99959fa1`; hunk causes: ADJACENT, CODE-BLOCK(ADJACENT), CODE-BLOCK(ADJACENT)
- hunk 1 (base L13-16):
  - ours: base L15-16: «Under `runtime: tmux`, a phase worker whose role `legion.yaml`'s `worker_backends` puts on `claude` runs Claude Code in Oh My Pi's place: the shim runs» -> «A phase worker whose role `legion.yaml`'s `worker_backends` puts on `claude` runs Claude Code in Oh My Pi's place under either runtime: the shim runs»; all changes: `[-Under `runtime: tmux`, a-] {+A+} phase worker whose role … in Oh My Pi's [-place:-] {+place under either runtime:+} the shim runs`
  - theirs: base L13-14: «Agent Sandbox pod per issue running the published worker image, with one container and `legion launcher` per resident role (`docs/kubernetes.md`;» -> «Agent Sandbox pod per issue running the published worker image, with one container whose `legion launcher` runs every resident role (`docs/kubernetes.md`;»; all changes: ` … image, with one container [-and-] {+whose+} `legion launcher` [-per-] {+runs every+} resident role (`docs/kubernetes.md`;`
- hunk 2 (base L66-67):
  - ours: base L66: «…m` runs in Oh My Pi's place for a phase worker whose role `worker_backends` puts on claude (tmux only; the daemon builds the argv, `claude -p --input-format stream-json --output-format stream-json …` with `--session-id <new id>` on a fresh launch or `--resume <id>` on a relaunch): turns the daemon's prompt, abort, get_…» -> «…m` runs in Oh My Pi's place for a phase worker whose role `worker_backends` puts on claude, under either runtime (the daemon builds the argv, `claude -p --input-format stream-json --output-format stream-json …` with `--session-id <new id>` on a fresh launch or `--resume <id>` on a relaunch; one definition of it, `packa…»; all changes: ` … role `worker_backends` puts on [-claude (tmux only; the-] {+claude, under either runtime (the+} daemon builds the argv, … `--resume <id>` on a [-relaunch):-] {+relaunch; one definition of it, `packages/daemon/internal/claudelaunch`):+} turns the daemon's prompt, … transcript exists before registration, [-sends pi-legion's phase-stall follow-up,-] takes Envoy deliveries as … exits with Claude Code's [-status-] {+status. It is transport: the phase reminder runs in the worker's own MCP server (`packages/claude-envoy`), driven by Claude Code's hooks+}`
  - theirs: base L67: «legion launcher --connect tcp://<host>:<port> --token-file <path> --sandbox <name> --role <role> [--pod-uid <uid>] --private-dir <dir> [--stop-grace <duration>] [--session-instance <project:region:instance> --session-database <name> --session-user <login>] PID 1 of one role con…» -> «legion launcher --connect tcp://<host>:<port> --sandbox <name> --roles-dir <dir> --role <r>… [--pod-uid <uid>] [--stop-grace <d>] [--session-instance <project:region:instance> --session-database <name> --session-user <login>] legion launcher --connect tcp://<host>:<port> --toke…»; all changes: `{+legion launcher --connect tcp://<host>:<port> --sandbox <name> --roles-dir <dir> --role <r>… [--pod-uid <uid>] [--stop-grace <d>] [--session-instance <project:region:instance> --session-database <name> --session-user <login>]+} legion launcher --connect tcp://<host>:<port> … --role <role> [--pod-uid <uid>] [---private-dir <dir>-] [--stop-grace [-<duration>]-] {+<d>]+} [--session-instance <project:region:instance> --session-database <name> --session-user <login>] {+--private-dir <dir> Kubernetes runtime; daemon-spawned. The multi-role form is+} PID 1 of [-one role container in-] an issue [-po`
- hunk 3 (base L75-78):
  - ours: base L77: «LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/claude-backend-tmux.sh The Claude Code backend's devbox-only live proof: an implementer `worker_backends` puts on claude registers its transcript, completes its phase through the `l…» -> «LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> LEGION_E2E_MODEL_ROUTE=gateway LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/claude-backend-tmux.sh The Claude Code backend's devbox-only live proof under tmux: an implementer `worker_backends` puts on claude registers its tran…»; all changes: `LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> {+LEGION_E2E_MODEL_ROUTE=gateway+} LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/claude-backend-tmux.sh The … Code backend's devbox-only live [-proof:-] {+proof under tmux:+} an implementer `worker_backends` puts … after its pane is [-killed-] {+killed; LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file> instead, for a devbox with no Hawk gateway+} (scripts/e2e/README.md) {+LEGION_E2E_RUNTIME_CONTEXT=<restricted context> LEGION_E2E_IMAGE=<worker image@sha256> LEGION_E2E_STORAGE_CLASS=<Stora`
  - theirs: base L75: «…-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file> instead, for a devbox with no Hawk gateway (scripts/e2e/README.md)» -> «…-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file> instead, for a devbox with no Hawk gateway; LEGION_E2E_MODEL_ROUTE=box-login instead, on the operator's own Oh My Pi login, where the operator allows it (the profile's agent.db a symlink to the operator's, no credential read; the operator's global omp build, refused witho…»; all changes: ` … devbox with no Hawk [-gateway-] {+gateway; LEGION_E2E_MODEL_ROUTE=box-login instead, on the operator's own Oh My Pi login, where the operator allows it (the profile's agent.db a symlink to the operator's, no credential read; the operator's global omp build, refused without the refresh-race fix, in place of .omp-pin; one box-login proof at a time)+} (scripts/e2e/README.md)`
  - theirs: insert after base L76 (ending «ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file> instead, for a devbox with no Hawk gateway;»): «LEGION_E2E_MODEL_ROUTE=box-login instead, on the operator's own Oh My Pi login, as for Stage 2;»
  - theirs: base L78: «…orageClass> LEGION_E2E_SESSION_DB_PORT=<port> LEGION_E2E_REPO_SECOND_OWNER=<owner>/<name> bash scripts/e2e/stage4a-sandbox-runtime.sh» -> «…orageClass> LEGION_E2E_SESSION_DB_PORT=<port> LEGION_E2E_REPO_SECOND_OWNER=<owner>/<name> LEGION_E2E_PREVIOUS_IMAGE=<worker image of main at contract 37 or 38, before one container@sha256> bash scripts/e2e/stage4a-sandbox-runtime.sh»; all changes: ` … audience> LEGION_E2E_STORAGE_CLASS=<StorageClass> LEGION_E2E_SESSION_DB_PORT=<port> LEGION_E2E_REPO_SECOND_OWNER=<owner>/<name> {+LEGION_E2E_PREVIOUS_IMAGE=<worker image of main at contract 37 or 38, before one container@sha256>+} bash scripts/e2e/stage4a-sandbox-runtime.sh`

### COMMANDS (new item): 22817ba1d2a3 — PR #2059

Theirs adds a new command (`claude-backend-tmux.sh`) right after the Stage 3 command whose description ours edits. Expected: clean when the new command item is placed after the item it belongs with, separated from ours' changed line by the blank line; replay must watch the diff's blank-line alignment.

- merge `22817ba1d2a3`; base `a5b302e42403`; ours `0ec47d562429`; theirs `3442ab023b9f`; hunk causes: CODE-BLOCK(ADJACENT)
- hunk 1 (base L72-72):
  - ours: base L72: «…ive a scratch Dispatch issue through the Go workflow, the design gate, review rounds, the daemon's merge of the READY head, production check, restart, held worker, pending status write, and pane credentials against sjawhar/legion-smoke (scripts/e2e/README.md)» -> «…ive a scratch Dispatch issue through the Go workflow, the design gate, review rounds, the architect's acceptance, the daemon's merge of the READY head and the close it makes, the implementer's production check after the close, restart, held worker, pending status write, and pane credentials against sjawhar/legion-smoke…»; all changes: ` … gate, review rounds, the {+architect's acceptance, the+} daemon's merge of the READY [-head,-] {+head and the close it makes, the implementer's+} production [-check,-] {+check after the close,+} restart, held worker, pending … `
  - theirs: insert after base L72 (ending «ker, pending status write, and pane credentials against sjawhar/legion-smoke (scripts/e2e/README.md)»): «LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/claude-backend-tmux.sh The Claude Code backend's devbox-only live proof: an implementer `worker_backends` puts on claude registers its transcript, completes its phase through the `legion` MCP tool's handoff_complete with a pushed handoff commit, and resumes the same Clau…»

### CONTROL (SAME-SENTENCE): 760468db6c78 — PR #2106

Both reword the same sentence of the repository-map row L115 ("The `envoy-infra` Pulumi plugin, a Go module outside the root `go.work`."). Expected: conflict.

- merge `760468db6c78`; base `131ccd0a0cd5`; ours `f77093c2a071`; theirs `b450e3b55ce9`; hunk causes: SAME-SENTENCE
- hunk 1 (base L115-115):
  - ours: base L115: «The `envoy-infra` Pulumi plugin, a Go module outside the root `go.work`.» -> «The `envoy-infra` Pulumi plugin and the `envoy-infra` command (`seed`, `render`), a Go module outside the root `go.work`.»; all changes: `The `envoy-infra` Pulumi [-plugin,-] {+plugin and the `envoy-infra` command (`seed`, `render`),+} a Go module outside the root `go.work`.`
  - theirs: base L115: «The `envoy-infra` Pulumi plugin, a Go module outside the root `go.work`.» -> «The `envoy-infra` Pulumi plugin and the `envoy-infra seed` command, a Go module outside the root `go.work`.»; all changes: `The `envoy-infra` Pulumi [-plugin,-] {+plugin and the `envoy-infra seed` command,+} a Go module outside the root `go.work`.`

### CONTROL (SAME-SENTENCE, Commands): 82fd7d27b4f2 — PR #2186

Both add `LEGION_E2E_MODEL_ROUTE=gateway` and the key-file alternative to the same claude-backend-tmux command (L77). Expected: conflict.

- merge `82fd7d27b4f2`; base `6cdb99959fa1`; ours `b1d8a21b1e8f`; theirs `b4bea3858263`; hunk causes: CODE-BLOCK(SAME-SENTENCE)
- hunk 1 (base L77-77):
  - ours: base L77: «LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/claude-backend-tmux.sh The Claude Code backend's devbox-only live proof: an implementer `worker_backends` puts on claude registers its transcript, completes its phase through the `l…» -> «LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> LEGION_E2E_MODEL_ROUTE=gateway LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/claude-backend-tmux.sh The Claude Code backend's devbox-only live proof under tmux: an implementer `worker_backends` puts on claude registers its tran…»; all changes: `LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> {+LEGION_E2E_MODEL_ROUTE=gateway+} LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/claude-backend-tmux.sh The … Code backend's devbox-only live [-proof:-] {+proof under tmux:+} an implementer `worker_backends` puts … after its pane is [-killed-] {+killed; LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file> instead, for a devbox with no Hawk gateway+} (scripts/e2e/README.md) {+LEGION_E2E_RUNTIME_CONTEXT=<restricted context> LEGION_E2E_IMAGE=<worker image@sha256> LEGION_E2E_STORAGE_CLASS=<Stora`
  - theirs: base L77: «LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/claude-backend-tmux.sh The Claude Code backend's devbox-only live proof: an implementer `worker_backends` puts o…» -> «LEGION_E2E_MODEL_ROUTE=gateway LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic API key> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/claude-backend-tmux.sh The Claude Code backend's devbox-only live proof: an imple…»; all changes: `{+LEGION_E2E_MODEL_ROUTE=gateway+} LEGION_E2E_CLAUDE_KEY_FILE=<file holding an Anthropic … after its pane is [-killed-] {+killed; LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file> instead, for a devbox with no Hawk gateway, where that one key file serves the Claude Code pane too+} (scripts/e2e/README.md)`


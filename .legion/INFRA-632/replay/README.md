# INFRA-632 merge replay

Real conflicting merges of `AGENTS.md`, `scripts/e2e/README.md` and `docs/kubernetes.md`, from `../planning/pairs-AGENTS.md`, `pairs-e2e-README.md` and `pairs-kubernetes.md` (the pair list is the plan's Acceptance 3), ported onto the new layout and merged again with `git merge-tree --write-tree`.

## Run

```sh
TMPDIR=/some/scratch bash .legion/INFRA-632/replay/run.sh [repo root]
```

The repo root defaults to the checkout holding the script.
`run.sh` reads the three files from commit `94850c01741e` (`REPLAY_BASE` overrides it), this pull request's head commit of the three guides, and commits them as the base of a throwaway repository under `${TMPDIR:-/tmp}/replay-$$`.
It checks each patch with `git apply --check`, applies it on a branch of its own, and runs `bun scripts/semantic-line-breaks.ts` on every file the patch changes, so each patched file is proved to keep the layout.
Then it merges each pair.
For each pair it prints a classification line, computed from the two patches' hunks, then one line: merge, file, class, expected, observed, PASS or FAIL.
It exits 1 on any FAIL, a layout failure included.
It needs `git` and `bun`.

Patches are named `<merge>-<file>-ours.patch` and `<merge>-<file>-theirs.patch`, with `<file>` one of `agents`, `e2e` and `k8s`, because three merges (`dadb5f515a3e`, `2fc4b487c8fa` and `b1d8a21b1e8f`) conflicted in two of the files.
The demo patches are `append-<file>-<list>-ours.patch` and `-theirs.patch`.

## Classification

The classification works on the base file, from the hunks of the two patches.
A side's changed lines are the lines it deletes or rewrites, and its insertion points are the lines it inserts after.
Two changed lines collide when they are the same line or next to each other.
A changed line `c` and an insertion after line `n` collide when the insertion touches `c`: `n` is `c`, or `n` is `c - 1`.
Two insertions collide only at the same point ("same place").
Any unchanged line between two edits keeps them apart, which is what `git merge` needs.
A pair whose edits do not collide is "different sentences", or "different items" when a side adds or removes a whole item or section.
Otherwise it is "same or adjacent", with the colliding lines named, and it is expected to conflict, as every control is.
Every other pair is expected to merge clean.
The two Kubernetes evidence pairs are expected "either" and only recorded.

The patch places a new item where `diff -u` puts it, and `git merge-tree` computes its own diff, which can put an inserted item's blank line on the other side of it.
In the pairs here git put the blank line at the end of the inserted block: an item inserted between two items touches the item after it and is kept apart from the item before it by the blank line.
So a new item added directly after an item the other side edits merges clean (`990ebd7341e0`, `22817ba1d2a3`), and one added directly before an item whose first line the other side edits conflicts (`b1d8a21b1e8f`, AGENTS).

## How each side was ported

Each patch applies to the head file, commit `94850c01741e`, and only to it; "the head" below means that commit.
A patch carries the side's sentence-level change of the pair's layout-caused hunks, one sentence per line, and nothing else of the side.
A SAME-SENTENCE hunk of a mixed merge is left out.

- **Forward**: the head does not hold the side's change.
  The patch makes the side's edits at the matching sentences and items of the head file.
  A new item, section or paragraph goes next to the item it belongs with, as each guide's file-head note says.

- **Reverse**: the head already holds the side's change, because the side is main, or because its pull request was squash-merged since.
  That change cannot be made twice, so the patch undoes it: it removes each sentence and item the side added, puts each sentence it changed back to the base wording, and restores what it removed.
  It touches the same head lines that the side's change put there, so the merge geometry is the same as for the forward change.
  Where a later commit rewrote a sentence the side changed, the patch puts back the side's base wording of that sentence.

- **Forward copy**: the head holds both sides' new script (index item and section) at the end of the script list, where they were appended before the file-head note's rule existed, and those three items and sections sit next to each other.
  A reverse port would replay that end-of-list placement, which the rule now forbids.
  So for `9a8af2f1743e`, `ee9ec60b7930` and `141989f72b63` (both sides) and `f95c4f77352a` (ours) the patch adds a second copy of the head's own item and section, word for word, where the plan's pair list and the rule put it.
  The duplicate heading is harmless to the replay.

A control applies a side's edit onto the head's own wording of the sentence when the head holds neither side's wording; each such case is listed below.

Some branch-parent commits are not in the local history (squash-merged pull requests): `bf53039b7bdc`, `7207e4db3483`, `7afd5d58123d`, `a67cadd13fb4` and `4eb48dee5c3b`.
Each of those sides is in the head, so its patch is made from the head's text and the merge base's wording, both of which are local (a reverse port, or for `a67cadd13fb4` a forward copy); the planning files name what each side changed.

## Pairs

Observed is from a run of `run.sh` on this commit.
Line numbers are lines of the head.

| Merge | File | Class | Expected | Observed |
| --- | --- | --- | --- | --- |
| dadb5f515a3e | AGENTS | different sentences | clean | clean |
| 19aaa96fe03b | AGENTS | different sentences | clean | clean |
| 9c96283b82a0 | AGENTS | different sentences | clean | clean |
| fb434588aa78 | AGENTS | different sentences | clean | clean |
| 2dce61d9cb6e | AGENTS | different sentences | clean | clean |
| 2fc4b487c8fa | AGENTS | different sentences | clean | clean |
| 22817ba1d2a3 | AGENTS | different items | clean | clean |
| b1d8a21b1e8f | AGENTS | same or adjacent (ours inserts after 310, theirs changes 311) | conflict | conflict, 1 hunk |
| 760468db6c78 (control) | AGENTS | same or adjacent (472 / 472) | conflict | conflict, 1 hunk |
| 82fd7d27b4f2 (control) | AGENTS | same or adjacent (305 / 305, 309 / 308-309) | conflict | conflict, 1 hunk |
| append (agents) | AGENTS Commands | different items | clean | clean |
| append (agents, control) | AGENTS Commands | same place (after 93) | conflict | conflict, 1 hunk |
| 9a8af2f1743e | e2e README | different items | clean | clean |
| ee9ec60b7930 | e2e README | different items | clean | clean |
| f95c4f77352a | e2e README | different items | clean | clean |
| 990ebd7341e0 | e2e README | different items | clean | clean |
| f714cec148b6 | e2e README | different items | clean | clean |
| bc1c2593a9cf | e2e README | different items | clean | clean |
| dadb5f515a3e | e2e README | different sentences | clean | clean |
| 141989f72b63 | e2e README | different items | clean | clean |
| 3ac0f1f0ca6e (control) | e2e README | same or adjacent (763, 4144, 4146, 4153) | conflict | conflict, 3 hunks |
| 9164e75b4c3c (control) | e2e README | same or adjacent (525 / 525) | conflict | conflict, 1 hunk |
| append (e2e) | e2e README index | different items | clean | clean |
| append (e2e, control) | e2e README index | same place (after 50) | conflict | conflict, 1 hunk |
| 0019fc59433e | kubernetes | different items | clean | clean |
| 600bb5490bb2 | kubernetes | different items | clean | clean |
| 3264771a1719 | kubernetes | different items | clean | clean |
| 4209950d9193 | kubernetes | different items | clean | clean |
| f22898635c73 | kubernetes | different items | clean | clean |
| b1d8a21b1e8f (evidence) | kubernetes | same or adjacent (ours inserts after 1921, theirs changes 1922) | either | conflict, 1 hunk |
| 2fc4b487c8fa (evidence) | kubernetes | same or adjacent (ours inserts after 785, theirs changes 786) | either | conflict, 1 hunk |
| e0ab76dcd45f (control) | kubernetes | same or adjacent (2247 / 2246) | conflict | conflict, 1 hunk |
| 702806bf4da7 (control) | kubernetes | same or adjacent (3485-3487 / 3484, 3487) | conflict | conflict, 1 hunk |
| eceefebffe3f (control) | kubernetes | same or adjacent (652 / 652-653) | conflict | conflict, 1 hunk |
| append (k8s) | kubernetes tool list | different items | clean | clean |
| append (k8s, control) | kubernetes tool list | same place (after 292) | conflict | conflict, 1 hunk |

Twenty of the twenty-one counted real pairs merge clean.
`b1d8a21b1e8f` on `AGENTS.md` conflicts in one of its three hunks, and the plan expected it clean:
ours adds the `claude-backend-sandbox.sh` command right after the `claude-backend-tmux.sh` command, the item it belongs with, and theirs adds `LEGION_E2E_PREVIOUS_IMAGE` to the Stage 4a command, the next item's first line.
Git puts the blank line at the end of the inserted item, so the insertion touches that line (the plan's hunk list for this merge leaves out ours' insertion).
Its other two hunks, the Architecture paragraph and the `claude-bridge`/`launcher` commands, merge.
Checked by hand and not part of `run.sh`: with the new item placed after the Stage 4a item instead, the other Kubernetes-runtime proof of the same shape, the pair merges clean.
Every control conflicts, and both Kubernetes evidence pairs conflict: each inserts a paragraph directly before a paragraph whose first line the other side rewrites.

## Each pair

### AGENTS.md

- **dadb5f515a3e** (base `135d22e0f63b`).
  Ours `63d4f066ff01`, forward: the record sentence (line 613) becomes "The record lives on the pull request alone: Dispatch opens no ask and takes no document edit on a closed issue, so the pull request's `Production:` line is the record whether or not the issue is closed".
  Theirs `8298fc710323` (main), reverse: the `pending` sentence (line 605) goes back to "its session and the issue's volume, the pod goes, and wakes the check".

- **19aaa96fe03b** (base `5c4548f0aeb4`).
  Ours `887d0fecb158`, forward: the same record sentence (line 613).
  Theirs `ba9a24e238c3` (main), reverse: the `fail` sentence (lines 609-611) goes back to its base wording, on one line.

- **9c96283b82a0** (base `7bf2759c6e17`).
  Theirs `9e62d7451f2c` (main), reverse: removes `actions: write` from the Release jobs bullet (line 497).
  Ours `45c6016a44a0`, forward, **substituted**: ours rewrote the Deploys bullet's clause "`packages/claude-envoy`, which no workflow releases, built from a checkout of a commit on `main`", and main has since rewritten that clause ("whose built files main carries, committed by the rebuild workflow").
  The patch makes the same kind of edit on the head's clause (line 520): it ends "…(`claude-envoy-rebuild.yaml`): a Kubernetes deployment takes it from the worker image, which carries that commit's build of it, and a tmux one uses a checkout of `main`."

- **fb434588aa78** (base `bc98d3e729b9`).
  Ours `e137db372728`, reverse: removes the joinable-root and ancestry-hold sentences ours inserted after "…as any root does." (lines 640-651).
  Theirs `aa64baf632c2` (main), reverse: removes the boot-recording sentences theirs inserted after "…`todo`." (lines 635-638), with the clause "labelled or not" a later commit added inside them.
  Line 639 stays between them.

- **2dce61d9cb6e** (base `6d505c12f146`).
  Ours `bf53039b7bdc`, reverse: the Stage 3 command's description (line 298) loses "the architect's acceptance", "and the close it makes" and "the implementer's production check after the close", back to "the daemon's merge of the READY head, production check, restart".
  Theirs `9e9b2c493fd1` (main), reverse: removes the repository-cache clause from the Stage 4a description (line 314), in the wording the head has now.

- **2fc4b487c8fa** (base `56ba79b8cf6d`).
  Ours `6bae3e0e82fa`, forward: the `claude-bridge` description (lines 145-147) reads "puts on claude, under either runtime (… on a relaunch; one definition of it, `packages/daemon/internal/claudelaunch`):", drops "sends pi-legion's phase-stall follow-up," and gains the line "It is transport: the phase reminder runs in the worker's own MCP server (`packages/claude-envoy`), driven by Claude Code's hooks".
  Theirs `3ca092a69891` (main), reverse: removes `[--stop-grace <d>] [--session-instance …]` from the single-role `legion launcher` form (line 149) and the session-flags sentence (lines 158-162).
  The multi-role form, which main added later, keeps its flags.

- **22817ba1d2a3** (base `a5b302e42403`).
  Ours `0ec47d562429`, reverse: the same Stage 3 description change as `2dce61d9cb6e`'s ours (line 298).
  Theirs `3442ab023b9f` (main), reverse: removes the `claude-backend-tmux.sh` command item (lines 305-310).

- **b1d8a21b1e8f** (base `8411fe855388`).
  Ours `8c83c9a21c80`, forward: "Under `runtime: tmux`, a phase worker … place:" becomes "A phase worker … place under either runtime:" (line 18);
  the `claude-bridge` change as in `2fc4b487c8fa` (lines 145-147);
  "devbox-only live proof:" becomes "devbox-only live proof under tmux:" (line 307);
  and the new `claude-backend-sandbox.sh` command item goes right after the `claude-backend-tmux.sh` item.
  Ours' `LEGION_E2E_MODEL_ROUTE=gateway` and key-file alternative on the `claude-backend-tmux.sh` command are not ported: the head carries both already, in the form main's side of `82fd7d27b4f2` wrote.
  Theirs `6cdb99959fa1` (main), reverse: "with one container and `legion launcher` per resident role" (line 16);
  the `legion launcher` item back to its single-role form (lines 149, 151-157);
  the Stage 2 box-login clause (lines 292-294) and the Stage 3 box-login line (line 301) removed;
  `LEGION_E2E_PREVIOUS_IMAGE` removed from the Stage 4a command (line 311).

- **760468db6c78 (control)** (base `131ccd0a0cd5`).
  The head holds ours' wording of the repository-map sentence (line 472).
  Ours `f77093c2a071`, reverse: "The `envoy-infra` Pulumi plugin, a Go module outside the root `go.work`."
  Theirs `b450e3b55ce9` (main), forward onto the head's sentence: "The `envoy-infra` Pulumi plugin and the `envoy-infra seed` command, a Go module outside the root `go.work`."

- **82fd7d27b4f2 (control)** (base `6cdb99959fa1`).
  The head holds theirs' version of the `claude-backend-tmux.sh` command.
  Theirs `b4bea3858263` (main), reverse: removes the leading `LEGION_E2E_MODEL_ROUTE=gateway` (line 305) and the key-file alternative (lines 308-309).
  Ours `b1d8a21b1e8f`, forward onto the head's command: `LEGION_E2E_MODEL_ROUTE=gateway` moves to ours' place after `LEGION_E2E_CLAUDE_KEY_FILE` (line 305), and the key-file line takes ours' wording, without "where that one key file serves the Claude Code pane too" (line 309).

### scripts/e2e/README.md

- **9a8af2f1743e** (base `a6fa12112d16`), forward copies.
  Ours `41d4aba56b35`: the `dispatch-claimant-delivery.sh` item after the `dispatch-session-settings.sh` item (after line 113) and its section after `## dispatch-session-settings.sh` (after line 2695), next to the other `dispatch-*` proofs.
  Theirs `d1f4b8a64a96` (main): the `no-required-checks.sh` item after the `merged-root-waits.sh` item (after line 149) and its section after `## merged-root-waits.sh` (after line 3087).

- **ee9ec60b7930** (base `002a4ee98749`), forward copies.
  Ours `55e34c2565b7`: the `child-adoption.sh` item after the `merged-root-waits.sh` item (after line 149) and its section after `## merged-root-waits.sh` (after line 3087).
  Theirs `63cd684a8256` (main): the `no-required-checks.sh` item before the `merged-root-waits.sh` item (after line 139) and its section before `## merged-root-waits.sh` (after line 3042), the other side of that whole-daemon sibling.

- **f95c4f77352a** (base `b6d8fa2ac33c`).
  Ours `c2fefc336831`, forward copy: the `child-adoption.sh` item right after the `merged-root-waits.sh` item (after line 149), as ours placed it.
  Theirs `c3d09eec924c` (main), reverse: the `merged-root-waits.sh` item goes back to "keeps a tree's order," and "is refused 409 `SIGNOFF_CHILD_IN_WORKFLOW` naming the child, and lands once that child is parked;" (lines 142-143).

- **990ebd7341e0** (base `371d6c879565`).
  Ours `40a963e258e5`, forward: the check item `stop-hook-reminds-the-open-phase` (its tmux wording) between `claude-implementer-registers` and `claude-implementer-handoff` (after line 725), where ours put it.
  Theirs `f2c17ded5305` (main), reverse: "a child of the run's `legion claude-bridge`" becomes "a child of `legion claude-bridge`" (line 721) and the bridge-parent sentences go (lines 722-724).

- **f714cec148b6** (base `61552da1b1b7`).
  Ours `7207e4db3483`, reverse: the `pod-baseline` item goes back to `XDG_STATE_HOME=/home/legion/.local/state/architect` with "a state home per role container" and no `TMPDIR` clause (lines 1157-1159), and to the `/home/legion/.local/state/architect/omp/profiles/legion` path (line 1161).
  Theirs `2127101bddff` (main), reverse: removes the `setup-ran` item (lines 1168-1187), with the sentences later commits added inside it.

- **bc1c2593a9cf** (base `098c3cc7387a`).
  Ours `7afd5d58123d`, reverse: removes the three input items PR #2024 added before `LEGION_E2E_MODEL_GATEWAY_URL` (`LEGION_E2E_POD_REACHABILITY`, `LEGION_E2E_MODEL_ROUTE`, `LEGION_E2E_STORAGE_CLASS`, lines 952-965), and puts back "required" for the two gateway inputs (lines 966, 971), "on the devbox's private address" for the worker-stream port (line 978) and the base wording of the session-database port (lines 984-985).
  Theirs `c13a98917e34` (main), reverse: removes the `LEGION_E2E_REPO_SECOND_OWNER` item (lines 987-992).

- **dadb5f515a3e** (base `135d22e0f63b`).
  Ours `63d4f066ff01`, forward: "The proof sends it nothing there: the issue is closed, and a closed issue takes no Dispatch write (409 `ISSUE_CLOSED`), so" becomes "The proof sends it nothing there, so" (line 2208).
  Theirs `8298fc710323` (main), reverse, every sentence of its hunk: the one PVC of children 3 and 4 (line 2188), the children's parks (lines 2201-2202), the pending report (lines 2211-2212), the relaunch and the second suspension in `production-check-bound` (lines 2224, 2228), `node-release` (lines 2234-2235, 2243-2244), `close` (lines 2249-2252), `re-admission` (lines 2257-2260) and `operator-close` (lines 2265, 2267).
  Theirs' nearest changes are lines 2201-2202 and 2211-2212, so lines 2203-2207 and 2209-2210 stay unchanged on either side of ours' line.

- **141989f72b63** (base `3442ab023b9f`), forward copies, each next to the script section that sources it, as the plan's pair list says.
  Ours `a67cadd13fb4`: `## lib/kind-sandbox.sh` after `## controller-sweep-kind.sh` (after line 3012), the script ours' text named as its user.
  Theirs `61552da1b1b7` (main): `## lib/bridge-tunnel.sh` after the Stage 4a section (after line 1469).
  In the head both sections already sit next to each other in the `lib/` group (lines 4209 and 4226).

- **3ac0f1f0ca6e (control)** (base `448a41003a17`).
  Theirs `b08a56bce0c8` (main), reverse: `agent-secrets` back to `secrets` in the four sentences (lines 763-764, 4144, 4146, 4153), with the base's `secrets get <KEY> --no-request` check.
  Ours `337d8e587665`, forward onto the head's sentences: `<runner>` in place of the head's `agent-secrets` (lines 763, 4153, with ours' runner clause), "the key runner" (line 4144) and ours' new `LEGION_E2E_APP_KEY_RUNNER` row after it, and "Each App input defaults to" (line 4146).

- **9164e75b4c3c (control)** (base `5c07a93847ef`).
  The head holds ours' wording "through the configured route:" (line 525).
  Ours `4eb48dee5c3b`, reverse: "through the Hawk model gateway, the route every devbox agent session uses:".
  Theirs `920e2b2e94e4` (main), forward onto the head's sentence: "through the Hawk model gateway, on a devbox whose agent sessions use that gateway:".
  Ours' change to the next sentence is not ported: the control is the one sentence both sides changed, and main has since added the box-login route inside that next sentence.

### docs/kubernetes.md

- **0019fc59433e** (base `decf80f922a1`).
  Ours `21fcfc9033fb`, reverse: `dispatch-writing-specs` back to `dispatch-brainstorming` (line 281).
  Theirs `fc379b99147a` (main), reverse: removes the `asd-ste100` item (lines 275-276).

- **600bb5490bb2** (base `0af6288a7a9f`).
  Ours `03b71899847f`, forward: the item "the tester's `ce-dogfood`" after `ce-simplify-code` (after line 272).
  Theirs `167e30b9c674` (main), reverse: removes the `asd-ste100` item (lines 275-276).

- **3264771a1719** (base `0587774a744f`).
  Ours `9d01535edaa1`, forward: the `ce-dogfood` item as above.
  Theirs `d1f4b8a64a96` (main), reverse: `dispatch-writing-specs` back to `dispatch-brainstorming` (line 281).

- **4209950d9193** (base `c8974e2eb6aa`).
  Ours `a7f72b00fe8a`, forward: `agent-browser --version &&` after `chromium --version &&` in the toolchain check (after line 451).
  Theirs `0587774a744f` (main), reverse: removes `gcc --version &&` (line 440).

- **f22898635c73** (base `9e62d7451f2c`).
  Theirs `14571f6f4aea` (main), reverse: removes the `psql`, `redis-cli`, `sqlite3`, `nc` and `dig` items (lines 313-322).
  Ours `918580af3adc`, forward, **substituted**: ours appended "and the `claude` the `claude-code` row reports" after "with `go version` running", the end of the old enumeration, and dropped the "and" before `go`.
  In the head the tools are a list that ends the sentence, so the clause goes into the sentence's own line, before "and these on `PATH` with `go version` running:" (line 287), and the dropped "and" has no counterpart.

- **b1d8a21b1e8f (evidence)** (base `8411fe855388`).
  Ours `8c83c9a21c80`, forward: the paragraph "**A Claude Code role's model route is the operator's too.** …" directly before the `setup` paragraph, as ours placed it (after line 1921).
  Theirs `6cdb99959fa1` (main), reverse: the `setup` paragraph's first sentence goes back to "an argv run in every role's container — each workflow role's in an issue pod, and the controller's under `controller: daemon` —" (lines 1922-1923).
  Git conflicts: the inserted paragraph's blank line ends up after it, so the insertion touches line 1922.

- **2fc4b487c8fa (evidence)** (base `56ba79b8cf6d`).
  Ours `6bae3e0e82fa`, forward: the paragraph "A role on Claude Code is outside all of this. …" directly before "The variables are a generation's" (after line 785).
  Theirs `3ca092a69891` (main), reverse: the base wording of its three sentences there (lines 786-792).
  Theirs' "under either store" to "under any store" is not ported: the head's sentence (line 794) names no store any more.
  Git conflicts, as for `b1d8a21b1e8f`.

- **e0ab76dcd45f (control)** (base `bc307fb5675e`).
  Both sides are in the head.
  Ours `48898be0dc12`, reverse: "since no role container of the pod has started" (line 2247).
  Theirs `0587774a744f` (main), reverse: removes "— when the retry runs before the daemon replaces the pod, which it does within seconds of reading the failure (see `workspace-fetch` above)" (lines 2244-2246).
  These lines of the head are still wrapped at about 100 columns, two sentences on line 2246, so the two adjacent sentences meet on adjacent lines.

- **702806bf4da7 (control)** (base `ae69d783458f`).
  Both sides are in the head, ours in the form main later gave it.
  Ours `e1569f0f0b29`, reverse: removes the GKE Autopilot exception to the `nproc` formula (lines 3485-3487).
  Theirs `098c3cc7387a` (main), reverse: "⌈Σ cpu limits⌉" back to "⌈Σ cpu⌉" and "Σ memory limits" back to "Σ memory" (lines 3484, 3487).
  Theirs' change to the scheduler sentence is not ported: main has since rewritten it.

- **eceefebffe3f (control, hunk 1 alone)** (base `c03eee9a141e`).
  Theirs `b95eba2806b7` (main), reverse: "Three keys …, all inside … mapping:" and the `session_store` sentence back to "Two keys in `legion.yaml`, both inside the `runtime.kubernetes` mapping:" (lines 652-653).
  Ours `6de2ffbb95db`, forward onto the head's sentence: "…mapping, and all required:" (line 652).

### Appended items demo

The demo items are invented, each in the item shape of its list.

- AGENTS `## Commands`: `append-agents-commands-ours` adds `legion replay-demo-a <team>` after `legion status <team>`, and `-theirs` adds `legion replay-demo-b` after `legion legions`.
  The control `append-agents-same-ours` and `-theirs` add `replay-demo-c` and `replay-demo-d` at one point, after `legion status <team>`.

- e2e README index list: `append-e2e-index-ours` adds `replay-demo-a.sh` after `stage1-skeleton.sh`, and `-theirs` adds `replay-demo-b.sh` after `burstable-kind.sh`.
  The control adds `replay-demo-c.sh` and `replay-demo-d.sh` at one point, after `stage1-skeleton.sh`.

- Kubernetes capability-check tool list: `append-k8s-tools-ours` adds `replay-demo-a` after `gcc`, and `-theirs` adds `replay-demo-b` after `jq`.
  The control adds `replay-demo-c` and `replay-demo-d` at one point, after `gcc`.

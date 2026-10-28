# Replay classification: `docs/kubernetes.md`

Source and method as for the root AGENTS.md file. Most of this file is already hard-wrapped at about 100 columns, so most hunks sit in wrapped paragraphs; a hunk that starts mid-paragraph is judged on the whole sentence.

Merges: 19; layout-caused: 3; not layout-caused (at least one SAME-SENTENCE hunk): 16.

Hunks by cause: ADJACENT 6, APPEND 1, CODE-BLOCK(SAME-SENTENCE) 3, SAME-SENTENCE 38.

SAME-SENTENCE hunks by kind (clash = same words or fact; clauses = different clauses of one long sentence; enum = different items of one hand-written enumeration; rewrite = main rewrote or moved the text the branch edited; dup = both sides carry two versions of the same new text): clash 29, clauses 4, dup 2, enum 6.

Line numbers are base (merge-base) line numbers of that merge. `[auto]` marks a hunk labelled from the script's sentence diff without a manual read (only in merges already decided by another hunk).

## Every merge

### 702806bf4da7 — PR #2024 (e2e/stage4a-gke, 2026-10-19) — not layout-caused

base `ae69d783458f`, ours (branch parent) `e1569f0f0b29`, theirs (main parent) `098c3cc7387a`

- hunk 1: **SAME-SENTENCE** — base L1967-1970 (4 lines, longest 98 chars). [clash] L1967: both edit "inside a pod `nproc` is max(2, ⌈Σ cpu⌉)…"

### f714cec148b6 — PR #2085 (legion/INFRA-31, 2026-10-21) — not layout-caused

base `61552da1b1b7`, ours (branch parent) `7207e4db3483`, theirs (main parent) `2127101bddff`

- hunk 1: **SAME-SENTENCE** — base L1051-1055 (5 lines, longest 106 chars). [clash] L1051-1055 plugin-install sentence rewritten by both

### 905d57e3a97c — PR #2150, #2259 (sami/single-container-pod, 2026-10-24) — not layout-caused

base `2b4308b002cb`, ours (branch parent) `ed8941e8c0da`, theirs (main parent) `83c521465089`

- hunk 1: **SAME-SENTENCE** — base L1939-1948 (10 lines, longest 98 chars). [clash] L1947: ours "the bundle's root," -> "root (its own, bound there…)", theirs the `GH_CONFIG_DIR=…` item right after it

### aa5389c12a67 — PR #2150, #2259 (sami/single-container-pod, 2026-10-24) — not layout-caused

base `6fcac818ec32`, ours (branch parent) `f06246d78b95`, theirs (main parent) `2b4308b002cb`

- hunk 1: **SAME-SENTENCE** — base L1894-1897 (4 lines, longest 98 chars). [clauses] L1894: ours "every role container" -> "`roles`", theirs drops the "so the path the clone's alternates names…" tail

### 29b2d894bb65 — PR #2150, #2259 (sami/single-container-pod, 2026-10-25) — not layout-caused

base `531ba202f6d2`, ours (branch parent) `a636841acffe`, theirs (main parent) `4792c8fa421f`

- hunk 1: **SAME-SENTENCE** — base L2719-2729 (11 lines, longest 102 chars). [clash] theirs deletes the L2719-2729 passage whose first sentence ours edits
- hunk 2: **SAME-SENTENCE** — base L2753-2757 (5 lines, longest 96 chars). [clash] L2753: both edit "because **the role container has no client…**"

### 2fc4b487c8fa — PR #2186 (legion/INFRA-220, 2026-10-25) — LAYOUT-CAUSED

base `56ba79b8cf6d`, ours (branch parent) `6bae3e0e82fa`, theirs (main parent) `3ca092a69891`

- hunk 1: **ADJACENT** — base L690-698 (9 lines, longest 97 chars). ours inserts the paragraph "A role on Claude Code is outside all of this. …" before the L690 paragraph, whose first sentences ("The variables are a generation's, but the URL file's mount is the pod's. A pod whose providers volume…") theirs rewrites

### 9cdb4130c10a — PR #2185 (legion/INFRA-354, 2026-10-25) — not layout-caused

base `5c182bd16f0a`, ours (branch parent) `3c5b8aaef2e4`, theirs (main parent) `158654c89e8b`

- hunk 1: **SAME-SENTENCE** — base L2705-2705 (1 lines, longest 88 chars). [clash] L2705 "…after the Job is created:" both
- hunk 2: **CODE-BLOCK(SAME-SENTENCE)** — base L2716-2716 (1 lines, longest 82 chars). [clash] L2716 selector line
- hunk 3: **SAME-SENTENCE** — base L2772-2810 (39 lines, longest 97 chars). [clash] L2772-2810 both rewrite the Services evidence

### ace35f9ce9f3 — PR #2173 (legion/INFRA-80, 2026-10-25) — not layout-caused

base `b95eba2806b7`, ours (branch parent) `98bd4a90f4b7`, theirs (main parent) `0306bcbe16df`

- hunk 1: **SAME-SENTENCE** — base L3113-3115 (3 lines, longest 96 chars). [clauses] L3113: theirs "those arrive through the image's package runners" clause, ours "the issue's own disk … outlives any one pod" clause

### eceefebffe3f — PR #2173 (legion/INFRA-80, 2026-10-25) — not layout-caused

base `c03eee9a141e`, ours (branch parent) `6de2ffbb95db`, theirs (main parent) `b95eba2806b7`

- hunk 1: **SAME-SENTENCE** — base L604-604 (1 lines, longest 72 chars). [clash, hand count] L604: ours "both inside the `runtime.kubernetes` mapping:" -> "mapping, and both required:", theirs "Two keys … both inside" -> "Three keys … all inside"
- hunk 2: **SAME-SENTENCE** — base L613-615 (3 lines, longest 99 chars). [clash] L613-615 `postgres` store sentence
- hunk 3: **SAME-SENTENCE** — base L636-658 (23 lines, longest 100 chars). [clash] L636-657
- hunk 4: **SAME-SENTENCE** — base L660-661 (2 lines, longest 99 chars). [clash] L660-661
- hunk 5: **SAME-SENTENCE** — base L690-698 (9 lines, longest 97 chars). [clash] L690-698
- hunk 6: **SAME-SENTENCE** — base L702-703 (2 lines, longest 96 chars). [clash] L702 "on it, `pvc` or `postgres`,"
- hunk 7: **SAME-SENTENCE** — base L1033-1034 (2 lines, longest 117 chars). [clash] L1033-1034
- hunk 8: **SAME-SENTENCE** — base L1039-1039 (1 lines, longest 52 chars). [clash] L1039
- hunk 9: **SAME-SENTENCE** — base L1494-1498 (5 lines, longest 98 chars). [clash] L1494-1498 `session_store` refusal bullets
- hunk 10: **CODE-BLOCK(SAME-SENTENCE)** — base L3669-3670 (2 lines, longest 128 chars). [clash] `runtime:` yaml block L3669-3670 comments

### f22898635c73 — PR #2186 (legion/INFRA-220, 2026-10-25) — not layout-caused

base `9e62d7451f2c`, ours (branch parent) `918580af3adc`, theirs (main parent) `14571f6f4aea`

- hunk 1: **SAME-SENTENCE** — base L192-193 (2 lines, longest 143 chars). [enum] capability-check tool list L192-193: ours drops "and" before `go` and appends ", and the `claude` the `claude-code` row reports" after "with `go version` running"; theirs "`ssh-keygen` and `flock`" -> "`ssh-keygen`, `flock`, `psql`, `redis-cli`, `sqlite3`, `nc` and `dig`"

### 0019fc59433e — PR #2136, #2160 (infra-140-s2, 2026-10-26) — not layout-caused

base `decf80f922a1`, ours (branch parent) `21fcfc9033fb`, theirs (main parent) `fc379b99147a`

- hunk 1: **SAME-SENTENCE** — base L275-275 (1 lines, longest 118 chars). [enum] skills enumeration L275: theirs adds `asd-ste100` before `dispatch`, ours renames `dispatch-brainstorming` -> `dispatch-writing-specs`

### 2479de885900 — PR #2186 (legion/INFRA-220, 2026-10-26) — not layout-caused

base `f2c17ded5305`, ours (branch parent) `c481e666f8cc`, theirs (main parent) `8298fc710323`

- hunk 1: **SAME-SENTENCE** — base L281-281 (1 lines, longest 90 chars). [enum] capability-check tool list L281: ours drops "and" before `go`, theirs adds `gcc` after `go`
- hunk 2: **SAME-SENTENCE** — base L970-982 (13 lines, longest 101 chars). [clash] L979: ours extends "It prints `marked lost` or `failed to mark lost` per claim…", theirs replaces that sentence

### 3264771a1719 — PR #2219 (legion/INFRA-248, 2026-10-26) — not layout-caused

base `0587774a744f`, ours (branch parent) `9d01535edaa1`, theirs (main parent) `d1f4b8a64a96`

- hunk 1: **SAME-SENTENCE** — base L298-300 (3 lines, longest 132 chars). [enum] shipped agents/skills enumeration L298-300: ours "rubrics and `ce-simplify-code`" -> "rubrics, `ce-simplify-code` and the tester's `ce-dogfood`", theirs `dispatch-brainstorming` -> `dispatch-writing-specs`

### 4209950d9193 — PR #2219 (legion/INFRA-248, 2026-10-26) — not layout-caused

base `c8974e2eb6aa`, ours (branch parent) `a7f72b00fe8a`, theirs (main parent) `0587774a744f`

- hunk 1: **SAME-SENTENCE** — base L383-383 (1 lines, longest 473 chars). [enum] capability-check probe command L383 (one inline `docker run … -c '… && …'`): ours adds `agent-browser --version &&` after `chromium --version`, theirs adds `gcc --version &&` after `go version`

### 600bb5490bb2 — PR #2219 (legion/INFRA-248, 2026-10-26) — not layout-caused

base `0af6288a7a9f`, ours (branch parent) `03b71899847f`, theirs (main parent) `167e30b9c674`

- hunk 1: **SAME-SENTENCE** — base L273-275 (3 lines, longest 118 chars). [enum] shipped agents/skills enumeration L273-275: ours adds the tester's `ce-dogfood`, theirs adds `asd-ste100` to the Envoy plugin's skills

### a3dd8e7a0730 — PR #2150, #2259 (sami/single-container-pod, 2026-10-26) — not layout-caused

base `12cbe2cdcc62`, ours (branch parent) `b54364dbd7de`, theirs (main parent) `7f6ef46fd42f`

- hunk 1: **SAME-SENTENCE** — base L615-621 (7 lines, longest 96 chars). [clash] L615-621 [auto]
- hunk 2: **SAME-SENTENCE** — base L1030-1037 (8 lines, longest 127 chars). [clash] L1034 "Every role of the issue shares the issue's checkout, the issue's own volume…"
- hunk 3: **SAME-SENTENCE** — base L1185-1188 (4 lines, longest 101 chars). [clash] L1185-1188 [auto]
- hunk 4: **SAME-SENTENCE** — base L1285-1287 (3 lines, longest 101 chars). [clash] L1285 `resources.<role>` sentence
- hunk 5: **APPEND** — base insertion before L1334 (0 lines, longest 0 chars). both insert at L1334 [auto]
- hunk 6: **SAME-SENTENCE** — base L1542-1544 (3 lines, longest 99 chars). [clash] L1543 "…and everything a role container writes"
- hunk 7: **ADJACENT** — base L1577-1578 (2 lines, longest 97 chars). L1577-1578 [auto]
- hunk 8: **ADJACENT** — base L2113-2115 (3 lines, longest 93 chars). L2113-2115 [auto]
- hunk 9: **SAME-SENTENCE** — base L2182-2185 (4 lines, longest 99 chars). [clash] L2182-2185
- hunk 10: **SAME-SENTENCE** — base L2201-2210 (10 lines, longest 100 chars). [clash] L2208
- hunk 11: **SAME-SENTENCE** — base L2212-2215 (4 lines, longest 98 chars). [clauses] L2212
- hunk 12: **ADJACENT** — base L2450-2457 (8 lines, longest 96 chars). L2450-2457 [auto]
- hunk 13: **SAME-SENTENCE** — base L2756-2756 (1 lines, longest 91 chars). [clauses] L2756 "Each of its role containers mounts the volume at `/legion`:"
- hunk 14: **SAME-SENTENCE** — base L3480-3491 (12 lines, longest 100 chars). [clash] L3480-3491

### b1d8a21b1e8f — PR #2186 (legion/INFRA-220, 2026-10-26) — LAYOUT-CAUSED

base `8411fe855388`, ours (branch parent) `8c83c9a21c80`, theirs (main parent) `6cdb99959fa1`

- hunk 1: **ADJACENT** — base L1990-1991 (2 lines, longest 96 chars). ours inserts the paragraph "**A Claude Code role's model route is the operator's too.** …" before the `setup` paragraph L1990, whose first sentence theirs rewrites ("an argv run in every role's container" -> "an argv run for every role")

### c77f8bb32f31 — PR #2259 (sami/2150-doc-followup, 2026-10-26) — not layout-caused

base `0587774a744f`, ours (branch parent) `32d8101851ef`, theirs (main parent) `336d98570451`

- hunk 1: **SAME-SENTENCE** — base insertion before L1425 (0 lines, longest 0 chars). [dup] both insert the one-container rollback text at L1425, in two versions (stacked PRs #2150/#2259)
- hunk 2: **SAME-SENTENCE** — base insertion before L1620 (0 lines, longest 0 chars). [dup] both insert the "Moving … to one `roles` container" upgrade text at L1620, two versions
- hunk 3: **CODE-BLOCK(SAME-SENTENCE)** — base L3048-3050 (3 lines, longest 87 chars). [clash] L3048-3050 `kubectl exec … -c architect` block
- hunk 4: **SAME-SENTENCE** — base L3058-3062 (5 lines, longest 100 chars). [clash] L3058-3062 "The `export` above sets that one…"

### e0ab76dcd45f — PR #2150, #2259 (sami/single-container-pod, 2026-10-26) — LAYOUT-CAUSED

base `bc307fb5675e`, ours (branch parent) `48898be0dc12`, theirs (main parent) `0587774a744f`

- hunk 1: **ADJACENT** — base L2427-2428 (2 lines, longest 97 chars). hard-wrapped list item L2427-2428: theirs extends "…and every later write of a locked file fails on it" with the retry clause, ours rewrites the next sentence "No process can hold one when the retry runs, since no role container of the pod has started…" (adjacent sentences)

## Proposed replay pairs

Only three merges are layout-caused by this task's definition, and two of them insert a paragraph directly before a paragraph whose first sentence the other side rewrites, and the third edits adjacent sentences. Under one sentence per line those three would still conflict unless the inserted paragraph is placed elsewhere or a blank line ends up between the two changes. Five more merges are SAME-SENTENCE [enum]: different items of the hand-written enumerations the spec names (the shipped agents/skills paragraph near L273-300, the capability-check tool list, the capability probe command). They are proposed as pairs because the spec's acceptance covers "add different items to one list", and they merge clean only if the restructure turns each enumeration into a list with one item per line. Two SAME-SENTENCE controls follow.

### ENUM (skills list): 0019fc59433e — PR #2136, #2160

Theirs adds `asd-ste100` before `dispatch`; ours renames `dispatch-brainstorming` two items later. Expected: clean once each skill is its own list line.

- merge `0019fc59433e`; base `decf80f922a1`; ours `21fcfc9033fb`; theirs `fc379b99147a`; hunk causes: SAME-SENTENCE
- hunk 1 (base L275-275):
  - ours: base L275: «`dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP or plugins are» -> «`dispatch`, `dispatch-first`, `dispatch-writing-specs` and `envoy` skills in its), so a build whose OMP or plugins are»; all changes: ``dispatch`, `dispatch-first`, [-`dispatch-brainstorming`-] {+`dispatch-writing-specs`+} and `envoy` skills in … `
  - theirs: base L275: «`dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP or plugins are» -> «`asd-ste100`, `dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP or plugins are»

### ENUM (agents and skills list): 600bb5490bb2 — PR #2219

Ours adds the tester's `ce-dogfood` beside `ce-simplify-code`; theirs adds `asd-ste100` to the Envoy plugin's skills. Expected: clean as a list.

- merge `600bb5490bb2`; base `0af6288a7a9f`; ours `03b71899847f`; theirs `167e30b9c674`; hunk causes: SAME-SENTENCE
- hunk 1 (base L273-275):
  - ours: base L273-275: «planner's `plan-gap-analyst` and `plan-reviewer`, in `agents/`, and the pair's rubrics and `ce-simplify-code` with Legion's other skills in its `dist/skills`; the Envoy plugin ships the `dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP or plugins are» -> «planner's `plan-gap-analyst` and `plan-reviewer`, in `agents/`, and the pair's rubrics, `ce-simplify-code` and the tester's `ce-dogfood` with Legion's other skills in its `dist/skills`; the Envoy plugin ships the `dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP…»; all changes: ` … `agents/`, and the pair's [-rubrics-] {+rubrics, `ce-simplify-code`+} and [-`ce-simplify-code`-] {+the tester's `ce-dogfood`+} with Legion's other skills … `
  - theirs: base L273-275: «…mplify-code` with Legion's other skills in its `dist/skills`; the Envoy plugin ships the `dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP or plugins are» -> «…mplify-code` with Legion's other skills in its `dist/skills`; the Envoy plugin ships the `asd-ste100`, `dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP or plugins are»

### ENUM (agents and skills list): 3264771a1719 — PR #2219

Ours adds `ce-dogfood`; theirs renames `dispatch-brainstorming` -> `dispatch-writing-specs`. Expected: clean as a list. (At this base the two edits sit on adjacent wrapped lines; one sentence per line alone would join them onto one line.)

- merge `3264771a1719`; base `0587774a744f`; ours `9d01535edaa1`; theirs `d1f4b8a64a96`; hunk causes: SAME-SENTENCE
- hunk 1 (base L298-300):
  - ours: base L298-300: «planner's `plan-gap-analyst` and `plan-reviewer`, in `agents/`, and the pair's rubrics and `ce-simplify-code` with Legion's other skills in its `dist/skills`; the Envoy plugin ships the `asd-ste100`, `dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP or plugins a…» -> «planner's `plan-gap-analyst` and `plan-reviewer`, in `agents/`, and the pair's rubrics, `ce-simplify-code` and the tester's `ce-dogfood` with Legion's other skills in its `dist/skills`; the Envoy plugin ships the `asd-ste100`, `dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a b…»; all changes: ` … `agents/`, and the pair's [-rubrics-] {+rubrics, `ce-simplify-code`+} and [-`ce-simplify-code`-] {+the tester's `ce-dogfood`+} with Legion's other skills … `
  - theirs: base L298-300: «…skills`; the Envoy plugin ships the `asd-ste100`, `dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP or plugins are» -> «…skills`; the Envoy plugin ships the `asd-ste100`, `dispatch`, `dispatch-first`, `dispatch-writing-specs` and `envoy` skills in its), so a build whose OMP or plugins are»; all changes: ` … the `asd-ste100`, `dispatch`, `dispatch-first`, [-`dispatch-brainstorming`-] {+`dispatch-writing-specs`+} and `envoy` skills in … `

### ENUM (capability probe): 4209950d9193 — PR #2219

Capability probe command L383: ours adds `agent-browser --version`, theirs `gcc --version`, at different points of one `&&` chain in an inline code span. Expected: clean only if the probe becomes a fenced block with one command per line; as one inline span it stays one line and conflicts.

- merge `4209950d9193`; base `c8974e2eb6aa`; ours `a7f72b00fe8a`; theirs `0587774a744f`; hunk causes: SAME-SENTENCE
- hunk 1 (base L383-383):
  - ours: base L383: «…sion && nc -h && dig -v && ssh-keygen -t ed25519 -N "" -f /tmp/k && chromium --version && uv python install 3.13 && uv run --python 3.13 python -c "print(1)"'`.» -> «…sion && nc -h && dig -v && ssh-keygen -t ed25519 -N "" -f /tmp/k && chromium --version && agent-browser --version && uv python install 3.13 && uv run --python 3.13 python -c "print(1)"'`.»; all changes: ` … && chromium --version && {+agent-browser --version &&+} uv python install 3.13 … `
  - theirs: base L383: «…--version && npm --version && aws --version && kubectl version --client && go version && gopls version && python3 --version && jq --version && flock --version && psql --version && redis-cli --version && sqlite3 --version && nc -h && dig -v && ssh-keygen -t ed25519 -N "" -f /tmp/k && chromium --version && uv python inst…» -> «…--version && npm --version && aws --version && kubectl version --client && go version && gcc --version && gopls version && python3 --version && jq --version && flock --version && psql --version && redis-cli --version && sqlite3 --version && nc -h && dig -v && ssh-keygen -t ed25519 -N "" -f /tmp/k && chromium --version …»; all changes: ` … --client && go version {+&& gcc --version+} && gopls version && … `

### ENUM (capability-check tool list): f22898635c73 — PR #2186

Theirs adds `psql`, `redis-cli`, `sqlite3`, `nc`, `dig` after `flock`; ours drops the "and" before `go` and adds the `claude` clause after "with `go version` running". Expected: clean as a list if the "with `go version` running" tail is a line of its own; ours' tail edit sits next to theirs' last added item, so watch adjacency.

- merge `f22898635c73`; base `9e62d7451f2c`; ours `918580af3adc`; theirs `14571f6f4aea`; hunk causes: SAME-SENTENCE
- hunk 1 (base L192-193):
  - ours: base L192-193: «profile's lock, and `go`, `curl`, `wget`, `python3`, `node`, `bun`, `uv`, `kubectl`, `jq`, `ssh-keygen` and `flock` on `PATH` with `go version` running — and prints the table, one `probe-image: capability <name>: <status> (<detail>)` line per» -> «profile's lock, `go`, `curl`, `wget`, `python3`, `node`, `bun`, `uv`, `kubectl`, `jq`, `ssh-keygen` and `flock` on `PATH` with `go version` running, and the `claude` the `claude-code` row reports — and prints the table, one `probe-image: capabili…»; all changes: `profile's lock, [-and-] `go`, `curl`, `wget`, `python3`, … `PATH` with `go version` [-running-] {+running, and the `claude` the `claude-code` row reports+} — and prints the … `
  - theirs: base L192-193: «…k, and `go`, `curl`, `wget`, `python3`, `node`, `bun`, `uv`, `kubectl`, `jq`, `ssh-keygen` and `flock` on `PATH` with `go version` running — and prints the table, one `probe-image: capability <name>: <status> (<detail>)` line per» -> «…k, and `go`, `curl`, `wget`, `python3`, `node`, `bun`, `uv`, `kubectl`, `jq`, `ssh-keygen`, `flock`, `psql`, `redis-cli`, `sqlite3`, `nc` and `dig` on `PATH` with `go version` running — and prints the table, one `probe-image: capability <name>: <status> (<detail>)` line per»; all changes: ` … `bun`, `uv`, `kubectl`, `jq`, [-`ssh-keygen`-] {+`ssh-keygen`, `flock`, `psql`, `redis-cli`, `sqlite3`, `nc`+} and [-`flock`-] {+`dig`+} on `PATH` with `go … `

### ADJACENT (paragraph before an edited paragraph): b1d8a21b1e8f — PR #2186

Ours inserts the Claude Code model-route paragraph before the `setup` paragraph (L1990), whose first sentence theirs rewrites. Expected: clean only if the replay puts ours' paragraph after the blank line that precedes `setup` with that blank line unchanged, or elsewhere; inserted flush against it, it conflicts.

- merge `b1d8a21b1e8f`; base `8411fe855388`; ours `8c83c9a21c80`; theirs `6cdb99959fa1`; hunk causes: ADJACENT
- hunk 1 (base L1990-1991):
  - ours: insert before base L1990 (start of hunk): «**A Claude Code role's model route is the operator's too.** A phase worker `worker_backends` puts on `claude` runs Claude Code in its pod, and Claude Code reads its route from its own environment: `ANTHROPIC_API_KEY`, and `ANTHROPIC_BASE_URL` and `ANTHROPIC_MODEL` for a gateway. Legion adds no key and no settings file for it. The credential goes in `provider_keys` (`ANTHROPIC_API_KEY: <the provide…»
  - theirs: base L1990-1991: «`setup` is the deployment's setup command, an argv run in every role's container — each workflow role's in an issue pod, and the controller's under `controller: daemon` — before each start of» -> «`setup` is the deployment's setup command, an argv run for every role — each workflow role in its own namespaces of an issue pod, and the controller in its container under `controller: daemon` — before each start of»; all changes: ` … command, an argv run [-in-] {+for+} every [-role's container-] {+role+} — each workflow [-role's-] {+role+} in {+its own namespaces of+} an issue pod, and the [-controller's-] {+controller in its container+} under `controller: daemon` — before each start of`

### ADJACENT (paragraph before an edited paragraph): 2fc4b487c8fa — PR #2186

Ours inserts "A role on Claude Code is outside all of this. …" before the L690 paragraph whose first two sentences theirs rewrites. Expected: as for `b1d8a21b1e8f`.

- merge `2fc4b487c8fa`; base `56ba79b8cf6d`; ours `6bae3e0e82fa`; theirs `3ca092a69891`; hunk causes: ADJACENT
- hunk 1 (base L690-698):
  - ours: insert before base L690 (start of hunk): «A role on Claude Code is outside all of this. Its conversation is Claude Code's own transcript, under the role's `CLAUDE_CONFIG_DIR` on the issue's volume, never a row of Oh My Pi's table, so its generation is told neither session variable and its launcher stats that file as a `pvc` launch does; a replacement pod continues it with `--resume <session id>`, the id read from the recorded path. Two co…»
  - theirs: base L690-696: «The variables are a generation's, but the URL file's mount is the pod's.» -> «The variables are a generation's, but the store a pod was created under is the pod's: the providers volume's session item under `postgres`, and the three session-proxy flags each role container's `legion launcher` carries under `cloudsql-iam` (`sessionStoreInPod`).»; all changes: `The variables are a generation's, but the [-URL file's mount-] {+store a pod was created under+} is the [-pod's.-] {+pod's: the providers volume's session item under `postgres`, and the three session-proxy flags each role container's `legion launcher` carries under `cloudsql-iam` (`sessionStoreInPod`).+}`
  - theirs: base L690-696: «A pod whose providers volume projects another session store than a pod created now would — one created before `postgres` was turned on, or after it was turned off — holds a move (`movedSessionStore`, `internal/runtime/sandbox/addresses.…» -> «A pod that holds another session store than a pod created now would holds a move (`movedSessionStore`, `internal/runtime/sandbox/addresses.go`) — a store turned on, turned off, changed from one kind to the other, or, under `cloudsql-iam…»; all changes: `A pod [-whose providers volume projects-] {+that holds+} another session store than … pod created now would [-— one created before `postgres` was turned on, or after it was turned off —-] holds a move (`movedSessionStore`, [-`internal/runtime/sandbox/addresses.go`):-] {+`internal/runtime/sandbox/addresses.go`) — a store turned on, turned off, changed from one kind to the other, or, under `cloudsql-iam`, a changed instance, database or login — and+} the next relaunch of … `
  - theirs: base L690-696: «The image probe's container is told `OMP_SESSION_SQL_DSN_FILE` too, so `legion probe-image`, which reads the providers directory as the shim does, never exports the URL into Oh My Pi's environment.» -> «Under `postgres` the image probe's container is told `OMP_SESSION_SQL_DSN_FILE` too, so `legion probe-image`, which reads the providers directory as the shim does, never exports the URL into Oh My Pi's environment; under `cloudsql…»; all changes: `[-The-] {+Under `postgres` the+} image probe's container is … into Oh My Pi's [-environment.-] {+environment; under `cloudsql-iam` the probe's pod mounts no session key and is told no pointer at all (`sessionDSNPointer`, `manifest.go`).+}`
  - theirs: base L698: «A resumed session also restores the model it last used, and under either store the Oh My Pi» -> «A resumed session also restores the model it last used, and under any store the Oh My Pi»; all changes: ` … last used, and under [-either-] {+any+} store the Oh My Pi`

### ADJACENT (adjacent sentences): e0ab76dcd45f — PR #2150, #2259

List item L2427-2428: theirs extends one sentence, ours rewrites the next. Expected: conflict under INFRA-631's rule (adjacent sentences); include only as evidence of that limit.

- merge `e0ab76dcd45f`; base `bc307fb5675e`; ours `48898be0dc12`; theirs `0587774a744f`; hunk causes: ADJACENT
- hunk 1 (base L2427-2428):
  - ours: base L2427-2428: «No process can hold one when the retry runs, since no role container of the pod has started and no other pod mounts the disk.» -> «No process can hold one when the retry runs, since the pod's `roles` container has not started and no other pod mounts the disk.»; all changes: ` … the retry runs, since [-no role-] {+the pod's `roles`+} container [-of the pod-] has {+not+} started and no other pod mounts the disk.`
  - theirs: base L2427: «   works on, and every later write of a locked file fails on it.» -> «   works on, and every later write of a locked file fails on it — when the retry runs before the daemon replaces the pod, which it does within seconds of reading the failure (see `workspace-fetch` above).»; all changes: ` … locked file fails on [-it.-] {+it — when the retry runs before the daemon replaces the pod, which it does within seconds of reading the failure (see `workspace-fetch` above).+}`

### CONTROL (SAME-SENTENCE): 702806bf4da7 — PR #2024

Both edit "inside a pod `nproc` is max(2, ⌈Σ cpu⌉)…" (L1967). Expected: conflict.

- merge `702806bf4da7`; base `ae69d783458f`; ours `e1569f0f0b29`; theirs `098c3cc7387a`; hunk causes: SAME-SENTENCE
- hunk 1 (base L1967-1970):
  - ours: base L1967-1969: «from the pod's cgroup, so inside a pod `nproc` is max(2, ⌈Σ cpu⌉) and `/proc/meminfo`'s `MemTotal` is about Σ memory of the pod's containers, and an active role may use the pod's whole reservation while its siblings idle.» -> «from the pod's cgroup, so `/proc/meminfo`'s `MemTotal` is about Σ memory of the pod's containers, and an active role may use the pod's whole reservation while its siblings idle.»
  - ours: insert after base L1970 (ending «container's; each container's own request and limit are what the API shows and the scheduler counts.»): «`nproc` inside the sandbox is max(2, ⌈Σ cpu⌉) too, except on GKE Autopilot, confirmed live by [Stage 4a's live proof](../scripts/e2e/README.md): there `nproc` reflects gVisor's own host-vCPU presentation instead, a known gap ([kubernetes/kubernetes#91665](https://github.com/kubernetes/kubernetes/issues/91665)), and the pod's own cgroup cpu quota (`cpu.cfs_quota_us`/`cpu.cfs_period_us`) is what act…»
  - theirs: base L1967-1970: «from the pod's cgroup, so inside a pod `nproc` is max(2, ⌈Σ cpu⌉) and `/proc/meminfo`'s `MemTotal` is about Σ memory of the pod's containers, and an active role may use the pod's whole reservation while its siblings idle.» -> «from the pod's cgroup, so inside a pod `nproc` is max(2, ⌈Σ cpu limits⌉) and `/proc/meminfo`'s `MemTotal` is about Σ memory limits of the pod's containers, and an active role may use the pod's whole reservation while its siblings idle.»; all changes: ` … `nproc` is max(2, ⌈Σ [-cpu⌉)-] {+cpu limits⌉)+} and `/proc/meminfo`'s `MemTotal` is about Σ memory {+limits+} of the pod's containers, … `
  - theirs: base L1967-1970: «…ng set, not one container's; each container's own request and limit are what the API shows and the scheduler counts.» -> «…ng set, not one container's; each container's own request and limit are what the API shows, and its requests are what the scheduler counts.»; all changes: ` … are what the API [-shows-] {+shows,+} and {+its requests are what+} the scheduler counts.`

### CONTROL (SAME-SENTENCE, hand count): eceefebffe3f — PR #2173

Hunk 1, L604: ours "both inside the `runtime.kubernetes` mapping:" -> "mapping, and both required:", theirs "Two keys … both" -> "Three keys … all". Expected: conflict (replay hunk 1 alone).

- merge `eceefebffe3f`; base `c03eee9a141e`; ours `6de2ffbb95db`; theirs `b95eba2806b7`; hunk causes: SAME-SENTENCE, SAME-SENTENCE, SAME-SENTENCE, SAME-SENTENCE, SAME-SENTENCE, SAME-SENTENCE, SAME-SENTENCE, SAME-SENTENCE, SAME-SENTENCE, CODE-BLOCK(SAME-SENTENCE)
- hunk 1 (base L604-604):
  - ours: base L604: «Two keys in `legion.yaml`, both inside the `runtime.kubernetes` mapping:» -> «Two keys in `legion.yaml`, both inside the `runtime.kubernetes` mapping, and both required:»; all changes: `Two keys in `legion.yaml`, both inside the `runtime.kubernetes` [-mapping:-] {+mapping, and both required:+}`
  - theirs: base L604: «Two keys in `legion.yaml`, both inside the `runtime.kubernetes` mapping:» -> «Three keys in `legion.yaml`, all inside the `runtime.kubernetes` mapping: `session_store` chooses the kind, and each kind carries the one key that opens its store.»; all changes: `[-Two-] {+Three+} keys in `legion.yaml`, [-both-] {+all+} inside the `runtime.kubernetes` mapping: {+`session_store` chooses the kind, and each kind carries the one key that opens its store.+}`
- hunk 2 (base L613-615):
  - ours: base L613-615: «`pvc` keeps each conversation a file in the `sessions` directory of the issue's volume. `postgres` keeps every pod agent's conversation, each role's and a daemon-launched controller's, in Oh My Pi's table.» -> «`postgres` keeps every pod agent's conversation, each role's and a daemon-launched controller's, in Oh My Pi's table, and it is the only store this runtime has. The loader refuses `pvc` and an omitted `session_store` alike, with the…»; all changes: `[-`pvc` keeps each conversation a file in the `sessions` directory of the issue's volume.-] `postgres` keeps every pod … in Oh My Pi's [-table.-] {+table, and it is the only store this runtime has. The loader refuses `pvc` and an omitted `session_store` alike, with the same line and its reason (`readSessionStore`, `internal/config/kubernetes.go`): ``` runtime.kubernetes.session_store must be 'postgres': an issue pod's disk is deleted with the pod, so a session kept as a file on it ('pvc') dies with it and the next launch's resume fails on the missing file, retiring the claim after three failur`
  - theirs: insert before base L613 (start of hunk): «```yaml runtime: kubernetes: session_store: cloudsql-iam every pod signs in to Cloud SQL as itself; no URL is held anywhere session_database: the database each pod signs in to; all three members are required instance: example-project:us-central1:example-instance the connection name, project:region:instance name: legion_sessions the database in that instance user: legion-sessions@example-project.ia…»
  - theirs: base L613-615: «`postgres` keeps every pod agent's conversation, each role's and a daemon-launched controller's, in Oh My Pi's table.» -> «`postgres` and `cloudsql-iam` both keep every pod agent's conversation, each role's and a daemon-launched controller's, in Oh My Pi's table; what differs is how a pod reaches that database and what it must hold to do so (`Store` and `Databas…»; all changes: ``postgres` [-keeps-] {+and `cloudsql-iam` both keep+} every pod agent's conversation, … in Oh My Pi's [-table.-] {+table; what differs is how a pod reaches that database and what it must hold to do so (`Store` and `Database`, `internal/ompsessions/store.go`), which for the second of them is [What `cloudsql-iam` puts in a pod](#what-cloudsql-iam-puts-in-a-pod), below. Each kind names its own key and refuses the other's, an inert key being refused and never ignored: `runtime.kubernetes.session_dsn_secret is not used when runtime.kubernetes.session_store is cloudsql-iam; remove it`, and `runtime.`
- hunk 3 (base L636-658):
  - ours: base L636-639: «Every agent in the deployment can read that URL file, as every pod mounts it, and so can read and rewrite every conversation in the table, the controller's included, with the login's rights. That is the trade LEGION-654's spec acc…» -> «**One login, every conversation: what that costs.** Every role container mounts the providers Secret with the session database's URL among its keys, and the agent is told where that file is (`OMP_SESSION_SQL_DSN_FILE`, above), so …»; all changes: `{+**One login, every conversation: what that costs.**+} Every {+role container mounts the providers Secret with the session database's URL among its keys, and the agent is told where that file is (`OMP_SESSION_SQL_DSN_FILE`, above), so every+} agent in the deployment [-can read that URL file, as every pod mounts it,-] {+holds the login the whole table sits behind. Oh My Pi's two tables are keyed by the session's recorded path+} and {+carry no tenant, project or issue column,+} so [-can read and rewrite-] {+rights over the table are rights over+} every conversation in {+it:+} the [-table, the c`
  - ours: base L641-645: «A resume under `postgres` is held to the table, not the volume. A role's launcher looks the recorded session up in the table through the same URL file before it starts the child (`internal/launcher`, `resumable`), and refuses the …» -> «Under `pvc` a session was a file on a disk only its own issue's pod could mount, so no issue could reach another's at all, and this release widens that boundary rather than keeping it. That is the trade LEGION-654's spec accepts f…»; all changes: `{+Under `pvc` a session was a file on a disk only its own issue's pod could mount, so no issue could reach another's at all, and this release widens that boundary rather than keeping it. That is the trade LEGION-654's spec accepts for sessions that outlive their pods, and it is a trade to weigh before the rollout rather than after: a deployment that cannot accept one agent reaching another's transcript has no setting in this release that stops it. Where it is closed properly is `dispatch://INFRA-228` — a credential per issue, or a row-level policy the database itself enforces — and nothing in `
  - ours: base L647-655: «Neither `workspace-init` looks for session files: an issue pod is not told `LEGION_EXPECT_ISSUE_VOLUME` for a session, and the controller's pod is not told `LEGION_RESUME_SESSION_FILE`.» -> «Neither `workspace-init` looks for a session file, and neither is told to expect one: there is no disk on which a session could be waiting.»; all changes: `Neither `workspace-init` looks for {+a+} session [-files: an issue pod-] {+file, and neither+} is [-not-] told [-`LEGION_EXPECT_ISSUE_VOLUME` for-] {+to expect one: there is no disk on which+} a [-session, and the controller's pod is not told `LEGION_RESUME_SESSION_FILE`.-] {+session could be waiting.+}`
  - ours: base L647-655: «So a claim resumed on a new volume (an issue closed as `done` or its tree drained, its volume deleted, then re-admitted) provisions its workspace from the issue's pushed branch (`legion/<issue>`, or the repository's default branch wh…» -> «So every launch of a claim — a resume in a pod of its own making, and a resume in a pod created after the last one went — provisions its workspace from the issue's pushed branch (`legion/<issue>`, or the repository's default branch w…»; all changes: `So {+every launch of+} a claim [-resumed on-] {+—+} a [-new volume (an issue closed as `done` or-] {+resume in a pod of+} its [-tree drained, its volume deleted, then re-admitted)-] {+own making, and a resume in a pod created after the last one went —+} provisions its workspace from … and continues its own [-session;-] {+session from the table;+} anything it had not pushed [-is gone-] {+went+} with the [-old volume.-] {+pod before it.+}`
  - ours: base L647-655: «A child issue re-admitted as a root of its own keeps its roles' sessions under either store (`Machine.Retree`): under `postgres` they are in the table, and under `pvc` on the issue's own volume, which its Sandbox keeps across the move while the old tree lingers ([Volume retention](#volume-retention)).» -> «A child issue re-admitted as a root of its own keeps its roles' sessions the same way (`Machine.Retree`): they are rows, and no disk is carried into the new tree ([The disk dies with its pod](#the-disk-dies-with-its-pod)).»; all changes: ` … keeps its roles' sessions [-under either store-] {+the same way+} (`Machine.Retree`): [-under `postgres`-] they are [-in-] {+rows, and no disk is carried into+} the [-table, and under `pvc` on the issue's own volume, which-] {+new tree ([The disk dies with+} its [-Sandbox keeps across the move while the old tree lingers ([Volume retention](#volume-retention)).-] {+pod](#the-disk-dies-with-its-pod)).+}`
  - ours: base L657: «Such an agent is told so before its next turn.» -> «An agent whose workspace is newer than its session is told so before its next turn.»; all changes: `[-Such an-] {+An+} agent {+whose workspace is newer than its session+} is told so before its next turn.`
  - theirs: base L641-646: «A resume under `postgres` is held to the table, not the volume. A role's launcher looks the recorded session up in the table through the same URL file before it starts the child (`internal/launcher`, `resumable`), and refuses the …» -> «Under `cloudsql-iam` there is no such credential to hold: what the agent reads carries a login and no password at all (`Endpoint.AgentURL`, `internal/ompsessions/store.go`), and what it names is a unix socket in the generation's o…»; all changes: `{+Under `cloudsql-iam` there is no such credential to hold: what the agent reads carries a login and no password at all (`Endpoint.AgentURL`, `internal/ompsessions/store.go`), and what it names is a unix socket in the generation's own directory on the role container's own memory-backed private volume, which no other container of the pod mounts. Any process in the role's container can still dial that socket, as any process in a pod can read the URL file under `postgres`: this store removes the password, not that trade, and the rights the login has in the database are still every conversation's.`
  - theirs: base L652-655: «A child issue re-admitted as a root of its own keeps its roles' sessions under either store (`Machine.Retree`): under `postgres` they are in the table, and under `pvc` on the issue's own volume, which its Sandbox keeps across the move while the old tree lingers ([Volume retention](#volume-retention)).» -> «A child issue re-admitted as a root of its own keeps its roles' sessions whatever the store (`Machine.Retree`): under `postgres` and `cloudsql-iam` they are in the table, and under `pvc` on the issue's own volume, which its Sandbox keeps across the move while the old tree lingers ([Volume retention](#v…»; all changes: ` … keeps its roles' sessions [-under either-] {+whatever the+} store (`Machine.Retree`): under `postgres` {+and `cloudsql-iam`+} they are in the … `
- hunk 4 (base L660-661):
  - ours: base L660-661: «row's `mtime_ms` under `postgres`, which Oh My Pi moves on every write and `legion sessions import` sets to the copied file's, and the file's own under `pvc`.» -> «row's `mtime_ms`, which Oh My Pi moves on every write and `legion sessions import` sets to the copied file's.»; all changes: `row's [-`mtime_ms` under `postgres`,-] {+`mtime_ms`,+} which Oh My Pi … sets to the copied [-file's, and the file's own under `pvc`.-] {+file's.+}`
  - theirs: base L660-661: «row's `mtime_ms` under `postgres`, which Oh My Pi moves on every write and `legion sessions import` sets to the copied file's, and the file's own under `pvc`.» -> «row's `mtime_ms` under either database store, which Oh My Pi moves on every write and `legion sessions import` sets to the copied file's, and the file's own under `pvc`.»; all changes: `row's `mtime_ms` under [-`postgres`,-] {+either database store,+} which Oh My Pi … `
- hunk 5 (base L690-698):
  - ours: base L690-694: «…s volume projects another session store than a pod created now would — one created before `postgres` was turned on, or after it was turned off — holds a move (`movedSessionStore`, `internal/runtime/sandbox/addresses.go`): the next relaunch of any of its roles replaces the pod, as it does a pod that dials a moved worker…» -> «…s volume projects another session store than a pod created now would — one created before this release, under a deployment whose sessions were still files — holds a move (`movedSessionStore`, `internal/runtime/sandbox/addresses.go`): the next relaunch of any of its roles replaces the pod, as it does a pod that dials a …»; all changes: ` … — one created before [-`postgres` was turned on, or after it was turned off-] {+this release, under a deployment whose sessions were still files+} — holds a move … `
  - ours: base L698: «A resumed session also restores the model it last used, and under either store the Oh My Pi» -> «A resumed session also restores the model it last used, and the Oh My Pi»
  - theirs: base L690-696: «The variables are a generation's, but the URL file's mount is the pod's.» -> «The variables are a generation's, but the store a pod was created under is the pod's: the providers volume's session item under `postgres`, and the three session-proxy flags each role container's `legion launcher` carries under `cloudsql-iam` (`sessionStoreInPod`).»; all changes: `The variables are a generation's, but the [-URL file's mount-] {+store a pod was created under+} is the [-pod's.-] {+pod's: the providers volume's session item under `postgres`, and the three session-proxy flags each role container's `legion launcher` carries under `cloudsql-iam` (`sessionStoreInPod`).+}`
  - theirs: base L690-696: «A pod whose providers volume projects another session store than a pod created now would — one created before `postgres` was turned on, or after it was turned off — holds a move (`movedSessionStore`, `internal/runtime/sandbox/addresses.…» -> «A pod that holds another session store than a pod created now would holds a move (`movedSessionStore`, `internal/runtime/sandbox/addresses.go`) — a store turned on, turned off, changed from one kind to the other, or, under `cloudsql-iam…»; all changes: `A pod [-whose providers volume projects-] {+that holds+} another session store than … pod created now would [-— one created before `postgres` was turned on, or after it was turned off —-] holds a move (`movedSessionStore`, [-`internal/runtime/sandbox/addresses.go`):-] {+`internal/runtime/sandbox/addresses.go`) — a store turned on, turned off, changed from one kind to the other, or, under `cloudsql-iam`, a changed instance, database or login — and+} the next relaunch of … `
  - theirs: base L690-696: «The image probe's container is told `OMP_SESSION_SQL_DSN_FILE` too, so `legion probe-image`, which reads the providers directory as the shim does, never exports the URL into Oh My Pi's environment.» -> «Under `postgres` the image probe's container is told `OMP_SESSION_SQL_DSN_FILE` too, so `legion probe-image`, which reads the providers directory as the shim does, never exports the URL into Oh My Pi's environment; under `cloudsql…»; all changes: `[-The-] {+Under `postgres` the+} image probe's container is … into Oh My Pi's [-environment.-] {+environment; under `cloudsql-iam` the probe's pod mounts no session key and is told no pointer at all (`sessionDSNPointer`, `manifest.go`).+}`
  - theirs: base L698: «A resumed session also restores the model it last used, and under either store the Oh My Pi» -> «A resumed session also restores the model it last used, and under any store the Oh My Pi»; all changes: ` … last used, and under [-either-] {+any+} store the Oh My Pi`
- hunk 6 (base L702-703):
  - ours: base L702-703: «resuming a session on it, `pvc` or `postgres`, until the route offers that model again.» -> «resuming a session on it until the route offers that model again.»; all changes: `resuming a session on [-it, `pvc` or `postgres`,-] {+it+} until the route offers that model again.`
  - ours: base L702-703: «Keep the route unchanged across the switch below in particular: there every session outlives its volume.» -> «Keep the route unchanged across the copy below in particular: there every session outlives the volume it was written on.»; all changes: `Keep the route unchanged across the [-switch-] {+copy+} below in particular: there every session outlives [-its volume.-] {+the volume it was written on.+}`
  - theirs: base L702: «resuming a session on it, `pvc` or `postgres`, until the route offers that model again.» -> «resuming a session on it, whatever the store, until the route offers that model again.»; all changes: `resuming a session on it, [-`pvc` or `postgres`,-] {+whatever the store,+} until the route offers that model again.`
- hunk 7 (base L1033-1034):
  - ours: base L1033-1034: «**A rollout that also turns on `session_store: postgres` does not drain.** It replaces steps 1 to 3 below with steps 1 to 5 of [Copying file sessions before turning it on](#copying-file-sessions-before-turning-it-on),» -> «**Every rollout of this release turns `session_store: postgres` on**, since it is the only store the loader accepts ([Selecting the store](#selecting-the-store)), so the copy is part of this upgrade rather than an option beside it: …»; all changes: `[-**A-] {+**Every+} rollout [-that also-] {+of this release+} turns [-on-] `session_store: postgres` [-does not drain.** It replaces-] {+on**, since it is the only store the loader accepts ([Selecting the store](#selecting-the-store)), so the copy is part of this upgrade rather than an option beside it:+} steps 1 to 3 below [-with-] {+are replaced by+} steps 1 to 5 … `
  - theirs: base L1033-1034: «**A rollout that also turns on `session_store: postgres` does not drain.** It replaces steps 1 to 3 below with steps 1 to 5 of [Copying file sessions before turning it on](#copying-file-sessions-before-turning-it-on),» -> «**A rollout that also turns on a database store — `session_store: postgres` or `cloudsql-iam` — does not drain.** It replaces steps 1 to 3 below with steps 1 to 5 of [Copying file sessions before turning it on](#copying-file-sessions-before-turning-it-on),»; all changes: `**A rollout that also turns on {+a database store —+} `session_store: postgres` {+or `cloudsql-iam` —+} does not drain.** It … `
- hunk 8 (base L1039-1039):
  - ours: base L1039: «and the rollout, with `session_store: postgres` set.» -> «and the rollout, whose `legion.yaml` names the database and its secret.»; all changes: `and the rollout, [-with `session_store: postgres` set.-] {+whose `legion.yaml` names the database and its secret.+}`
  - theirs: base L1039: «and the rollout, with `session_store: postgres` set.» -> «and the rollout, with the store's own keys set: `session_dsn_secret` under `postgres`, and `session_database` under `cloudsql-iam`. The copy itself runs the same way under either, since `legion sessions import` takes the database as a `--dsn-file` URL …»; all changes: `and the rollout, with [-`session_store: postgres` set.-] {+the store's own keys set: `session_dsn_secret` under `postgres`, and `session_database` under `cloudsql-iam`. The copy itself runs the same way under either, since `legion sessions import` takes the database as a `--dsn-file` URL the machine running it can reach, which under `cloudsql-iam` is the operator's to supply: no pod holds one.+}`
- hunk 9 (base L1494-1498):
  - ours: base L1494-1496: «- `session_store` other than `pvc` or `postgres`; `postgres` without `session_dsn_secret`, or with one that is empty or not a Secret data key (`[-._a-zA-Z0-9]+`); and a `session_dsn_secret` under `pvc` (an inert key is refused, neve…» -> «- a `session_store` that is not `postgres`, and an omitted `session_store`, which is the same refusal rather than a default; and `postgres` without `session_dsn_secret`, or with one that is empty or not a Secret data key (`[-._a-zA-…»; all changes: `- {+a+} `session_store` [-other-] {+that is not `postgres`, and an omitted `session_store`, which is the same refusal rather+} than [-`pvc` or `postgres`;-] {+a default; and+} `postgres` without `session_dsn_secret`, or … a Secret data key [-(`[-._a-zA-Z0-9]+`); and a `session_dsn_secret` under `pvc` (an inert key is refused, never ignored).-] {+(`[-._a-zA-Z0-9]+`).+}`
  - theirs: base L1494-1498: «- `session_store` other than `pvc` or `postgres`; `postgres` without `session_dsn_secret`, or with one that is empty or not a Secret data key (`[-._a-zA-Z0-9]+`); and a `session_dsn_secret` under `pvc` (an inert key is refused, never ignored). At boot the daemon a…» -> «- `session_store` other than `pvc`, `postgres` or `cloudsql-iam`; `postgres` without `session_dsn_secret`, or with one that is empty or not a Secret data key (`[-._a-zA-Z0-9]+`); `cloudsql-iam` without `session_database`, or with one that is not a mapping, names a…»; all changes: `- `session_store` other than [-`pvc`-] {+`pvc`, `postgres`+} or [-`postgres`;-] {+`cloudsql-iam`;+} `postgres` without `session_dsn_secret`, or … Secret data key (`[-._a-zA-Z0-9]+`); {+`cloudsql-iam` without `session_database`, or with one that is not a mapping, names a member but `instance`, `name`+} and {+`user`, leaves one of the three empty, or whose `instance` is not a connection name the Cloud SQL connector uses whole (`project:region:instance`, or `domain:project:region:instance` for a domain-scoped project, with no whitespace);+} a `session_dsn_secret` under `pvc` {+or `cloudsql-iam`; `
- hunk 10 (base L3669-3670):
  - ours: base L3669: «default; postgres stores each agent's conversation in Postgres (see Session store)» -> «never read by this daemon, and refused by the Go daemon (see Session store)»; all changes: `[-default; postgres stores each agent's conversation in Postgres-] {+never read by this daemon, and refused by the Go daemon+} (see Session store)`
  - ours: base L3670: «required with postgres: the key of legion-<project>-providers holding one postgres:// URL» -> «likewise: the Go daemon requires this key, beside session_store: postgres»; all changes: `[-required with postgres:-] {+likewise:+} the [-key of legion-<project>-providers holding one postgres:// URL-] {+Go daemon requires this key, beside session_store: postgres+}`
  - theirs: base L3669: «default; postgres stores each agent's conversation in Postgres (see Session store)» -> «default; postgres or cloudsql-iam keeps each agent's conversation in a database (see Session store)»; all changes: `default; postgres [-stores-] {+or cloudsql-iam keeps+} each agent's conversation in [-Postgres-] {+a database+} (see Session store)`
  - theirs: base L3670: «required with postgres: the key of legion-<project>-providers holding one postgres:// URL» -> «with postgres alone: the key of legion-<project>-providers holding one postgres:// URL     session_database: with cloudsql-iam alone: no URL anywhere, each pod signs in to the instance as itself       instance: example-project:us-…»; all changes: `[-required-] with [-postgres:-] {+postgres alone:+} the key of legion-<project>-providers holding one postgres:// URL {+session_database: with cloudsql-iam alone: no URL anywhere, each pod signs in to the instance as itself instance: example-project:us-central1:example-instance the Cloud SQL connection name, project:region:instance name: legion_sessions the database in that instance user: legion-sessions@example-project.iam the IAM login every pod signs in as+}`


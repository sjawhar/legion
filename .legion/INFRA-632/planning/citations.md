# Dependencies on root `AGENTS.md`, `scripts/e2e/README.md` and `docs/kubernetes.md`

**Summary**

- **Only one check reads the layout of any of the three files:** `packages/daemon/internal/runtime/sandbox/reclaim_doc_test.go` parses the reclaim paragraph and its two bullet lists in `docs/kubernetes.md` (lines 2226–2272 at this head). Its assumptions are specific; see 1a.
- **Two other checks read the files' text.**
  - `packages/pi-legion/src/skills-guard.test.ts` scans root `AGENTS.md` and `docs/kubernetes.md` one line at a time for banned phrases. It cannot fail because of a re-layout, but it can stop catching a phrase.
  - `packages/envoy/cmd/dispatch/memory_profile_test.go` reads `scripts/e2e/README.md`. It runs only when an environment variable is set, and it asserts nothing about the text.
- **Nothing checks `scripts/e2e/README.md` per script.** No test or check asserts a section or index row for each script, and no check parses its checkpoint tables.
- **Nothing checks the content of root `AGENTS.md`.** `roles_test.go` does not read it. The claude-envoy build does not copy it. The private-names and plugin-names checks have no exemption for any of the three files.
- **Two Go tests pin heading names in strings:** `config/kubernetes_test.go:484` ("Operator configuration") and `sandbox/layout_test.go:138` ("Upgrading a deployment with running trees").
- **Every in-file anchor resolves.** None is broken in any of the three files (section 3).
- **The docs site does not render any of the three files** (last section).

## 1. Code and tests that read the files' content

**a. `packages/daemon/internal/runtime/sandbox/reclaim_doc_test.go:44-260`.** The tests `TestTheReclaimParagraphNamesEveryDirectoryTheRendererPutsOnTheIssueVolume` and `TestTheReclaimParagraphNeverDeletesWhatItExempts` call `reclaimParagraphLists`, which reads `docs/kubernetes.md` (`:144`).

The markers (`:44-48`):
- `reclaimParagraphStart = "The remedy for a volume near full is manual"`
- `reclaimParagraphEnd = "As for the rest:"`
- `reclaimDeleteHeading = "Delete list"`
- `reclaimExemptHeading = "Exempt, each for a reason"`

The marker text sits at `docs/kubernetes.md:2226-2227`, `:2232`, `:2241` and `:2272`.

What it assumes about the layout:
- **The start and end markers may wrap anywhere.** They are found in text with all whitespace collapsed (`flattenKeepingLines`, `:152-153`). The end marker must follow the start marker. The region runs from the line the start marker begins on to the line the end marker begins on (`:164`). A reworded marker is a hard fatal (`:155`, `:160`).
- **The two list headings must start at column 0.** The check is `strings.HasPrefix(strings.TrimRight(line," "), "Delete list" | "Exempt, each for a reason")`, with no left trim (`:172-178`).
- **Each bullet must start at column 0 with `- `** (`:180`). A bullet's continuation lines are joined only while the next line starts with a space and is not blank (`:186-189`). A continuation line at column 0 is dropped; so is one after a blank line. Blank lines between items are fine, because the current list stays selected until the next heading.
- **A delete-list bullet must be exactly one backticked absolute path with nothing after it** (`bulletPath`, `whole=true`, `:221-234`). Any prose, even on a continuation line, makes the bullet count as no path.
- **An exempt bullet must open with a backticked path under `/legion`.** Only that first code span counts.
- **Any other `- ` bullet inside the region joins the most recent list.** A bullet before `As for the rest:` whose first code span is a `/legion/...` path is added to the exempt list, even if it comes from a later paragraph turned into a list (for example the `/legion/scratch/tmp` paragraph at `:2253`).
- **Both lists must yield at least one path**, or the test fails (`:199-210`).
- **The placeholders must stay literally `/legion/scratch/state/<role>` and `/legion/uv/python/<issue>`** (`:66-71`).

**b. `packages/pi-legion/src/skills-guard.test.ts:122-163`.** Its roots include `docs/kubernetes.md` (`:143`) and root `AGENTS.md` (`:147`). It runs `forbidden = /no GitHub credential|never reads GitHub|has no App|cannot read GitHub|holds no repository credential/` over the whole file, then over each line, and reports `path:line` (`:154-161`). Only per-line hits fail the test.
- **What a re-layout changes:** a phrase split across a line break matches the whole file but no line, so it slips through silently. A re-layout cannot make this test fail.
- **CI trigger:** this test is why `AGENTS.md`, `README.md` and `docs/kubernetes.md` are in `.github/workflows/envoy-and-contracts.yaml`'s path filters at `:29-31`, `:87-89` and `:597-599`. The comment at `:574-575` says so. A pull request that edits only these files therefore runs the `pi_legion` job.

**c. `packages/envoy/cmd/dispatch/memory_profile_test.go:125-153`** (`TestProfileTheColdEditOfTheHeaviestDocuments`).
- It is skipped unless `DISPATCH_EDIT_PROFILE_OUT` is set (`:23`, `:127-129`).
- It reads `scripts/e2e/README.md` (`:147-148`), appends a sentinel sentence, uploads the file to Dispatch, and edits the sentinel word "wurd"/"wird" (`edit_memory_test.go:49`).
- It makes no assertion about layout. An upload that is refused is only logged ("not profiled", `:162-164`).
- Its memory profile would change if the README's tables became lists.

**d. Nothing else.** I found no other reader of the three files:
- **`.github/scripts/*`:** no script reads any of them. `check-private-names.sh` and `check-plugin-names.sh` scan every tracked file line by line, and neither has an entry for these three files.
- **`scripts/e2e/lib/*.test.ts`:** `script-functions.test.ts:102-105` reads `*.sh` and `lib/*.{sh,ts,py,jq}`, not the README.
- **`docs/site/generators/*`:** they read `skills/*/SKILL.md`, `deploy/kubernetes/*` and contract sources.
- **The claude-envoy build:** `packages/claude-envoy/scripts/build.ts:97-126` stages skills and agents only. The `"AGENTS.md"` in `packages/claude-envoy/package.json:18` is the package's own guide.
- **`packages/daemon/internal/prompts/`:** the prompts only say "Read the repository's `AGENTS.md`" (`roles/architect.md:7`, `roles/architect-root.md:7`, `mechanics/headless.md:3`), meaning whatever repository they work in.
- **`roles_test.go`:** it reads only the embedded `roles/*` files (`:49`).
- **Root `CLAUDE.md:1`** is `See @AGENTS.md`, which includes the whole file. That has no layout constraint.

## 2. Citations of a section by heading or anchor

### `docs/kubernetes.md`

**Tests or runtime strings that pin a heading:**
- "Operator configuration":
  - `packages/daemon/internal/config/kubernetes.go:458` (error string)
  - `config/kubernetes_test.go:484` (asserts that exact string)
- "Upgrading a deployment with running trees":
  - `runtime/sandbox/install.go:178` (refusal string)
  - `sandbox/layout_test.go:138` (asserted with `strings.Contains`), and the comment at `:127`
  - Quoted back inside `docs/kubernetes.md:1343-1345`, in a code span that wraps across lines.
- "Copying file sessions before turning it on": `scripts/sessions-import-pods.sh:48` (stderr message; `sessions-import-pods.test.sh` does not assert it) and `:4`.

**All other citations, grouped by heading:**

| Heading or label cited | Where it is cited |
|---|---|
| "Operator configuration" | `AGENTS.md:67`<br>`skills/legion-worker/SKILL.md:60` and its generated copy `packages/claude-envoy/legion/skills/legion-worker/SKILL.md:60`<br>`packages/pi-legion/agents/deep-worker.md:8`, `oracle.md:7`, `plan-gap-analyst.md:9`, `plan-reviewer.md:10`, `thermonuclear-code-quality.md:5`, `thermonuclear-deep-review.md:5`<br>`deploy/kubernetes/daemon/legion.yaml.example:153-154`<br>`deploy/kubernetes/operator-route/README.md:5`, `pod.yml:3` |
| "Model roles" (bold label at `:5570`) | `packages/pi-legion/README.md:48` |
| "Pin by digest, never by tag" | `AGENTS.md:147`<br>`.github/workflows/release.yaml:448`, `release-envoy-listener.yaml:685-686`, `worker-image.yaml:120`, `:252-253`<br>`packages/envoy/AGENTS.md:4179-4180`<br>`packages/envoy/infra/README.md:95`<br>`docs/site/src/content/docs/legion/running-legion.md:221` |
| "Contract discipline" (the first one, `:2016`; a second, the TypeScript daemon's, is at `:5854`) | `AGENTS.md:59`, `:75`<br>`packages/pi-legion/AGENTS.md:175`, `:236`, `:299` |
| "The repository cache" | `AGENTS.md:59`, `:75`<br>`packages/daemon/internal/workspace/fetch.go:691` |
| "What `cloudsql-iam` puts in a pod" | `AGENTS.md:69` |
| "Copying file sessions before turning it on" | `AGENTS.md:73` |
| "Probing a configuration before the daemon boots on it" | `AGENTS.md:74` |
| "Anatomy of a Sandbox pod" | `AGENTS.md:75`<br>`skills/legion-worker/references/tools.md:158` and its claude-envoy copy |
| "The disk dies with its pod" | `AGENTS.md:75`<br>`runtime/sandbox/podkind.go:105-106` |
| "Selecting the store" | `AGENTS.md:75`<br>`deploy/kubernetes/daemon/legion.yaml.example:95`<br>`running-legion.md:300` |
| "The controller" | `AGENTS.md:34` |
| "Operator-launched controller" | `AGENTS.md:127` |
| "Running a workload in the cluster from a role" | `AGENTS.md:129`<br>`packages/daemon/docker/worker.Dockerfile:372` |
| "Per-deployment toolchains layer on top" | `worker.Dockerfile:90-91` |
| "Issue sizing: one reservation per pod" | `legion.yaml.example:107` ("Issue sizing")<br>`scripts/e2e/README.md:1379`<br>`running-legion.md:152`, a URL anchor `#issue-sizing-one-reservation-per-pod` |
| "Configuration" (two headings share the name: `:2104` and `:5199`) | `legion.yaml.example:138` |
| "Daemon-launched controller" | `running-legion.md:492` |
| "Cutting over to the pod-owned disk" | `running-legion.md:557` |
| "A shell on an issue's disk" | `docs/site/src/content/docs/legion/troubleshooting.md:504-505` |
| "RBAC the Go daemon needs" | `runtime/sandbox/live_install_test.go:201` |
| "The daemon's Kubernetes request rate" | `runtime/sandbox/clients.go:15` |
| "A pod whose address moved" | `runtime/sandbox/addresses_test.go:170`<br>`scripts/e2e/stage4b-sandbox-tree.sh:5251`, `:5308` |
| "Controller liveness" | `stage4b-sandbox-tree.sh:5824` |
| "The tools a role's own work needs" | `skills/legion-worker/references/tools.md:186-187`, `:415`, and their copies |
| The paragraph that opens "Sharing `BUN_INSTALL_CACHE_DIR`" (`:3212`) | `runtime/sandbox/plugins.go:175` |
| The sentence "No container is told …" (`:3142`, mid-line) | `internal/shim/shim_test.go:1035` |

The bold paragraph labels "Guaranteed or Burstable" (`:4497`), "The bound" (`:4584`), "The node-disk bound" (`:4623`), "The claim" (`:5935`) and "Switching back" (`:6064`) are cited only inside `docs/kubernetes.md`, at `:2157`, `:2179`, `:2674`, `:3334`, `:6011`, `:6035` and `:6130`.

**Citations of the whole file, with no section:**
- `AGENTS.md:14`, `:125`, `:126`
- `README.md:129`
- `docs/deployment.md:7`
- `worker.Dockerfile:46`
- `sandbox/manifest.go:990`, `install.go:168`
- `api/version.go:363` (the rollback SQL `delete from schema_version where version = 54`)
- `packages/pi-legion/README.md:33`
- `deploy/kubernetes/operator-route/README.md:72`
- `docs/site/src/content/docs/dispatch/envoy.md:176`

**Dated records:** 43 files under `docs/solutions` and 1 under `docs/plans` cite `docs/kubernetes.md`. Some cite it by line number (`:78-81`, `:157`, `:916`, `:917`, `:1188-1192`), so those references go stale.

### `scripts/e2e/README.md`

| Section cited | Where it is cited |
|---|---|
| `## stage4b-sandbox-tree.sh`, as `#stage4b-sandbox-treesh` | `docs/kubernetes.md:3621`, `:5116`, `:5925` |
| `## stage4a-sandbox-runtime.sh` | `scripts/e2e/stage4a-sandbox-runtime.sh:107` ("README.md's Stage 4a section")<br>`docs/kubernetes.md:4576` (link with no anchor)<br>`docs/kubernetes.md:283` (the `image-probe` and `root-ready` check rows) |
| `## Port admission` ("README's port-admission note") | `stage4a-sandbox-runtime.sh:116`<br>`stage4b-sandbox-tree.sh:94`, `:197` |
| `## lib/github-apps.sh` | `AGENTS.md:78` (`(scripts/e2e/README.md, "lib/github-apps.sh")`) |
| `## lib/operator-dispatch.sh`, as `#liboperator-dispatchsh` | `packages/pi-envoy/scripts/README.md:71` |
| `## lib/rig.sh` | `packages/pi-envoy/scripts/README.md:142` (`#librigsh`)<br>`docs/deployment.md:210-211` (`real_bin_path`) |
| The intro on kind's node containers (`:22-26`) and the containers and limits | `docs/testing.md:31`, `:43` |

**Citations of the whole file:**
- `AGENTS.md:76`, `:77`, `:79`, `:80`, `:81`, `:128`
- `packages/daemon/internal/runtime/sandbox/live_test.go:7`
- `scripts/e2e/stage3-4b13b-acceptance.sh:21`
- One captured fixture line, `scripts/e2e/lib/testdata/stage4b-tools-session.jsonl:15`

**Dated records:** 3 files under `docs/solutions`, 0 under `docs/plans`. One cites `README.md:1031`.

### Root `AGENTS.md`

| Section cited | Where it is cited |
|---|---|
| "Deploys come only from main" (bullet at `:147`) | `.github/workflows/release.yaml:547-548` |
| "Handover" (`:177`) | `packages/daemon/cmd/legion/controller.go:380` |
| "This repository is public" (`:149`) | `packages/dispatch/e2e/delivery-seed.ts:5`<br>`packages/daemon/internal/capabilities/capabilities_test.go:84` |
| Tech Stack "Container images" bullet (`:47`) | `packages/pi-legion/scripts/skill-scenarios/rig.sh:203`<br>`scripts/e2e/lib/bridge-tunnel.sh:7` |
| Commands, the `bunx biome check` line (`:53`) | `.github/workflows/pr-and-main.yaml:128` |
| "name no person" rule (`:150`) | `packages/pi-legion/CHANGELOG.md:386-387` |

**Dated records:** 2 files under `docs/solutions` cite "Deploys come only from main", and 1 plan (`2026-10-14-envoy-infra-plan.md:613`) cites "WHERE TO LOOK". In all, 63 files under `docs/solutions` and `docs/plans` mention a bare `AGENTS.md`; some of those mean a package guide or a generic one.

## 3. In-file anchors

**`docs/kubernetes.md`:** about 120 `](#…)` links using 38 distinct slugs. All resolve. The slugs follow GitHub's rules: apostrophes, colons and backticks are dropped (`#the-deployments-capability-report`, `#issue-sizing-one-reservation-per-pod`, `#what-cloudsql-iam-puts-in-a-pod`, `#trust-model-the-provisioning-token`).
- **Duplicate headings:** `### Configuration` (`:2104`, `:5199`) and `### Contract discipline` (`:2016`, `:5854`). Every link uses the plain slug, which goes to the first, Go-daemon heading. That is intended (`:5204`, `:5858` say "the Go daemon's"). Reordering these sections would retarget the links.
- **Link text that differs from the target heading:**
  - `:1769` "Moving from one container per role…" goes to `#pinning-a-release-that-drops-what-a-running-pod-still-uses`.
  - `:5969` "Settings order" goes to `#operator-configuration`.
  - `:6130` "Switching back" goes to `#daemon-launched-controller`.
- **Links into other files:**
  - `../scripts/e2e/README.md#stage4b-sandbox-treesh` (`:3621`, `:5116`, `:5925`) resolves to heading `:1273`.
  - `site/src/content/docs/legion/concepts.md#supervision` (`:5081`) resolves to `concepts.md:401`.

**`scripts/e2e/README.md`:** 32 distinct slugs, all resolve:
- The `#lib…sh`/`#libheld-portpy` slugs go to the `## lib/<file>` headings at `:2433` to `:3517`.
- Others: `#stage3-devbox-workflowsh` (`:345`), `#claude-backend-tmuxsh` (`:736`), `#dispatch-user-turnssh` (`:1741`), `#merged-root-waitssh` (`:2244`), `#port-admission` (`:851`), `#the-natives-download` (`:2663`), `#the-bridge-legion_e2e_pod_reachabilitybridge` (`:1001`).
- Link text that differs from the target: `require_proof_human`, `read_app_logins`, `close_smoke_cleanup` and `` `lib/app-logins.ts` `` go to `#libworkflowsh`; `real_bin_path` goes to `#librigsh`; `make_omp_home` goes to `#libomp-homesh`; "notes" goes to `#libmodel-gateway-unservedsh`.
- `### How it fails` appears twice (`:156`, `:1987`); neither is linked.
- Lines starting with `#` inside fenced blocks (`:895`, `:2443`, …) are not headings.

**Root `AGENTS.md`:** has no `](#…)` links. Its headings are `:5`, `:39`, `:49`, `:84`, `:98`, `:107`, `:137`, `:152` and `:509`.

## 4. Tests or checks that assert root `AGENTS.md` content

The skills-guard negative per-line scan (1b) is the only one. I found none of the following:
- a test asserting that `AGENTS.md` mentions something
- a generated copy of root `AGENTS.md` in claude-envoy
- a private-names or plugin-names exemption for it (`check-plugin-names.sh:33-81` lists only package guides)
- a check that reads its tables

One consideration that is not a check: a re-layout could push `AGENTS.md` from 514 lines to over 1000. `skills/thermonuclear-code-quality/SKILL.md:38` has reviewers block "a file from under 1000 lines to over 1000" unless the pull request names the constraint that keeps the text in that file.

## Does `docs/site` render these files?

No.
- **Content source:** the site is Astro with Starlight (`docs/site/astro.config.mjs`; Markdown through Astro's remark pipeline, with astro-mermaid, Expressive Code and `starlight-links-validator`). Its only content collection is `docsLoader()` over `docs/site/src/content/docs` (`src/content.config.ts`).
- **Nothing pulls them in:** no generator reads the three files, and there is no symlink or copy of them under the content directory.
- **CI does not trigger:** `.github/workflows/docs.yaml:16-33` does not trigger on them.
- **The only site link into them** is the external URL `https://github.com/sjawhar/legion/blob/main/docs/kubernetes.md#issue-sizing-one-reservation-per-pod` (`running-legion.md:152`). The links validator does not check external URLs, so that heading's slug has to stay as it is by hand.
- **Where they render:** only on GitHub (GFM). A re-layout with semantic line breaks renders the same there; turning a table into a list is the only visible change.

## Files

- `packages/daemon/internal/runtime/sandbox/reclaim_doc_test.go:14-260` — the reclaim parser
- `docs/kubernetes.md:2226-2272` — the reclaim paragraph
- `packages/pi-legion/src/skills-guard.test.ts:122-163` — per-line phrase scan
- `.github/workflows/envoy-and-contracts.yaml:29-31,87-89,574-599` — path filters
- `packages/envoy/cmd/dispatch/memory_profile_test.go:23,125-153` — opt-in README profile
- `packages/daemon/internal/config/kubernetes.go:458` with `kubernetes_test.go:484` — pinned heading name
- `packages/daemon/internal/runtime/sandbox/install.go:178` with `layout_test.go:126-160` — pinned heading name
- `scripts/sessions-import-pods.sh:4,48` — heading name in a refusal message
- `docs/site/astro.config.mjs`, `docs/site/src/content.config.ts` — site setup
- `.github/scripts/check-private-names.sh`, `check-plugin-names.sh:33-81` — line-based scans, no exemption for these files
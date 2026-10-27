# INFRA-631 planning research (planner, 2026-10-27, main 4e350fdf)

Spec: $TMPDIR/p631/spec631.md. Root spec: $TMPDIR/p631/spec588.md. Baseline: $TMPDIR/p631/baseline.md.
Citation inventory (scout): agent output summarized below.

## Files today
- packages/envoy/AGENTS.md: 4218 lines, 547 KB. p50 line 95 chars (hard-wrapped at ~100), 56 lines >1000 chars (max 13,899). Headings: only H1/H2 (Overview 5, Where to look 21 [table 23-50 then ~2000 lines of wrapped prose with no sub-headings], Critical conventions 2061 [bullets, 13 over 3000 chars], Security 2119, Operational notes 2125, Listener API 2168 [table 2185-2197, rows up to 5,091], Targeted Dispatch messages 2235, Topic shapes 2391, Secrets broker 2558 [wrapped prose]). Tables: 23-50 Where to look (26 rows), 594-598 benchmark (numbers, rows <=155), 2185-2197 Listener API.
- packages/envoy/cmd/dispatch/README.md: 1498 lines, 162 KB, 19 lines >1000 chars. Tables: 18-28 Required configuration, 65-68, 1239-1253 access-log fields, 1295-1376 Routes (80 rows, up to 2,433), 1398-1402 edit ops, 1416-1427 + 1437-1457 element weights (short, mirror pmdoc/elements.go), 1460/1469/1486 error tables. `[Search](#search)` intra-file links at 586, 591, 1327, 1328.
- No HTML comments, no reference link definitions, no nested lists. Fences: AGENTS 2143-2149 (inside an Operational notes bullet, the durable recipe), 4182-4186; README 9 fences.

## What depends on the files
- .github/scripts/check-body-embed-limit.sh (CI lint job): needs ONE physical line of packages/envoy/AGENTS.md naming both DISPATCH_BODY_MAX and maxInputChars. AGENTS.md:2101 bullet itself says "keep this bullet one line however long it grows"; packages/contracts/AGENTS.md:34 calls it "the body-cap bullet".
- Headings cited by code/text: "Operational notes" (cmd/listener/durable.go:92 error text; infra/aws/rendering/metricfilters.go:120 alarm text; the durable recipe must stay under it), "Topic shapes" (settlement_replay_test.go:283), README "Database migrations" (migrations 0113/0115/0116/0117 sql comments, deploy/scripts/autodeploy.sh:209), "Pre-deploy census" (duty/exclusive.go:139), "Search" (scripts/corpus-copy.sh), plus many markdown guides citing README sections by name and AGENTS sections by name. Quoted phrases: AGENTS:1316-1320 pin rule sentence; AGENTS:88 "The cross-task rule is scoped"; AGENTS:1199 bold "Every pool's size is set in code."; row names "NATS credential", "Stream definition" cited by plans.
- No anchor links (`file#...`) from outside; 11 historical line-number citations in docs/plans and docs/solutions (dated records).
- search_text_test.go walks every .md under packages/envoy (content only).
- No markdown formatter/linter in repo (Biome has no markdown support). Root scripts tests run only when a CI job lists them (pr-and-main.yaml:255).
- A merge touching only these files starts release-envoy-listener.yaml (image build+attest; a `docs:` subject bumps no version), release.yaml worker_image, docs.yaml, envoy-and-contracts jobs.
- 41 open PRs touch one or both files (list: $TMPDIR/p631/openprs.txt).

## Measurements
1. git adjacency: two branches changing adjacent lines (2 and 3) conflict in `git merge-tree`; a change and an insertion separated by one unchanged line merge clean; two insertions at the same point always conflict.
2. Existing tools: markdownlint-cli2 0.24.0 + markdownlint-sentences-per-line 0.1.7 report 945 (AGENTS) / 281 (README) multi-sentence lines today. Its splitter (sentences-per-line) only starts a sentence at an uppercase A-Z letter: after prettier 3 + prettier-plugin-sentences-per-line 0.2.5 (proseWrap never) reflow, it reports 9 issues while 287 (AGENTS) / 230 (README incl. table rows) sentence boundaries remain, e.g. "... has a stable `blockId`. `pmdoc.Parse` mints ...". Neither tool checks blank lines between list items or list-item length; both skip tables. Prettier also rewrote `*x*` to `_x_` (5 places) and table delimiter rows; otherwise words unchanged.
3. Replay of 51 real conflicting merges on the two files (35 PRs, conflict events found by `git merge-tree` on each PR merge commit's parents; 2026-10-20..27 subset in $TMPDIR/p631/pairs.txt), applying a layout transform to base/ours/theirs and re-merging:
   - T0 identity: 51 conflicted events, 66 hunks.
   - T1 prettier+plugin sembr: 45 / 62.
   - T2 T1 + split sentences starting with code span/bold/digit: 43 / 59.
   - T3 T2 + blank line between every sentence: 40 / 57.
   - T4 T2 + clause breaks (; : —) on lines >160: 42 / 55.
   Residual T4 hunks by cause: 20 README "Database migrations" per-migration paragraphs appended at the same point; 16 table rows (Where to look, Listener API, README Routes); ~5 list items with no blank line between; 7 same sentence (incl. hand counts "the broker's 32 HTTP routes" vs 29, "Seven periodic maintenance loops"); 3 shared lines; a few same-point inserts.
4. After T2 reflow: 341 AGENTS lines still >400 chars (single sentences up to 3,340 chars, mostly enumerations); list items: AGENTS 13 items >3000 chars, largest 13,928 chars / 30 lines; README items small.
5. Migrations: 0129_document_edit_notices.up.sql has no README paragraph; its .up.sql header comment describes it. Migration files carry header comments already.

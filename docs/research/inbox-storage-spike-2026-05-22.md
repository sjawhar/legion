# Inbox Storage Spike — GitHub-as-backend (2026-05-22)

> Throwaway research. Companion to the inbox feature brainstorm in
> `docs/superpowers/specs/2026-05-22-inbox-design.md` (forthcoming).

## Goal

Disambiguate three candidate shapes for storing first-class inbox items in
GitHub today (with native Legion tracker as the eventual target):

- **Shape A**: marker comments only — every item / reply / sub-item / status
  change is a comment on the parent (work) issue, carrying HTML-comment
  metadata for kind / urgency / parent / status.
- **Shape B**: sub-issues per item — each item is a GitHub sub-issue of the
  parent. Replies are comments on the sub-issue. Status maps to native
  `state` + `state_reason`. Sub-items are sub-sub-issues.
- **Shape C**: hybrid — top-level items as sub-issues (native state +
  per-item URL + native notifications); sub-items as marker comments inside
  the sub-issue.

Decision criteria the user explicitly named (in priority order):

1. **Querying mechanics** — what does the dashboard have to call to read
   state? How does that scale across many issues?
2. **Parallel native-GitHub-UI interaction** — what does the client have to
   implement to coexist with humans posting via GitHub directly?
3. **Migration cleanliness** to the native Legion tracker once it lands.

UX / aesthetic concerns (comment-soup readability, etc.) were explicitly
deprioritized.

## Method

A shared fixture (`fixture.py`): 10 top-level items + 1 sub-item + 1
sub-sub-item = 12 items total, 27 replies, statuses 9/2/1
(open/resolved/cancelled). Includes one 20-reply thread to stress long
threads, and depth-3 nesting to stress GraphQL complexity.

For each shape:

- A poster posts the fixture into a fresh parent issue.
- A parser reads it back via the shape's natural API path.
- Round-trip parity is checked against fixture counts.
- Stats are collected (REST vs GraphQL call counts, wall time, payload size).
- A "concurrent native edit" is simulated by posting a plain comment to
  the parent issue without markers; the parser is re-run.

Spike artifacts at `/tmp/inbox-spike/`. Test issues #614, #615, #628 in
sjawhar/legion (closed after the spike — labeled `inbox-spike`).

## Headline numbers

| Metric | Shape A (markers) | Shape B (sub-issues) | Shape C (hybrid) |
|---|---|---|---|
| **Parse one issue's full inbox** | 1 REST, 0.4s, 71 KB | 1 GraphQL **+ N RESTs** (13 calls for 12 items), 4.9s, 47 KB | **1 GraphQL**, 0.6s, 8 KB |
| **Cross-issue / global query** (all open items across N parent issues) | N REST calls (linear in N) | 1 GraphQL (label-search) | 1 GraphQL (label-search) |
| **Post 12 items + 27 replies + 3 statuses** | 42 REST, 39s | 54 calls (12 create + 12 add-sub + 27 cmt + 3 close), 48s | 52 calls (10 create + 10 add-sub + 30 cmt + 2 close), 40s |
| **Round-trip parity** | ✓ exact | ✗ off-by-2 on status (see findings) | ✓ exact |
| **Issue-list pollution per work-issue with K inbox items** | 0 | K + nested | K (no nested) |
| **Concurrent plain comment on parent** | ignored by parser; parser stays correct | ignored (parser only reads sub-issues' comments) | ignored |

## Major emergent findings (not visible from designing on paper)

### 1. GraphQL query-complexity cap forces multi-pass reads for Shape B

A natural one-shot read for Shape B is:
```graphql
issue { subIssues(first: 50) { nodes {
  comments(first: 50) { nodes { ... } }
  subIssues(first: 50) { nodes {
    comments(first: 50) { ... }
    subIssues(first: 50) { ... }
  }}
}}}
```

This **fails** with:

> `By the time this query traverses to the comments connection, it is
> requesting up to 6,250,000 possible nodes which exceeds the maximum
> limit of 500,000.`

The node-cost is the **product** of `first` values along the path. At 3
levels of `subIssues(50)` × `comments(50)`, you hit 6.25M nodes (50⁵), well
over the 500K cap.

Workarounds, all bad:

- **Reduce widths** (e.g., `comments(first: 10)`): now you can't read long
  threads in one shot.
- **Drop the deepest comments level**: now sub-sub-item replies require a
  second pass.
- **Two-pass** (tree-only GraphQL + per-leaf REST): **what the spike
  used**. Costs 1 + N API calls instead of 1. For 12 items, that's 13
  calls vs Shape A's 1 call or Shape C's 1 call.

**Conclusion**: Shape B's "one fast tree query" appeal evaporates once you
need real comment depth. Reading is **dramatically cheaper** in Shape A
(comments are flat per issue) and Shape C (only 1 level of `subIssues` +
their comments — fits under the cap with room to spare).

### 2. Linear-bot auto-closes parents when sub-issues close (REPO-SPECIFIC)

In `sjawhar/legion`, posting a closed sub-issue caused **Linear-bot** (the
Linear-GitHub bidirectional sync app installed on the repo) to also close
the parent issue 2-3 seconds later. Confirmed via `closed_by` field:

| Issue | I closed it via | closed_by |
|---|---|---|
| #618 (I1ai, depth-3, fixture says `resolved`) | my close_issue call | `sjawhar` (my token) |
| #617 (I1a, depth-2, fixture says `open`) | NOT closed by me | `linear[bot]` ← auto |
| #616 (I1, depth-1, fixture says `open`) | NOT closed by me | `linear[bot]` ← auto |

This is **not** a default GitHub behavior — it's the Linear sync app's
rule that "parent closes when all sub-issues complete." In any Legion repo
with Linear sync installed (which is most of them today), Shape B and the
top-level part of Shape C will fight Linear-bot whenever a worker resolves
a sub-issue.

**Mitigations**:

- Use marker fields in the body for status-of-record, not native open/closed.
  Defeats the main appeal of Shape B (native state).
- Disable Linear-bot's auto-close for inbox-tagged issues. Requires admin
  config — fragile.
- Treat Linear-bot's behavior as desired semantics (parent work-issue
  completes when all its inbox items resolve). This is actually
  defensible — if every inbox item is resolved, the work-issue probably IS
  done. But it conflates "inbox empty" with "work complete," which the
  user explicitly flagged as a conceptual mismatch.

Shape A is **immune** — comments don't have state, so Linear-bot has
nothing to act on.

### 3. Sub-issue depth ≥ 3 works (no hard depth cap hit in the spike)

GitHub doc claims depth ≤ 8. Spike confirmed depth = 3 works (parent →
item → sub-item → sub-sub-item). No depth-related errors. Linear-bot does
NOT distinguish depth — it auto-closes at every level.

### 4. Concurrent native edits are invisible to all three parsers

When a human comments via GitHub UI directly on the parent issue (no
markers), all three shapes' parsers silently skip it:

- Shape A: comment has no marker → not classified as item/reply/status.
- Shape B / C: parent issue's own comments aren't read by the parser at
  all — only sub-issue comments are.

Implication for the dashboard: **plain comments on the work issue are
orphan from the inbox's perspective in every shape.** If we want them
surfaced, we need an explicit "show me freeform parent-issue comments"
view, distinct from the inbox view.

For shapes B/C, human-via-GitHub-UI does work natively for replies on
existing sub-issue items (comments on those sub-issues become replies
automatically). Humans creating *new* sub-issues via UI could become
inbox items if we lean into "label = inbox item." That's actually a real
ergonomic win for Shape B.

### 5. Repo-pollution magnitude (Legion-specific concern)

Per work issue, the inbox creates:

| Shape | New GitHub issues per work-issue with K top-level + L nested inbox items |
|---|---|
| A | 0 |
| B | K + L |
| C | K |

For Legion at scale (hundreds of issues × ~5 inbox items each), B/C add
500-1500 issues to the repo's issue list. They WILL show up in search,
default `gh issue list`, GitHub notifications. Mitigated by labels and
filters, but not invisible.

## Round-trip results in detail

All three shapes round-tripped item count + reply count exactly. Status
breakdown was:

- **Shape A**: 9 open / 2 resolved / 1 cancelled ✓ (matches fixture)
- **Shape B**: 8 open / 3 resolved / 1 cancelled ✗ (Linear-bot auto-closed
  I1a, which the fixture said should be open — see finding #2)
- **Shape C**: 9 open / 2 resolved / 1 cancelled ✓ (matches fixture;
  nested resolved item is a marker comment, not a closed issue, so
  Linear-bot doesn't act on it)

## Parser code size

| Shape | LOC | Notes |
|---|---|---|
| A | ~80 LOC | marker parsing + event stream → state derivation |
| B | ~90 LOC | tree GraphQL + per-leaf REST (two-pass) |
| C | ~80 LOC | single GraphQL + marker parsing on inner comments |

Code complexity is roughly equivalent. Shape B's two-pass adds slight
orchestration overhead.

## Recommendation framing

Returning to the brainstorm with one strong recommendation and two real
trade-offs:

**Lean: Shape C (hybrid).** Reasoning:

- Parse cost matches Shape A (1 call) for single-issue reads — Shape B's
  two-pass is a measurable real cost.
- Cross-issue query is **1 GraphQL call** via label-search (vs A's N REST
  calls).
- Top-level items get native primitives that humans can interact with via
  GitHub UI in parallel — replies, status, per-item URLs, mentions.
- Sub-items as marker comments avoids Linear-bot's auto-close at depth.
- Repo pollution is bounded to top-level item count, not the full nested
  tree.
- Migration: top-level sub-issues → native items is a clean 1:1 map;
  marker comments → child-rows is the same migration A would need.

**Trade-off 1: pollution.** Even Shape C adds K issues per work-issue.
For a 200-issue repo with 5 top-level inbox items each, that's +1000
issues. The mitigation is `inbox-item` label + a dashboard view; in the
GitHub default `gh issue list`, they'll be noise.

**Trade-off 2: Linear-bot lurks.** Top-level inbox items in Shape C are
still real sub-issues. If all of them close, Linear-bot closes the work
issue. Whether that's a feature or a bug is a design choice — see the
brainstorm.

**Alternative: Shape A** if pollution + Linear-bot interaction are
disqualifying. The cost is losing native GitHub UI parallelism (humans
can't create inbox items by opening a sub-issue) and accepting linear
N-REST cost for cross-issue queries. The cross-issue cost is real but
fixable (per-issue cache + only fetch issues whose `updated_at` advanced).

Shape B straight is not recommended — it has Shape C's pollution problem
PLUS the GraphQL complexity cap forcing two-pass reads PLUS Linear-bot
interactions at every nesting level.

## Things deferred

- **Incremental sync** ergonomics — dashboard will poll every few
  seconds; what's the cheap delta query? Being measured in a parallel
  spike; results will be appended below in `## Incremental sync findings`
  when ready.
- **Comment body size limit** (65,536 bytes) — not stressed; D-004-style
  84-reply threads would test it but spike used 20-reply max.
- **Linear backend** — explicitly out of scope for this spike.
- **Native tracker schema** — out of scope; that's the eventual target,
  not the current backend.

## Test artifacts

- Code: `/tmp/inbox-spike/` (throwaway).
- Fixture parent issues (now closed with `inbox-spike` label):
  - #614 — Shape A parent
  - #615 — Shape B parent
  - #628 — Shape C parent
- Consolidated JSON report: `/tmp/inbox-spike/final_report.json`

## Incremental sync findings

Measured against the spike parent issues #614 / #615 / #628 with tiny labeled
deltas (new issues #639, #640) on 2026-05-22.

### Delta polling cost per shape

| Shape | Delta query | No-change cost | 1-event delta | 5-event delta | Notes |
|---|---|---|---|---|---|
| A | `GET /repos/{o}/{r}/issues/{n}/comments?since=T` | 1 REST, 0 KB, 0 cmts | 1 REST, 1.6 KB, 1 cmt | 1 REST, 8.1 KB, 5 cmts | Exact deltas, no false positives. Cheapest per-parent polling. |
| B | GraphQL `repository.issues(filterBy:{ since:T, labels:[...]})` + REST `comments?since=T` per changed child | 1 GraphQL, 0.1 KB, 0 issues | 1 GraphQL + 1 REST = 2 calls per changed child | scales as 1 + K (K = changed children) | New sub-issues discoverable from the single GraphQL delta. Reply deltas need a second pass. |
| C | Same as B | Same as B | Same as B | Same as B | Same hybrid trade-off as B for replies; sub-item deltas (marker comments inside a sub-issue) come for free in the per-changed-child REST. |

### GraphQL `filterBy` probes (real GitHub limits)

| Probe | Result |
|---|---|
| `Issue.subIssues(filterBy:{ since:T })` | **Not supported** — `Field 'subIssues' doesn't accept argument 'filterBy'` |
| `Issue.comments(filterBy:{ since:T })` | **Not supported** — `Field 'comments' doesn't accept argument 'filterBy'` |
| `Repository.issues(filterBy:{ since:T, labels:[...] })` | **Supported, accurate** |

Implication: there's no native primitive to ask "which sub-issues of issue N changed since T?" — you have to discover changed issues at the repo level (via label + since) and then per-child REST for comment deltas.

### Alias batching for cross-parent state

| Query | Result | Cost |
|---|---|---|
| Full alias query for #614 + #615 + #628 with Shape B recursive sub-issues + comments | **Rejected** — 2.5M nodes, over 500K cap | 1 failed GraphQL |
| Reduced alias query: Shape A comments + Shape B tree-only + Shape C top-level + comments | **Succeeded** | 1 GraphQL, 21.9 KB, complexity cost 26 |

Aliases work for cross-parent state when no shape fans out comments under nested sub-issues. That implicitly favors Shape A (flat comments per parent) and Shape C (one level of sub-issues with their own comments).

### Implication for the recommendation

**Discovery and rate-limit math.** A dashboard polling every ~5s for 10 active parent issues:

- **Shape A:** 1 REST per known parent per cycle = 10 calls / 5s = 120 calls/min. GitHub primary rate limit is 5000/h ≈ 83/min — **at risk of hitting the cap**. Plus Shape A has no native label primitive for discovery; the dashboard has to cache the parent list.
- **Shape C:** 1 GraphQL delta (cost ~1) per cycle + 1 REST per *changed* child. Typical idle cycle = 1 call. Plus discovery is trivial via `label:inbox-item` search.

**The earlier recommendation (lean Shape C, fallback Shape A) is reinforced.** For a long-lived dashboard polling many parents, Shape C is meaningfully cheaper on the steady-state cost and has a natural discovery story. Shape A's main advantage (zero repo pollution) remains intact, but the polling-cost penalty is real at scale.

## Test artifact (incremental sync)

- `/tmp/inbox-spike/incremental_sync.md` (raw)
- Throwaway delta issues #639 / #640 (closed and labeled `inbox-spike`)

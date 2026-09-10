# Dispatch global search and the duplicate-issue gate — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A human finds any word written anywhere in Dispatch (issue titles, documents, comments, asks, messages) from a `Ctrl/Cmd+K` palette or the rail's `Search` control and lands on the text; an agent gets the same search as `dispatch_search`; `dispatch_issue` refuses a near-duplicate title with the existing candidates unless the caller passes `force: true`.

**Architecture:** Postgres full-text search in the existing database — generated `tsvector` columns + GIN indexes (migration `0010`), one `GET /api/v1/search` handler (`api/search.go`) running a five-branch `union all` ranked by `ts_rank_cd` with `ts_headline` snippets, a title-lexeme overlap gate in `createIssue` (`api/duplicates.go`), one new tool spec in contracts consumed unchanged by every host, an SPA `features/search/` palette, and a `?q=` document highlight.

**Tech Stack:** Go 1.26 + pgx v5 + Postgres 16 (`websearch_to_tsquery`, `ts_rank_cd`, `ts_headline`), Bun + TypeScript (contracts, envoy-client, React 19 SPA, Playwright e2e).

**Spec:** `docs/superpowers/specs/2026-09-10-dispatch-search-design.md` (all sections normative; § Design fixes the SQL shape and the duplicate rule, § Errors the codes).

**Measured on the live corpus copy** (`~/tmp/dispatch-preflight/live-20260910T170821Z.dump`: 50 issues, 90 documents / 166 versions / 2.8 MB markdown, 9 comments, 91 asks, 103 messages): ranking for `dispatch` < 3 ms; 20 document headlines over full text 143 ms, over a 4 000-char window 33 ms; the duplicate rule flags 12 of 820 native title pairs, all real re-runs; `ts_rank_cd` of a single shared title word is 0.1, of a weight-A title word 1.0.

---

## Scope boundary

**In:** migration 0010; `GET /api/v1/search`; duplicate gate on `POST /api/v1/issues` (`force`); `model` types; contracts (`dispatch_search`, `force`, result types, `snippetSegments`); envoy-client (`DispatchClient.search`, executor cases); roster pins + tool docs in pi-envoy / envoy-plugin / claude-envoy-bridge; `skills/dispatch/SKILL.md` "Search first"; SPA palette, rail control, shortcut, `?q=` highlight in `DocView`; `search.e2e.ts` (chromium + iphone); the corpus-latency fixture (`scripts/restore-dispatch-dump.sh` + `TestSearchLatencyOnCorpus`); server README/AGENTS docs.

**Out:** an SPA issue-creation form (none exists; the 409 is an API/agent path); searching events, refs, project names, binary artifacts; a Proof-editor highlight (PR 4 re-implements the `?q=` contract as a ProseMirror inline decoration — see Decision 9); deploy (PR 5 lane).

## Global Constraints

- Postgres owns the index: every `search` column is `generated always as (...) stored`; no Go code writes or refreshes it. `to_tsvector('english', …)` (two-argument form) is the only tokenizer; `websearch_to_tsquery('english', q)` the only query parser.
- One round trip per search (plus one `numnode` validation query). No Go-side ranking, filtering, or snippet extraction beyond HTML-escaping and sentinel replacement.
- Snippets: the server HTML-escapes the headline text (`html.EscapeString`) and then replaces the private-use sentinels `\uE000`/`\uE001` with `<mark>`/`</mark>`; `<mark>` is the only markup a snippet ever contains. Every consumer renders snippets through `snippetSegments()` from `@legion/contracts` — never `innerHTML`.
- Error table (spec § Errors) is normative; no silent fallbacks: an unsearchable query is `400 INVALID_QUERY`, a missing `?q=` term in a document shows the status line, an API failure in the palette shows `QueryError` with Retry.
- The 409 gate runs before `createIssue`'s transaction: nothing is written and no `projects.next_number` is consumed when it fires.
- Colours only through `web/src/theme/classes.ts` (`no-raw-colors.test.ts` scans `web/src` and `e2e`); new pairs are registered with `registerText` so `palette.test.ts` proves contrast.
- Red-first: every task's tests are written and run to failure before the implementation.
- Comments describe current behaviour, never history.
- Go checks are package-scoped while implementing: `cd packages/envoy && gofmt -l ./internal/dispatch && go vet ./internal/dispatch/... && DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' go test ./internal/dispatch/api/ -run <Test> -count=1 -v` (dev Postgres from `scripts/dev-postgres.sh`; the test harness creates an isolated database per test — `api/server_test.go` `openEmptyTestStore`).
- TS checks are per package with the package's own scripts (`bun run lint && bun run typecheck && bun run test`, plus `bun run build` in contracts and `bun run e2e` in dispatch). No dependency changes are expected; if one appears, commit `bun.lock`.
- Migration numbering: this plan uses `0010_search.up.sql`. `store.Migrate` applies files in filename order and records each version once (gaps are fine), PR 3 (#887) claims `schema_migrations` version 8 from Go, and the project-documents plan reserves 0009. **At every rebase** run `ls packages/envoy/internal/dispatch/store/migrations` against `main@origin`; if a `0010_*.up.sql` landed, renumber to the next free number (file name and nothing else — the version is parsed from the file name).
- jj, not git: commit = `jj describe -m "<msg>"` then `jj new`. One task, one commit.

## Decisions where the spec under-specifies (decided here; not open questions)

1. **Result kinds are exactly** `issue | document | comment | ask | message`; `document` covers only `artifacts.kind = 'doc'` and reads the latest `artifact_versions` row through the `(artifact_id, number)` unique index. Suggestions are comments (their `body`; `suggestion->>'replace_with'` is not indexed).
2. **Ranking:** `ts_rank_cd(vector, tsquery)` with default weights; issue keys and titles carry weight `A` (a title word scores 1.0 against a body word's 0.1), everything else weight `D`. Ties break by the issue's `updated_at desc`, then `kind`, then `id`. No length normalisation.
3. **Snippet window:** the headline source is a 4 000-character window starting 1 500 characters before the first case-insensitive occurrence of the query's first positive term (`firstTerm`: the first whitespace token not starting with `-` and not `or`/`OR`, surrounding double quotes stripped); when the raw term does not occur (a stem-only match) the source is the first 4 000 characters. Options: `StartSel=\uE000, StopSel=\uE001, MaxWords=24, MinWords=12, MaxFragments=1`.
4. **`href` is an SPA-relative path** built server-side (`searchHref`), because the server already knows which artifact is primary (`/issues/KEY/spec` vs `/issues/KEY/artifacts/<slug>`); `q` is appended URL-encoded for documents only. The tool renders `new URL(href, config.url)`.
5. **`limit`:** default 20, maximum 50 — `400 INVALID_LIMIT` outside 1..50 or non-integer. `project` is a filter only (an unknown project yields an empty result, like `GET /issues?project=`).
6. **Duplicate gate scope and rule** (spec § Design): same project only; the declared `parent` is excluded and its title lexemes are removed from both sides so children named `<parent title>: <part>` never flag each other; `external` creations skip the gate (external titles are `owner/repo#n`, whose lexemes are the repository's and therefore shared by every external issue of that repository — five of them for `trajectory-labs-pbc/agent-c#…` on the live corpus); `force: true` skips it. Candidate when `shared ≥ 3 ∧ 2·shared ≥ shorter` or `1 ≤ shared = shorter`. Status is not consulted (spec Decision 1 recommendation; if Sami picks (b), add `and i.closed_at is null` to the `cand` CTE — one line).
7. **409 body:** `{error: "possible duplicate of <KEY>: <title>", code: "POSSIBLE_DUPLICATE", candidates: [{key, title, status, snippet, shared_terms, href}]}`, at most 5, ordered by `shared_terms desc, updated_at desc`. `snippet` is the candidate's title with every shared term in `<mark>` (`HighlightAll=true`). The executor returns a **result** (not a thrown error) so the model reads the candidates: `text` lists them and says how to proceed; `details = { duplicates: candidates }` (no `topic`, so hosts do not subscribe).
8. **Palette interaction:** `Ctrl+K` (and `Meta+K`) toggles from anywhere except while typing in a `textarea`/`contenteditable`; the rail `Search` control (a `button` styled as a field with a `⌘K`/`Ctrl K` hint, `aria-keyshortcuts="Control+K Meta+K"`) opens it; on the phone drawer the button closes the drawer first. Combobox pattern: `input[role=combobox]` + `ul[role=listbox]` + `li[role=option]`, `aria-activedescendant` on the input; `ArrowDown/ArrowUp` move (wrapping), the first option is active whenever the result list changes, `Enter` navigates the active option (click navigates the clicked one), `Escape` closes and focus returns to the opener (`useDialog`). Results are grouped by issue (group order = rank of the group's best hit); a group header is the issue key + title + status; a `done` group renders with `textMutedOnSurface`. Kind badges are text (`issue`, `doc`, `comment`, `ask`, `message`) in `badgeLow`. Rows are `min-h-11` (44 px). The query is debounced 150 ms and fetched with TanStack `useQuery` (`queryKey: ["search", q]`, `enabled: q.trim().length >= 2`, `placeholderData: keepPreviousData`).
9. **Document highlight contract:** `?q=<query>` on a document route highlights the first case-insensitive occurrence of `firstHighlightTerm(q)` (same tokenisation as Decision 3, in TS) inside a single rendered text node, wrapping it in `<mark class="dispatch-anchor-history" data-dispatch-search-hit>` and calling `scrollIntoView({block: "center"})` once per `[markdown, term]` change; a term split across inline elements (`*astro*labe`) is reported as not found. When PR 4 replaces `DocView` with the Proof editor, the same `?q=` is consumed by an inline `Decoration` in the editor host — the URL contract, not the DOM technique, is what `search.e2e.ts` asserts (`mark[data-dispatch-search-hit]` visible with the term).
10. **`asks.search`** indexes `question || ' ' || coalesce(answer->>'text', '')`; option labels are not indexed (they are the agent's choices, not the human's words).
11. **Latency fixture is env-gated, not CI:** `TestSearchLatencyOnCorpus` skips without `DISPATCH_BENCH_DATABASE_URL`; the PR body pastes its output from the restored live copy.
12. **The tool's issue-less set** is `dispatch_issue`, `dispatch_resolve_ask`, `dispatch_search` in both `resolveIssueArguments` and the `ensureIssue` guard of `executeDispatchTool` — a named constant `issueFreeTools` replaces the two duplicated conditions.

---

## File Structure

```
packages/envoy/internal/dispatch/
├── store/migrations/0010_search.up.sql       // NEW (Task 2): generated tsvector columns + GIN indexes
├── model/model.go                            // Task 2: SearchIssue, SearchArtifact, SearchResult, SearchResponse, DuplicateCandidate
├── api/server.go                             // Task 2: `GET /api/v1/search` route
├── api/search.go                             // NEW (Task 2): handler, searchQuery, firstTerm, markSnippet, searchHref, headlineOptions
├── api/search_test.go                        // NEW (Task 2)
├── api/duplicates.go                         // NEW (Task 3): duplicateCandidates, duplicateQuery
├── api/issues.go                             // Task 3: createIssue input.Force + gate call
├── api/duplicates_test.go                    // NEW (Task 3)
├── api/search_bench_test.go                  // NEW (Task 7): TestSearchLatencyOnCorpus (env-gated)
packages/envoy/scripts/restore-dispatch-dump.sh   // NEW (Task 7)
packages/envoy/cmd/dispatch/{README,AGENTS}.md    // Task 7: route rows, error codes, search section
packages/contracts/src/
├── dispatch-tools.ts, dispatch-tools.test.ts // Task 1: dispatch_search spec, dispatch_issue.force
├── dispatch-api.ts                           // Task 1: SearchResultKind, SearchIssueRef, SearchArtifactRef, SearchResult, SearchResponse, DuplicateCandidate, CreateIssueInput.force, error shape
├── dispatch-snippet.ts, dispatch-snippet.test.ts // NEW (Task 1): snippetSegments, snippetText
├── index.ts                                  // Task 1: export * from "./dispatch-snippet"
packages/contracts/AGENTS.md                  // Task 4: eleven tools
packages/envoy-client/src/
├── dispatch-http.ts                          // Task 4: search(), SearchOptions
├── dispatch-execute.ts                       // Task 4: issueFreeTools, dispatch_search case, dispatch_issue 409 rendering + force
├── __tests__/dispatch-execute.test.ts, __tests__/dispatch-http.test.ts   // Task 4
packages/envoy-client/README.md               // Task 4
packages/pi-envoy/extensions/envoy.test.ts    // Task 4: roster count + dispatch_search execution fixture
packages/pi-envoy/{AGENTS,README}.md          // Task 4
packages/envoy-plugin/src/__tests__/dispatch-tools.test.ts, packages/envoy-plugin/{AGENTS,README}.md   // Task 4
packages/claude-envoy-bridge/tests/envoy-mcp-server.test.ts, packages/claude-envoy-bridge/README.md    // Task 4
skills/dispatch/SKILL.md                      // Task 4: "Search first" section, dispatch_search, POSSIBLE_DUPLICATE
packages/dispatch/web/src/
├── api/client.ts, __tests__/client.test.ts   // Task 5: search()
├── api/types.ts                              // Task 5: re-export the new contract types
├── theme/classes.ts                          // Task 5: SEARCH_HIT_BG/TEXT, searchHitBg, searchHitText, kbdHint
├── features/search/search-model.ts, search-model.test.ts          // NEW (Task 5): groupResults, firstHighlightTerm, stepActive, kindLabel
├── features/search/SearchPalette.tsx, SearchPalette.test.tsx      // NEW (Task 5)
├── features/search/SearchButton.tsx          // NEW (Task 5)
├── features/search/useSearchShortcut.ts      // NEW (Task 5)
├── features/doc/DocView.tsx, DocView.test.tsx                     // Task 5: highlightTerm
├── features/doc/DocEditor.tsx                // Task 5: pass highlightTerm through
├── features/issue/IssuePage.tsx              // Task 5: read ?q=
├── app.tsx                                   // Task 5: palette state, shortcut, NavigationContents.onSearch
packages/dispatch/AGENTS.md                   // Task 5: features/search in Layout
packages/dispatch/e2e/search.e2e.ts           // NEW (Task 6)
```

**Task order and dependencies:** Task 1 (contracts) and Task 2 (Go search) have no dependencies and run in parallel. Task 3 (gate) depends on Task 2. Task 4 (envoy-client, hosts, skill) and Task 5 (SPA) depend on Task 1 only — run them in parallel with Task 2/3. Task 6 (e2e) depends on Tasks 2 and 5. Task 7 (latency fixture, server docs) depends on Tasks 2 and 3. Task 8 (PR) depends on everything. Each task owns its files exclusively (File Structure); cross-task use is by import only.

---

### Task 1: contracts — `dispatch_search`, `force`, result types, `snippetSegments`

**Files:**
- Modify: `packages/contracts/src/dispatch-tools.ts`, `dispatch-tools.test.ts`, `dispatch-api.ts`, `index.ts`
- Create: `packages/contracts/src/dispatch-snippet.ts`, `dispatch-snippet.test.ts`

**Interfaces (produces):**
```ts
// dispatch-api.ts
export type SearchResultKind = "issue" | "document" | "comment" | "ask" | "message";
export interface SearchIssueRef { readonly key: string; readonly title: string; readonly status: string; }
export interface SearchArtifactRef { readonly slug: string; readonly name: string; }
export interface SearchResult {
  readonly kind: SearchResultKind;
  readonly issue: SearchIssueRef;
  readonly artifact?: SearchArtifactRef;   // document, and anchored comment/ask
  readonly id: string;                      // issue key, artifact id, comment id, ask id, message id
  readonly snippet: string;                 // HTML-escaped text; <mark> is the only element
  readonly rank: number;
  readonly href: string;                    // SPA-relative deep link
}
export interface SearchResponse { readonly results: SearchResult[]; readonly took_ms: number; }
export interface DuplicateCandidate {
  readonly key: string; readonly title: string; readonly status: string;
  readonly snippet: string; readonly shared_terms: number; readonly href: string;
}
export interface CreateIssueInput { …existing…; readonly force?: boolean; }
export interface DispatchServiceErrorShape {
  readonly error?: string; readonly code?: string;
  readonly candidates?: TargetCandidate[] | DuplicateCandidate[];   // TARGET_AMBIGUOUS | POSSIBLE_DUPLICATE
}

// dispatch-snippet.ts
export interface SnippetSegment { readonly text: string; readonly mark: boolean; }
/** Splits a server snippet on <mark>/</mark> and decodes the five entities html.EscapeString emits
 *  (&lt; &gt; &amp; &#39; &#34;); the result carries no markup. */
export function snippetSegments(snippet: string): SnippetSegment[];
/** Plain text for terminals: marked runs wrapped in ** **. */
export function snippetText(snippet: string): string;
```
Tool spec additions in `dispatchToolSpecs` (append `dispatch_search` **last**, after `dispatch_read`, so existing index-based fixtures stay valid):
```ts
  {
    name: "dispatch_search",
    description:
      "Search every issue, document, comment, ask, and message for a keyword or phrase and get deep links. " +
      "Use it before creating an issue or a design document, and to find where a word was written. " +
      "Websearch syntax: \"quoted phrase\", -excluded, OR.",
    arguments: (z) => ({
      query: z.string({ min: 2 }).describe("Keyword, phrase, or websearch expression; at least 2 characters."),
      project: z.string().describe("Optional project key to search within.").optional(),
      limit: z.number({ int: true, min: 1, max: 50 }).describe("Maximum results, 1-50; default 20.").optional(),
    }),
  },
```
and on `dispatch_issue`: `force: z.boolean().describe("Create even though POSSIBLE_DUPLICATE listed similar issues; pass it only after reading them.").optional()`.

- [ ] **Step 1: Failing tests.**
  - `dispatch-tools.test.ts`: the name-list assertion gains `"dispatch_search"` at the end; `validCalls` gains `dispatch_search: { query: "astrolabe" }`; add `test("dispatch_search rejects a one-character query and a limit above 50")` (`schemaFor("dispatch_search").safeParse({ query: "a" }).success === false`, `{ query: "ok", limit: 51 }` false, `{ query: "ok", limit: 50, project: "LEGION" }` true) and `test("dispatch_issue accepts force")`.
  - `dispatch-snippet.test.ts`:
```ts
test("snippetSegments splits marks and decodes escaped entities", () => {
  expect(snippetSegments("Use the &lt;b&gt;<mark>astrolabe</mark>&lt;/b&gt; &amp; sextant")).toEqual([
    { text: "Use the <b>", mark: false },
    { text: "astrolabe", mark: true },
    { text: "</b> & sextant", mark: false },
  ]);
  expect(snippetSegments("")).toEqual([]);
  expect(snippetSegments("&#39;quoted&#34; &lt;mark&gt;not a mark&lt;/mark&gt;")).toEqual([
    { text: `'quoted" <mark>not a mark</mark>`, mark: false },
  ]);
});
test("snippetText renders marks as bold", () => {
  expect(snippetText("the <mark>astrolabe</mark> here")).toBe("the **astrolabe** here");
});
```
- [ ] **Step 2: Run** `cd packages/contracts && bun run test`. Expected: failures (`dispatch_search` missing, module `./dispatch-snippet` not found).
- [ ] **Step 3: Implement** the types, the tool spec, `force`, `dispatch-snippet.ts` (split with `/<mark>|<\/mark>/`, toggling `mark`; decode entities with a fixed five-entry table — `&amp;` last), and the `index.ts` export.
- [ ] **Step 4: Run** `cd packages/contracts && bun run lint && bun run typecheck && bun run test && bun run build`. Expected: green.
- [ ] **Step 5: Commit** — `jj describe -m "feat(contracts): dispatch_search tool, issue force flag, search result types, snippet segments" && jj new`

---

### Task 2: Go — migration 0010, `GET /api/v1/search`

**Files:**
- Create: `packages/envoy/internal/dispatch/store/migrations/0010_search.up.sql`, `packages/envoy/internal/dispatch/api/search.go`, `api/search_test.go`
- Modify: `packages/envoy/internal/dispatch/model/model.go`, `api/server.go`

**Migration (exact text):**
```sql
-- 0010_search.up.sql
-- Full-text search columns. Generated columns: Postgres computes them on every write;
-- no application code refreshes them. Issue keys and titles carry weight A so a title
-- hit outranks a body hit. artifact_versions gets the stored vector without an index:
-- the search reads exactly one latest row per document through
-- artifact_versions_artifact_id_number_key, and an index over every version could only
-- surface stale versions.
alter table issues add column search tsvector generated always as
  (setweight(to_tsvector('english', key), 'A') || setweight(to_tsvector('english', title), 'A')) stored;
create index issues_search on issues using gin (search);
alter table artifact_versions add column search tsvector generated always as
  (to_tsvector('english', coalesce(markdown, ''))) stored;
alter table comments add column search tsvector generated always as
  (to_tsvector('english', body)) stored;
create index comments_search on comments using gin (search);
alter table asks add column search tsvector generated always as
  (to_tsvector('english', question || ' ' || coalesce(answer->>'text', ''))) stored;
create index asks_search on asks using gin (search);
alter table messages add column search tsvector generated always as
  (to_tsvector('english', body)) stored;
create index messages_search on messages using gin (search);
```

**Interfaces (produces):**
```go
// model/model.go
type SearchIssue struct{ Key, Title, Status string }                  // json: key, title, status
type SearchArtifact struct{ Slug, Name string }                       // json: slug, name
type SearchResult struct {
	Kind     string          `json:"kind"`
	Issue    SearchIssue     `json:"issue"`
	Artifact *SearchArtifact `json:"artifact,omitempty"`
	ID       string          `json:"id"`
	Snippet  string          `json:"snippet"`
	Rank     float64         `json:"rank"`
	Href     string          `json:"href"`
}
type SearchResponse struct {
	Results []SearchResult `json:"results"`   // encoded as [] when empty, never null
	TookMS  int64          `json:"took_ms"`
}
type DuplicateCandidate struct {              // used by Task 3
	Key         string `json:"key"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Snippet     string `json:"snippet"`
	SharedTerms int    `json:"shared_terms"`
	Href        string `json:"href"`
}

// api/search.go
const (
	searchDefaultLimit = 20
	searchMaxLimit     = 50
	markStart          = "\uE000" // private-use sentinels: ts_headline writes them, markSnippet turns them into <mark>
	markEnd            = "\uE001"
	headlineOptions    = "StartSel=" + markStart + ", StopSel=" + markEnd + ", MaxWords=24, MinWords=12, MaxFragments=1"
)
func (s *server) search(w http.ResponseWriter, r *http.Request)
func firstTerm(q string) string          // Decision 3
func markSnippet(headline string) string // html.EscapeString, then sentinels → <mark>/</mark>
func searchHref(kind, issueKey string, artifact *model.SearchArtifact, primary bool, id, q string) string // Decision 4
```
Handler flow: `requireAuthenticated` → `q := strings.TrimSpace(r.URL.Query().Get("q"))`; `utf8.RuneCountInString(q) < 2` → `400 INVALID_QUERY "q must be at least 2 characters"` → parse `limit` (absent → 20; `strconv.Atoi` error or outside 1..50 → `400 INVALID_LIMIT`) → `select numnode(websearch_to_tsquery('english', $1))` = 0 → `400 INVALID_QUERY "query has no searchable terms"` → `started := time.Now()` → run `searchQuery` → scan rows into `model.SearchResult` (`slug`/`name` scanned as `*string`; `Artifact` set when `slug` is non-null; `Href` from `searchHref`; `Snippet` from `markSnippet`) → `writeJSON(200, model.SearchResponse{Results: results, TookMS: time.Since(started).Milliseconds()})`.

`searchQuery` (`$1` q, `$2` project or `''`, `$3` limit, `$4` firstTerm, `$5` headlineOptions):
```sql
with q as (select websearch_to_tsquery('english', $1) as tsq, $4::text as term),
hits as (
  select 'issue' as kind, i.key as issue_key, null::uuid as artifact_id, i.key as id,
         ts_rank_cd(i.search, q.tsq) as rank, i.title as text
    from issues i, q where i.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'document', a.issue_key, a.id, a.id::text, ts_rank_cd(v.search, q.tsq), v.markdown
    from artifacts a
    join issues i on i.key = a.issue_key
    join lateral (select v.search, v.markdown from artifact_versions v
                   where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'comment', c.issue_key, (c.anchor->>'artifact_id')::uuid, c.id::text, ts_rank_cd(c.search, q.tsq), c.body
    from comments c join issues i on i.key = c.issue_key, q
   where c.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'ask', k.issue_key, (k.anchor->>'artifact_id')::uuid, k.id::text, ts_rank_cd(k.search, q.tsq),
         k.question || ' ' || coalesce(k.answer->>'text', '')
    from asks k join issues i on i.key = k.issue_key, q
   where k.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'message', m.issue_key, null, m.id::text, ts_rank_cd(m.search, q.tsq), m.body
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)
),
ranked as (
  select h.kind, h.issue_key, h.artifact_id, h.id, h.rank, h.text, i.title as issue_title, i.status, i.updated_at
    from hits h join issues i on i.key = h.issue_key
   order by h.rank desc, i.updated_at desc, h.kind, h.id
   limit $3
)
select r.kind, r.issue_key, r.issue_title, r.status, ar.slug, ar.name, coalesce(ar.is_primary, false) as is_primary, r.id, r.rank,
       ts_headline('english',
         case when q.term <> '' and strpos(lower(r.text), lower(q.term)) > 0
              then substr(r.text, greatest(1, strpos(lower(r.text), lower(q.term)) - 1500), 4000)
              else left(r.text, 4000) end,
         q.tsq, $5) as headline
  from ranked r left join artifacts ar on ar.id = r.artifact_id, q
 order by r.rank desc, r.updated_at desc, r.kind, r.id
```
Route: `mux.HandleFunc("GET /api/v1/search", s.search)` next to `GET /api/v1/inbox` in `Register`.

- [ ] **Step 1: Failing tests** (`api/search_test.go`; helpers from `server_test.go`: `newTestHandler`, `newTestHandlerWithStore`, `dispatchRequest`, `agentRequest`, `decodeBody`; from `interactions_test.go`: `createInteractionIssue`):

```go
func searchRequest(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet, "/api/v1/search?"+query, nil, "alice")
}

func seedSearchCorpus(t *testing.T, handler http.Handler) (issueKey, commentID, askID, messageID string) {
	t.Helper()
	// TEST project exists via RepoProjectsRaw; create the corpus through the public API only.
	issue := createInteractionIssue(t, handler, "SRCH", "Navigation instruments", "# Instruments\n\nThe astrolabe measures altitude.\n\n"+strings.Repeat("Filler sentence. ", 400)+"\n")
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Replace the sextant diagram."}, "alice")
	ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{"question": "Keep the quadrant?", "options": []map[string]string{{"label": "Yes"}}}, "alice")
	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{"body": "Compass calibration done."}, "alice")
	// … fatal on non-201, decode ids …
}

func TestSearchFindsEveryKindWithSnippetsAndHrefs(t *testing.T) {
	handler := newTestHandler(t)
	key, commentID, askID, messageID := seedSearchCorpus(t, handler)
	cases := []struct{ q, kind, id, href, mark string }{
		{"astrolabe", "document", "<primary artifact id>", "/issues/" + key + "/spec?q=astrolabe", "<mark>astrolabe</mark>"},
		{"sextant", "comment", commentID, "/issues/" + key + "/comments/" + commentID, "<mark>sextant</mark>"},
		{"quadrant", "ask", askID, "/issues/" + key + "/asks/" + askID, "<mark>quadrant</mark>"},
		{"compass", "message", messageID, "/issues/" + key + "/log", "<mark>Compass</mark>"},
		{"instruments", "issue", key, "/issues/" + key, "<mark>instruments</mark>"},   // stemmed: title says "instruments", query "instruments"; the document also matches and ranks below the title hit
	}
	// for each: 200, results[0] has kind/id/href, snippet contains mark, issue.status == "triage", took_ms >= 0
}

func TestSearchDocumentSnippetWindowsAroundADeepMatch(t *testing.T) {
	// spec = 12 000 chars of filler, then "The astrolabe measures altitude."; expect the snippet to contain <mark>astrolabe</mark> (the window follows the raw term, not the document start)
}

func TestSearchReadsOnlyTheLatestDocumentVersion(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Instruments", "The astrolabe measures altitude.\n")
	// POST /api/v1/artifacts/{primary}/edits replace astrolabe → sextant (sessionRequest with "actor": sessionActor(), as every bearer edit), then wait until artifact_versions has 2 rows (poll ≤ 2 s; Settle is 20 ms in tests)
	// "astrolabe" → no document result; "sextant" → document result for the primary artifact
}

func TestSearchRanksTitleHitsAboveBodyHits(t *testing.T) {
	// issue A titled "Astrolabe" with spec "nothing here"; issue B titled "Other" with a spec mentioning astrolabe twice → results[0].kind == "issue" && results[0].id == A.Key
}

func TestSearchFiltersByProjectAndHonoursLimit(t *testing.T) {
	// two projects each with an "astrolabe" spec; ?q=astrolabe&project=SRCH → only SRCH keys; ?q=astrolabe&limit=1 → exactly one result
}

func TestSearchEscapesMarkupInSnippets(t *testing.T) {
	// comment body `<script>alert(1)</script> astrolabe <mark>x</mark>` → snippet contains "&lt;script&gt;" and "&lt;mark&gt;x&lt;/mark&gt;" and exactly one real "<mark>astrolabe</mark>"
}

func TestSearchRejectsInvalidQueries(t *testing.T) {
	cases := map[string]string{"q=a": "INVALID_QUERY", "": "INVALID_QUERY", "q=the": "INVALID_QUERY", "q=astrolabe&limit=0": "INVALID_LIMIT", "q=astrolabe&limit=51": "INVALID_LIMIT", "q=astrolabe&limit=x": "INVALID_LIMIT"}
	// each → 400 with the code
}

func TestSearchAuthentication(t *testing.T) {
	// no identity header → 401; agentRequest with "agent-token" → 200; wrong bearer → 401
}

func TestFirstTermSkipsOperators(t *testing.T) {
	// table: `astrolabe` → astrolabe; `-daemon astrolabe` → astrolabe; `"merge queue" -x` → merge; `OR astrolabe` → astrolabe; `-only` → ""
}
```

- [ ] **Step 2: Run** — `go test ./internal/dispatch/api/ -run 'TestSearch|TestFirstTerm' -v`. Expected: compile failure (`firstTerm` undefined), then 404s from the missing route.
- [ ] **Step 3: Implement** the migration, the model types, `search.go`, and the route. `firstTerm`: `strings.Fields(q)`, skip tokens starting with `-` or equal to `or`/`OR`, strip `"` from both ends, return the first non-empty. `markSnippet`: `strings.NewReplacer(markStart, "<mark>", markEnd, "</mark>").Replace(html.EscapeString(headline))`. `searchHref`: `issue` → `/issues/KEY`; `document` → `/issues/KEY/spec` when primary else `/issues/KEY/artifacts/` + `url.PathEscape(slug)`, then `?q=` + `url.QueryEscape(q)`; `comment` → `/issues/KEY/comments/<id>`; `ask` → `/issues/KEY/asks/<id>`; `message` → `/issues/KEY/log`.
- [ ] **Step 4: Run** — `gofmt -l ./internal/dispatch && go vet ./internal/dispatch/... && go test ./internal/dispatch/api/ ./internal/dispatch/store/ -count=1`. Expected: PASS (the store test suite applies every embedded migration to a fresh database — 0010 included). Smoke the real surface: `cd packages/envoy && DATABASE_URL=<dev url> DISPATCH_AGENT_TOKEN=local-agent-token DISPATCH_IDENTITY='header:X-Dispatch-User' DISPATCH_ALLOWED_LOGINS=sjawhar DISPATCH_INSECURE_COOKIE=1 DISPATCH_DEFAULT_PROJECT=LOCAL go run ./cmd/dispatch` (boot log shows `apply migration 10`), then `curl -s -H 'X-Dispatch-User: sjawhar' 'http://127.0.0.1:8766/api/v1/search?q=dispatch' | jq '.took_ms, .results[0]'` and `curl -s -H 'Authorization: Bearer local-agent-token' '…/search?q=a'` → `INVALID_QUERY`. Paste both into the PR.
- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch): full-text search over issues, documents, comments, asks, and messages (GET /api/v1/search)" && jj new`

---

### Task 3: Go — the duplicate-issue gate on `POST /api/v1/issues`

**Files:**
- Create: `packages/envoy/internal/dispatch/api/duplicates.go`, `api/duplicates_test.go`
- Modify: `packages/envoy/internal/dispatch/api/issues.go`

**Interfaces (produces):**
```go
// duplicates.go
const duplicateHeadlineOptions = "StartSel=" + markStart + ", StopSel=" + markEnd + ", HighlightAll=true"
// duplicateCandidates returns issues in project whose title near-duplicates title (spec § Design rule),
// excluding parentKey and ignoring the parent title's terms. parentKey may be "".
func (s *server) duplicateCandidates(ctx context.Context, q queryer, project, title, parentKey string) ([]model.DuplicateCandidate, error)
```
`duplicateQuery` (`$1` project, `$2` title, `$3` parent key or `''`, `$4` duplicateHeadlineOptions):
```sql
with parent as (select coalesce((select title from issues where key = $3), '') as title),
new_title as (
  select array(select unnest(tsvector_to_array(to_tsvector('english', $2)))
               except select unnest(tsvector_to_array(to_tsvector('english', p.title)))) as lex
    from parent p),
cand as (
  select i.key, i.title, i.status, i.updated_at,
         array(select unnest(tsvector_to_array(to_tsvector('english', i.title)))
               except select unnest(tsvector_to_array(to_tsvector('english', p.title)))) as lex
    from issues i, parent p
   where i.project_key = $1 and i.key <> $3),
scored as (
  select c.key, c.title, c.status, c.updated_at, n.lex as new_lex,
         (select count(*) from (select unnest(c.lex) intersect select unnest(n.lex)) s)::int as shared,
         least(cardinality(c.lex), cardinality(n.lex)) as shorter
    from cand c, new_title n
   where c.lex && n.lex)
select key, title, status, shared,
       ts_headline('english', title,
         to_tsquery('simple', (select string_agg(quote_literal(x), ' | ') from unnest(new_lex) x)), $4) as headline
  from scored
 where (shared >= 3 and 2 * shared >= shorter) or (shared >= 1 and shared = shorter)
 order by shared desc, updated_at desc
 limit 5
```
(`to_tsquery('simple', …)` re-parses already-stemmed lexemes without stemming or stop-word removal; `quote_literal` quoting was verified against apostrophes, backslashes, and hyphenated tokens.) Each candidate: `Snippet = markSnippet(headline)`, `Href = "/issues/" + key`.

`createIssue` changes: the input struct gains `Force bool \`json:"force"\`` (the decoder uses `DisallowUnknownFields`, so without this field `force` is a 400). After the `INVALID_ISSUE` check and before `s.begin`:
```go
	if input.External == "" && !input.Force {
		candidates, err := s.duplicateCandidates(r.Context(), s.deps.Store.Pool, input.Project, input.Title, parentKey)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if len(candidates) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      fmt.Sprintf("possible duplicate of %s: %s", candidates[0].Key, candidates[0].Title),
				"code":       "POSSIBLE_DUPLICATE",
				"candidates": candidates,
			})
			return
		}
	}
```

- [ ] **Step 1: Failing tests** (`api/duplicates_test.go`):

```go
func createIssueRequest(t *testing.T, handler http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
}

func TestCreateIssueRejectsNearDuplicateTitleWithCandidates(t *testing.T) {
	handler := newTestHandler(t)
	first := createIssueRequest(t, handler, map[string]any{"project": "TEST", "title": "Global search across issues and documents"})
	// 201
	second := createIssueRequest(t, handler, map[string]any{"project": "TEST", "title": "Global search for issues and documents"})
	if second.Code != http.StatusConflict { t.Fatalf(...) }
	body := decodeBody[struct {
		Code       string                     `json:"code"`
		Candidates []model.DuplicateCandidate `json:"candidates"`
	}](t, second)
	// Code == "POSSIBLE_DUPLICATE"; len(Candidates) == 1; Candidates[0].Key == TEST-1; Status == "triage"; SharedTerms == 4;
	// Snippet == "<mark>Global</mark> <mark>search</mark> across <mark>issues</mark> and <mark>documents</mark>"; Href == "/issues/TEST-1"
	third := createIssueRequest(t, handler, map[string]any{"project": "TEST", "title": "Unrelated"})
	// 201 and key == "TEST-2": the refused call consumed no issue number
}

func TestCreateIssueForceBypassesTheGate(t *testing.T) {
	// same two titles, second with "force": true → 201, key TEST-2
}

func TestCreateIssueGateIgnoresParentTitleTerms(t *testing.T) {
	// parent "Dispatch global search" (TEST-1); child A {"parent": "TEST-1", "title": "Dispatch global search: server"} → 201;
	// child B {"parent": "TEST-1", "title": "Dispatch global search: SPA"} → 201 (siblings share only parent terms);
	// child A again with the same parent → 409 naming TEST-2
}

func TestCreateIssueGateIsScopedToProjectAndSkipsExternal(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	// project OTHER created via POST /api/v1/projects; "Global search across issues and documents" in TEST then in OTHER → both 201
	// external: after the native "Global search across issues and documents" exists, {"external": "owner/repo#41", "title": "Global search across issues and documents"} → 201 (an external creation skips the gate even with an identical title; owner/repo maps to TEST via RepoProjectsRaw)
}

func TestCreateIssueGateRule(t *testing.T) {
	// existing "Astrolabe calibration notes": new "Astrolabe" → 409 (containment: shared 1 = shorter 1)
	// existing "Global search across issues and documents": new "Search palette loses focus on phone" → 201 (shared 1 < shorter)
	// existing "Red-teamer hiring test on the platform: candidate attack authoring and live results": new "Chief of staff: red-teamer ops, onboarding, weekly check-ins, contractor comms, platform access" → 201 (shared 4 — platform, red, red-team, teamer — shorter 11: 8 < 11)
}
```

- [ ] **Step 2: Run** — `go test ./internal/dispatch/api/ -run 'TestCreateIssue(Rejects|Force|Gate)' -v`. Expected: the duplicate calls return 201 (gate absent) and `force` returns `400 INVALID_JSON` (unknown field).
- [ ] **Step 3: Implement** `duplicates.go` and the `createIssue` changes.
- [ ] **Step 4: Run** — `gofmt -l ./internal/dispatch && go vet ./internal/dispatch/... && go test ./internal/dispatch/api/ -count=1`. Expected: PASS (existing `createIssue` tests still pass: every fixture title in `issues_test.go`/`interactions_test.go` is distinct within its project, and external creations skip the gate). Smoke: with the server from Task 2 running, `curl -s -X POST -H 'X-Dispatch-User: sjawhar' -H 'Content-Type: application/json' -d '{"project":"LOCAL","title":"<an existing LOCAL title>"}' http://127.0.0.1:8766/api/v1/issues | jq` → 409 body; add `"force":true` → 201.
- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch): POST /issues refuses a near-duplicate title with candidates unless force is set" && jj new`

---

### Task 4: envoy-client, host rosters, tool docs, and the skill

**Files:**
- Modify: `packages/envoy-client/src/dispatch-http.ts`, `dispatch-execute.ts`, `__tests__/dispatch-execute.test.ts`, `__tests__/dispatch-http.test.ts`, `packages/envoy-client/README.md`
- Modify: `packages/pi-envoy/extensions/envoy.test.ts`, `packages/pi-envoy/AGENTS.md`, `packages/pi-envoy/README.md`
- Modify: `packages/envoy-plugin/src/__tests__/dispatch-tools.test.ts`, `packages/envoy-plugin/AGENTS.md`, `packages/envoy-plugin/README.md`
- Modify: `packages/claude-envoy-bridge/tests/envoy-mcp-server.test.ts`, `packages/claude-envoy-bridge/README.md`
- Modify: `packages/contracts/AGENTS.md`, `skills/dispatch/SKILL.md`

**Interfaces (produces):**
```ts
// dispatch-http.ts
export interface SearchOptions { readonly project?: string; readonly limit?: number; }
async search(query: string, options: SearchOptions = {}): Promise<SearchResponse>   // GET /api/v1/search?q=&project=&limit=
// dispatch-execute.ts
const issueFreeTools = new Set(["dispatch_issue", "dispatch_resolve_ask", "dispatch_search"]);  // replaces both `tool === "dispatch_issue" || tool === "dispatch_resolve_ask"` conditions
```
`dispatch_search` case: `client.search(stringArg(args, "query"), { project?, limit? })` →
```
text:
  `3 results for "astrolabe" (12 ms)` then one line per result:
  `LEGION-2 [triage] document spec.md — …the **astrolabe** measures… → https://dispatch.example/issues/LEGION-2/spec?q=astrolabe`
  (kind label, artifact name when present, `snippetText(snippet)`, `new URL(result.href, input.config.url).toString()`); zero results → `No results for "astrolabe".`
details: { query, results }   // the rows unchanged (no topic)
```
`dispatch_issue` case: forward `force` when supplied; wrap `client.issue(...)` in `try/catch`; on `DispatchServiceError` with `code === "POSSIBLE_DUPLICATE"` return
```
text: `Not created: "<title>" looks like a duplicate.\n<KEY> [status] <title> → <absolute href>\n…\nReference the existing issue, or call dispatch_issue again with force: true after reading it.`
details: { duplicates: error.candidates }
```
(any other error rethrows).

- [ ] **Step 1: Failing tests.**
  - `__tests__/dispatch-http.test.ts`: `test("search encodes q, project, and limit and returns the response body")` — fake fetch records the URL; expect `/api/v1/search?q=astrolabe+sextant&project=LEGION&limit=5` (URLSearchParams encoding) with the bearer header.
  - `__tests__/dispatch-execute.test.ts` (fixture pattern: `response()`, `config`, `fetchImpl` recording requests):
    - `test("dispatch_search needs no issue and renders grouped results with absolute links")` — args `{ query: "astrolabe" }`, no `LEGION_ISSUE` in env, fake `/api/v1/search` → two results (document with `<mark>`, comment); expect exactly one request (no `/issues/resolve`, no issue creation), `result.text` contains `LEGION-2 [triage] document spec.md — …the **astrolabe** measures…` and `http://dispatch.test/issues/LEGION-2/spec?q=astrolabe`; `result.details` equals `{ query: "astrolabe", results }`; `dispatchSubscriptionTopic(result.details) === null`.
    - `test("dispatch_search rejects a one-character query before any request")` — `executeDispatchTool({ tool: "dispatch_search", args: { query: "a" } … })` rejects (Zod) with zero requests.
    - `test("dispatch_issue returns the duplicate candidates instead of creating")` — fake `POST /api/v1/issues` → 409 `{ error, code: "POSSIBLE_DUPLICATE", candidates: [{ key: "LEGION-12", title: "Global search across issues and documents", status: "triage", snippet: "<mark>Global</mark> <mark>search</mark> …", shared_terms: 4, href: "/issues/LEGION-12" }] }`; expect the tool resolves (does not throw), `text` contains `LEGION-12 [triage]` and `force: true`, `details.duplicates[0].key === "LEGION-12"`.
    - `test("dispatch_issue forwards force")` — args `{ …, force: true }` → the POST body has `force: true` → 201 → `Created …`.
  - `packages/pi-envoy/extensions/envoy.test.ts`: `"declares all ten native Dispatch tools"` → `"declares all eleven native Dispatch tools"`, `toHaveLength(11)`, `toContain("dispatch_search")`; add `test("executes dispatch_search without an issue and returns rows in details")` mirroring the `dispatch_ask` fixture (fake fetch answers `/api/v1/search`; expect `requests[0].url.pathname === "/api/v1/search"`, `searchParams.get("q") === "astrolabe"`, bearer header, `result.details.results` length 1, no `topic`).
  - `packages/claude-envoy-bridge/tests/envoy-mcp-server.test.ts`: the tool-name list gains `dispatch_search`; the required-fields map gains `dispatch_search: ["query"]`.
  - `packages/envoy-plugin/src/__tests__/dispatch-tools.test.ts`: the table gains `["dispatch_search", ["query"]]`.
- [ ] **Step 2: Run** — `cd packages/envoy-client && bun run test`; `cd packages/pi-envoy && bun run test`; `cd packages/claude-envoy-bridge && bun test`; `cd packages/envoy-plugin && bun test`. Expected: failures on the new cases (unknown tool / missing method / count 10).
- [ ] **Step 3: Implement** `search()`, `issueFreeTools`, the two executor cases, and the roster-test edits.
- [ ] **Step 4: Docs and skill.**
  - Every list of "the ten native Dispatch tools" gains `dispatch_search` and says eleven: `packages/contracts/AGENTS.md` (lines "source of the ten native Dispatch tools"), `packages/envoy-client/README.md`, `packages/pi-envoy/AGENTS.md` + `README.md`, `packages/envoy-plugin/AGENTS.md` + `README.md`, `packages/claude-envoy-bridge/README.md`.
  - `skills/dispatch/SKILL.md`: a new section directly after `## Your issue` and before `## Asking`:
    ````markdown
    ## Search first

    Before you create an issue or start a design document, search:
    ```ts
    dispatch_search({ query, project?, limit? })
    ```
    It returns every issue, document, comment, ask, and message that contains the words, with the
    issue key and a link. Cite the hit you build on (`dispatch://KEY` or the document reference), or
    state "no prior issue" in the spec. Websearch syntax applies: `"merge queue"`, `-daemon`, `OR`.

    `dispatch_issue` refuses a title that near-duplicates an issue in the same project and returns
    the candidates (`POSSIBLE_DUPLICATE`). Read them; reference the existing issue, or repeat the
    call with `force: true` when it is genuinely new work.
    ````
    and, in the `dispatch_issue` paragraph, `dispatch_issue({ project, title, parent?, external?, spec?, force? })`. Read the section back against Tasks 3–4's behaviour (the 409 shape, the `force` semantics), not against the diff.
- [ ] **Step 5: Run** every package's `bun run lint && bun run typecheck && bun run test` (envoy-client, pi-envoy, envoy-plugin, claude-envoy-bridge, contracts). Expected: green. Real surface: with the Task 2 server running and `DISPATCH_URL=http://127.0.0.1:8766 DISPATCH_TOKEN=local-agent-token`, start `omp` inside this worktree (the root `package.json` `omp.extensions` entry loads `packages/pi-envoy/extensions/envoy.ts` from source) and call `dispatch_search` with `{"query":"astrolabe"}` and `dispatch_issue` with a title that duplicates an existing `LOCAL` issue; paste both tool results into the PR.
- [ ] **Step 6: Commit** — `jj describe -m "feat(envoy-client): dispatch_search tool and duplicate-aware dispatch_issue; skill says search first" && jj new`

---

### Task 5: SPA — palette, rail control, shortcut, `?q=` highlight

**Files:**
- Create: `packages/dispatch/web/src/features/search/search-model.ts`, `search-model.test.ts`, `SearchPalette.tsx`, `SearchPalette.test.tsx`, `SearchButton.tsx`, `useSearchShortcut.ts`
- Modify: `packages/dispatch/web/src/api/client.ts`, `__tests__/client.test.ts`, `api/types.ts`, `theme/classes.ts`, `features/doc/DocView.tsx`, `DocView.test.tsx`, `features/doc/DocEditor.tsx`, `features/issue/IssuePage.tsx`, `app.tsx`, `packages/dispatch/AGENTS.md`

**Interfaces (produces):**
```ts
// api/client.ts
search(query: string, options: { project?: string; limit?: number } = {}): Promise<SearchResponse>  // pathWithQuery("/api/v1/search", { q: query, ...options })
// api/types.ts re-exports SearchResult, SearchResultKind, SearchResponse, SearchIssueRef, SearchArtifactRef, DuplicateCandidate

// theme/classes.ts
export const SEARCH_HIT_BG = pair(P.AMBER_100, P.AMBER_950);
export const SEARCH_HIT_TEXT = pair(P.AMBER_950, P.AMBER_100);
export const searchHitBg = "bg-amber-100 dark:bg-amber-950";
export const searchHitText = "text-amber-950 dark:text-amber-100";
registerText("search hit text", SEARCH_HIT_TEXT, SEARCH_HIT_BG);
export const kbdHint = `${surfaceMutedBg} ${textMutedOnSurfaceMuted}`;   // the ⌘K chip

// features/search/search-model.ts
export interface ResultGroup { readonly issue: SearchIssueRef; readonly results: SearchResult[]; }
export function groupResults(results: readonly SearchResult[]): ResultGroup[];   // groups keep first-appearance order (= best rank); results keep server order
export function firstHighlightTerm(query: string): string | undefined;           // Decision 3 tokenisation; undefined when no positive term
export function stepActive(index: number, delta: 1 | -1, count: number): number; // wraps; 0 when count is 0
export function kindLabel(kind: SearchResultKind): "issue" | "doc" | "comment" | "ask" | "message";
export function optionId(result: SearchResult): string;                           // `search-option-${kind}-${id}`

// features/search/SearchPalette.tsx
export function SearchPalette({ open, onClose }: { open: boolean; onClose: () => void }): ReactNode;
// features/search/SearchButton.tsx
export function SearchButton({ onOpen }: { onOpen: () => void }): ReactNode;  // rail control: <button aria-keyshortcuts="Control+K Meta+K"> "Search" + kbd hint
// features/search/useSearchShortcut.ts
export function useSearchShortcut(toggle: () => void): void;   // window keydown: (ctrlKey || metaKey) && key === "k" → preventDefault + toggle, unless target is textarea/contenteditable

// features/doc/DocView.tsx
interface DocViewProps { highlight?: DocViewHighlight; highlightTerm?: string; markdown: string; onSelectionChange?: … }
```
Palette behaviour (Decision 8). Markup: `<div role="dialog" aria-modal="true" aria-label="Search">` (fixed, top-centred on desktop, full-width sheet under the header on compact), backdrop `backdrop50`, panel `card` + `borderDefault` + `shadow-2xl`, input `inputClasses(true)` with `role="combobox"`, `aria-expanded`, `aria-controls="search-results"`, `aria-activedescendant`; states: `q.trim().length < 2` → hint `Type at least 2 characters`; pending → `Searching…`; error → `<QueryError message="Search failed." onRetry>`; empty → `No results for "<q>"`; results → groups. Group header: `<li role="presentation">` with key (`font-medium`), title (`textSecondaryOnSurface`), status badge; `status === "done"` → the whole group `textMutedOnSurface`. Option row: `<li role="option" id aria-selected className="min-h-11 …">` with kind badge (`badgeLow`), artifact name when present, snippet from `snippetSegments` (marks → `<mark className={`${searchHitBg} ${searchHitText} rounded px-0.5`}>`), active row `selectedCardBg` + `selectedCardBorder`. `Enter`/click → `navigate(result.href)` then `onClose()`. `useDialog` provides focus trap, Escape, focus restore, scroll lock. The palette lives in `AppShell` (`const [searchOpen, setSearchOpen] = useState(false)`, `useSearchShortcut(() => setSearchOpen((open) => !open))`), rendered after `<Margin />`; `NavigationContents` gains `onSearch: () => void` and renders `<SearchButton onOpen={() => { onClose(); onSearch(); }} />` directly under the `Dispatch` title row.

`DocView` highlight: a second effect keyed on `[markdown, highlightTerm]`: remove any `mark[data-dispatch-search-hit]` (unwrap), then when `highlightTerm` is set walk `article` text nodes (`document.createTreeWalker(root, NodeFilter.SHOW_TEXT)`), find the first node whose `toLowerCase()` contains the term, wrap the range in `<mark class="dispatch-anchor-history" data-dispatch-search-hit>` (`range.surroundContents`), `mark.scrollIntoView({ block: "center" })`; not found → `setSearchTermMissing(true)` and render `<p role="status">"<term>" is not in this version.</p>` in the same slot as the existing "Text changed." notice. `IssuePage` reads `new URLSearchParams(search).get("q")` → `highlightTerm = firstHighlightTerm(q)` → both `ArtifactDocument` usages → `DocEditor` (`highlightTerm` prop) → `DocView` (live and version views).

- [ ] **Step 1: Failing tests.**
  - `search-model.test.ts`: `groupResults` keeps first-appearance order and per-group server order; `firstHighlightTerm('"merge queue" -daemon') === "merge"`, `firstHighlightTerm("-daemon OR astrolabe") === "astrolabe"`, `firstHighlightTerm("-only") === undefined`; `stepActive(0, -1, 3) === 2`, `stepActive(2, 1, 3) === 0`, `stepActive(0, 1, 0) === 0`; `kindLabel("document") === "doc"`.
  - `SearchPalette.test.tsx` (happy-dom, `@testing-library/react`, `MemoryRouter`, a `QueryClient`, `api.search` replaced via `spyOn`): `renders grouped results with marked snippets and dims a done issue` (two results in LEGION-2 (`done`) and one in LEGION-3; expect two `role="presentation"` headers, three options, `mark` text `astrolabe`, LEGION-2 header has `text-slate-500` is **not** asserted — assert `aria-label`/`data-status="done"` on the group instead); `does not query below two characters` (type `a` → no `api.search` call, hint visible); `arrow keys move aria-activedescendant and Enter navigates to the href` (a `Routes` probe renders the current pathname + search: expect `/issues/LEGION-2/spec?q=astrolabe`); `Escape calls onClose`; `shows QueryError with Retry when the request fails`.
  - `__tests__/client.test.ts`: `search` requests `/api/v1/search?q=astrolabe&project=LEGION&limit=5` (existing fetch-recording pattern).
  - `DocView.test.tsx`: `highlights the first occurrence of highlightTerm and scrolls to it` (markdown `"Alpha\n\nThe astrolabe measures. Another astrolabe."`, `highlightTerm="astrolabe"` → exactly one `mark[data-dispatch-search-hit]` with text `astrolabe`, the `article` still contains two occurrences of `astrolabe`, `Element.prototype.scrollIntoView` spy called once); `reports a term that is not in the document` (`highlightTerm="sextant"` → status text `"sextant" is not in this version.`, no mark); `re-highlights when the markdown changes` (rerender with new markdown → one mark again).
  - `theme`: no new test — `palette.test.ts` picks up the registered pair; `no-raw-colors.test.ts` scans the new files.
- [ ] **Step 2: Run** `cd packages/dispatch && bun run typecheck; bun run test`. Expected: type errors (`api.search`, `highlightTerm`), failing new tests.
- [ ] **Step 3: Implement** per Interfaces; `packages/dispatch/AGENTS.md` Layout gains `web/src/features/search/` (palette, rail control, shortcut, `?q=` highlight contract, and that snippets render through `snippetSegments`, never `innerHTML`).
- [ ] **Step 4: Run** `cd packages/dispatch && bun run lint && bun run typecheck && bun run test`. Expected: green, including `theme/*.test.ts`. Real surface: `bun run build:web`, run the Task 2 server with `DISPATCH_WEB_DIST=../dispatch/web/dist`, open `http://127.0.0.1:8766/` in a browser (header identity via a `X-Dispatch-User` header extension, or the e2e's `asUser` context), press `Ctrl+K`, type a word from a `LOCAL` document, `Enter`, see the mark scrolled into view; check both colour schemes (`prefers-color-scheme` emulation in devtools).
- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch-web): Ctrl/Cmd+K search palette, rail Search control, and ?q= document highlight" && jj new`

---

### Task 6: e2e — `search.e2e.ts` (chromium + iphone)

**Files:**
- Create: `packages/dispatch/e2e/search.e2e.ts`

Helpers already available: `createProject`, `createIssue` (with `spec`), `createComment`, `createMessage` (`e2e/api.ts`); `resetDatabase` (`e2e/seed.ts`); `asUser` (`e2e/users.ts`). Seed for every scenario: project `CORE`; `CORE-1` "Navigation instruments" with spec `"# Instruments\n\nThe astrolabe measures altitude. Every astrolabe is brass.\n"` (two mentions, so the document outranks the single-mention comment and is the first option); `CORE-2` "Other work" with a comment `"Move the astrolabe diagram."`; `CORE-3` "Unrelated" with message `"No instruments here."`. No raw colour utilities in locators (`no-raw-colors.test.ts` scans `e2e/`).

- [ ] **Step 1: Write the scenarios (they fail until Tasks 2 and 5 are in the build):**
  - `test("Ctrl+K opens the palette, results group by issue, Enter opens the document at the highlighted term")` — desktop only (`test.skip(testInfo.project.name === "iphone")`): `page.goto("/")`, `page.keyboard.press("Control+k")`, `getByRole("combobox", { name: /search/i }).fill("astrolabe")`, expect `getByRole("option")` count 2, first option text matches `/doc.*spec\.md/` and contains a `mark` with `astrolabe`; `ArrowDown` sets `aria-activedescendant` to the second option's id; `ArrowUp` back; `Enter` → `expect(page).toHaveURL(/\/issues\/CORE-1\/spec\?q=astrolabe$/)`; `expect(page.locator("mark[data-dispatch-search-hit]")).toHaveText("astrolabe")` and `toBeInViewport()`; the palette is gone (`getByRole("dialog", { name: "Search" })` hidden).
  - `test("the rail Search control opens the palette and Escape returns focus to it")` — both projects: on iphone `getByRole("button", { name: "Open navigation" })` first; click `getByRole("button", { name: /search/i })`; combobox focused; type `astrolabe`; `Escape` → dialog hidden; on desktop `expect(getByRole("button", { name: /search/i })).toBeFocused()`.
  - `test("phone: result rows are at least 44px and tapping a comment result lands on the comment")` — iphone only: open via drawer, fill `astrolabe`, for each option `expect((await option.boundingBox())!.height).toBeGreaterThanOrEqual(44)`; tap the comment option → `toHaveURL(/\/issues\/CORE-2\/comments\/[0-9a-f-]+$/)`; the margin sheet shows the comment text.
  - `test("no results and short queries are stated, not silent")` — both projects: fill `a` → `Type at least 2 characters`; fill `zzqqxx` → `No results for "zzqqxx"`.
- [ ] **Step 2: Run** `cd packages/dispatch && bun run e2e -- search.e2e.ts` (needs the dev Postgres; `run-server.sh` boots the Go server with `DISPATCH_TEST_HOOKS=1`). Expected: PASS on `chromium` and `iphone`; keep `test-results/` screenshots of the palette (both viewports) for the PR.
- [ ] **Step 3: Commit** — `jj describe -m "test(dispatch-e2e): global search palette, rail control, phone rows, document highlight" && jj new`

---

### Task 7: latency fixture and server docs

**Files:**
- Create: `packages/envoy/scripts/restore-dispatch-dump.sh`, `packages/envoy/internal/dispatch/api/search_bench_test.go`
- Modify: `packages/envoy/cmd/dispatch/README.md`, `packages/envoy/cmd/dispatch/AGENTS.md`

`restore-dispatch-dump.sh <dump-file> [database-name]` (default name `dispatch_copy_<basename>` with every character outside `[a-z0-9]` replaced by `_`; `set -euo pipefail`; requires `psql`/`pg_restore`; admin URL from `DISPATCH_ADMIN_URL` default `postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable`): `drop database if exists … with (force)`, `create database …`, `pg_restore --no-owner --no-privileges -d <url>`, then prints `DATABASE_URL=postgres://…/<name>?sslmode=disable`. (The restored copy is at the dump's schema version; the server or the test applies later migrations on first use.) PR 3's `check-documents` preflight uses the same script for its production-copy run.

`search_bench_test.go`:
```go
// TestSearchLatencyOnCorpus measures GET /api/v1/search against a real corpus copy
// (scripts/restore-dispatch-dump.sh) and enforces the p95 < 100 ms bound from the spec.
// It skips unless DISPATCH_BENCH_DATABASE_URL is set; CI never runs it.
func TestSearchLatencyOnCorpus(t *testing.T) {
	databaseURL := os.Getenv("DISPATCH_BENCH_DATABASE_URL")
	if databaseURL == "" { t.Skip("DISPATCH_BENCH_DATABASE_URL must point at a restored corpus copy") }
	database, err := store.Open(ctx, databaseURL) … database.Migrate(ctx) … handler := handler over database (NewDeps as in newTestHandlerWithStore, docs service with Settle: time.Hour)
	terms := []string{"dispatch", "search", "anchor", "astrolabe", "\"merge queue\" -daemon", "legion", "proof", "document", "smoke", "token"}
	// warm once per term, then 10 rounds × 10 terms = 100 requests through httptest; record durations; sort; p50, p95, max
	t.Logf("search latency over %d requests: p50=%v p95=%v max=%v", …)
	if p95 > 100*time.Millisecond { t.Fatalf("p95 %v exceeds 100ms", p95) }
}
```

- [ ] **Step 1: Run the fixture on the live copy:** `bash packages/envoy/scripts/restore-dispatch-dump.sh ~/tmp/dispatch-preflight/live-20260910T170821Z.dump dispatch_search_bench` → prints the URL; `cd packages/envoy && DISPATCH_BENCH_DATABASE_URL=<url> go test ./internal/dispatch/api/ -run TestSearchLatencyOnCorpus -count=1 -v`. Expected: `PASS` with a logged p95 well under 100 ms (the probe's headline path measured 33 ms for 20 documents). Paste the log line into the PR. Drop the copy afterwards: `psql <admin url> -c 'drop database dispatch_search_bench with (force)'`.
- [ ] **Step 2: Docs.**
  - `packages/envoy/cmd/dispatch/README.md` § Routes: a row `| \`/api/v1/search?q=&project=&limit=\` | GET | cookie, trusted header, or bearer | Full-text search over issue titles, latest document text, comments, asks, and messages; ranked results with \`<mark>\` snippets and SPA \`href\`s; \`limit\` 1–50 (default 20). \`400 INVALID_QUERY\` under 2 characters or stop words only; \`400 INVALID_LIMIT\`. |`; the `POST /api/v1/issues` row gains `Refuses a title that near-duplicates an issue in the project with \`409 POSSIBLE_DUPLICATE\` and candidates unless \`force\` is true; external references skip the check.`; a `## Search` section under `## Running locally` describing the generated columns, the 2 s settle lag for live text, the snippet contract (escaped text, `<mark>` only), the duplicate rule in one sentence, and how to run the latency fixture.
  - `packages/envoy/cmd/dispatch/AGENTS.md` § Startup and persistence: one sentence that migration 0010 adds generated `search` columns nobody writes; § Routes: the same two rows in that table's shape.
  Read both back against Tasks 2–3's behaviour.
- [ ] **Step 3: Commit** — `jj describe -m "test(dispatch): corpus latency fixture for search; docs for the search route and duplicate gate" && jj new`

---

### Task 8: full verification and PR

- [ ] **Step 1: Rebase check** — `jj git fetch && jj rebase -d main@origin`; `ls packages/envoy/internal/dispatch/store/migrations` (renumber `0010_search.up.sql` if taken — see Global Constraints); if #887 landed, re-read `skills/dispatch/SKILL.md` (both PRs touch it), `api/server.go` `Register`, `api/issues.go` `createIssue` (PR 3 reorders `SeedText`; the gate sits above `s.begin` and is untouched), and `model/model.go`.
- [ ] **Step 2: Full verification** (paste outputs in the PR):
  - `cd packages/envoy && gofmt -l . && go vet ./... && DISPATCH_TEST_DATABASE_URL=<url> sh -c 'go list ./... | grep -v /integration | xargs go test -count=1'`
  - `bun install --frozen-lockfile` (repo root)
  - `cd packages/contracts && bun run lint && bun run typecheck && bun run test && bun run build`; the same `lint/typecheck/test` in `packages/envoy-client`, `packages/pi-envoy`, `packages/envoy-plugin`, `packages/claude-envoy-bridge`
  - `cd packages/dispatch && bun run lint && bun run typecheck && bun run test && bun run e2e`
  - Latency fixture (Task 7 Step 1) on the live copy.
  - Real surfaces: the Task 2 curl pair, the Task 4 OMP tool results, the Task 5 browser walk-through (both colour schemes), and a phone-width check of the palette from the drawer.
- [ ] **Step 3: PR** — bookmark `dispatch/search`; `jj git push`; title `feat(dispatch): global search — palette, GET /api/v1/search, dispatch_search, duplicate-issue gate`; body: the acceptance checklist below with evidence per line, the spec path, the migration-number note, and the "PR 4 re-implements the `?q=` highlight in the Proof editor" hand-off; register with the merge queue. Thermo pair applies (production Go).

---

## Acceptance pass (surfaces, drivers, tooling)

| Deliverable | Surface a human/operator touches | What drives it today | Gap closed in this plan |
|---|---|---|---|
| Palette, rail control, grouped results, keyboard, phone rows | Browser at `/` signed in (header identity as `run-server.sh` configures it), `Ctrl/Cmd+K`, drawer `Search` on the iPhone project | Playwright e2e against the real Go server + Postgres (`e2e/playwright.config.ts`, both projects) | `search.e2e.ts` (Task 6); unit tests for the model/palette/DocView (Task 5) |
| Document highlight from a result | `/issues/KEY/spec?q=<term>` in the browser | nothing drove `?q=` (new contract) | `DocView.test.tsx` + the e2e's `mark[data-dispatch-search-hit]` assertion; PR 4 keeps the URL contract |
| `GET /api/v1/search` | `curl` with `X-Dispatch-User` or the agent bearer against the running server | Go API tests against Postgres (`api/*_test.go` harness) | `api/search_test.go` (Task 2); the curl smoke is pasted in the PR |
| p95 < 100 ms on the live corpus | the same route against a restored copy of production | nothing (no restore fixture, no latency measurement) | `scripts/restore-dispatch-dump.sh` + `TestSearchLatencyOnCorpus` (Task 7) — reusable for PR 3's `check-documents` preflight too |
| `dispatch_search` for agents | an OMP session with pi-envoy loaded calling the tool against a live server | envoy-client executor tests (fetch-mocked); pi-envoy fixture executes a tool through the registered `execute` | new executor + fixture tests (Task 4); the real call from `omp` in the worktree (the root `package.json` `omp.extensions` entry loads the local extension) is pasted in the PR |
| Duplicate gate | `dispatch_issue` from an OMP session; `curl -X POST /api/v1/issues` | Go API tests; executor tests | `api/duplicates_test.go` (Task 3), executor 409 rendering test (Task 4), the OMP call with a duplicate title in the PR |
| Skill rule "search first" | the text an agent reads at session start | nothing programmatic | read against Tasks 3–4 behaviour (Task 4 Step 4); no grep-for-wording test |

Cheapest real substitute for the restricted path (production): the live dump copy through `restore-dispatch-dump.sh` — plain commands an operator types; the browser surface uses the same header identity the e2e harness uses, never a minted cookie.

## PR body checklist (spec § Acceptance)

- [ ] **1** — palette → document at the term: `search.e2e.ts` first scenario; screenshot.
- [ ] **2** — phone drawer → palette, rows ≥ 44 px, tap lands on the comment: `search.e2e.ts` iphone scenarios; screenshot.
- [ ] **3** — rail control on both layouts, `Esc` restores focus: `search.e2e.ts` second scenario.
- [ ] **4** — API shape with cookie/header and bearer: `TestSearchFindsEveryKindWithSnippetsAndHrefs`, `TestSearchAuthentication`; curl output.
- [ ] **5** — `dispatch_search({query:"astrolabe"})` returns the hit with key + absolute link: executor test, pi-envoy fixture, real `omp` call output.
- [ ] **6** — near-duplicate `dispatch_issue` → candidates, `force: true` → created: `TestCreateIssueRejectsNearDuplicateTitleWithCandidates`, `TestCreateIssueForceBypassesTheGate`, executor tests, real `omp` call output.
- [ ] **7** — p95 < 100 ms: `TestSearchLatencyOnCorpus` log line from the live copy.
- [ ] **8** — `INVALID_QUERY` / `INVALID_LIMIT`: `TestSearchRejectsInvalidQueries`, e2e "short queries" scenario.
- [ ] **9** — skill section present and read against behaviour.
- [ ] Migration number confirmed against `main@origin` at the final rebase.

## Self-review

- **Spec coverage:** Decision 1 left to Sami with a one-line switch (Decision 6 here); Acceptance 1–9 each mapped above; Requirements — corpus (T2 migration + query), Postgres-only (no new service anywhere), `href` deep links (T2 `searchHref`, T5 navigation), settle lag stated (T7 docs); Design — every file named in File Structure; Errors — `INVALID_QUERY`/`INVALID_LIMIT`/401/500/409/Zod/`?q=` status line all have a test or a scenario; Rejected — nothing re-proposed.
- **Placeholders:** none — every task names files, tests, commands, expected output.
- **Type consistency:** `SearchResult`/`DuplicateCandidate` JSON field names identical in Go (`model.go`, Task 2) and TS (`dispatch-api.ts`, Task 1): `kind, issue{key,title,status}, artifact{slug,name}, id, snippet, rank, href, took_ms`, `key,title,status,snippet,shared_terms,href`; `markStart`/`markEnd` defined once in `search.go` and reused by `duplicates.go`; `firstTerm` (Go) and `firstHighlightTerm` (TS) implement the same tokenisation (Decision 3), each with its own table test; `snippetSegments` is the single snippet parser for SPA and tool.
- **1:1 file ownership:** verified against File Structure — `model.go` (T2 only, including `DuplicateCandidate`), `server.go` (T2), `issues.go` (T3), `SKILL.md` (T4), `classes.ts`/`app.tsx`/`DocView.tsx` (T5), server README/AGENTS (T7), `packages/dispatch/AGENTS.md` (T5).
- **Judgment calls the executor must not "fix":** the duplicate rule and its exemptions (Decision 6), title weight A, the 4 000/1 500 window, sentinels + escape-then-replace, `limit` bounds, `document` hrefs pointing at `/spec` for the primary artifact, the 409 returned as a tool *result*, no GIN on `artifact_versions`, no `innerHTML`.

## Hardening ledger

(empty — filled by the implementer/reviewer as hardening items are found and closed)

package api

import (
	"cmp"
	"context"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// relatedTermLimit is the most lexemes the related-item check's keyword query ORs together
	// (INFRA-568): More Like This keeps a text's 25 most distinctive terms, and a check's text can
	// be a 10,000-word spec.
	relatedTermLimit = 25
	// relatedMatchBudget bounds the chosen terms' summed df, and with it how many rows their OR
	// query can match and so its time: measured on production on 27 October 2026, 2,033 matches
	// answered in 92 ms and 4,759 in 170 ms.
	relatedMatchBudget = 3000
	// relatedProbeLimit is the most lexemes absent from the statistics the picker counts live, by
	// their frequency in the text. Each costs a bounded exists per kind; counting every lexeme of a
	// long spec that way would be thousands of probes.
	relatedProbeLimit = 50
	// relatedCommonFloor is the df at or under which a lexeme is never too common to pick, so a
	// small project, where a tenth of its items is a handful, still has terms.
	relatedCommonFloor = 10
)

// SearchTermStatsInterval is how often RunSearchTermStatsRefresh rebuilds every project's term
// statistics. A lexeme written since the last pass is probed live (relatedProbeLimit), so a stale
// table costs probes, not results.
const SearchTermStatsInterval = 10 * time.Minute

// searchTermStatsLease names the lease one refresh pass runs under, as the suggestion-outcome
// sweep's does: two processes rebuilding the same rows would be the same work twice.
const searchTermStatsLease = "search-term-stats"

// relatedQOpener is the related-item check's q in place of search's (searchQOpener): the chosen
// lexemes as a tsquery already built (relatedTerms.TSQuery), never re-stemmed through
// websearch_to_tsquery, and no own_key, since no issue key is the whole text. Prefixed to
// searchQueryBody or searchQueryMeaningBody, the check ranks exactly as search does.
const relatedQOpener = `
with q as (select $1::tsquery as tsq, $4::text as term, ''::text as own_key),
`

// searchTermItemsSQL selects the search vector of every searchable item of the project
// {project} stands for, one row per item, each kind scoped exactly as searchKeywordLegsSQL scopes
// it: an issue, the latest version of each doc artifact (search reads no older one), a comment
// or an ask by its issue's project or else its document's, a message by its issue's.
const searchTermItemsSQL = `
select i.search from issues i where i.project_key = {project}
union all
select v.search from artifacts a
  join lateral (select v.search from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
 where a.kind = 'doc' and a.project_key = {project}
union all
select c.search from comments c
  left join issues i on i.key = c.issue_key
  left join artifacts a on a.id = c.artifact_id
 where coalesce(i.project_key, a.project_key) = {project}
union all
select k.search from asks k
  left join issues i on i.key = k.issue_key
  left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
 where coalesce(i.project_key, a.project_key) = {project}
union all
select m.search from messages m join issues i on i.key = m.issue_key where i.project_key = {project}`

// searchTermItemsTSStat is searchTermItemsSQL as the format() string ts_stat's query is built
// from in SQL: %1$L quotes the project key as a literal, so the key never enters SQL text by Go
// concatenation.
var searchTermItemsTSStat = strings.ReplaceAll(searchTermItemsSQL, "{project}", "%1$L")

// searchTermTotalsClaim makes the project's totals row exist, so the select after it can lock it:
// that row lock serialises two refreshes of one project (a check's and the leased pass), each of
// which deletes and reinserts the same keys. Locking the projects row instead would hold every
// event append of the project, which takes that row, for the whole rebuild. The placeholder is
// never seen: it commits only with the counts the rebuild writes into it.
const (
	searchTermTotalsClaim = `insert into search_term_totals (project_key, items, refreshed_at) values ($1, 0, now())
on conflict (project_key) do nothing`
	searchTermTotalsLock = `select 1 from search_term_totals where project_key = $1 for update`
	searchTermStatsClear = `delete from search_term_stats where project_key = $1`
)

// searchTermStatsRebuild writes the project's lexemes with the number of items holding each
// (ts_stat's ndoc) and its item count, in one statement so both read the same moment. refreshed_at
// is that statement's start, which is what the picker compares item creation times with
// (relatedTermsSQL).
//
// Parameters: $1 the project key, $2 searchTermItemsTSStat.
var searchTermStatsRebuild = `
with stats as (
  insert into search_term_stats (project_key, lexeme, ndoc)
  select $1, word, ndoc from ts_stat(format($2::text, $1::text))
)
update search_term_totals
   set items = (select count(*) from (` + strings.ReplaceAll(searchTermItemsSQL, "{project}", "$1::text") + `
       ) items),
       refreshed_at = statement_timestamp()
 where project_key = $1
returning items`

// refreshSearchTermStats replaces the project's search_term_stats rows and its search_term_totals
// row in one transaction, and answers the project's item count (the picker's N).
func refreshSearchTermStats(ctx context.Context, pool *store.Pool, project string) (items int, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, statement := range []string{searchTermTotalsClaim, searchTermTotalsLock, searchTermStatsClear} {
		if _, err := tx.Exec(ctx, statement, project); err != nil {
			return 0, err
		}
	}
	if err := tx.QueryRow(ctx, searchTermStatsRebuild, project, searchTermItemsTSStat).Scan(&items); err != nil {
		return 0, err
	}
	return items, tx.Commit(ctx)
}

// refreshEverySearchTermStats is one pass of RunSearchTermStatsRefresh: every project, each in its
// own transaction, so one project's failure leaves the others' rows fresh.
func refreshEverySearchTermStats(ctx context.Context, pool *store.Pool) {
	started := time.Now()
	rows, err := pool.Query(ctx, `select key from projects order by key`)
	if err != nil {
		slog.Warn("dispatch: list the projects whose search term statistics to refresh", "error", err)
		return
	}
	projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		slog.Warn("dispatch: list the projects whose search term statistics to refresh", "error", err)
		return
	}
	refreshed, items := 0, 0
	for _, project := range projects {
		count, err := refreshSearchTermStats(ctx, pool, project)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("dispatch: refresh a project's search term statistics", "project", project, "error", err)
			continue
		}
		refreshed++
		items += count
	}
	slog.Info("dispatch: search term statistics refreshed",
		"projects", refreshed, "failed", len(projects)-refreshed, "items", items, "duration", time.Since(started).Round(time.Millisecond))
}

// RunSearchTermStatsRefresh rebuilds every project's term statistics, the related-item check's df
// and N (INFRA-568), at boot and then every interval, until ctx is done. Each pass runs under
// searchTermStatsLease for one interval, so one process refreshes per interval however many serve
// the database; leases is required, as it is of every leased loop.
func RunSearchTermStatsRefresh(ctx context.Context, pool *store.Pool, interval time.Duration, leases duty.Leases) {
	for {
		duty.Exclusive(ctx, leases, searchTermStatsLease, interval, func(ctx context.Context) {
			refreshEverySearchTermStats(ctx, pool)
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// ensureSearchTermStats starts a refresh of project's statistics in the background, unless one
// this process started is still running, and returns at once with a channel closed when that
// refresh ends. A check that finds a project with no totals row (one created since the last pass,
// or a server whose first pass has not reached it) calls it and picks in probe mode meanwhile;
// many checks of that project then cost one refresh, not one each. The refresh runs on the
// server's lifetime, bounded by one interval, by which the leased pass would have refreshed the
// project anyway.
func (s *server) ensureSearchTermStats(project string) <-chan struct{} {
	s.termStats.mu.Lock()
	defer s.termStats.mu.Unlock()
	if done, running := s.termStats.running[project]; running {
		return done
	}
	if s.termStats.running == nil {
		s.termStats.running = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	s.termStats.running[project] = done
	go func() {
		defer func() {
			s.termStats.mu.Lock()
			delete(s.termStats.running, project)
			s.termStats.mu.Unlock()
			close(done)
		}()
		ctx, cancel := context.WithTimeout(s.lifetime(), SearchTermStatsInterval)
		defer cancel()
		started := time.Now()
		items, err := refreshSearchTermStats(ctx, s.deps.Store.Pool, project)
		if err != nil {
			slog.Warn("dispatch: refresh the search term statistics of a project the related-item check found without them",
				"project", project, "error", err)
			return
		}
		slog.Info("dispatch: search term statistics refreshed for a project the related-item check found without them",
			"project", project, "items", items, "duration", time.Since(started).Round(time.Millisecond))
	}()
	return done
}

// searchTermRefreshes is the projects whose refresh ensureSearchTermStats has running, each with
// the channel closed when it ends. The zero value is ready.
type searchTermRefreshes struct {
	mu      sync.Mutex
	running map[string]chan struct{}
}

// relatedExclusion names the check's source owner, whose items are left out of df and out of the
// probes: an issue (a new issue, an ask's issue, exclude_issue) or an unlinked project document
// (an ask on it). Both empty excludes nothing.
type relatedExclusion struct {
	issueKey   string
	artifactID string
}

// relatedTerm is one lexeme the picker chose.
type relatedTerm struct {
	Lexeme string
	// Surface is the lexeme as the text spells it (the first whitespace-separated word whose own
	// vector holds the lexeme, a word that is only that lexeme first, its outer punctuation
	// trimmed), or empty where no single word of the text yields it.
	Surface string
	DF      int
	Score   float64
}

// relatedTerms is what pickRelatedTerms chose for a text.
type relatedTerms struct {
	Terms []relatedTerm
	// TSQuery is Terms as one tsquery text, each lexeme quoted by quote_literal and joined with
	// " | ", to be cast with ::tsquery (relatedQOpener); empty when Terms is.
	TSQuery string
	// StatsPresent is whether the project had a totals row. Without one every lexeme was probed
	// (probe mode) and a refresh was started.
	StatsPresent bool
	// Items is the project's item count at its last refresh (N); zero in probe mode.
	Items int
	// NoLexemes is a text that holds no lexeme at all: blank, or stopwords alone.
	NoLexemes bool
}

// relatedTermsSQL reads, for every lexeme of the text, its frequency there (tf), its df from the
// project's statistics, and its surface word, in one statement.
//
// The text is tokenized as a document version's vector is built (0087_search_vector_bound): ask
// blocks stripped, search_text, then search_vector, so its lexemes are the ones stored vectors
// hold. Each lexeme's tf is its position count.
//
// df is null for a lexeme the statistics do not hold, and for every lexeme when the project has
// no totals row; the caller probes those. Otherwise it is ndoc less the excluded owner's items
// the refresh counted: those holding the lexeme that existed at refreshed_at (created at or
// before it, the refresh statement's start), and of a document the latest version created by
// then, which is the version the refresh read. Issues, comments, asks and messages keep no
// history, so their current vector stands in for the one counted; a word one gained since is
// subtracted though it was not counted, which only lowers df (never below zero), and a source's
// word lowered to zero is the word only the source holds.
//
// Parameters: $1 project, $2 text, $3 excluded issue key (empty for none), $4 excluded document
// artifact id (null for none).
const relatedTermsSQL = `
with body as (
  select search_text(regexp_replace($2::text, '(?s):::ask\{.*?:::', '', 'g')) as text
),
text_lexemes as (
  select u.lexeme, coalesce(array_length(u.positions, 1), 1) as tf
    from body cross join lateral unnest(search_vector('', body.text)) u
),
totals as (
  select items, refreshed_at from search_term_totals where project_key = $1
),
own_items as (
  select i.search from issues i cross join totals t
   where i.key = $3 and i.project_key = $1 and i.created_at <= t.refreshed_at
  union all
  select v.search from artifacts a cross join totals t
    join lateral (select v.search from artifact_versions v
                  where v.artifact_id = a.id and v.created_at <= t.refreshed_at
                  order by v.number desc limit 1) v on true
   where a.kind = 'doc' and a.project_key = $1 and (a.issue_key = $3 or a.id = $4::uuid)
  union all
  select c.search from comments c cross join totals t
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
   where (c.issue_key = $3 or c.artifact_id = $4::uuid) and coalesce(i.project_key, a.project_key) = $1
     and c.created_at <= t.refreshed_at
  union all
  select k.search from asks k cross join totals t
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
   where (k.issue_key = $3 or coalesce(k.artifact_id, k.block_artifact_id) = $4::uuid)
     and coalesce(i.project_key, a.project_key) = $1 and k.created_at <= t.refreshed_at
  union all
  select m.search from messages m cross join totals t
    join issues i on i.key = m.issue_key
   where m.issue_key = $3 and i.project_key = $1 and m.created_at <= t.refreshed_at
),
own_counts as (
  select u.lexeme, count(*) as n
    from own_items o cross join lateral unnest(o.search) u
   group by u.lexeme
),
words as (
  select w.word, min(w.n) as n
    from body cross join lateral regexp_split_to_table(body.text, '\s+') with ordinality w(word, n)
   where w.word <> ''
   group by w.word
),
surfaces as (
  select distinct on (u.lexeme) u.lexeme, regexp_replace(w.word, '^[^[:alnum:]]+|[^[:alnum:]]+$', '', 'g') as word
    from words w
    cross join lateral (select to_tsvector('english', w.word) as vector) wv
    cross join lateral unnest(wv.vector) u
   order by u.lexeme, length(wv.vector) > 1, w.n
)
select l.lexeme, l.tf, quote_literal(l.lexeme),
       case when s.ndoc is not null then greatest(s.ndoc - coalesce(o.n, 0), 0) end,
       coalesce(sf.word, ''), t.items
  from text_lexemes l
  left join totals t on true
  left join search_term_stats s on t.items is not null and s.project_key = $1 and s.lexeme = l.lexeme
  left join own_counts o on o.lexeme = l.lexeme
  left join surfaces sf on sf.lexeme = l.lexeme`

// relatedProbeSQL answers which of the given lexemes some item of the project holds, leaving out
// the excluded owner's items. Issues, comments, asks and messages each probe their search index
// with an exists; documents have no index on their versions' vectors (0010_search), so they are
// read once for all the lexemes: each document's latest version, as search reads it, tested
// against the lexemes ORed, and only a version that matches is unnested.
//
// Parameters: $1 project, $2 lexemes, $3 excluded issue key (empty for none), $4 excluded document
// artifact id (null for none).
const relatedProbeSQL = `
with probe as (
  select l.lexeme, quote_literal(l.lexeme)::tsquery as tq from unnest($2::text[]) l(lexeme)
),
any_probe as (
  select string_agg(quote_literal(lexeme), ' | ')::tsquery as tq from probe
),
document_hits as (
  select distinct u.lexeme
    from artifacts a
    join lateral (select v.search from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
    cross join any_probe
    cross join lateral unnest(v.search) u
   where a.kind = 'doc' and a.project_key = $1
     and a.issue_key is distinct from $3 and a.id is distinct from $4::uuid
     and v.search @@ any_probe.tq and u.lexeme = any($2::text[])
)
select p.lexeme from probe p
 where exists (select 1 from issues i where i.search @@ p.tq and i.project_key = $1 and i.key <> $3)
    or exists (select 1 from document_hits d where d.lexeme = p.lexeme)
    or exists (select 1 from comments c
                 left join issues i on i.key = c.issue_key
                 left join artifacts a on a.id = c.artifact_id
                where c.search @@ p.tq and coalesce(i.project_key, a.project_key) = $1
                  and c.issue_key is distinct from $3 and c.artifact_id is distinct from $4::uuid)
    or exists (select 1 from asks k
                 left join issues i on i.key = k.issue_key
                 left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
                where k.search @@ p.tq and coalesce(i.project_key, a.project_key) = $1
                  and k.issue_key is distinct from $3
                  and coalesce(k.artifact_id, k.block_artifact_id) is distinct from $4::uuid)
    or exists (select 1 from messages m join issues i on i.key = m.issue_key
                where m.search @@ p.tq and i.project_key = $1 and m.issue_key <> $3)`

var (
	// relatedDigitsLexeme is a lexeme of digits and separators alone: a number, a version, a date
	// or a part of one.
	relatedDigitsLexeme = regexp.MustCompile(`^[0-9[:punct:]]+$`)
	// relatedHexLexeme is a hash, a uuid or a part of one: hex digits and separators with a run
	// of 12 or more hex digits.
	relatedHexLexeme = regexp.MustCompile(`^[0-9a-f[:punct:]]*[0-9a-f]{12,}[0-9a-f[:punct:]]*$`)
)

// relatedNoise reports a lexeme the picker never chooses: a number or a hash matches nothing a
// person would call related. A lexeme holding a backslash is left out too: quote_literal writes
// it as E'...', a form tsquery input does not read.
func relatedNoise(lexeme string) bool {
	return relatedDigitsLexeme.MatchString(lexeme) || relatedHexLexeme.MatchString(lexeme) ||
		strings.ContainsRune(lexeme, '\\')
}

// relatedCandidate is a lexeme of the text while the picker weighs it.
type relatedCandidate struct {
	relatedTerm
	tf     int
	quoted string
	known  bool
}

// pickRelatedTerms chooses the related-item check's keywords from text (INFRA-568's More Like
// This): each lexeme scored tf × idf, idf = ln(1 + (N − df + 0.5)/(df + 0.5)) (Lucene's BM25
// idf), where df is how many of the project's items hold it and N how many items the project
// has. It keeps at most relatedTermLimit lexemes by score, ties by lower df then lexeme, and drops
// a lexeme no other item holds (df 0), one in more than a tenth of the project (past
// relatedCommonFloor), and numbers and hashes (relatedNoise); it stops adding once the summed df
// would pass relatedMatchBudget. The excluded owner's items count toward no df.
//
// df comes from the project's statistics (refreshSearchTermStats). A lexeme they do not hold is
// probed live, the relatedProbeLimit most frequent in the text, and a hit counts as df 1. A
// project with no statistics starts their refresh (ensureSearchTermStats) and is picked in probe
// mode, every lexeme probed and N taken as 0.
func (s *server) pickRelatedTerms(ctx context.Context, q queryer, project, text string, exclude relatedExclusion) (relatedTerms, error) {
	var artifactID *string
	if exclude.artifactID != "" {
		artifactID = &exclude.artifactID
	}
	rows, err := q.Query(ctx, relatedTermsSQL, project, text, exclude.issueKey, artifactID)
	if err != nil {
		return relatedTerms{}, err
	}
	var candidates []relatedCandidate
	var items *int
	for rows.Next() {
		var candidate relatedCandidate
		var df *int
		if err := rows.Scan(&candidate.Lexeme, &candidate.tf, &candidate.quoted, &df, &candidate.Surface, &items); err != nil {
			rows.Close()
			return relatedTerms{}, err
		}
		if df != nil {
			candidate.DF, candidate.known = *df, true
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return relatedTerms{}, err
	}
	if len(candidates) == 0 {
		return relatedTerms{NoLexemes: true}, nil
	}
	picked := relatedTerms{StatsPresent: items != nil}
	if items != nil {
		picked.Items = *items
	} else {
		s.ensureSearchTermStats(project)
	}

	candidates = slices.DeleteFunc(candidates, func(candidate relatedCandidate) bool {
		return relatedNoise(candidate.Lexeme)
	})
	if err := probeRelatedCandidates(ctx, q, project, exclude.issueKey, artifactID, candidates); err != nil {
		return relatedTerms{}, err
	}

	commonAbove := max(float64(relatedCommonFloor), float64(picked.Items)/10)
	candidates = slices.DeleteFunc(candidates, func(candidate relatedCandidate) bool {
		return !candidate.known || candidate.DF == 0 || float64(candidate.DF) > commonAbove
	})
	for index := range candidates {
		candidate := &candidates[index]
		// N is floored at df: in probe mode N is 0, and an idf below zero would rank the words the
		// text repeats lowest.
		n := float64(max(picked.Items, candidate.DF))
		df := float64(candidate.DF)
		candidate.Score = float64(candidate.tf) * math.Log(1+(n-df+0.5)/(df+0.5))
	}
	slices.SortFunc(candidates, func(a, b relatedCandidate) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.DF, b.DF), strings.Compare(a.Lexeme, b.Lexeme))
	})

	quoted := make([]string, 0, relatedTermLimit)
	matches := 0
	for _, candidate := range candidates {
		if len(picked.Terms) == relatedTermLimit || matches+candidate.DF > relatedMatchBudget {
			break
		}
		matches += candidate.DF
		picked.Terms = append(picked.Terms, candidate.relatedTerm)
		quoted = append(quoted, candidate.quoted)
	}
	picked.TSQuery = strings.Join(quoted, " | ")
	return picked, nil
}

// probeRelatedCandidates counts live, as df 1 or 0, the relatedProbeLimit most frequent of the
// candidates the statistics do not hold, marking each probed one known; the rest stay unknown and
// are not picked.
func probeRelatedCandidates(
	ctx context.Context, q queryer, project, excludeIssue string, excludeArtifact *string, candidates []relatedCandidate,
) error {
	var unknown []*relatedCandidate
	for index := range candidates {
		if !candidates[index].known {
			unknown = append(unknown, &candidates[index])
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.SortFunc(unknown, func(a, b *relatedCandidate) int {
		return cmp.Or(cmp.Compare(b.tf, a.tf), strings.Compare(a.Lexeme, b.Lexeme))
	})
	unknown = unknown[:min(len(unknown), relatedProbeLimit)]
	lexemes := make([]string, len(unknown))
	byLexeme := make(map[string]*relatedCandidate, len(unknown))
	for index, candidate := range unknown {
		lexemes[index] = candidate.Lexeme
		byLexeme[candidate.Lexeme] = candidate
		candidate.known = true
	}
	rows, err := q.Query(ctx, relatedProbeSQL, project, lexemes, excludeIssue, excludeArtifact)
	if err != nil {
		return err
	}
	hits, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, lexeme := range hits {
		byLexeme[lexeme].DF = 1
	}
	return nil
}

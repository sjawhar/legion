package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
)

// buildTermStatistics runs one full term-statistics build for every project, the build the
// server runs itself where the schema carries it (buildEverySearchTermStats), so
// `search_term_totals` holds a row for each. The refresh builds a project with no totals row in
// full whatever else it is asked to do, so a measured pass on a database with no rows here would
// be measuring a first-use full build.
//
// The build is the one the database's own schema carries: the measurement protocol seeds a
// database migrated from main with --no-migrate and this tool, where the refresh of this release
// reads a search_term_totals column and a queue table that are not there.
func (r *seedRun) buildTermStatistics(ctx context.Context) error {
	if err := buildEverySearchTermStats(ctx, r.database.Pool); err != nil {
		return err
	}
	var built int
	if err := r.database.Pool.QueryRow(ctx, `select count(*) from search_term_totals`).Scan(&built); err != nil {
		return err
	}
	if built != len(r.plan.projects) {
		return fmt.Errorf("the term statistics were built for %d projects, not %d", built, len(r.plan.projects))
	}
	return nil
}

// reportTables are the tables the command prints a row count for, in the order it prints them.
var reportTables = []string{
	"projects", "repo_projects", "issues", "issue_links", "issue_components",
	"issue_component_members", "architecture_snapshots", "components", "component_depends",
	"artifacts", "artifact_versions", "document_blocks", "document_block_texts",
	"document_block_versions", "document_indexed_versions", "asks", "ask_followers", "comments",
	"comment_blocks", "comment_block_texts", "comment_block_state", "messages", "message_blocks",
	"message_block_state", "refs", "block_refs", "comment_block_refs", "message_block_refs",
	"events", "embeddings", "search_term_stats", "search_term_totals",
}

// report prints every row count, every distribution the spec states as one, the measurement
// subjects and the words, which is what a sibling measurement binds. It is the whole record of a
// run: nobody should have to read this command's code to use its database.
func (r *seedRun) report(ctx context.Context) error {
	fmt.Fprintln(r.out, "\nrow counts")
	counts, err := r.rowCounts(ctx)
	if err != nil {
		return err
	}
	for _, table := range reportTables {
		fmt.Fprintf(r.out, "  %-26s %10d\n", table, counts[table])
	}

	fmt.Fprintln(r.out, "\ndistributions")
	if err := r.reportDistributions(ctx); err != nil {
		return err
	}

	if err := r.reportWords(ctx); err != nil {
		return err
	}
	if err := r.reportSubjects(ctx); err != nil {
		return err
	}

	var size string
	if err := r.database.Pool.QueryRow(ctx,
		`select pg_size_pretty(pg_database_size(current_database()))`).Scan(&size); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "\ndatabase size %s, wall time %s\n", size, time.Since(r.started).Round(time.Second))
	return nil
}

func (r *seedRun) rowCounts(ctx context.Context) (map[string]int, error) {
	counts := make(map[string]int, len(reportTables))
	var parts []string
	for _, table := range reportTables {
		parts = append(parts, fmt.Sprintf("select %s as name, count(*) as rows from %s", quoteLiteral(table), table))
	}
	rows, err := r.database.Pool.Query(ctx, strings.Join(parts, " union all "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		counts[name] = count
	}
	return counts, rows.Err()
}

// reportDistributions prints every figure the spec states as a distribution rather than a row
// count, which is what the review's finding 10 asks for: a row count cannot show any of them.
func (r *seedRun) reportDistributions(ctx context.Context) error {
	type reading struct {
		label string
		query string
	}
	readings := []reading{
		{"issues per project", `select project_key || '=' || count(*) from issues group by project_key order by count(*) desc, project_key`},
		{"parent chain depth", `with recursive chain as (
			select key, 1 as depth from issues where parent_key is null
			union all
			select i.key, c.depth + 1 from issues i join chain c on i.parent_key = c.key)
			select 'depth ' || depth || '=' || count(*) from chain group by depth order by depth`},
		{"children per parent", `select 'max=' || max(children) || ' issues with children=' || count(*)
			from (select parent_key, count(*) as children from issues where parent_key is not null group by parent_key) p`},
		{"issues with 3 or more blockers", `select count(*)::text from (
			select issue_key from issue_links where kind = 'blocked_by' group by issue_key having count(*) >= 3) b`},
		{"documents with 28 versions", `select count(*)::text from (
			select artifact_id from artifact_versions group by artifact_id having count(*) = 28) v`},
		{"versions per document", `select 'min=' || min(versions) || ' max=' || max(versions) || ' mean=' || round(avg(versions), 2)
			from (select artifact_id, count(*) as versions from artifact_versions group by artifact_id) v`},
		{"latest version size bands", `with latest as (
			select distinct on (artifact_id) artifact_id, length(markdown) as bytes
			from artifact_versions order by artifact_id, number desc
		), banded as (
			select case when bytes >= 100*1024 then '100-660 KB'
			            when bytes >= 10*1024 then '10-20 KB'
			            when bytes >= 5*1024 then '5-10 KB'
			            else 'under 5 KB' end as band
			from latest
		)
			select band || '=' || count(*) from banded group by band order by band`},
		{"blocks of the latest versions", `select 'blocks=' || count(*) || ' of which tables=' ||
			count(*) filter (where type = 'table') ||
			' (a table is one block: its rows carry ids the renderer reports no offset for)'
			from document_blocks`},
		{"anchored comments and asks", `select 'comments=' || (select count(*) from comments where anchor is not null) ||
			' of ' || (select count(*) from comments) || ' asks=' || (select count(*) from asks where anchor is not null) ||
			' of ' || (select count(*) from asks) || ' on a table row=' ||
			(select count(*) from comments where anchor->>'block_id' = ` + quoteLiteral(r.tableRow) + `)`},
		{"open asks", `select count(*) || ' of ' || (select count(*) from asks) from asks where state = 'open'`},
		{"root messages", `select count(*) || ' of ' || (select count(*) from messages) from messages where in_reply_to is null`},
		{"events with a session actor", `select count(*) || ' of ' || (select count(*) from events) || ' over ' ||
			count(distinct actor->>'id') || ' sessions' from events where actor->>'kind' = 'session'`},
		{"rows with an embedding", `select kind || '=' || count(*) filter (where embedding is not null) || '/' || count(*)
			from embeddings group by kind order by kind`},
		{"issues with a repository of their own", `select count(*) || ' of ' || (select count(*) from issues) from issues where repo is not null`},
		{"graph edges by kind", `select kind || '->' || to_kind || '=' || rows from (
			select kind, to_kind, count(*) as rows from graph_edges group by 1, 2) e order by kind, to_kind`},
	}
	for _, item := range readings {
		rows, err := r.database.Pool.Query(ctx, item.query)
		if err != nil {
			return fmt.Errorf("%s: %w", item.label, err)
		}
		values, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("%s: %w", item.label, err)
		}
		fmt.Fprintf(r.out, "  %-32s %s\n", item.label, strings.Join(values, ", "))
	}
	fmt.Fprintf(r.out, "  %-32s %s\n", "versions written per band", bandReport(r.bands))
	return nil
}

// quoteLiteral writes a string as a SQL literal, for the one reading whose value is read off the
// rendered document rather than fixed in the source (the table-row block id).
func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func bandReport(bands map[string]int) string {
	names := make([]string, 0, len(bands))
	for name := range bands {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, bands[name]))
	}
	return strings.Join(parts, ", ")
}

// wordRanks are the vocabulary ranks the command measures to choose its rare, medium and common
// word. Word 1 is drawn most often and word 20,000 least, so the hit counts fall across this
// list and the three words are chosen from what the database actually holds.
var wordRanks = []int{1, 5, 20, 100, 400, 1_500, 4_000, 7_000, 10_000, 13_000, 16_000, 19_000, 20_000}

// reportWords measures how many documents each candidate word is in, at their latest version,
// and prints a rare word (fewer hits than api/search.go's per-arm limit, contracts.SearchKindDepth),
// a medium one and a common one. The counts are measured rather than assumed: the English
// stemmer, not the draw, decides what a tsvector finally holds.
func (r *seedRun) reportWords(ctx context.Context) error {
	words := make([]string, len(wordRanks))
	for index, rank := range wordRanks {
		words[index] = r.plan.vocabulary.wordAtRank(rank)
	}
	rows, err := r.database.Pool.Query(ctx, `
		with latest as (
			select distinct on (artifact_id) artifact_id, search
			from artifact_versions order by artifact_id, number desc
		)
		select w.word, (select count(*) from latest where latest.search @@ plainto_tsquery('english', w.word))
		from unnest($1::text[]) as w (word)
	`, words)
	if err != nil {
		return fmt.Errorf("count the candidate words: %w", err)
	}
	type hit struct {
		word string
		docs int
	}
	hits, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (hit, error) {
		var found hit
		return found, row.Scan(&found.word, &found.docs)
	})
	if err != nil {
		return fmt.Errorf("count the candidate words: %w", err)
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].docs < hits[b].docs })
	// The rare word is the one closest to search's per-arm limit from below: a word no document
	// holds would make the search check trivially true, and one above the limit cannot be
	// compared with what the route returns. The medium word is then halfway up to the most
	// common one, so the three are in order and each is a different selectivity for a plan.
	rarest := 0
	for index, found := range hits {
		if found.docs > 0 && found.docs < contracts.SearchKindDepth {
			rarest = index
		}
	}
	rare, medium, common := hits[rarest], hits[(rarest+len(hits))/2], hits[len(hits)-1]
	r.subject.RareWord, r.subject.RareHits = rare.word, rare.docs
	fmt.Fprintln(r.out, "\nwords (documents whose latest version matches plainto_tsquery('english', word))")
	fmt.Fprintf(r.out, "  vocabulary %d words, Zipf exponent %g\n", vocabularySize, zipfExponent)
	fmt.Fprintf(r.out, "  rare    %-12s %d documents (search lists at most %d of a kind)\n", rare.word, rare.docs, contracts.SearchKindDepth)
	fmt.Fprintf(r.out, "  medium  %-12s %d documents\n", medium.word, medium.docs)
	fmt.Fprintf(r.out, "  common  %-12s %d documents\n", common.word, common.docs)
	return nil
}

// reportSubjects prints the named rows a measurement binds: the review's finding 9 is that a
// sibling cannot find them otherwise.
func (r *seedRun) reportSubjects(ctx context.Context) error {
	plan := r.plan
	hub := plan.issues[plan.hub]
	var children, blockers int
	if err := r.database.Pool.QueryRow(ctx, `
		select (select count(*) from issues where parent_key = $1),
		       (select count(*) from issue_links where issue_key = $1 and kind = 'blocked_by')
	`, hub.Key).Scan(&children, &blockers); err != nil {
		return err
	}
	anchored := plan.documents[plan.anchored]
	var anchoredComments, anchoredAsks, tableRowAnchors int
	if err := r.database.Pool.QueryRow(ctx, `
		select (select count(*) from comments where anchor->>'artifact_id' = $1),
		       (select count(*) from asks where anchor->>'artifact_id' = $1),
		       (select count(*) from comments where anchor->>'artifact_id' = $1 and anchor->>'block_id' = $2)
	`, anchored.ID, r.tableRow).Scan(&anchoredComments, &anchoredAsks, &tableRowAnchors); err != nil {
		return err
	}
	chain := plan.documents[plan.chainHead]
	deepest := plan.issues[plan.deepest]
	sessions := plan.sessions[:min(5, len(plan.sessions))]
	large := plan.documents[0]

	fmt.Fprintln(r.out, "\nmeasurement subjects")
	fmt.Fprintf(r.out, "  largest project              %s with %d issues\n", plan.projects[0].Key, plan.projects[0].Issues)
	fmt.Fprintf(r.out, "  heavily linked issue         %s with %d children and %d blockers\n", hub.Key, children, blockers)
	fmt.Fprintf(r.out, "  deepest parent chain         %s at depth %d\n", deepest.Key, deepest.Depth)
	fmt.Fprintf(r.out, "  anchored document            %s (artifact %s): %d anchored comments, %d anchored asks, %d of them on table row %s\n",
		anchored.RefKey, anchored.ID, anchoredComments, anchoredAsks, tableRowAnchors, r.tableRow)
	fmt.Fprintf(r.out, "  long mention chain           issue %s, document %s, %d hops\n",
		issueKeyOf(plan, chain), chain.RefKey, chainLength-1)
	fmt.Fprintf(r.out, "  large document               %s (artifact %s), %d versions\n", large.RefKey, large.ID, large.Versions)
	fmt.Fprintf(r.out, "  sessions for agent activity  %s\n", strings.Join(sessions, " "))
	fmt.Fprintf(r.out, "  embedding model              %s\n", r.subject.Model)
	fmt.Fprintf(r.out, "  query vector                 the embedding of %s; read it with:\n", r.subject.QueryVectorRow)
	fmt.Fprintf(r.out, "                               select embedding from embeddings where kind = '%s' and id = '%s'\n",
		strings.Fields(r.subject.QueryVectorRow)[0], strings.Fields(r.subject.QueryVectorRow)[1])
	fmt.Fprintf(r.out, "  server URL of every mention  %s\n", serverURL)
	return nil
}

func issueKeyOf(plan *seedPlan, document documentPlan) string {
	if document.Issue < 0 {
		return "(a project document)"
	}
	return plan.issues[document.Issue].Key
}

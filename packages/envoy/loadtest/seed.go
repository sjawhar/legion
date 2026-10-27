package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// commitTriggerSQL installs loadtest.event_commits and its trigger on events; stack.sh pipes it
// to psql once the seed's outbox has drained (loadgen trigger-sql).
//
//go:embed commit-trigger.sql
var commitTriggerSQL string

// specName is the primary document every issue is created with
// (internal/dispatch/api/issue_create.go: slug spec, name spec.md).
const specName = "spec.md"

type seedIssue struct {
	Key        string `json:"key"`
	Project    string `json:"project"`
	ArtifactID string `json:"artifact_id"`
	SpecName   string `json:"spec_name"`
	SpecBytes  int    `json:"spec_bytes"`
	// Quotes are the spec's requirement cells ("Requirement <placeholder>-R<n> holds") anchored
	// comments quote.
	Quotes []string `json:"quotes"`
	// AnchoredCommentIDs and PlainCommentIDs are the issue's seeded root comments, the roots
	// replies answer.
	AnchoredCommentIDs []string `json:"anchored_comment_ids"`
	PlainCommentIDs    []string `json:"plain_comment_ids"`
	// Route is the issue's route, session:<sessionID(i)> for the issue's index i in key order.
	Route string `json:"route"`
}

type seedFile struct {
	Projects []string    `json:"projects"`
	Issues   []seedIssue `json:"issues"`
}

// sortIssues puts issues in the one order seed routes them in and run maps sessions onto them.
func sortIssues(issues []seedIssue) {
	sort.Slice(issues, func(i, j int) bool { return issues[i].Key < issues[j].Key })
}

func sessionID(i int) string { return fmt.Sprintf("ses-lg-%04d", i) }

func readSeed(path string) (seedFile, error) {
	var sf seedFile
	b, err := os.ReadFile(path)
	if err != nil {
		return sf, fmt.Errorf("read seed: %w", err)
	}
	if err := json.Unmarshal(b, &sf); err != nil {
		return sf, fmt.Errorf("seed %s: %w", path, err)
	}
	if len(sf.Issues) == 0 || len(sf.Projects) == 0 {
		return sf, fmt.Errorf("seed %s holds no issues or no projects", path)
	}
	sortIssues(sf.Issues)
	return sf, nil
}

var seedProjects = []struct {
	key    string
	issues int
}{{"LGA", 400}, {"LGB", 300}, {"LGC", 200}, {"LGD", 100}}

func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	var ef endpointFlags
	ef.register(fs)
	out := fs.String("out", "", "seed.json path (default $LOADTEST_ROOT/seed.json)")
	comments := fs.Int("comments", 8, "comments per issue, every second one anchored")
	workers := fs.Int("workers", 8, "concurrent writers")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *comments < 2 || *workers < 1 {
		return fmt.Errorf("-comments must be at least 2 (one anchored, one plain root) and -workers at least 1")
	}
	if *out == "" {
		*out = filepath.Join(loadtestRoot(), "seed.json")
	}
	ep, err := ef.resolve()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := checkStack(ctx, ep); err != nil {
		return err
	}
	sf, err := seed(ctx, newAPI(ep.dispatch, ep.token, 64), *comments, *workers)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(sf, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, b, 0o644)
}

// seed creates the projects, issues and comments, then routes issue i (in key order) to session
// i. Content is deterministic: each issue's generator is seeded by its project and number, never
// the clock, so which worker writes it does not change it.
func seed(ctx context.Context, a *api, comments, workers int) (seedFile, error) {
	var sf seedFile
	for _, p := range seedProjects {
		res := a.callRetry(ctx, "POST", "/api/v1/projects", who{}, map[string]any{"key": p.key, "name": "Load " + p.key, "actor": sessionActor("ses-seed")})
		if !res.ok() && res.status != 409 {
			return sf, fmt.Errorf("create project %s: %d %s %v", p.key, res.status, trunc(res.body), res.err)
		}
		sf.Projects = append(sf.Projects, p.key)
	}

	type job struct {
		project, n int
	}
	jobs := make(chan job)
	var mu sync.Mutex
	var created atomic.Int64
	start := time.Now()
	g, gctx := errgroup.WithContext(ctx)
	for w := range workers {
		g.Go(func() error {
			sess := fmt.Sprintf("ses-seed-%d", w)
			for j := range jobs {
				is, err := seedIssueWith(gctx, a, sess, seedProjects[j.project].key, j.project, j.n, comments)
				if err != nil {
					return err
				}
				mu.Lock()
				sf.Issues = append(sf.Issues, is)
				mu.Unlock()
				if n := created.Add(1); n%50 == 0 {
					log.Printf("seeded %d issues in %s", n, time.Since(start).Round(time.Second))
				}
			}
			return nil
		})
	}
	g.Go(func() error {
		defer close(jobs)
		for pi, p := range seedProjects {
			for n := range p.issues {
				select {
				case jobs <- job{pi, n}:
				case <-gctx.Done():
					return nil
				}
			}
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return sf, err
	}
	log.Printf("seeded %d issues in %s; routing them", len(sf.Issues), time.Since(start).Round(time.Second))

	// Second pass, in the order run maps sessions onto issues, so no step spends its warm-up on
	// route PATCHes.
	sortIssues(sf.Issues)
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for i := range sf.Issues {
		g.Go(func() error {
			route := "session:" + sessionID(i)
			res := a.callRetry(gctx, "PATCH", "/api/v1/issues/"+sf.Issues[i].Key, who{}, map[string]any{"route": route, "actor": sessionActor("ses-seed")})
			if !res.ok() {
				return fmt.Errorf("route %s: %d %s %v", sf.Issues[i].Key, res.status, trunc(res.body), res.err)
			}
			sf.Issues[i].Route = route
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return sf, err
	}
	log.Printf("seed done: %d issues in %s", len(sf.Issues), time.Since(start).Round(time.Second))
	return sf, nil
}

func seedIssueWith(ctx context.Context, a *api, sess, project string, projectIndex, n, comments int) (seedIssue, error) {
	r := rand.New(rand.NewPCG(uint64(projectIndex)<<32|uint64(n), 42))
	big := n%5 == 0 // 20%: 40-50 KB table-heavy specs
	size := 4000 + r.IntN(8000)
	if big {
		size = 40000 + r.IntN(10000)
	}
	placeholder := fmt.Sprintf("%s-%d", project, n)
	spec, quotes := specMarkdown(r, placeholder, size, big)
	res := a.callRetry(ctx, "POST", "/api/v1/issues", who{}, map[string]any{
		"project": project, "title": fmt.Sprintf("Load issue %s %s", placeholder, sentence(r, 5)),
		"spec": spec, "force": true, "actor": sessionActor(sess),
	})
	if !res.ok() {
		return seedIssue{}, fmt.Errorf("create issue %s: %d %s %v", placeholder, res.status, trunc(res.body), res.err)
	}
	var created struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(res.body, &created); err != nil || created.Key == "" || created.PrimaryArtifactID == "" {
		return seedIssue{}, fmt.Errorf("decode issue %s: %v %s", placeholder, err, trunc(res.body))
	}
	is := seedIssue{Key: created.Key, Project: project, ArtifactID: created.PrimaryArtifactID, SpecName: specName, SpecBytes: len(spec), Quotes: quotes}
	for c := range comments {
		anchored := c%2 == 1
		payload := map[string]any{"body": fmt.Sprintf("Seed comment %d: %s", c, sentence(r, 25)), "actor": sessionActor(sess)}
		if anchored {
			payload["anchor"] = map[string]any{"artifact": "spec", "quote": quotes[r.IntN(len(quotes))]}
		}
		res := a.callRetry(ctx, "POST", "/api/v1/issues/"+is.Key+"/comments", who{}, payload)
		var comment struct {
			ID string `json:"id"`
		}
		if res.ok() {
			_ = json.Unmarshal(res.body, &comment)
		}
		if comment.ID == "" {
			return seedIssue{}, fmt.Errorf("seed comment on %s: %d %s %v", is.Key, res.status, trunc(res.body), res.err)
		}
		if anchored {
			is.AnchoredCommentIDs = append(is.AnchoredCommentIDs, comment.ID)
		} else {
			is.PlainCommentIDs = append(is.PlainCommentIDs, comment.ID)
		}
	}
	return is, nil
}

// --- spec generation -------------------------------------------------------------------------

var words = strings.Fields(`the outbox publishes each committed event to its issue topic and every session that
watches the issue receives it within a second while the listener keeps interests in a key value bucket
dispatch reads issues documents comments and asks from postgres through a shared pool of connections
a reviewer anchors a comment to a line of the specification and the author answers with a new version
latency throughput memory goroutines profile scaling knee saturation connection queue lock contention`)

func sentence(r *rand.Rand, n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(words[r.IntN(len(words))])
	}
	return b.String()
}

// specMarkdown builds a spec of roughly target bytes: sections of prose and 6-column tables. Every
// table row carries a stable, unique requirement cell ("Requirement <key>-R<n> holds") that load
// comments anchor to, and the document carries one "Revision token r0" line that spec edits and
// uploads bump.
func specMarkdown(r *rand.Rand, key string, target int, tableHeavy bool) (string, []string) {
	var b strings.Builder
	var quotes []string
	fmt.Fprintf(&b, "# Spec for %s\n\n%s\n\n", key, revisionToken(0))
	row := 0
	section := 0
	for b.Len() < target {
		section++
		fmt.Fprintf(&b, "## Section %d\n\n%s.\n\n%s.\n\n", section, sentence(r, 40), sentence(r, 30))
		rows := 4
		if tableHeavy {
			rows = 18
		}
		b.WriteString("| Id | Requirement | Owner | Status | Evidence | Notes |\n|---|---|---|---|---|---|\n")
		for range rows {
			row++
			q := fmt.Sprintf("Requirement %s-R%d holds", key, row)
			quotes = append(quotes, q)
			fmt.Fprintf(&b, "| R%d | %s | team-%d | open | %s | %s |\n", row, q, r.IntN(9), sentence(r, 6), sentence(r, 5))
		}
		b.WriteString("\n")
	}
	return b.String(), quotes
}

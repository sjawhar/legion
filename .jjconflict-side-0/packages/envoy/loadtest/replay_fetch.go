package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// replay-fetch reads one project's issues, event logs, artifact listings and document texts from
// a Dispatch read-only and writes a replay bundle (replay_bundle.go). Every response it needs is
// cached under <out>/raw/, so a rerun fetches only what is missing and rebuilds the bundle from the
// cache. It reaches events through each issue's log, so an event with no issue key is not in the
// window; the replay accepts that.

const (
	replayPageLimit = 200
	// replayPageSlack: an issue's log stops paging once a page ends this far past -until, so a
	// cached log does not grow with the writes made since.
	replayPageSlack = 10 * time.Minute
	replayMaxConc   = 4
)

func runReplayFetch(args []string) error {
	fl := flag.NewFlagSet("replay-fetch", flag.ContinueOnError)
	project := fl.String("project", "", "project key, e.g. LINUX")
	atFlag := fl.String("at", "", "the state instant (RFC 3339)")
	untilFlag := fl.String("until", "", "the window's end, exclusive (RFC 3339)")
	out := fl.String("out", "", "bundle directory; its raw/ is the response cache")
	conc := fl.Int("concurrency", replayMaxConc, "requests in flight, 1 to 4")
	keys := fl.String("keys", "", "comma-separated issue keys: a debug subset, with no placeholders")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *project == "" || *atFlag == "" || *untilFlag == "" || *out == "" {
		return fmt.Errorf("-project, -at, -until and -out are required")
	}
	at, err := time.Parse(time.RFC3339Nano, *atFlag)
	if err != nil {
		return fmt.Errorf("-at: %w", err)
	}
	until, err := time.Parse(time.RFC3339Nano, *untilFlag)
	if err != nil {
		return fmt.Errorf("-until: %w", err)
	}
	if !until.After(at) {
		return fmt.Errorf("-until must be after -at")
	}
	if *conc < 1 || *conc > replayMaxConc {
		return fmt.Errorf("-concurrency must be 1 to %d", replayMaxConc)
	}
	base := os.Getenv("DISPATCH_URL")
	tokenFile := os.Getenv("DISPATCH_TOKEN_FILE")
	if base == "" || tokenFile == "" {
		return fmt.Errorf("DISPATCH_URL and DISPATCH_TOKEN_FILE must be set")
	}
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("read DISPATCH_TOKEN_FILE: %w", err)
	}
	var subset []string
	if *keys != "" {
		for _, k := range strings.Split(*keys, ",") {
			if k = strings.TrimSpace(k); k != "" {
				subset = append(subset, k)
			}
		}
	}
	f := &replayFetcher{
		api:  newAPI(base, strings.TrimSpace(string(tok)), *conc),
		raw:  filepath.Join(*out, bundleRawDir),
		conc: *conc,
		lg:   log.New(os.Stderr, "replay-fetch ", log.LstdFlags),
	}
	counts, err := f.run(context.Background(), *project, at.UTC(), until.UTC(), *out, subset)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(counts, "", "  ")
	fmt.Println(string(b))
	return nil
}

// replayFetcher is replay-fetch's read-only client and response cache.
type replayFetcher struct {
	api  *api
	raw  string
	conc int
	lg   *log.Logger
	gets atomic.Int64
}

// httpStatusError is a non-2xx answer: it names the route template, never the URL or the body.
type httpStatusError struct {
	route  string
	status int
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("GET %s: status %d", e.route, e.status) }

// get sends one GET, retrying a transport error, 429 or 5xx with exponential backoff up to
// attempts tries. A transport error is reported by its type only, since its text names the host.
func (f *replayFetcher) get(ctx context.Context, route, path string, attempts int) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		f.gets.Add(1)
		res := f.api.call(ctx, http.MethodGet, path, who{}, nil)
		if res.ok() {
			return res.body, nil
		}
		if !res.transient() || attempt >= attempts || ctx.Err() != nil {
			if res.err != nil {
				inner := res.err
				var ue *url.Error
				if errors.As(inner, &ue) {
					inner = ue.Err
				}
				return nil, fmt.Errorf("GET %s: transport error (%T) after %d attempts", route, inner, attempt)
			}
			return nil, &httpStatusError{route: route, status: res.status}
		}
		backoff := min(time.Second<<(attempt-1), 30*time.Second)
		if !sleepUntil(ctx, time.Now().Add(backoff)) {
			return nil, ctx.Err()
		}
	}
}

// cached returns the cached response at rel under raw/, fetching and caching it when absent.
func (f *replayFetcher) cached(ctx context.Context, rel, route, path string) ([]byte, error) {
	p := filepath.Join(f.raw, rel)
	if b, err := os.ReadFile(p); err == nil {
		return b, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	b, err := f.get(ctx, route, path, 6)
	if err != nil {
		return nil, err
	}
	return b, writeFileAtomic(p, b)
}

// issueLog is one issue's event log, ascending by seq, as far as replay-fetch pages it.
type issueLog struct {
	Key    string
	Number int
	Events []replayEvent
}

// fetchLog pages an issue's events from the cache or Dispatch, stopping at a short page or at a
// page ending more than replayPageSlack past until.
func (f *replayFetcher) fetchLog(ctx context.Context, key string, number int, until time.Time) (issueLog, error) {
	lg := issueLog{Key: key, Number: number}
	after := 0
	for {
		rel := filepath.Join("events", key, strconv.Itoa(after)+".json")
		path := fmt.Sprintf("/api/v1/issues/%s/events?after=%d&limit=%d", url.PathEscape(key), after, replayPageLimit)
		body, err := f.cached(ctx, rel, "/api/v1/issues/{key}/events", path)
		if err != nil {
			return lg, err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(body, &page); err != nil {
			return lg, fmt.Errorf("%s events after %d: %w", key, after, err)
		}
		for _, raw := range page {
			ev, err := parseReplayEvent(raw)
			if err != nil {
				return lg, fmt.Errorf("%s events after %d: %w", key, after, err)
			}
			lg.Events = append(lg.Events, ev)
		}
		if len(page) < replayPageLimit {
			return lg, nil
		}
		last := lg.Events[len(lg.Events)-1]
		if last.CreatedAt.After(until.Add(replayPageSlack)) {
			return lg, nil
		}
		after = last.Seq
	}
}

// listedArtifact is one entry of GET /api/v1/issues/{key}/artifacts.
type listedArtifact struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Slug      string          `json:"slug"`
	Kind      string          `json:"kind"`
	Primary   bool            `json:"primary"`
	CreatedAt time.Time       `json:"created_at"`
	Versions  []listedVersion `json:"versions"`
}

type listedVersion struct {
	Number    int       `json:"number"`
	Size      *int      `json:"size"`
	MIME      *string   `json:"mime"`
	CreatedAt time.Time `json:"created_at"`
}

// versionAt is the listed version current at at: the largest number created before it.
func versionAt(versions []listedVersion, at time.Time) (listedVersion, bool) {
	var best listedVersion
	found := false
	for _, v := range versions {
		if v.CreatedAt.Before(at) && (!found || v.Number > best.Number) {
			best, found = v, true
		}
	}
	return best, found
}

// docText reads a document version's markdown: ok is false when Dispatch will not serve it (any
// non-2xx but 401 and 403 once retries are spent), which is cached as <n>.unreadable.
func (f *replayFetcher) docText(ctx context.Context, id string, n int) ([]byte, bool, error) {
	dir := filepath.Join(f.raw, "versions", id)
	okPath := filepath.Join(dir, strconv.Itoa(n)+".json")
	badPath := filepath.Join(dir, strconv.Itoa(n)+".unreadable")
	body, err := os.ReadFile(okPath)
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(badPath); err == nil {
			return nil, false, nil
		}
		body, err = f.get(ctx, "/api/v1/artifacts/{id}/versions/{n}",
			fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", url.PathEscape(id), n), 3)
		var se *httpStatusError
		if errors.As(err, &se) && se.status != http.StatusUnauthorized && se.status != http.StatusForbidden {
			return nil, false, writeFileAtomic(badPath, []byte(strconv.Itoa(se.status)+"\n"))
		}
		if err != nil {
			return nil, false, err
		}
		if err := writeFileAtomic(okPath, body); err != nil {
			return nil, false, err
		}
	} else if err != nil {
		return nil, false, err
	}
	var v struct {
		Markdown *string `json:"markdown"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, false, fmt.Errorf("artifact %s version %d: %w", id, n, err)
	}
	if v.Markdown == nil {
		return nil, false, nil
	}
	return []byte(*v.Markdown), true, nil
}

// run fetches, reconstructs and writes the bundle, returning its counts.
func (f *replayFetcher) run(ctx context.Context, project string, at, until time.Time, out string, subset []string) (replayCounts, error) {
	var counts replayCounts
	if err := os.MkdirAll(f.raw, 0o755); err != nil {
		return counts, err
	}
	listBody, err := f.cached(ctx, "issues.json", "/api/v1/issues", "/api/v1/issues?project="+url.QueryEscape(project))
	if err != nil {
		return counts, err
	}
	var listed []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(listBody, &listed); err != nil {
		return counts, fmt.Errorf("issue listing: %w", err)
	}
	numbers := map[string]int{}
	for _, is := range listed {
		n, err := issueNumber(project, is.Key)
		if err != nil {
			return counts, err
		}
		numbers[is.Key] = n
	}
	keys := subset
	if keys == nil {
		for k := range numbers {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if _, ok := numbers[k]; !ok {
			return counts, fmt.Errorf("issue %s is not in project %s's listing", k, project)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return numbers[keys[i]] < numbers[keys[j]] })
	f.lg.Printf("listing: %d issues, fetching %d logs", len(listed), len(keys))

	logs := make([]issueLog, len(keys))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(f.conc)
	for i, k := range keys {
		g.Go(func() error {
			lg, err := f.fetchLog(gctx, k, numbers[k], until)
			logs[i] = lg
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return counts, err
	}
	rec, err := buildState(project, at, until, logs, subset != nil)
	if err != nil {
		return counts, err
	}
	f.lg.Printf("logs: %d issues at at, %d window events", countExisting(rec.State.Issues), len(rec.Window))

	listings := make(map[string][]listedArtifact)
	var existing []string
	for _, is := range rec.State.Issues {
		if is.Exists {
			existing = append(existing, is.Key)
		}
	}
	listingsByIdx := make([][]listedArtifact, len(existing))
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(f.conc)
	for i, k := range existing {
		g.Go(func() error {
			body, err := f.cached(gctx, filepath.Join("artifacts", k+".json"), "/api/v1/issues/{key}/artifacts",
				"/api/v1/issues/"+url.PathEscape(k)+"/artifacts")
			if err != nil {
				return err
			}
			if err := json.Unmarshal(body, &listingsByIdx[i]); err != nil {
				return fmt.Errorf("%s artifacts: %w", k, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return counts, err
	}
	for i, k := range existing {
		listings[k] = listingsByIdx[i]
	}
	unlisted := attachArtifacts(&rec.State, listings, rec.CreatedBefore, at)
	byID := map[string]listedArtifact{}
	for _, ls := range listings {
		for _, la := range ls {
			byID[la.ID] = la
		}
	}
	setBlockVersions(&rec.State, byID)

	for _, d := range []string{bundleDocsDir, bundleCreatedDir} {
		if err := os.RemoveAll(filepath.Join(out, d)); err != nil {
			return counts, err
		}
		if err := os.MkdirAll(filepath.Join(out, d), 0o755); err != nil {
			return counts, err
		}
	}
	var docs []*replayArtifact
	for i := range rec.State.Issues {
		for j := range rec.State.Issues[i].Artifacts {
			if a := &rec.State.Issues[i].Artifacts[j]; a.Kind == "doc" {
				docs = append(docs, a)
			}
		}
	}
	docBytes := make([]int, len(docs))
	texts := make([]string, len(docs))
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(f.conc)
	for i, a := range docs {
		g.Go(func() error {
			for n := a.Version; n >= 1; n-- {
				text, ok, err := f.docText(gctx, a.ID, n)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				rel := bundleDocsDir + "/" + a.ID + ".md"
				if err := os.WriteFile(filepath.Join(out, filepath.FromSlash(rel)), text, 0o644); err != nil {
					return err
				}
				a.TextPath, a.TextVersion, docBytes[i], texts[i] = rel, n, len(text), string(text)
				return nil
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return counts, err
	}
	f.lg.Printf("docs: %d texts", len(docs))

	created := createdDocIDs(rec.Window)
	specs, err := f.createdSpecs(ctx, rec.Window)
	if err != nil {
		return counts, err
	}
	fetchIDs := slices.Clone(created)
	seenID := map[string]bool{}
	for _, id := range created {
		seenID[id] = true
	}
	for _, s := range specs {
		if s.ID != "" && !seenID[s.ID] {
			seenID[s.ID] = true
			fetchIDs = append(fetchIDs, s.ID)
		}
	}
	fetchedOK := make([]bool, len(fetchIDs))
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(f.conc)
	for i, id := range fetchIDs {
		g.Go(func() error {
			text, ok, err := f.docText(gctx, id, 1)
			if err != nil || !ok {
				return err
			}
			fetchedOK[i] = true
			return os.WriteFile(filepath.Join(out, bundleCreatedDir, id+".md"), text, 0o644)
		})
	}
	if err := g.Wait(); err != nil {
		return counts, err
	}
	readable := map[string]bool{}
	for i, id := range fetchIDs {
		readable[id] = fetchedOK[i]
	}
	createdSpecs := map[string]string{}
	for _, s := range specs {
		if readable[s.ID] {
			createdSpecs[s.Key] = s.ID
		}
	}
	f.lg.Printf("created: %d documents, %d specs", len(created), len(specs))

	docTexts := map[string]string{}
	for i, a := range docs {
		if a.TextPath != "" {
			docTexts[a.ID] = texts[i]
		}
	}
	dropResolvedOutsideText(&rec.State, docTexts)
	mapsUnavailable, err := f.fetchBlockMaps(ctx, &rec.State, rec.Window, byID, docTexts)
	if err != nil {
		return counts, err
	}
	f.lg.Printf("block maps: %d read, %d unavailable", len(rec.State.BlockMaps), mapsUnavailable)

	roster := buildRoster(rec.State, rec.Window, rec.Origins)
	counts = stateCounts(rec.State, rec.Window)
	counts.ArtifactsUnlisted = unlisted
	counts.Roster = len(roster)
	for i, a := range docs {
		counts.DocBytes += docBytes[i]
		switch {
		case a.TextVersion == 0:
			counts.DocsUnreadable++
		case a.TextVersion < a.Version:
			counts.DocsFellBack++
		}
	}
	for _, id := range created {
		if readable[id] {
			counts.CreatedDocs++
		} else {
			counts.CreatedDocsUnreadable++
		}
	}
	counts.CreatedSpecs = len(createdSpecs)
	counts.CreatedSpecsUnreadable = len(specs) - len(createdSpecs)
	counts.VersionSizesMissing = versionSizesMissing(docs)
	counts.BlockMaps, counts.BlockMapsUnavailable = len(rec.State.BlockMaps), mapsUnavailable
	counts.AnchorRules = countAnchorRules(rec.State, rec.Window, docTexts)
	f.lg.Printf("requests: %d GETs sent this run", f.gets.Load())

	stateJSON, err := json.MarshalIndent(rec.State, "", "  ")
	if err != nil {
		return counts, err
	}
	rosterJSON, err := json.MarshalIndent(roster, "", "  ")
	if err != nil {
		return counts, err
	}
	var window bytes.Buffer
	for _, ev := range rec.Window {
		window.Write(ev.Raw)
		window.WriteByte('\n')
	}
	for name, data := range map[string][]byte{
		bundleStateFile:  append(stateJSON, '\n'),
		bundleRosterFile: append(rosterJSON, '\n'),
		bundleWindowFile: window.Bytes(),
	} {
		if err := writeFileAtomic(filepath.Join(out, name), data); err != nil {
			return counts, err
		}
	}
	files, err := hashBundleFiles(out)
	if err != nil {
		return counts, err
	}
	meta := replayMeta{Project: project, At: at, Until: until, FetchedAt: time.Now().UTC(), Counts: counts,
		CreatedSpecs: createdSpecs, Files: files}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return counts, err
	}
	return counts, writeFileAtomic(filepath.Join(out, bundleMetaFile), append(metaJSON, '\n'))
}

// issueNumber is the integer after the dash of a project's issue key.
func issueNumber(project, key string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(key, project+"-"))
	if err != nil || !strings.HasPrefix(key, project+"-") || n < 1 {
		return 0, fmt.Errorf("issue key %q is not %s-<number>", key, project)
	}
	return n, nil
}

// hashBundleFiles is the sha256 of every regular file under dir but raw/ and bundle.meta.json,
// by slash path.
func hashBundleFiles(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := walkBundle(dir, func(rel, abs string) error {
		if rel == bundleMetaFile {
			return nil
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		files[rel] = sha256Hex(b)
		return nil
	})
	return files, err
}

// walkBundle calls fn for every regular file of the bundle under dir, raw/ excluded, in sorted
// slash-path order.
func walkBundle(dir string, fn func(rel, abs string) error) error {
	var rels []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == bundleRawDir {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			rels = append(rels, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if err := fn(rel, filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			return err
		}
	}
	return nil
}

// writeFileAtomic writes data to path through a temporary file and a rename, creating parents.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

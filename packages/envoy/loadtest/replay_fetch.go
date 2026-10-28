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
	"sync"
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
		transient := res.err != nil || res.status == http.StatusTooManyRequests || res.status >= 500
		if !transient || attempt >= attempts || ctx.Err() != nil {
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
		backoff := time.Second << (attempt - 1)
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
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
	Named     bool      `json:"named"`
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
				a.TextPath, a.TextVersion, docBytes[i] = rel, n, len(text)
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

	docTexts, err := readDocTexts(out, docs)
	if err != nil {
		return counts, err
	}
	dropResolvedOutsideText(&rec.State, docTexts)
	mapsUnavailable, err := f.fetchBlockMaps(ctx, &rec.State, rec.Window, byID, out)
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

// --- Reconstruction: pure functions of the fetched logs and listings. ---

// reconstruction is what buildState derives from the event logs.
type reconstruction struct {
	State replayState
	// Window is every event with at <= created_at < until, ordered by (created_at, id).
	Window []replayEvent
	// CreatedBefore lists, per existing issue, the artifacts an artifact.created made before at.
	CreatedBefore map[string][]string
	// Origins is each session's newest origin among events before until.
	Origins map[string]replayOrigin
}

// wireIssue is the issue snapshot of issue.created, issue.updated and issue.closed (Title set),
// or the claim payload of issue.claimed and issue.released (Title nil).
type wireIssue struct {
	Title             *string      `json:"title"`
	Status            string       `json:"status"`
	Parent            *string      `json:"parent"`
	Priority          *int         `json:"priority"`
	Labels            []string     `json:"labels"`
	Route             *string      `json:"route"`
	Claim             *replayClaim `json:"claim"`
	BlockedBy         []string     `json:"blocked_by"`
	PrimaryArtifactID string       `json:"primary_artifact_id"`
	CreatedAt         time.Time    `json:"created_at"`
	CreatedBy         *replayActor `json:"created_by"`
}

// wireComment is the comment a comment.* event carries.
type wireComment struct {
	ID           string        `json:"id"`
	Body         string        `json:"body"`
	ArtifactID   *string       `json:"artifact_id"`
	ReplyTo      *string       `json:"reply_to"`
	ThreadRootID *string       `json:"thread_root_id"`
	Anchor       *replayAnchor `json:"anchor"`
	AskID        *string       `json:"ask_id"`
	Resolved     bool          `json:"resolved"`
	ResolvedAt   *string       `json:"resolved_at"`
	ResolvedBy   *replayActor  `json:"resolved_by"`
	CreatedAt    time.Time     `json:"created_at"`
}

// wireAsk is the ask an ask.* event carries; an event without a state (a follower change) is not
// an ask snapshot.
type wireAsk struct {
	ID            string            `json:"id"`
	Kind          string            `json:"kind"`
	State         string            `json:"state"`
	Question      string            `json:"question"`
	Options       []replayAskOption `json:"options"`
	Multiple      bool              `json:"multiple"`
	Urgency       string            `json:"urgency"`
	Author        replayActor       `json:"author"`
	CreatedAt     time.Time         `json:"created_at"`
	ArtifactID    *string           `json:"artifact_id"`
	Anchor        *replayAnchor     `json:"anchor"`
	BlockID       *string           `json:"block_id"`
	BlockArtifact *struct {
		ID string `json:"id"`
	} `json:"block_artifact"`
	Answer     *replayAskAnswer   `json:"answer"`
	Approval   *replayAskApproval `json:"approval"`
	Resolution *struct {
		Kind   string      `json:"kind"`
		Reason string      `json:"reason"`
		Actor  replayActor `json:"actor"`
		At     time.Time   `json:"at"`
	} `json:"resolution"`
}

// commentStates are the comment.* events whose payload is the whole comment.
var commentStates = map[string]bool{
	"comment.created": true, "comment.edited": true, "comment.resolved": true,
	"comment.reopened": true, "comment.anchor_refreshed": true,
}

// buildState reconstructs the project at at from logs (in number order). With subset, the state
// holds only the logged issues that exist at at, with no placeholders.
func buildState(project string, at, until time.Time, logs []issueLog, subset bool) (*reconstruction, error) {
	rec := &reconstruction{
		State:         replayState{Project: project, At: at, Until: until},
		CreatedBefore: map[string][]string{},
		Origins:       map[string]replayOrigin{},
	}
	originAt := map[string]time.Time{}
	byNumber := map[int]replayIssue{}
	maxNumber := 0
	for _, lg := range logs {
		is, window, err := replayIssueAt(lg, at, until, rec)
		if err != nil {
			return nil, err
		}
		rec.Window = append(rec.Window, window...)
		for _, ev := range lg.Events {
			if ev.Actor.Kind == "session" && ev.Actor.Origin != nil && ev.CreatedAt.Before(until) &&
				!ev.CreatedAt.Before(originAt[ev.Actor.ID]) {
				rec.Origins[ev.Actor.ID], originAt[ev.Actor.ID] = *ev.Actor.Origin, ev.CreatedAt
			}
		}
		if is.Exists {
			byNumber[is.Number] = is
			maxNumber = max(maxNumber, is.Number)
		}
	}
	if subset {
		for _, lg := range logs {
			if is, ok := byNumber[lg.Number]; ok {
				rec.State.Issues = append(rec.State.Issues, is)
			}
		}
	} else {
		for n := 1; n <= maxNumber; n++ {
			is, ok := byNumber[n]
			if !ok {
				is = replayIssue{Key: fmt.Sprintf("%s-%d", project, n), Number: n}
			}
			rec.State.Issues = append(rec.State.Issues, is)
		}
	}
	sort.SliceStable(rec.Window, func(i, j int) bool {
		a, b := rec.Window[i], rec.Window[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return rec, nil
}

// replayIssueAt folds one issue's log into its state before at and returns its window events.
func replayIssueAt(lg issueLog, at, until time.Time, rec *reconstruction) (replayIssue, []replayEvent, error) {
	is := replayIssue{Key: lg.Key, Number: lg.Number}
	var window []replayEvent
	comments := map[string]int{} // id -> index in is.Comments
	replyTo := map[string]string{}
	threadRoot := map[string]string{}
	asks := map[string]int{}
	var askList []replayAsk
	for _, ev := range lg.Events {
		where := func(err error) error {
			return fmt.Errorf("%s event %d (%s): %w", lg.Key, ev.ID, ev.Type, err)
		}
		if strings.HasPrefix(ev.Type, "comment.") && commentStates[ev.Type] && ev.CreatedAt.Before(until) {
			var c wireComment
			if err := json.Unmarshal(ev.Payload, &c); err != nil {
				return is, nil, where(err)
			}
			if ev.Type == "comment.created" {
				if c.ReplyTo != nil {
					replyTo[c.ID] = *c.ReplyTo
				}
				if c.ThreadRootID != nil {
					threadRoot[c.ID] = *c.ThreadRootID
				}
			}
		}
		if !ev.CreatedAt.Before(at) {
			if ev.CreatedAt.Before(until) {
				window = append(window, ev)
			}
			continue
		}
		switch {
		case strings.HasPrefix(ev.Type, "issue."):
			var w wireIssue
			if err := json.Unmarshal(ev.Payload, &w); err != nil {
				return is, nil, where(err)
			}
			if w.Title != nil {
				is.Exists = true
				is.Title, is.Status, is.Parent, is.Priority = *w.Title, w.Status, w.Parent, w.Priority
				is.Labels, is.Route, is.Claim, is.BlockedBy = w.Labels, w.Route, w.Claim, w.BlockedBy
				is.PrimaryArtifactID, is.CreatedAt, is.CreatedBy = w.PrimaryArtifactID, w.CreatedAt, w.CreatedBy
			} else if ev.Type == "issue.claimed" || ev.Type == "issue.released" {
				is.Claim = w.Claim
				if w.Status != "" {
					is.Status = w.Status
				}
			}
		case commentStates[ev.Type]:
			var c wireComment
			if err := json.Unmarshal(ev.Payload, &c); err != nil {
				return is, nil, where(err)
			}
			i, ok := comments[c.ID]
			if !ok {
				if ev.Type != "comment.created" {
					continue
				}
				comments[c.ID] = len(is.Comments)
				is.Comments = append(is.Comments, replayComment{
					ID: c.ID, CreatedAt: c.CreatedAt, Actor: ev.Actor, ArtifactID: c.ArtifactID,
					ReplyTo: c.ReplyTo, AskID: c.AskID,
				})
				i = len(is.Comments) - 1
			}
			cm := &is.Comments[i]
			cm.Body, cm.Anchor = c.Body, c.Anchor
			cm.Open, cm.ResolvedAt, cm.ResolvedBy = !c.Resolved, c.ResolvedAt, c.ResolvedBy
			if !c.Resolved {
				cm.ResolvedAt, cm.ResolvedBy = nil, nil
			}
		case strings.HasPrefix(ev.Type, "ask."):
			var a wireAsk
			if err := json.Unmarshal(ev.Payload, &a); err != nil {
				return is, nil, where(err)
			}
			if a.ID == "" || a.State == "" {
				continue
			}
			ra := replayAsk{
				ID: a.ID, Kind: a.Kind, State: a.State, Question: a.Question, Options: a.Options,
				Multiple: a.Multiple, Urgency: a.Urgency, Author: a.Author, CreatedAt: a.CreatedAt,
				ArtifactID: a.ArtifactID, Anchor: a.Anchor, BlockID: a.BlockID, Answer: a.Answer, Approval: a.Approval,
			}
			if a.BlockArtifact != nil {
				ra.BlockArtifactID = &a.BlockArtifact.ID
			}
			if a.Resolution != nil {
				ra.Resolution = &replayAskResolution{Kind: a.Resolution.Kind, Reason: a.Resolution.Reason, By: a.Resolution.Actor, At: a.Resolution.At}
			}
			if i, ok := asks[a.ID]; ok {
				askList[i] = ra
			} else {
				asks[a.ID] = len(askList)
				askList = append(askList, ra)
			}
		case ev.Type == "artifact.created":
			var p struct {
				Artifact struct {
					ID string `json:"id"`
				} `json:"artifact"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return is, nil, where(err)
			}
			rec.CreatedBefore[lg.Key] = append(rec.CreatedBefore[lg.Key], p.Artifact.ID)
		}
	}
	if !is.Exists {
		return replayIssue{Key: lg.Key, Number: lg.Number}, window, nil
	}
	// A resolved typed ask block is kept while its block may be in its document's text at At,
	// where its state is the resolve's; dropResolvedOutsideText drops the rest once the texts are
	// read.
	for _, a := range askList {
		if a.State == "open" || a.State == "answered" || (a.State == "resolved" && a.BlockID != nil && a.Resolution != nil) {
			is.Asks = append(is.Asks, a)
		}
	}
	root := func(id string) string {
		if r, ok := threadRoot[id]; ok {
			return r
		}
		cur, seen := id, map[string]bool{}
		for {
			next, ok := replyTo[cur]
			if !ok || seen[next] {
				return cur
			}
			seen[cur] = true
			cur = next
		}
	}
	refs := map[string]bool{}
	for _, ev := range window {
		if !commentStates[ev.Type] {
			continue
		}
		var c wireComment
		if err := json.Unmarshal(ev.Payload, &c); err != nil {
			return is, nil, fmt.Errorf("%s event %d (%s): %w", lg.Key, ev.ID, ev.Type, err)
		}
		if ev.Type == "comment.created" {
			if c.ReplyTo != nil {
				refs[*c.ReplyTo] = true
				refs[root(c.ID)] = true
			}
		} else {
			refs[c.ID] = true
		}
	}
	for i := range is.Comments {
		c := &is.Comments[i]
		if c.ReplyTo != nil {
			r := root(c.ID)
			c.ThreadRootID = &r
		}
		c.WindowRefs = refs[c.ID]
	}
	return is, window, nil
}

// attachArtifacts sets each existing issue's artifacts from today's listings: those with a version
// before at. It returns how many artifacts an artifact.created made before at that today's listing
// lacks.
func attachArtifacts(st *replayState, listings map[string][]listedArtifact, createdBefore map[string][]string, at time.Time) int {
	unlisted := 0
	for i := range st.Issues {
		is := &st.Issues[i]
		if !is.Exists {
			continue
		}
		listedIDs := map[string]bool{}
		for _, la := range listings[is.Key] {
			listedIDs[la.ID] = true
			v, ok := versionAt(la.Versions, at)
			if !ok || !la.CreatedAt.Before(at) {
				continue
			}
			is.Artifacts = append(is.Artifacts, replayArtifact{
				ID: la.ID, Name: la.Name, Slug: la.Slug, Kind: la.Kind, Primary: la.Primary,
				CreatedAt: la.CreatedAt, Version: v.Number, Size: v.Size, MIME: v.MIME,
				VersionSizes: versionSizes(la.Versions, v.Number),
			})
		}
		for _, id := range createdBefore[is.Key] {
			if !listedIDs[id] {
				unlisted++
			}
		}
	}
	return unlisted
}

// createdDocIDs are the documents artifact.created window events made, in window order.
func createdDocIDs(window []replayEvent) []string {
	var ids []string
	seen := map[string]bool{}
	for _, ev := range window {
		if ev.Type != "artifact.created" {
			continue
		}
		var p struct {
			Artifact struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"artifact"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || p.Artifact.Kind != "doc" || seen[p.Artifact.ID] {
			continue
		}
		seen[p.Artifact.ID] = true
		ids = append(ids, p.Artifact.ID)
	}
	return ids
}

// createdSpec is an issue an issue.created window event made, with its primary (spec) document.
type createdSpec struct {
	Key string
	ID  string // "" when neither the event nor the issue's artifact listing names it
}

// createdSpecs are the issues issue.created window events made, in window order, each with its
// spec's id: the event's primary_artifact_id, else the primary document of the issue's listing.
func (f *replayFetcher) createdSpecs(ctx context.Context, window []replayEvent) ([]createdSpec, error) {
	var specs []createdSpec
	seen := map[string]bool{}
	for _, ev := range window {
		if ev.Type != "issue.created" {
			continue
		}
		var p struct {
			Key               string `json:"key"`
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		key := p.Key
		if ev.IssueKey != nil {
			key = *ev.IssueKey
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		specs = append(specs, createdSpec{Key: key, ID: p.PrimaryArtifactID})
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(f.conc)
	for i := range specs {
		if specs[i].ID != "" {
			continue
		}
		g.Go(func() error {
			k := specs[i].Key
			body, ok, err := f.cachedOrUnavailable(gctx, filepath.Join("artifacts", k+".json"), "/api/v1/issues/{key}/artifacts",
				"/api/v1/issues/"+url.PathEscape(k)+"/artifacts")
			if err != nil || !ok {
				return err
			}
			var listing []listedArtifact
			if err := json.Unmarshal(body, &listing); err != nil {
				return fmt.Errorf("%s artifacts: %w", k, err)
			}
			for _, la := range listing {
				if la.Primary {
					specs[i].ID = la.ID
					break
				}
			}
			return nil
		})
	}
	return specs, g.Wait()
}

// buildRoster lists the sessions the replay acts as: claim holders at at, routed sessions, then
// window writers, each once under its first source. An origin comes from the naming actor when it
// carries one, else from origins.
func buildRoster(st replayState, window []replayEvent, origins map[string]replayOrigin) []replayRosterEntry {
	var out []replayRosterEntry
	seen := map[string]bool{}
	add := func(id, source string, origin *replayOrigin) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		e := replayRosterEntry{SessionID: id, Source: source}
		o, ok := origins[id]
		if origin != nil {
			o, ok = *origin, true
		}
		if ok {
			e.SessionTitle, e.Machine, e.Cwd = o.SessionTitle, o.Machine, o.Cwd
		}
		out = append(out, e)
	}
	for _, is := range st.Issues {
		if is.Claim != nil && is.Claim.Actor.Kind == "session" {
			add(is.Claim.Actor.ID, "claim", is.Claim.Actor.Origin)
		}
	}
	for _, is := range st.Issues {
		if is.Route != nil && strings.HasPrefix(*is.Route, "session:") {
			add(strings.TrimPrefix(*is.Route, "session:"), "route", nil)
		}
	}
	for _, ev := range window {
		if ev.Actor.Kind == "session" {
			add(ev.Actor.ID, "window", ev.Actor.Origin)
		}
	}
	return out
}

func countExisting(issues []replayIssue) int {
	n := 0
	for _, is := range issues {
		if is.Exists {
			n++
		}
	}
	return n
}

// stateCounts is the part of replayCounts the state and window alone decide.
func stateCounts(st replayState, window []replayEvent) replayCounts {
	c := replayCounts{WindowEvents: len(window), WindowByType: map[string]int{}}
	openRoots := map[string]int{} // document id -> open anchored root comments at at
	docs := map[string]bool{}
	for _, is := range st.Issues {
		if !is.Exists {
			c.Placeholders++
			continue
		}
		c.Issues++
		for _, a := range is.Artifacts {
			if a.Kind == "doc" {
				c.Documents++
				docs[a.ID] = true
			} else {
				c.Files++
			}
		}
		for _, cm := range is.Comments {
			if !cm.Open {
				c.CommentsResolved++
				continue
			}
			c.CommentsOpen++
			if cm.Anchor != nil {
				c.AnchoredOpen++
				if cm.ReplyTo == nil {
					openRoots[cm.Anchor.ArtifactID]++
				}
			}
		}
		for _, a := range is.Asks {
			if a.State == "resolved" {
				c.AsksResolvedBlocks++
			} else {
				c.AsksOpen++
			}
		}
	}
	written := map[string]bool{}
	for _, ev := range window {
		c.WindowByType[ev.Type]++
		switch ev.Type {
		case "comment.created":
			var p struct {
				Anchor *replayAnchor `json:"anchor"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil && p.Anchor != nil && docs[p.Anchor.ArtifactID] {
				written[p.Anchor.ArtifactID] = true
			}
		case "artifact.version":
			var p struct {
				ArtifactID string `json:"artifact_id"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil && docs[p.ArtifactID] {
				written[p.ArtifactID] = true
			}
		}
	}
	var perDoc []int
	for id := range written {
		perDoc = append(perDoc, openRoots[id])
		c.WrittenDocsOpenRootsSum += openRoots[id]
	}
	c.WrittenDocs = len(perDoc)
	if len(perDoc) > 0 {
		sort.Ints(perDoc)
		mid := len(perDoc) / 2
		if len(perDoc)%2 == 1 {
			c.WrittenDocsOpenRootsMedian = float64(perDoc[mid])
		} else {
			c.WrittenDocsOpenRootsMedian = float64(perDoc[mid-1]+perDoc[mid]) / 2
		}
	}
	return c
}

// --- version sizes, answer versions, block maps --------------------------------------------------

// versionSizes are the listed sizes of versions 1..final, -1 for a number unlisted or sizeless.
func versionSizes(versions []listedVersion, final int) []int {
	out := make([]int, final)
	for i := range out {
		out[i] = -1
	}
	for _, v := range versions {
		if v.Number >= 1 && v.Number <= final && v.Size != nil {
			out[v.Number-1] = *v.Size
		}
	}
	return out
}

func versionSizesMissing(docs []*replayArtifact) int {
	n := 0
	for _, a := range docs {
		for _, s := range a.VersionSizes {
			if s < 0 {
				n++
			}
		}
	}
	return n
}

// setBlockVersions sets each typed ask block's AnswerVersion (answered) and ResolveVersion
// (resolved): the first version of its document listed at or after the answer or resolve, at most
// the document's version at At.
func setBlockVersions(st *replayState, byID map[string]listedArtifact) {
	for i := range st.Issues {
		is := &st.Issues[i]
		final := map[string]int{}
		for _, a := range is.Artifacts {
			final[a.ID] = a.Version
		}
		for j := range is.Asks {
			a := &is.Asks[j]
			if a.BlockID == nil || a.BlockArtifactID == nil {
				continue
			}
			first := func(at time.Time) int {
				best := 0
				for _, v := range byID[*a.BlockArtifactID].Versions {
					if !v.CreatedAt.Before(at) && v.Number <= final[*a.BlockArtifactID] && (best == 0 || v.Number < best) {
						best = v.Number
					}
				}
				return best
			}
			if a.Answer != nil && a.State != "open" {
				a.AnswerVersion = first(a.Answer.At)
			}
			if a.State == "resolved" && a.Resolution != nil {
				a.ResolveVersion = first(a.Resolution.At)
			}
		}
	}
}

// dropResolvedOutsideText drops each resolved typed ask block whose block is not in its
// document's text at At (texts by document id): nothing at At shows it.
func dropResolvedOutsideText(st *replayState, texts map[string]string) {
	for i := range st.Issues {
		is := &st.Issues[i]
		is.Asks = slices.DeleteFunc(is.Asks, func(a replayAsk) bool {
			if a.State != "resolved" {
				return false
			}
			return a.BlockArtifactID == nil || !slices.Contains(askBlockIDs(texts[*a.BlockArtifactID]), *a.BlockID)
		})
	}
}

// askBlockIDs are the block ids of the typed ask block directives in markdown.
func askBlockIDs(markdown string) []string {
	var ids []string
	for _, line := range strings.Split(markdown, "\n") {
		if m := askDirective.FindStringSubmatch(line); m != nil {
			ids = append(ids, m[1])
		}
	}
	return ids
}

// readDocTexts reads the bundle's texts at At by document id.
func readDocTexts(out string, docs []*replayArtifact) (map[string]string, error) {
	texts := map[string]string{}
	for _, a := range docs {
		if a.TextPath == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(a.TextPath)))
		if err != nil {
			return nil, err
		}
		texts[a.ID] = string(b)
	}
	return texts, nil
}

// windowAnchor is the anchor a window comment.created or ask.opened payload carries.
type windowAnchor struct {
	ID     string        `json:"id"`
	Anchor *replayAnchor `json:"anchor"`
	Ask    *struct {
		ID     string        `json:"id"`
		Anchor *replayAnchor `json:"anchor"`
	} `json:"ask"`
}

func (w windowAnchor) anchor() (string, *replayAnchor) {
	if w.Anchor != nil {
		return w.ID, w.Anchor
	}
	if w.Ask != nil && w.Ask.Anchor != nil {
		return w.Ask.ID, w.Ask.Anchor
	}
	return "", nil
}

// anchoredDocs are the document ids an anchor in the state or the window names, sorted.
func anchoredDocs(st *replayState, window []replayEvent) []string {
	set := map[string]bool{}
	for _, is := range st.Issues {
		for _, c := range is.Comments {
			if c.Anchor != nil {
				set[c.Anchor.ArtifactID] = true
			}
		}
		for _, a := range is.Asks {
			if a.Anchor != nil {
				set[a.Anchor.ArtifactID] = true
			}
		}
	}
	for _, ev := range window {
		if ev.Type != "comment.created" && ev.Type != "ask.opened" {
			continue
		}
		var w windowAnchor
		if json.Unmarshal(ev.Payload, &w) == nil {
			if _, a := w.anchor(); a != nil && a.ArtifactID != "" {
				set[a.ArtifactID] = true
			}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// servedBlocks is GET /api/v1/artifacts/{id}/blocks: byte ranges into the current markdown.
type servedBlock struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	From  int    `json:"from"`
	To    int    `json:"to"`
	Table *struct {
		Rows []struct {
			ID    string `json:"id"`
			Cells []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"cells"`
		} `json:"rows"`
	} `json:"table"`
}

// buildBlockMap cuts each served block's text out of the markdown it indexes; a block whose range
// falls outside it is left out and counted.
func buildBlockMap(version int, markdown string, served []servedBlock) (*replayBlockMap, int) {
	bm := &replayBlockMap{Version: version, Blocks: []replayBlock{}}
	bad := 0
	for _, s := range served {
		if s.From < 0 || s.To < s.From || s.To > len(markdown) {
			bad++
			continue
		}
		b := replayBlock{ID: s.ID, Type: s.Type, Text: markdown[s.From:s.To]}
		if s.Table != nil {
			for _, r := range s.Table.Rows {
				b.Inner = append(b.Inner, replayInnerBlock{ID: r.ID})
				for _, c := range r.Cells {
					b.Inner = append(b.Inner, replayInnerBlock{ID: c.ID, Text: c.Text})
				}
			}
		}
		bm.Blocks = append(bm.Blocks, b)
	}
	return bm, bad
}

// cachedOrUnavailable is cached for a route whose refusal (any non-2xx but 401 and 403) is kept as
// <rel>.unavailable, so a rerun does not ask again: ok is false for a refusal.
func (f *replayFetcher) cachedOrUnavailable(ctx context.Context, rel, route, path string) ([]byte, bool, error) {
	bad := filepath.Join(f.raw, rel+".unavailable")
	if _, err := os.Stat(bad); err == nil {
		return nil, false, nil
	}
	body, err := f.cached(ctx, rel, route, path)
	var se *httpStatusError
	if errors.As(err, &se) && se.status != http.StatusUnauthorized && se.status != http.StatusForbidden {
		return nil, false, writeFileAtomic(bad, []byte(strconv.Itoa(se.status)+"\n"))
	}
	return body, err == nil, err
}

// fetchBlockMaps reads the current blocks and markdown of every anchored document into
// st.BlockMaps, returning how many production would not serve.
func (f *replayFetcher) fetchBlockMaps(ctx context.Context, st *replayState, window []replayEvent, byID map[string]listedArtifact, out string) (int, error) {
	stateDocs := map[string]*replayArtifact{}
	for i := range st.Issues {
		for j := range st.Issues[i].Artifacts {
			a := &st.Issues[i].Artifacts[j]
			stateDocs[a.ID] = a
		}
	}
	ids := anchoredDocs(st, window)
	st.BlockMaps = map[string]*replayBlockMap{}
	var mu sync.Mutex
	var unavailable, outOfRange atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(f.conc)
	for _, id := range ids {
		g.Go(func() error {
			body, ok, err := f.cachedOrUnavailable(gctx, filepath.Join("blocks", id+".json"), "/api/v1/artifacts/{id}/blocks",
				"/api/v1/artifacts/"+url.PathEscape(id)+"/blocks")
			if err != nil || !ok {
				if err == nil {
					unavailable.Add(1)
				}
				return err
			}
			la, listed := byID[id]
			if !listed {
				meta, ok, err := f.cachedOrUnavailable(gctx, filepath.Join("artifact", id+".json"), "/api/v1/artifacts/{id}",
					"/api/v1/artifacts/"+url.PathEscape(id))
				if err != nil || !ok {
					if err == nil {
						unavailable.Add(1)
					}
					return err
				}
				if err := json.Unmarshal(meta, &la); err != nil {
					return fmt.Errorf("artifact %s: %w", id, err)
				}
			}
			cur := 0
			for _, v := range la.Versions {
				cur = max(cur, v.Number)
			}
			var markdown string
			if a := stateDocs[id]; a != nil && a.TextPath != "" && cur == a.Version && a.TextVersion == a.Version {
				b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(a.TextPath)))
				if err != nil {
					return err
				}
				markdown = string(b)
			} else {
				b, ok, err := f.docText(gctx, id, cur)
				if err != nil || !ok {
					if err == nil {
						unavailable.Add(1)
					}
					return err
				}
				markdown = string(b)
			}
			var served []servedBlock
			if err := json.Unmarshal(body, &served); err != nil {
				return fmt.Errorf("artifact %s blocks: %w", id, err)
			}
			bm, bad := buildBlockMap(cur, markdown, served)
			outOfRange.Add(int64(bad))
			mu.Lock()
			st.BlockMaps[id] = bm
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return 0, err
	}
	if n := outOfRange.Load(); n > 0 {
		f.lg.Printf("block maps: %d blocks with a range outside their markdown left out", n)
	}
	return int(unavailable.Load()), nil
}

// countAnchorRules counts the state's and the window's anchors by the branch chooseAnchor takes on
// the text at At (rng seed 1); an anchor on a document with no text at At counts as no_text.
func countAnchorRules(st replayState, window []replayEvent, texts map[string]string) map[string]int {
	counts := map[string]int{}
	add := func(id string, a *replayAnchor) {
		text, ok := texts[a.ArtifactID]
		if !ok {
			counts["no_text"]++
			return
		}
		blockID := ""
		if a.BlockID != nil {
			blockID = *a.BlockID
		}
		counts[chooseAnchor(text, st.BlockMaps[a.ArtifactID], blockID, a.Quote, a.Orphaned, false, 1, a.ArtifactID, id).Branch]++
	}
	for _, is := range st.Issues {
		for _, c := range is.Comments {
			if c.Anchor != nil {
				add(c.ID, c.Anchor)
			}
		}
		for _, a := range is.Asks {
			if a.Anchor != nil {
				add(a.ID, a.Anchor)
			}
		}
	}
	for _, ev := range window {
		if ev.Type != "comment.created" {
			continue
		}
		var w windowAnchor
		if json.Unmarshal(ev.Payload, &w) == nil {
			if id, a := w.anchor(); a != nil {
				add(id, a)
			}
		}
	}
	return counts
}

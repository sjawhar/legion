package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// refsSampleJSON is the measured shape of production's references answers on 2026-10-27: an array
// of 160 arrays, one per sampled issue in key order, holding the version count of every member of
// that issue's answer. It names no issue and no host, and nothing else of the sample is used.
//
//go:embed refs-sample-2026-10-27.json
var refsSampleJSON []byte

const (
	// refRNGSeed fixes every draw of the reference plan, so the same seed writes the same
	// documents with the same writers.
	refRNGSeed = 650
	// refTargetCount is how many cited documents the plan writes, refWriterPool how many session
	// actors write them, and refAuthorBytes the size production's author list of one version row
	// has on average, which each writer's padded origin reproduces.
	refTargetCount  = 1000
	refWriterPool   = 600
	refAuthorBytes  = 231
	refWorkers      = 8
	refTargetPrefix = "ref-target-"
	refSourceName   = "references.md"
	// refTimeout is this client's own timeout: a references answer of a seeded LGA issue is
	// megabytes, and the 2-vCPU box can take minutes to build one.
	refTimeout = 5 * time.Minute
	// The shape check's figures from production, with the band each one holds in.
	refProdRows      = 3187
	refProdBytes     = 231
	refShapeBand     = 0.25
	refDistinctLow   = 0.03
	refDistinctHigh  = 0.12
	refProgressEvery = 1000
)

// refTarget is one document to write: its number, the issue it lives on, how many versions it
// gets, the writers of the pool that write it (writers[0] creates it) and the writer of each
// version.
type refTarget struct {
	n        int
	issue    string
	versions int
	writers  []int
	order    []int
}

// refPlan is the whole write: every target, the target numbers each LGA issue cites, and the LGA
// keys in the order the sources are written.
type refPlan struct {
	targets []refTarget
	sources map[string][]int
	lga     []string
}

// refActor is a write's actor. It is a struct, not sessionActor's map, because the origin's
// padding counts the bytes this encodes to.
type refActor struct {
	Kind   string     `json:"kind"`
	ID     string     `json:"id"`
	Origin *refOrigin `json:"origin,omitempty"`
}

type refOrigin struct {
	Host         string `json:"host,omitempty"`
	Machine      string `json:"machine,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	SessionTitle string `json:"session_title,omitempty"`
}

func refsSample() ([][]int, error) {
	var rows [][]int
	if err := json.Unmarshal(refsSampleJSON, &rows); err != nil {
		return nil, fmt.Errorf("reference sample: %w", err)
	}
	if len(rows) == 0 {
		return nil, errors.New("the reference sample holds no issue")
	}
	return rows, nil
}

// makeRefPlan draws the whole write from one PCG generator, so it depends on nothing but the
// sample and the seed's keys: every target's version count comes from the pooled per-member
// version counts of the sample, and LGA issue i cites as many targets as the sample's issue at
// position i mod 160 had members.
func makeRefPlan(sf seedFile, sample [][]int) (refPlan, error) {
	rng := rand.New(rand.NewPCG(refRNGSeed, refRNGSeed))
	var pooled []int
	for _, row := range sample {
		pooled = append(pooled, row...)
	}
	var hosts, lga []string
	for _, is := range sf.Issues {
		switch is.Project {
		case "LGA":
			lga = append(lga, is.Key)
		case "LGB", "LGC", "LGD":
			hosts = append(hosts, is.Key)
		}
	}
	if len(pooled) == 0 || len(hosts) == 0 || len(lga) == 0 {
		return refPlan{}, fmt.Errorf("the reference plan needs sampled version counts (%d), host issues of LGB, LGC and LGD (%d) and LGA issues (%d)", len(pooled), len(hosts), len(lga))
	}
	sort.Strings(hosts)
	sort.Strings(lga)
	p := refPlan{sources: map[string][]int{}, lga: lga}
	for n := range refTargetCount {
		t := refTarget{n: n, issue: hosts[n%len(hosts)], versions: pooled[rng.IntN(len(pooled))]}
		perm := rng.Perm(refWriterPool)
		t.writers = perm[:1+rng.IntN(3)]
		t.order = make([]int, t.versions)
		for v := range t.order {
			if v == 0 {
				t.order[v] = t.writers[0]
			} else {
				t.order[v] = t.writers[rng.IntN(len(t.writers))]
			}
		}
		p.targets = append(p.targets, t)
	}
	for i, key := range lga {
		m := len(sample[i%len(sample)])
		cited := rng.Perm(refTargetCount)[:min(m, refTargetCount)]
		sort.Ints(cited)
		p.sources[key] = cited
	}
	return p, nil
}

// refWriter returns session actor w of the pool, its origin padded so the one-actor author list
// encodes to refAuthorBytes bytes, as production's do on average.
func refWriter(w int) refActor {
	a := refActor{Kind: "session", ID: fmt.Sprintf("ses-ref-%04d", w), Origin: &refOrigin{
		Host:    fmt.Sprintf("example-host-%03d", w%40),
		Machine: fmt.Sprintf("example-machine-%03d", w%40),
		Cwd:     fmt.Sprintf("/work/example/agents/ref-%04d/workspace", w),
	}}
	base, _ := json.Marshal([]refActor{a})
	title := fmt.Sprintf("Reference writer %04d ", w)
	pad := refAuthorBytes - len(base) - len(`,"session_title":""`)
	if pad > len(title) {
		title += strings.Repeat("x", pad-len(title))
	}
	a.Origin.SessionTitle = title[:pad]
	return a
}

// refsAPI is the package's Dispatch client with the longer timeout the reference writes and reads
// need.
func refsAPI(ep endpoints) *api {
	a := newAPI(ep.dispatch, ep.token, refWorkers)
	a.client.Timeout = refTimeout
	return a
}

// refsFlags are the flags every reference subcommand shares: the endpoints, and the seed.json the
// issue keys come from.
type refsFlags struct {
	endpointFlags
	seed string
}

func (f *refsFlags) register(fs *flag.FlagSet) {
	f.endpointFlags.register(fs)
	fs.StringVar(&f.seed, "seed", "", "seed.json path (default $LOADTEST_ROOT/seed.json)")
}

// resolveRefs checks the stack is the local one and reads its seed.
func (f *refsFlags) resolveRefs(ctx context.Context) (*api, seedFile, error) {
	ep, err := f.endpointFlags.resolve()
	if err != nil {
		return nil, seedFile{}, err
	}
	if err := checkStack(ctx, ep); err != nil {
		return nil, seedFile{}, err
	}
	path := f.seed
	if path == "" {
		path = filepath.Join(loadtestRoot(), "seed.json")
	}
	sf, err := readSeed(path)
	if err != nil {
		return nil, seedFile{}, err
	}
	return refsAPI(ep), sf, nil
}

func runSeedRefs(args []string) error {
	fs := flag.NewFlagSet("seed-refs", flag.ExitOnError)
	var f refsFlags
	f.register(fs)
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ctx := context.Background()
	a, sf, err := f.resolveRefs(ctx)
	if err != nil {
		return err
	}
	sample, err := refsSample()
	if err != nil {
		return err
	}
	p, err := makeRefPlan(sf, sample)
	if err != nil {
		return err
	}
	start := time.Now()
	versions, err := writeRefTargets(ctx, a, p)
	if err != nil {
		return err
	}
	issues, cites, err := writeRefSources(ctx, a, p)
	if err != nil {
		return err
	}
	fmt.Printf("seed-refs: %d targets, %d target versions, %s on %d issues citing %d targets in %s\n",
		len(p.targets), versions, refSourceName, issues, cites, time.Since(start).Round(time.Second))
	return nil
}

// writeRefTargets writes every version of every target, longest target first so the long tail
// does not finish alone at the end. One target's versions are written in order by one worker.
func writeRefTargets(ctx context.Context, a *api, p refPlan) (int, error) {
	start := time.Now()
	total := 0
	for _, t := range p.targets {
		total += t.versions
	}
	order := append([]refTarget(nil), p.targets...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].versions > order[j].versions })

	jobs := make(chan refTarget)
	var done, written atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	for range refWorkers {
		g.Go(func() error {
			for t := range jobs {
				name := fmt.Sprintf("%s%d.md", refTargetPrefix, t.n)
				for v := range t.versions {
					body := map[string]any{
						"name":    name,
						"content": fmt.Sprintf("# Reference target %d\n\nRevision %d of a document other issues cite.\n", t.n, v+1),
						"actor":   refWriter(t.order[v]),
					}
					res := a.callRetry(gctx, "POST", "/api/v1/issues/"+t.issue+"/artifacts", who{}, body)
					if !res.ok() {
						return fmt.Errorf("write %s version %d on %s: %d %s %v", name, v+1, t.issue, res.status, trunc(res.body), res.err)
					}
					if n := written.Add(1); n%refProgressEvery == 0 {
						log.Printf("seed-refs: %d/%d versions, %d targets done, %s", n, total, done.Load(), time.Since(start).Round(time.Second))
					}
				}
				done.Add(1)
			}
			return nil
		})
	}
	g.Go(func() error {
		defer close(jobs)
		for _, t := range order {
			select {
			case jobs <- t:
			case <-gctx.Done():
				return nil
			}
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return 0, err
	}
	log.Printf("seed-refs: wrote %d targets, %d versions in %s", len(p.targets), total, time.Since(start).Round(time.Second))
	return total, nil
}

// writeRefSources writes one references.md per LGA issue that cites at least one target. Each
// citation is a dispatch:// reference to a target document, which is what makes the references
// answer of that issue do member work.
func writeRefSources(ctx context.Context, a *api, p refPlan) (issues, cites int, err error) {
	start := time.Now()
	jobs := make(chan string)
	g, gctx := errgroup.WithContext(ctx)
	for range refWorkers {
		g.Go(func() error {
			for key := range jobs {
				var b strings.Builder
				b.WriteString("# References\n\nThe documents this issue builds on:\n\n")
				for _, n := range p.sources[key] {
					fmt.Fprintf(&b, "- dispatch://%s/artifact/%s%d-md\n", p.targets[n].issue, refTargetPrefix, n)
				}
				body := map[string]any{"name": refSourceName, "content": b.String(), "actor": refWriter(0)}
				res := a.callRetry(gctx, "POST", "/api/v1/issues/"+key+"/artifacts", who{}, body)
				if !res.ok() {
					return fmt.Errorf("write %s on %s: %d %s %v", refSourceName, key, res.status, trunc(res.body), res.err)
				}
			}
			return nil
		})
	}
	for _, key := range p.lga {
		if len(p.sources[key]) == 0 {
			continue
		}
		issues++
		cites += len(p.sources[key])
	}
	g.Go(func() error {
		defer close(jobs)
		for _, key := range p.lga {
			if len(p.sources[key]) == 0 {
				continue
			}
			select {
			case jobs <- key:
			case <-gctx.Done():
				return nil
			}
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return 0, 0, err
	}
	log.Printf("seed-refs: wrote %s on %d of %d LGA issues, citing %d targets in %s", refSourceName, issues, len(p.lga), cites, time.Since(start).Round(time.Second))
	return issues, cites, nil
}

// refsAnswer is what the shape check reads of a references answer: the author list of every
// version row of every member.
type refsAnswer struct {
	Members []struct {
		Artifact struct {
			Versions []struct {
				Authors json.RawMessage `json:"authors"`
			} `json:"versions"`
		} `json:"artifact"`
	} `json:"members"`
}

func runRefsShape(args []string) error {
	fs := flag.NewFlagSet("refs-shape", flag.ExitOnError)
	var f refsFlags
	f.register(fs)
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ctx := context.Background()
	a, sf, err := f.resolveRefs(ctx)
	if err != nil {
		return err
	}
	return refsShape(ctx, a, sf)
}

// refsShape reads the references answer of every LGA issue, with no query, and compares what the
// seed gives with what production gives: the version rows per answer, the share of author lists
// that differ, and the bytes an author list takes.
func refsShape(ctx context.Context, a *api, sf seedFile) error {
	var members, rows, bodyBytes, authorBytes []float64
	distinct, allRows := 0.0, 0.0
	for _, is := range sf.Issues {
		if is.Project != "LGA" {
			continue
		}
		res := a.callRetry(ctx, "GET", "/api/v1/issues/"+is.Key+"/references", who{}, nil)
		if !res.ok() {
			return fmt.Errorf("references of %s: %d %s %v", is.Key, res.status, trunc(res.body), res.err)
		}
		var answer refsAnswer
		if err := json.Unmarshal(res.body, &answer); err != nil {
			return fmt.Errorf("references of %s: %w", is.Key, err)
		}
		seen := map[string]bool{}
		r, ab := 0, 0
		for _, m := range answer.Members {
			for _, v := range m.Artifact.Versions {
				var compact bytes.Buffer
				_ = json.Compact(&compact, v.Authors)
				seen[compact.String()] = true
				ab += compact.Len()
				r++
			}
		}
		members = append(members, float64(len(answer.Members)))
		rows = append(rows, float64(r))
		bodyBytes = append(bodyBytes, float64(len(res.body)))
		authorBytes = append(authorBytes, float64(ab))
		distinct += float64(len(seen))
		allRows += float64(r)
	}
	if len(rows) == 0 {
		return errors.New("the seed holds no LGA issue")
	}
	var sumRows, sumAB float64
	for i := range rows {
		sumRows += rows[i]
		sumAB += authorBytes[i]
	}
	meanRows := sumRows / float64(len(rows))
	share := distinct / allRows
	perRow := sumAB / sumRows
	fmt.Printf("issues %d (LGA), empty %d\n", len(rows), refCountZero(members))
	fmt.Printf("%-16s %10s %10s %10s %12s\n", "per issue", "p50", "p90", "max", "mean")
	for _, s := range []struct {
		name string
		v    []float64
	}{{"members", members}, {"version rows", rows}, {"response bytes", bodyBytes}} {
		fmt.Printf("%-16s %10.0f %10.0f %10.0f %12.1f\n", s.name, refPct(s.v, 50), refPct(s.v, 90), refPct(s.v, 100), refMean(s.v))
	}
	fmt.Printf("distinct author lists / version rows: %.4f (sum over issues of distinct per issue / rows)\n", share)
	fmt.Printf("author bytes per row: %.1f\n", perRow)
	ok := refWithin(meanRows, refProdRows, refShapeBand) && share >= refDistinctLow && share <= refDistinctHigh && refWithin(perRow, refProdBytes, refShapeBand)
	fmt.Printf("shape check: mean rows %.0f vs %d (±%.0f%%), distinct share %.4f in [%.2f,%.2f], author bytes/row %.1f vs %d (±%.0f%%): %s\n",
		meanRows, refProdRows, refShapeBand*100, share, refDistinctLow, refDistinctHigh, perRow, refProdBytes, refShapeBand*100,
		map[bool]string{true: "PASS", false: "FAIL"}[ok])
	if !ok {
		return errors.New("the references answers do not have production's shape")
	}
	return nil
}

func runRefsFetch(args []string) error {
	fs := flag.NewFlagSet("refs-fetch", flag.ExitOnError)
	var f refsFlags
	f.register(fs)
	out := fs.String("out", "", "output TSV: key, status, ETag, body sha256, bytes")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *out == "" {
		return errors.New("-out is required: the TSV of every issue's references answer")
	}
	ctx := context.Background()
	a, sf, err := f.resolveRefs(ctx)
	if err != nil {
		return err
	}
	return refsFetch(ctx, a, sf, *out)
}

// refsFetch records the references answer of every seeded issue, in key order, so two builds'
// answers compare byte for byte.
func refsFetch(ctx context.Context, a *api, sf seedFile, out string) error {
	keys := make([]string, 0, len(sf.Issues))
	for _, is := range sf.Issues {
		keys = append(keys, is.Key)
	}
	sort.Strings(keys)
	var tsv strings.Builder
	nonEmpty, total := 0, 0
	for _, key := range keys {
		status, etag, body, err := refsGet(ctx, a, "/api/v1/issues/"+key+"/references")
		if err != nil {
			return fmt.Errorf("references of %s: %w", key, err)
		}
		sum := sha256.Sum256(body)
		fmt.Fprintf(&tsv, "%s\t%d\t%s\t%s\t%d\n", key, status, etag, hex.EncodeToString(sum[:]), len(body))
		total += len(body)
		if !bytes.HasPrefix(body, []byte(`{"members":[]`)) {
			nonEmpty++
		}
	}
	if err := os.WriteFile(out, []byte(tsv.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("fetched %d issues, %d with members, %d bytes in all -> %s\n", len(keys), nonEmpty, total, out)
	return nil
}

// refsGet reads one answer with its ETag, over the package client's own connections: a
// callResult carries the body alone, and the fetch records the header too.
func refsGet(ctx context.Context, a *api, path string) (int, string, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("ETag"), body, err
}

// refPct is percentile p of v by nearest rank over a copy, and refMean its mean. The shape check
// reads a few hundred values, so the copy costs nothing.
func refPct(v []float64, p int) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[(len(s)-1)*p/100]
}

func refMean(v []float64) float64 {
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

func refCountZero(v []float64) int {
	n := 0
	for _, x := range v {
		if x == 0 {
			n++
		}
	}
	return n
}

func refWithin(v, want, frac float64) bool { return v >= want*(1-frac) && v <= want*(1+frac) }

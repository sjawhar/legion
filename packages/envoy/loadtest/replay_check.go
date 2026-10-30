package main

// The replay seed's checks: replay-check-seed compares a seeded database with its bundle,
// replay-checksum fingerprints one, and replay-logical-dump writes one with every uuid and
// timestamp masked so that two seeds (a stock and a patched Dispatch, say) compare with cmp. Each
// reads Postgres only and prints keys, names, numbers and digests, never a text.

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// dbFlags are a check's flags: the stack (for its Postgres URL) and the database.
type dbFlags struct {
	ef endpointFlags
	db *string
}

func (f *dbFlags) register(fs *flag.FlagSet) {
	f.ef.register(fs)
	f.db = fs.String("db", "", "database name, overriding the Postgres URL's")
}

// url is the stack's endpoints and its Postgres URL with the database replaced.
func (f *dbFlags) url() (endpoints, string, error) {
	if *f.db == "" {
		return endpoints{}, "", fmt.Errorf("-db is required")
	}
	ep, err := f.ef.resolve()
	if err != nil {
		return endpoints{}, "", err
	}
	u, err := withDatabase(ep.pg, *f.db)
	return ep, u, err
}

func (f *dbFlags) connect(ctx context.Context) (*pgx.Conn, error) {
	_, u, err := f.url()
	if err != nil {
		return nil, err
	}
	return pgx.Connect(ctx, u)
}

// --- replay-check-seed ---------------------------------------------------------------------------

func runReplayCheckSeed(args []string) error {
	fs := flag.NewFlagSet("replay-check-seed", flag.ContinueOnError)
	var df dbFlags
	df.register(fs)
	bundleDir := fs.String("bundle", "", "replay bundle directory")
	keysFile := fs.String("keys-file", "", "the keys file the seed was given")
	out := fs.String("out", "", "write the results as JSON to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bundleDir == "" {
		return fmt.Errorf("-bundle is required")
	}
	b, err := readBundle(*bundleDir)
	if err != nil {
		return err
	}
	var keys map[string]bool
	if *keysFile != "" {
		if keys, err = readKeysFile(*keysFile); err != nil {
			return err
		}
	}
	plans, err := planIssues(b.State, keys)
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := df.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	ids, err := readReplayIDs(ctx, conn)
	if err != nil {
		return err
	}
	r, err := checkSeed(ctx, conn, b, plans, ids)
	if err != nil {
		return err
	}
	sa, err := checkSizesAndAnchors(ctx, conn, b, plans, ids)
	if err != nil {
		return err
	}
	for _, d := range r.Differences {
		fmt.Printf("difference %s %s %s: expected %s, found %s\n", d.Issue, d.Artifact, d.What, d.Expected, d.Found)
	}
	for _, m := range sa.SizeMismatches {
		fmt.Printf("version size mismatch %s %s version %d: expected change %d, found %d\n", m.Issue, m.Artifact, m.Version, m.Expected, m.Found)
	}
	for _, a := range sa.Outside {
		fmt.Printf("anchor outside its chosen block %s %s %s: %s\n", a.Issue, a.Kind, a.LoggedID, a.Why)
	}
	fmt.Printf("documents compared %d; differences %d; fallbacks %d (comments %d, asks %d)\n",
		r.Documents, len(r.Differences), r.FallbackComments+r.FallbackAsks, r.FallbackComments, r.FallbackAsks)
	fmt.Printf("version sizes: %d changes compared, %d mismatches, %d sign flipped, %d without a listed size\n",
		sa.SizeChanges, len(sa.SizeMismatches), sa.SignFlipped, sa.SizesMissing)
	fmt.Printf("anchors: %d recorded, %d outside the block their rule chose; by rule %s\n", sa.Anchors, len(sa.Outside), fmtCounts(sa.Rules))
	fmt.Printf("documents with answered or resolved typed ask blocks %d, compared with answered_at masked: %d equal\n", r.AskBlockDocs, r.AskBlockDocsEqual)
	for _, d := range r.Distribution {
		fmt.Printf("open anchored roots on written primary documents (%s): documents %d, median %.1f, write-weighted mean %.2f\n",
			d.When, d.Documents, d.Median, d.WeightedMean)
	}
	res := checkResult{
		Documents: r.Documents, Differences: len(r.Differences), VersionSizeMismatches: len(sa.SizeMismatches),
		AnchorsOutsideLoggedBlock: len(sa.Outside), SignFlipped: sa.SignFlipped,
		AnchorRules:    checkAnchorRules{sa.Rules[ruleLoggedQuote], sa.Rules[ruleInBlock], sa.Rules[ruleNearest]},
		AllAnchorRules: sa.Rules, DifferenceList: r.Differences, SizeMismatchList: sa.SizeMismatches, OutsideList: sa.Outside,
	}
	res.Passed = res.Differences == 0 && res.VersionSizeMismatches == 0 && res.AnchorsOutsideLoggedBlock == 0
	fmt.Printf("passed %v\n", res.Passed)
	if *out != "" {
		data, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	if !res.Passed {
		return fmt.Errorf("check failed: %d differences, %d version size mismatches, %d anchors outside their block", res.Differences, res.VersionSizeMismatches, res.AnchorsOutsideLoggedBlock)
	}
	return nil
}

// checkResult is replay-check-seed -out's file. Another team's tool reads these field names.
type checkResult struct {
	Documents                 int              `json:"documents"`
	Differences               int              `json:"differences"`
	VersionSizeMismatches     int              `json:"version_size_mismatches"`
	AnchorsOutsideLoggedBlock int              `json:"anchors_outside_logged_block"`
	AnchorRules               checkAnchorRules `json:"anchor_rules"`
	SignFlipped               int              `json:"sign_flipped"`
	Passed                    bool             `json:"passed"`
	// Detail: every branch's count (no_block_map, unanchorable included) and the lists, ids and
	// numbers only.
	AllAnchorRules   map[string]int   `json:"anchor_rules_all"`
	DifferenceList   []seedDifference `json:"difference_list"`
	SizeMismatchList []sizeMismatch   `json:"version_size_mismatch_list"`
	OutsideList      []anchorOutside  `json:"anchors_outside_list"`
}

type checkAnchorRules struct {
	LoggedQuote           int `json:"logged_quote"`
	InLoggedBlock         int `json:"in_logged_block"`
	NearestSurvivingBlock int `json:"nearest_surviving_block"`
}

type sizeMismatch struct {
	Issue    string `json:"issue"`
	Artifact string `json:"artifact"`
	Version  int    `json:"version"`
	Expected int    `json:"expected"`
	Found    int    `json:"found"`
}

type anchorOutside struct {
	Issue    string `json:"issue"`
	Kind     string `json:"kind"`
	LoggedID string `json:"logged_id"`
	Why      string `json:"why"`
}

type sizeAnchorCheck struct {
	SizeChanges, SignFlipped, SizesMissing int
	SizeMismatches                         []sizeMismatch
	Anchors                                int
	Rules                                  map[string]int
	Outside                                []anchorOutside
}

// checkSizesAndAnchors compares each seeded document's version size changes (Dispatch's
// coalesce(size, octet_length(markdown))) with production's (VersionSizes): equal, or equal with
// the sign flipped (counted, not a mismatch), else a mismatch; and checks every anchored item the
// seed should have placed: its branch is recorded in loadtest.replay_anchors and its stored quote
// lies, once, inside the span of the text at At its rule chose. An answer version's expected
// change counts its answers' answered_at as the seed's answer wrote it: Dispatch writes the time
// of the answer with its trailing zeros trimmed, so its length is not production's (the same
// attribute the text comparison masks). A resolve version needs no such count: a resolve writes
// only state="resolved" into its block, no time.
func checkSizesAndAnchors(ctx context.Context, conn *pgx.Conn, b *replayBundle, plans []seedIssuePlan, ids map[[2]string]string) (sizeAnchorCheck, error) {
	r := sizeAnchorCheck{Rules: map[string]int{}}
	type anchorRow struct {
		branch              string
		start, end, quoteAt int
	}
	rows, err := conn.Query(ctx, `select kind, logged_id, branch, span_start, span_end, quote_at from loadtest.replay_anchors`)
	if err != nil {
		return r, fmt.Errorf("read loadtest.replay_anchors: %w", err)
	}
	recorded := map[[2]string]anchorRow{}
	var kind, logged, branch string
	var start, end, at int
	if _, err := pgx.ForEachRow(rows, []any{&kind, &logged, &branch, &start, &end, &at}, func() error {
		recorded[[2]string{kind, logged}] = anchorRow{branch, start, end, at}
		return nil
	}); err != nil {
		return r, err
	}
	for _, p := range plans {
		if p.placeholder() {
			continue
		}
		is := p.Issue
		textByLocal := map[string]string{} // a document's text by its local id ("" for an unmapped one)
		for i := range is.Artifacts {
			a := &is.Artifacts[i]
			if a.Kind != "doc" {
				continue
			}
			raw, err := b.readFile(a.TextPath)
			if err != nil {
				return r, err
			}
			text := string(raw)
			local, ok := ids[[2]string{idArtifact, a.ID}]
			textByLocal[local] = text
			if !ok {
				continue
			}
			vrows, err := conn.Query(ctx, `select coalesce(size, octet_length(markdown)) from artifact_versions where artifact_id = $1 order by number`, local)
			if err != nil {
				return r, err
			}
			sizes, err := pgx.CollectRows(vrows, pgx.RowTo[int])
			if err != nil {
				return r, err
			}
			plan, err := docPlan(is, a, text)
			if err != nil {
				return r, err
			}
			answeredAt := map[int]int{} // version -> seeded less production answered_at bytes
			for v, steps := range plan.AskAt {
				for _, st := range steps {
					ask := &is.Asks[st.Ask]
					localAsk, ok := ids[[2]string{idAsk, ask.ID}]
					if st.Resolve || !ok || ask.Answer == nil {
						continue
					}
					var seeded string
					if err := conn.QueryRow(ctx, `select coalesce(answer->>'at', '') from asks where id = $1`, localAsk).Scan(&seeded); err != nil {
						return r, err
					}
					if seeded != "" {
						answeredAt[v] += len(seeded) - len(ask.Answer.At.UTC().Format(time.RFC3339Nano))
					}
				}
			}
			for n := 2; n <= min(len(sizes), len(a.VersionSizes)); n++ {
				if a.VersionSizes[n-1] < 0 || a.VersionSizes[n-2] < 0 {
					r.SizesMissing++
					continue
				}
				r.SizeChanges++
				want, got := a.VersionSizes[n-1]-a.VersionSizes[n-2]+answeredAt[n], sizes[n-1]-sizes[n-2]
				switch {
				case got == want:
				case got == -want:
					r.SignFlipped++
				default:
					r.SizeMismatches = append(r.SizeMismatches, sizeMismatch{p.Key, a.Name, n, want, got})
				}
			}
		}
		check := func(kind, logged string) {
			row, ok := recorded[[2]string{kind, logged}]
			if !ok {
				r.Outside = append(r.Outside, anchorOutside{p.Key, kind, logged, "no recorded branch"})
				return
			}
			r.Anchors++
			r.Rules[row.branch]++
			local := ids[[2]string{kind, logged}]
			table := "comments"
			if kind == idAsk {
				table = "asks"
			}
			var quote, doc string
			if err := conn.QueryRow(ctx, `select coalesce(anchor->>'quote', ''), coalesce(anchor->>'artifact_id', '') from `+table+` where id::text = $1`, local).Scan(&quote, &doc); err != nil {
				r.Outside = append(r.Outside, anchorOutside{p.Key, kind, logged, "seeded item not found"})
				return
			}
			text := textByLocal[doc]
			i := strings.Index(text, quote)
			switch {
			case quote == "" || text == "":
				r.Outside = append(r.Outside, anchorOutside{p.Key, kind, logged, "no stored quote or document"})
			case row.quoteAt >= 0 && row.quoteAt+len(quote) <= len(text) && text[row.quoteAt:row.quoteAt+len(quote)] == quote &&
				row.quoteAt >= row.start && row.quoteAt+len(quote) <= row.end:
				// A repeated logged quote pinned to its occurrence inside the span.
			case strings.Count(text, quote) != 1 || i < row.start || i+len(quote) > row.end || row.end > len(text):
				r.Outside = append(r.Outside, anchorOutside{p.Key, kind, logged, fmt.Sprintf("stored quote at byte %d, chosen span [%d, %d)", i, row.start, row.end)})
			}
		}
		plan := planComments(is)
		steps := append([]commentStep{}, plan.Main...)
		for _, t := range plan.ByAsk {
			steps = append(steps, t...)
		}
		for _, s := range steps {
			if s.Kind == commentAnchored {
				check(idComment, s.C.ID)
			}
		}
		for _, a := range is.Asks {
			if a.BlockID == nil && a.Approval == nil && a.Anchor != nil {
				check(idAsk, a.ID)
			}
		}
	}
	return r, nil
}

type seedDifference struct {
	Issue, Artifact, What, Expected, Found string
}

type openRootsDistribution struct {
	When         string
	Documents    int
	Median       float64
	WeightedMean float64
}

type seedCheck struct {
	Documents                       int
	Differences                     []seedDifference
	FallbackComments, FallbackAsks  int
	AskBlockDocs, AskBlockDocsEqual int
	Distribution                    []openRootsDistribution
}

// readReplayIDs reads loadtest.replay_ids from any connection or pool (as openAnchored takes a
// QueryRow), so the seed and the check have one reader.
func readReplayIDs(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (map[[2]string]string, error) {
	rows, err := q.Query(ctx, `select kind, logged_id, local_id from loadtest.replay_ids`)
	if err != nil {
		return nil, fmt.Errorf("read loadtest.replay_ids: %w", err)
	}
	out := map[[2]string]string{}
	var k, l, v string
	_, err = pgx.ForEachRow(rows, []any{&k, &l, &v}, func() error {
		out[[2]string{k, l}] = v
		return nil
	})
	return out, err
}

// textDigest names a text by its length and sha256 prefix.
func textDigest(s string) string {
	return fmt.Sprintf("%d bytes sha256 %s", len(s), sha256Hex([]byte(s))[:12])
}

func checkSeed(ctx context.Context, conn *pgx.Conn, b *replayBundle, plans []seedIssuePlan, ids map[[2]string]string) (seedCheck, error) {
	var r seedCheck
	diff := func(issue, artifact, what, expected, found string) {
		r.Differences = append(r.Differences, seedDifference{issue, artifact, what, expected, found})
	}
	ww := countWindowWrites(b.Window)
	var atSeed, withWindow, weights []int
	for _, p := range plans {
		if p.placeholder() {
			continue
		}
		is := p.Issue
		anchored := expectedOpenAnchored(is)
		for _, c := range is.Comments {
			if _, ok := ids[[2]string{idFallbackComment, c.ID}]; ok {
				r.FallbackComments++
			}
		}
		for _, a := range is.Asks {
			if _, ok := ids[[2]string{idFallbackAsk, a.ID}]; ok {
				r.FallbackAsks++
			}
		}
		for i := range is.Artifacts {
			a := &is.Artifacts[i]
			if a.Kind != "doc" {
				continue
			}
			r.Documents++
			local, ok := ids[[2]string{idArtifact, a.ID}]
			if !ok {
				diff(p.Key, a.Name, "mapping", "a local document", "none")
				continue
			}
			var maxV int
			var latest string
			err := conn.QueryRow(ctx, `select coalesce(max(number), 0),
				coalesce((select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1), '')
				from artifact_versions where artifact_id = $1`, local).Scan(&maxV, &latest)
			if err != nil {
				return r, err
			}
			if maxV != a.Version {
				diff(p.Key, a.Name, "version", fmt.Sprint(a.Version), fmt.Sprint(maxV))
			}
			open, err := openAnchored(ctx, conn, local)
			if err != nil {
				return r, err
			}
			if open != anchored[a.ID] {
				diff(p.Key, a.Name, "open-anchored", fmt.Sprint(anchored[a.ID]), fmt.Sprint(open))
			}
			raw, err := b.readFile(a.TextPath)
			if err != nil {
				return r, err
			}
			want, masked := maskAnsweredAt(string(raw))
			got, _ := maskAnsweredAt(latest)
			stepped := masked || resolvedBlock.MatchString(want)
			if stepped {
				r.AskBlockDocs++
			}
			if want != got {
				diff(p.Key, a.Name, "text", textDigest(want), textDigest(got))
			} else if stepped {
				r.AskBlockDocsEqual++
			}
		}
		if w := ww.Writes[p.Key]; w > 0 {
			pa := primaryArtifact(is)
			local, ok := ids[[2]string{idArtifact, pa.ID}]
			if !ok {
				continue
			}
			var n int
			if err := conn.QueryRow(ctx, `select count(*) from comments where anchor->>'artifact_id' = $1 and reply_to is null and not resolved`, local).Scan(&n); err != nil {
				return r, err
			}
			atSeed = append(atSeed, n)
			withWindow = append(withWindow, n+ww.AnchoredRoots[pa.ID])
			weights = append(weights, w)
		}
	}
	for _, d := range []struct {
		when   string
		values []int
	}{{"at the seed", atSeed}, {"with the window's anchored roots", withWindow}} {
		med, mean := distribution(d.values, weights)
		r.Distribution = append(r.Distribution, openRootsDistribution{d.when, len(d.values), med, mean})
	}
	return r, nil
}

// --- replay-checksum -----------------------------------------------------------------------------

func runReplayChecksum(args []string) error {
	fs := flag.NewFlagSet("replay-checksum", flag.ContinueOnError)
	var df dbFlags
	df.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := df.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	sum, err := replayChecksum(ctx, conn)
	if err != nil {
		return err
	}
	out, _ := json.Marshal(sum)
	fmt.Println(string(out))
	return nil
}

type replaySum struct {
	Issues           int64  `json:"issues"`
	Artifacts        int64  `json:"artifacts"`
	ArtifactVersions int64  `json:"artifact_versions"`
	Comments         int64  `json:"comments"`
	Asks             int64  `json:"asks"`
	Events           int64  `json:"events"`
	MD5              string `json:"md5"`
}

// md5Fields feeds each field length-prefixed, so field boundaries are part of the digest.
func md5Fields(h io.Writer, fields ...string) {
	var n [8]byte
	for _, f := range fields {
		binary.LittleEndian.PutUint64(n[:], uint64(len(f)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(f))
	}
}

func replayChecksum(ctx context.Context, conn *pgx.Conn) (replaySum, error) {
	var s replaySum
	if err := conn.QueryRow(ctx, `select (select count(*) from issues), (select count(*) from artifacts),
		(select count(*) from artifact_versions), (select count(*) from comments), (select count(*) from asks),
		(select count(*) from events)`).Scan(&s.Issues, &s.Artifacts, &s.ArtifactVersions, &s.Comments, &s.Asks, &s.Events); err != nil {
		return s, err
	}
	h := md5.New()
	var key, name, slug, md, sha string
	var number int
	rows, err := conn.Query(ctx, `select coalesce(a.issue_key, ''), a.name, a.slug, v.number, coalesce(v.markdown, ''), coalesce(v.sha256, '')
		from artifact_versions v join artifacts a on a.id = v.artifact_id
		order by 1, 2, 3, 4`)
	if err != nil {
		return s, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&key, &name, &slug, &number, &md, &sha}, func() error {
		// A typed ask block's answered_at is the time the seed answered it, so it is masked.
		masked, _ := maskAnsweredAt(md)
		md5Fields(h, "version", key, name, slug, fmt.Sprint(number), masked, sha)
		return nil
	}); err != nil {
		return s, err
	}
	var body string
	rows, err = conn.Query(ctx, `select issue_key, body from comments order by issue_key, created_at, id`)
	if err != nil {
		return s, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&key, &body}, func() error {
		md5Fields(h, "comment", key, body)
		return nil
	}); err != nil {
		return s, err
	}
	s.MD5 = hex.EncodeToString(h.Sum(nil))
	return s, nil
}

// --- replay-logical-dump -------------------------------------------------------------------------

func runReplayLogicalDump(args []string) error {
	fs := flag.NewFlagSet("replay-logical-dump", flag.ContinueOnError)
	var df dbFlags
	df.register(fs)
	out := fs.String("out", "", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	ctx := context.Background()
	conn, err := df.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	n, err := logicalDump(ctx, conn, w)
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d lines to %s\n", n, *out)
	return nil
}

var (
	uuidPattern      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	timestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}(:?\d{2})?)?`)
)

// ordinals numbers ids in order of first appearance within one scope.
type ordinals map[string]int

func (o ordinals) of(id string) int {
	if n, ok := o[id]; ok {
		return n
	}
	o[id] = len(o) + 1
	return o[id]
}

// maskText replaces every uuid in s by its ordinal in o and every timestamp by "<ts>".
func maskText(s string, o ordinals) string {
	s = uuidPattern.ReplaceAllStringFunc(s, func(u string) string { return fmt.Sprintf("<id%d>", o.of(strings.ToLower(u))) })
	return timestampPattern.ReplaceAllString(s, "<ts>")
}

// maskJSON masks a JSON value: every string through maskText, every object's keys kept.
func maskJSON(raw []byte, o ordinals) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "<undecodable>"
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return maskText(x, o)
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		case map[string]any:
			for k := range x {
				x[k] = walk(x[k])
			}
			return x
		}
		return v
	}
	return walk(v)
}

// dumpAnchor is a stored anchor (model.Anchor at 0114), its ids as ordinals in the document's
// scope. 0114 keeps no mark position in any column: a mark lives in the document's Yjs state only.
type dumpAnchor struct {
	Doc      int    `json:"doc"`
	Quote    string `json:"quote"`
	Block    int    `json:"block,omitempty"`
	Mark     int    `json:"mark"`
	Version  int    `json:"version"`
	Orphaned bool   `json:"orphaned"`
}

// logicalDump writes one JSON line per issue, document version, comment and ask, in an order and
// with ids that two seeds of the same bundle share: documents by issue key and name, comments and
// asks by issue key and creation order, every uuid as an ordinal (per document for block and mark
// ids, per issue otherwise) and every timestamp masked.
func logicalDump(ctx context.Context, conn *pgx.Conn, w *bufio.Writer) (int, error) {
	lines := 0
	emit := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		lines++
		_, err = w.Write(append(b, '\n'))
		return err
	}
	type issueRow struct {
		Key, Title, Status      string
		Priority                *int
		Labels                  []string
		Parent, Route, Assignee *string
		Blocked, Claim, Creator []byte
		ClaimedAt               *string
	}
	rows, err := conn.Query(ctx, `select i.key, i.title, i.status, i.priority, i.labels, i.parent_key, i.route, i.assignee,
		coalesce((select json_agg(l.target_key order by l.target_key) from issue_links l where l.issue_key = i.key and l.kind = 'blocked_by'), '[]'),
		i.claimed_by, i.created_by, i.claimed_at::text
		from issues i order by i.project_key, i.number`)
	if err != nil {
		return lines, fmt.Errorf("issues: %w", err)
	}
	issues, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (issueRow, error) {
		var x issueRow
		return x, r.Scan(&x.Key, &x.Title, &x.Status, &x.Priority, &x.Labels, &x.Parent, &x.Route, &x.Assignee,
			&x.Blocked, &x.Claim, &x.Creator, &x.ClaimedAt)
	})
	if err != nil {
		return lines, fmt.Errorf("issues: %w", err)
	}
	scopes := map[string]ordinals{} // issue key or document id -> ordinals
	scope := func(k string) ordinals {
		if scopes[k] == nil {
			scopes[k] = ordinals{}
		}
		return scopes[k]
	}
	for _, x := range issues {
		o := scope(x.Key)
		if err := emit(map[string]any{"issue": x.Key, "title": x.Title, "status": x.Status, "priority": x.Priority,
			"labels": x.Labels, "parent": x.Parent, "route": x.Route, "assignee": x.Assignee, "blocked_by": maskJSON(x.Blocked, o),
			"claimed_by": maskJSON(x.Claim, o), "claimed": x.ClaimedAt != nil, "created_by": maskJSON(x.Creator, o)}); err != nil {
			return lines, err
		}
	}
	// Documents and their versions; a document's ordinal is its place in this order within its issue.
	docOrd := map[string]int{} // local artifact id -> ordinal within its issue
	rows, err = conn.Query(ctx, `select id::text, coalesce(issue_key, project_key), name, slug, kind, is_primary
		from artifacts order by 2, name, slug`)
	if err != nil {
		return lines, fmt.Errorf("artifacts: %w", err)
	}
	type art struct {
		id, key, name, slug, kind string
		primary                   bool
	}
	arts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (art, error) {
		var a art
		return a, r.Scan(&a.id, &a.key, &a.name, &a.slug, &a.kind, &a.primary)
	})
	if err != nil {
		return lines, fmt.Errorf("artifacts: %w", err)
	}
	perIssue := map[string]int{}
	for _, a := range arts {
		perIssue[a.key]++
		docOrd[a.id] = perIssue[a.key]
		if err := emit(map[string]any{"artifact": a.key, "ordinal": docOrd[a.id], "name": a.name, "slug": a.slug, "kind": a.kind, "primary": a.primary}); err != nil {
			return lines, err
		}
		vrows, err := conn.Query(ctx, `select number, coalesce(markdown, ''), coalesce(sha256, ''), coalesce(size, 0), coalesce(mime, ''),
			named, coalesce(summary, ''), authors
			from artifact_versions where artifact_id = $1 order by number`, a.id)
		if err != nil {
			return lines, fmt.Errorf("artifact_versions: %w", err)
		}
		var number, size int
		var md, sha, mimeType, summary string
		var named bool
		var authors []byte
		o := scope(a.id)
		if _, err := pgx.ForEachRow(vrows, []any{&number, &md, &sha, &size, &mimeType, &named, &summary, &authors}, func() error {
			return emit(map[string]any{"version": a.key, "doc": docOrd[a.id], "number": number, "named": named,
				"summary": maskText(summary, o), "size": size, "mime": mimeType, "sha256": sha,
				"authors": maskJSON(authors, o), "markdown": maskText(md, o)})
		}); err != nil {
			return lines, err
		}
	}
	// Asks before comments, so a comment names its ask's ordinal.
	askOrd := map[string]int{}
	type askRow struct {
		ID, Key, Kind, State, Question, Urgency string
		Options, Anchor, Answer, Approval, Res  []byte
		Multiple                                bool
		BlockID                                 *string
		BlockArtifact                           *string
	}
	rows, err = conn.Query(ctx, `select id::text, issue_key, kind, state, question, urgency, options, anchor, answer, approval,
		resolution, multiple, block_id, block_artifact_id::text
		from asks order by issue_key, created_at, coalesce(block_id, ''), question, id`)
	if err != nil {
		return lines, fmt.Errorf("asks: %w", err)
	}
	asks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (askRow, error) {
		var x askRow
		return x, r.Scan(&x.ID, &x.Key, &x.Kind, &x.State, &x.Question, &x.Urgency, &x.Options, &x.Anchor, &x.Answer,
			&x.Approval, &x.Res, &x.Multiple, &x.BlockID, &x.BlockArtifact)
	})
	if err != nil {
		return lines, fmt.Errorf("asks: %w", err)
	}
	perIssue = map[string]int{}
	for _, x := range asks {
		perIssue[x.Key]++
		askOrd[x.ID] = perIssue[x.Key]
		o := scope(x.Key)
		line := map[string]any{"ask": x.Key, "ordinal": askOrd[x.ID], "kind": x.Kind, "state": x.State,
			"question": x.Question, "urgency": x.Urgency, "multiple": x.Multiple, "options": maskJSON(x.Options, o),
			"answer": maskJSON(x.Answer, o), "approval": maskJSON(x.Approval, o), "resolution": maskJSON(x.Res, o),
			"anchor": dumpAnchorOf(x.Anchor, docOrd, scope)}
		if x.BlockArtifact != nil {
			line["block_doc"] = docOrd[*x.BlockArtifact]
			if x.BlockID != nil {
				line["block"] = scope(*x.BlockArtifact).of(strings.ToLower(*x.BlockID))
			}
		}
		if err := emit(line); err != nil {
			return lines, err
		}
	}
	type commentRow struct {
		ID, Key, Body                   string
		Author, Anchor, Sug, ResolvedBy []byte
		ReplyTo, AskID, ArtifactID      *string
		Resolved                        bool
		Turn                            *string
	}
	rows, err = conn.Query(ctx, `select id::text, issue_key, body, author, anchor, suggestion, resolved_by, reply_to::text, ask_id::text,
		artifact_id::text, resolved, turn
		from comments order by issue_key, created_at, id`)
	if err != nil {
		return lines, fmt.Errorf("comments: %w", err)
	}
	comments, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (commentRow, error) {
		var x commentRow
		return x, r.Scan(&x.ID, &x.Key, &x.Body, &x.Author, &x.Anchor, &x.Sug, &x.ResolvedBy, &x.ReplyTo, &x.AskID,
			&x.ArtifactID, &x.Resolved, &x.Turn)
	})
	if err != nil {
		return lines, fmt.Errorf("comments: %w", err)
	}
	commentOrd := map[string]int{}
	perIssue = map[string]int{}
	for _, x := range comments {
		perIssue[x.Key]++
		commentOrd[x.ID] = perIssue[x.Key]
		o := scope(x.Key)
		line := map[string]any{"comment": x.Key, "ordinal": commentOrd[x.ID], "body": x.Body, "resolved": x.Resolved,
			"resolved_by": maskJSON(x.ResolvedBy, o), "author": maskJSON(x.Author, o), "suggestion": maskJSON(x.Sug, o),
			"turn": x.Turn, "anchor": dumpAnchorOf(x.Anchor, docOrd, scope)}
		if x.ArtifactID != nil {
			line["doc"] = docOrd[*x.ArtifactID]
		}
		if x.ReplyTo != nil {
			line["reply_to"] = commentOrd[*x.ReplyTo]
		}
		if x.AskID != nil {
			line["ask"] = askOrd[*x.AskID]
		}
		if err := emit(line); err != nil {
			return lines, err
		}
	}
	return lines, nil
}

// dumpAnchorOf masks a stored anchor; nil when there is none.
func dumpAnchorOf(raw []byte, docOrd map[string]int, scope func(string) ordinals) *dumpAnchor {
	if len(raw) == 0 {
		return nil
	}
	var a struct {
		ArtifactID string  `json:"artifact_id"`
		BlockID    *string `json:"block_id"`
		MarkID     string  `json:"mark_id"`
		Version    int     `json:"version"`
		Quote      string  `json:"quote"`
		Orphaned   bool    `json:"orphaned"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return &dumpAnchor{Quote: "<undecodable>"}
	}
	o := scope(a.ArtifactID)
	d := &dumpAnchor{Doc: docOrd[a.ArtifactID], Quote: a.Quote, Mark: o.of("mark:" + a.MarkID), Version: a.Version, Orphaned: a.Orphaned}
	if a.BlockID != nil {
		d.Block = o.of(strings.ToLower(*a.BlockID))
	}
	return d
}

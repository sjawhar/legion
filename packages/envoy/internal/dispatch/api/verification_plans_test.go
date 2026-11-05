package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestVerificationGraphQueryPlans(t *testing.T) {
	handler, database, _, s := newRegisteredTestServer(t, testServerOptions{})
	own := createInteractionIssue(t, handler, "VQP", "Measured", twoRequirements)
	document := uploadDocument(t, handler, "/api/v1/issues/"+own.Key, "verification.md", checkServing("k1", "dispatch://"+own.Key+"/spec#b-r1"))
	other := createInteractionIssue(t, handler, "VQO", "Unrelated", twoRequirements)
	unrelated := uploadDocument(t, handler, "/api/v1/issues/"+other.Key, "verification.md", twoChecks)
	// A large unrelated index is the read-cliff control: the named read must not walk it.
	_, err := database.Pool.Exec(t.Context(), `
 insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
 select $1::uuid, 'bulk-' || n, $2 || '#bulk-' || n, 'check', repeat('a', 64), 'Unrelated check', 1, now()
 from generate_series(1, 20000) n;
 `, unrelated.Artifact.ID, *unrelated.Artifact.IssueKey+"/"+unrelated.Artifact.Slug)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Pool.Exec(t.Context(), `
 insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, to_types)
 select artifact_id, block_id, node_id, 'block', $2 || '#r1', 'serves', 1, array['requirement']
 from document_blocks where artifact_id = $1::uuid and block_id like 'bulk-%'
 `, unrelated.Artifact.ID, other.Key+"/spec")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the giant-document control above and add many ordinary documents, so prepared
	// plan selection sees both skew and a production-sized document population.
	_, err = database.Pool.Exec(t.Context(), `
	 with added as (
	  insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
	  select $1, $2, 'scale-' || n, 'scale-' || n || '.md', 'doc', '{"kind":"user","id":"alice"}'::jsonb
	  from generate_series(1, 2000) n returning id, ref_key
	 )
	 insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
	 select id, 'ordinary-' || n, ref_key || '#ordinary-' || n, 'paragraph', repeat('b', 64), 'Ordinary block', 1, now()
	 from added cross join generate_series(1, 10) n
	`, other.Key, "VQO")
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"document_blocks", "block_refs", "artifacts", "check_results", "refs"} {
		if _, err := database.Pool.Exec(t.Context(), "analyze "+table); err != nil {
			t.Fatal(err)
		}
	}
	queries := []struct {
		name, sql string
		args      []any
	}{
		{"legacy coverage", servingBlockCountsSQL, []any{own.PrimaryArtifactID, []string{own.Key}}},
		{"graph coverage", graphServingBlockCountsSQL, []any{own.PrimaryArtifactID, []string{own.Key}}},
		{"legacy check metadata", `select block_id, changed_version, text_sha256 from document_blocks where artifact_id = $1::uuid and type = 'check'`, []any{document.Artifact.ID}},
		{"legacy results", `select ` + checkResultColumns + ` from check_results where check_artifact_id = $1::uuid order by check_block_id, created_at desc, id desc`, []any{document.Artifact.ID}},
		{"verification graph", graphCheckResultsSQL, []any{document.Artifact.ID}},
		{"serves targets", servesTargetsSQL, []any{document.Artifact.ID}},
	}
	for _, query := range queries {
		var encoded []byte
		if err := database.Pool.QueryRow(t.Context(), "explain (analyze, buffers, format json) "+query.sql, query.args...).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var plan []struct {
			Execution float64        `json:"Execution Time"`
			Planning  float64        `json:"Planning Time"`
			Plan      map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal(encoded, &plan); err != nil {
			t.Fatal(err)
		}
		if len(plan) != 1 {
			t.Fatal("missing EXPLAIN plan")
		}
		var largeScans []string
		var walk func(map[string]any)
		walk = func(node map[string]any) {
			if node["Node Type"] == "Seq Scan" && (node["Relation Name"] == "document_blocks" || node["Relation Name"] == "block_refs") {
				largeScans = append(largeScans, fmt.Sprint(node["Relation Name"]))
			}
			if plans, ok := node["Plans"].([]any); ok {
				for _, child := range plans {
					walk(child.(map[string]any))
				}
			}
		}
		walk(plan[0].Plan)
		t.Logf("%s: execution=%.3fms planning=%.3fms large-index-seq-scans=%s", query.name, plan[0].Execution, plan[0].Planning, strings.Join(largeScans, ","))
		t.Logf("%s EXPLAIN: %s", query.name, encoded)
		if len(largeScans) != 0 && (query.name == "graph coverage" || query.name == "verification graph" || query.name == "serves targets") {
			t.Errorf("%s scans the unrelated block index: %v", query.name, largeScans)
		}
	}
	oldTimes, graphTimes := make([]time.Duration, 0, 30), make([]time.Duration, 0, 30)
	for i := range 31 {
		start := time.Now()
		if _, err := s.liveCheckResults(t.Context(), database.Pool, own.Key, nil); err != nil {
			t.Fatal(err)
		}
		old := time.Since(start)
		start = time.Now()
		if _, err := s.verificationTab(t.Context(), database.Pool, own.Key, nil); err != nil {
			t.Fatal(err)
		}
		graph := time.Since(start)
		if i > 0 {
			oldTimes = append(oldTimes, old)
			graphTimes = append(graphTimes, graph)
		}
	}
	slices.Sort(oldTimes)
	slices.Sort(graphTimes)
	t.Logf("complete warm verification read p50: legacy=%s graph=%s (30 samples, same document and DB)", oldTimes[15], graphTimes[15])
}

package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// BenchmarkColdDocumentRead measures stored-document reads at the same two sizes as the resident
// read benchmark. A miss writes a tiny, non-Proof map update and compacts before its timer starts:
// it moves the stored head without changing rendered Markdown or allowing the log to grow across
// iterations. A hit leaves that head alone after one warm read.
func BenchmarkColdDocumentRead(b *testing.B) {
	for _, size := range []int{51 << 10, 524 << 10} {
		for _, read := range []struct {
			name string
			read func(*Service, string) error
		}{
			{"text", func(service *Service, artifactID string) error {
				_, _, err := service.TextWithToken(context.Background(), artifactID)
				return err
			}},
			{"blocks", func(service *Service, artifactID string) error {
				_, err := service.Blocks(context.Background(), artifactID)
				return err
			}},
		} {
			b.Run(fmt.Sprintf("%dKiB/%s/miss", size>>10, read.name), func(b *testing.B) {
				reads := make([]struct {
					service    *Service
					artifactID string
				}, b.N)
				for index := range reads {
					service, versioned, artifactID, live := newColdBenchmarkDocument(b, size)
					if err := read.read(service, artifactID); err != nil {
						b.Fatal(err)
					}
					moveBenchmarkStamp(b, versioned, artifactID, live, index)
					reads[index] = struct {
						service    *Service
						artifactID string
					}{service, artifactID}
				}
				b.ResetTimer()
				b.ReportAllocs()
				for _, item := range reads {
					if err := read.read(item.service, item.artifactID); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(fmt.Sprintf("%dKiB/%s/hit", size>>10, read.name), func(b *testing.B) {
				service, _, artifactID, _ := newColdBenchmarkDocument(b, size)
				if err := read.read(service, artifactID); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				b.ReportAllocs()
				for range b.N {
					if err := read.read(service, artifactID); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func newColdBenchmarkDocument(b *testing.B, size int) (*Service, *PgVersioned, string, *crdt.Doc) {
	b.Helper()
	database := storetest.Open(b)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into projects (key, name) values ('BENCH', 'Benchmark')`); err != nil {
		b.Fatal(err)
	}
	issueKey := "BENCH-" + suffix
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'BENCH', 1, 'Benchmark', '{"kind":"user","id":"alice"}', 'U')
	`, issueKey); err != nil {
		b.Fatal(err)
	}
	var artifactID string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, 'BENCH', 'benchmark', 'benchmark.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, issueKey).Scan(&artifactID); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		b.Fatal(err)
	}
	live := coldBenchmarkLiveDocument(b, size)
	versioned := NewPgVersioned(database)
	if _, err := versioned.AppendUpdate(ctx, artifactID, crdt.EncodeStateAsUpdateV1(live, nil)); err != nil {
		b.Fatal(err)
	}
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	b.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			b.Errorf("shutdown cold benchmark service: %v", err)
		}
	})
	return service, versioned, artifactID, live
}

func coldBenchmarkLiveDocument(b *testing.B, size int) *crdt.Doc {
	var markdown strings.Builder
	markdown.WriteString("# Benchmark\n\n")
	markdown.WriteString(strings.Repeat("word ", size/5+1))
	tree, err := pmdoc.Parse(markdown.String())
	if err != nil {
		b.Fatal(err)
	}
	update, err := encodeDocumentTree(tree)
	if err != nil {
		b.Fatal(err)
	}
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, update, nil); err != nil {
		b.Fatal(err)
	}
	return live
}

func moveBenchmarkStamp(b *testing.B, versioned *PgVersioned, artifactID string, live *crdt.Doc, iteration int) {
	b.Helper()
	before := live.StateVector()
	meta := live.GetMap("cold-read-benchmark")
	live.Transact(func(txn *crdt.Transaction) {
		meta.Set(txn, fmt.Sprintf("%d", iteration), "x")
	})
	if _, err := versioned.AppendUpdate(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(live, before)); err != nil {
		b.Fatal(err)
	}
	if _, err := versioned.Compact(context.Background(), artifactID, compactKeep); err != nil {
		b.Fatal(err)
	}
}

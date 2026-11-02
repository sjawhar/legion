package docs

import (
	"context"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// uploadedReadBackText is what the upload below stores: a document of several blocks, so the
// read-back its parse makes is a tree with children to find at the other end.
const uploadedReadBackText = `# Uploaded

The first paragraph of the upload.

## A section

- one item
- another item

| Requirement | Note |
| --- | --- |
| The first | holds |
| The second | holds too |

A closing paragraph.
`

// An upload parses the markdown it stores back, to refuse a document that reads back otherwise,
// and hands that parse to the rest of its request (Ledger.ReadBackOf). The parse is a whole tree
// of the document, and what outlives the request is the context the request ran on: a pooled pgx
// connection keeps the context of the query it last served (pgconn's PgConn holds its last
// ResultReader and MultiResultReader, each with that query's context, until the connection serves
// another query), and that context carries the ledger. So the request's end must leave the ledger
// holding no tree (Ledger.Discard): kept, a parsed document stays resident beside its room for as
// long as its connection sits idle in the pool, which is what took the retention of an upload
// from about 1 KB to between 6 and 12 KB a write (TestUploadsToARoomInUseKeepMemoryWithinTheBound).
func TestAnUploadsReadBackTreeGoesWhenItsRequestEnds(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# First\n\nSecond.\n")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the upload's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	if _, err := service.ReplaceText(joined, artifactID, uploadedReadBackText, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("upload the document: %v", err)
	}
	read, kept := ledger.readBacks[artifactID]
	if !kept || read.Tree == nil {
		t.Fatal("the upload kept no read-back of what it stored, so this test guards nothing")
	}
	blocks := len(read.Tree.Children)
	tree := weak.Make(read.Tree)
	read = pmdoc.WrittenReadBack{}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the upload: %v", err)
	}
	// The request ends here, as every caller of Join ends one.
	ledger.Discard()

	// What production keeps is kept here: the context the request ran on, which the pooled
	// connection holds, and through it the ledger. Nothing else holds the read-back - the tree
	// the write itself recorded is the one it parsed from the request's markdown, not this
	// reading of what it stored - so the collector must take it.
	for range 5 {
		runtime.GC()
		if tree.Value() == nil {
			break
		}
	}
	if held := tree.Value(); held != nil {
		t.Fatalf("the upload's read-back, a parsed tree of %d blocks, is still reachable once its request ended: the ledger the request's context carries is holding it",
			len(held.Children))
	}
	if blocks == 0 {
		t.Fatal("the upload's read-back was an empty tree, which proves nothing about what a document's parse retains")
	}
	runtime.KeepAlive(joined)
}

package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// openingText is the Yjs text that opens the first top-level paragraph of doc whose text starts
// with prefix, or nil when none does.
func openingText(doc *crdt.Doc, prefix string) *crdt.YXmlText {
	for _, child := range doc.GetXmlFragment(fragmentName).Children() {
		element, ok := child.(*crdt.YXmlElement)
		if !ok || element.NodeName != "paragraph" {
			continue
		}
		children := element.Children()
		if len(children) == 0 {
			continue
		}
		if text, ok := children[0].(*crdt.YXmlText); ok && strings.HasPrefix(text.ToString(), prefix) {
			return text
		}
	}
	return nil
}

// A write whose change panics in ygo's phase 2 - here a text observer the test installs on the
// fork, which panics as the transaction fires its observers, after the transaction's items are
// integrated and before its update is handed on - leaves the fork holding a change no update
// carries. The operation answers the panic, and the write drops its fork and the rendering it held
// (liveOperation.abandon), so no later operation of the transaction sees the panicked text: not a
// read joined to it (docTree, SuggestionKind, BlockForQuote), which reads a fork rebuilt from what
// the transaction recorded rather than taking a rendering, nor a version, nor a further write and
// its version. Every version the transaction writes is the document it commits, and the room holds
// that document. Warm and cold.
func TestAPanickedWriteLeavesNoForkForTheTransactionsLaterWrites(t *testing.T) {
	service, artifactID := newForkReadsRoom(t)
	checks := checkRenderings(service)
	for _, mode := range forkReadsModes {
		t.Run(mode.name, func(t *testing.T) {
			// Sections 61 to 63 for the warm transaction, 71 to 73 for the cold one.
			section := 61
			if mode.cold {
				section = 71
			}
			first := fmt.Sprintf("Paragraph %d opens", section)
			target := fmt.Sprintf("Paragraph %d opens", section+1)
			marker := fmt.Sprintf("PANICKED%d", section)
			suggestion := "s-panicked-" + mode.name
			mode.prepare(t, service, artifactID)
			checks.take()
			// versions are the numbers of the versions the transaction writes.
			var versions []int
			snapshot := func(ctx context.Context) error {
				result, err := service.SnapshotVersion(ctx, artifactID, reuseAgent)
				if err == nil {
					versions = append(versions, result.Version.Number)
				}
				return err
			}
			if err := joinedWrite(service, func(ctx context.Context) error {
				// An edit, then a suggestion, which leaves the write a rendering of its fork.
				if _, err := service.ApplyOps(ctx, artifactID, []model.EditOp{{Op: "replace", Find: first, With: first + " firmly"}}, reuseAgent, nil); err != nil {
					return fmt.Errorf("ApplyOps: %w", err)
				}
				if _, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkSuggestion, ID: suggestion, By: reuseAgent}, fmt.Sprintf("Step %d two is taken next", section), nil); err != nil {
					return fmt.Errorf("MarkQuote: %w", err)
				}
				write := mode.requireForkedAs(t, ctx, artifactID)
				if write.tree == nil {
					t.Fatal("the suggestion left the write no rendering")
				}

				panicErr := service.applyLive(ctx, artifactID, reuseAgent, func(doc *crdt.Doc, _ *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
					text := openingText(doc, target)
					if text == nil {
						return fmt.Errorf("no paragraph opens with %q", target)
					}
					// The observer goes as the panic leaves mutate, before anything else runs on
					// the fork.
					stop := text.Observe(func(crdt.YTextEvent) { panic("a text observer panics in phase 2") })
					defer stop()
					transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, marker+" ", nil) })
					return nil
				})
				if panicErr == nil || !strings.Contains(panicErr.Error(), "document mutation panicked") {
					t.Fatalf("the panicking write answered %v, want the panic", panicErr)
				}
				if write.fork != nil || write.tree != nil {
					t.Errorf("after the panic the write holds a fork = %t and a rendering = %t, want neither", write.fork != nil, write.tree != nil)
				}

				// The reads joined to the transaction read the rebuilt fork, and none takes a
				// rendering until a write records one.
				base := readForkReads(service)
				tree, err := service.docTree(ctx, artifactID)
				if err != nil {
					return fmt.Errorf("docTree: %w", err)
				}
				requireForkReads(t, "docTree after the panic", readForkReads(service).minus(base), forkReads{read: 1})
				markdown, err := documentMarkdown(tree)
				if err != nil {
					return fmt.Errorf("render docTree's tree: %w", err)
				}
				if strings.Contains(markdown, marker) || !strings.Contains(markdown, first+" firmly") {
					t.Errorf("docTree after the panic holds the panicked text = %t and the edit = %t, want only the edit", strings.Contains(markdown, marker), strings.Contains(markdown, first+" firmly"))
				}
				base = readForkReads(service)
				if kind, err := service.SuggestionKind(ctx, artifactID, suggestion); err != nil || kind != "replace" {
					t.Errorf("SuggestionKind after the panic = %q, %v; want replace", kind, err)
				}
				requireForkReads(t, "SuggestionKind after the panic", readForkReads(service).minus(base), forkReads{read: 1})
				if block, err := service.BlockForQuote(ctx, artifactID, marker); !errors.Is(err, pmdoc.ErrTargetNotFound) {
					t.Errorf("BlockForQuote(%q) after the panic = %q, %v; want pmdoc.ErrTargetNotFound", marker, block, err)
				}
				base = readForkReads(service)
				if err := snapshot(ctx); err != nil {
					return fmt.Errorf("SnapshotVersion after the panic: %w", err)
				}
				requireForkReads(t, "SnapshotVersion after the panic", readForkReads(service).minus(base), forkReads{read: 1})

				// A further write, which renders nothing new, and its version, which takes the
				// rendering that write recorded.
				if _, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkComment, ID: "c-panicked-" + mode.name, By: reuseAgent}, fmt.Sprintf("Step %d one is taken first", section+2), nil); err != nil {
					return fmt.Errorf("the further MarkQuote: %w", err)
				}
				if fresh, err := treeOf(write.fork); err != nil {
					t.Errorf("read the fork after the further write: %v", err)
				} else if markdown, err := documentMarkdown(fresh); err != nil || strings.Contains(markdown, marker) {
					t.Errorf("the fork after the further write holds the panicked text (render error %v)", err)
				}
				base = readForkReads(service)
				if err := snapshot(ctx); err != nil {
					return fmt.Errorf("SnapshotVersion after the further write: %w", err)
				}
				requireForkReads(t, "SnapshotVersion after the further write", readForkReads(service).minus(base), forkReads{renderingReused: 1})
				return nil
			}); err != nil {
				t.Fatalf("commit the transaction: %v", err)
			}
			checks.requireNoRenderingFailures(t)

			committed, err := documentMarkdown(persistedProofTree(t, service.store, artifactID))
			if err != nil {
				t.Fatalf("render the committed document: %v", err)
			}
			if strings.Contains(committed, marker) || !strings.Contains(committed, first+" firmly") {
				t.Errorf("the committed document holds the panicked text = %t and the edit = %t, want only the edit", strings.Contains(committed, marker), strings.Contains(committed, first+" firmly"))
			}
			for _, number := range versions {
				requireVersionIsTheCommittedDocument(t, service, artifactID, number)
			}
			room, _, err := service.TextWithToken(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read the room: %v", err)
			}
			if room != committed {
				t.Errorf("the room holds %d bytes of markdown, not the committed document's %d bytes", len(room), len(committed))
			}
		})
	}
}

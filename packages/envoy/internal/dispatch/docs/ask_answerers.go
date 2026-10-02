package docs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// AskAnswerer is an answered ask block in a document's live state and the person its
// server-owned answered_by attribute names.
type AskAnswerer struct {
	ArtifactID string
	AnsweredBy string
}

// UnreadableDocument is a document whose live state could not be read, and why.
type UnreadableDocument struct {
	ArtifactID string
	Err        error
}

// AnswererRename is how RenameAskAnswerers left one document: Skipped names why a stopping service
// left it alone, Err why the rename failed, and neither is set once its asks name what rename gives.
type AnswererRename struct {
	ArtifactID string
	Skipped    string
	Err        error
}

// AskAnswerers reads the answered_by attribute of every answered ask block in every document's
// live state, as a reader sees it (readDocument), so it loads no room and reads a closed issue's
// document too. A document whose state cannot be read is returned in unreadable and the rest are
// still read; err is a failure to list the documents at all.
func (s *Service) AskAnswerers(ctx context.Context) (answerers []AskAnswerer, unreadable []UnreadableDocument, err error) {
	artifactIDs, err := s.documentIDs(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, artifactID := range artifactIDs {
		doc, err := s.readDocument(ctx, artifactID)
		if err != nil {
			unreadable = append(unreadable, UnreadableDocument{ArtifactID: artifactID, Err: err})
			continue
		}
		if doc == nil {
			continue
		}
		tree, err := treeOf(doc)
		if err != nil {
			unreadable = append(unreadable, UnreadableDocument{ArtifactID: artifactID, Err: err})
			continue
		}
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if by, ok := askAnsweredBy(node); ok {
				answerers = append(answerers, AskAnswerer{ArtifactID: artifactID, AnsweredBy: by})
			}
			return true
		})
	}
	return answerers, unreadable, nil
}

// RenameAskAnswerers rewrites the answered_by attribute of every answered ask block in each of
// artifactIDs to what rename returns for it, leaving each value rename keeps. Each document
// that changes gets one server update appended to its state, never a replacement of it, so a
// browser holding the earlier state merges the rename when it syncs; a closed issue's document is
// renamed too. The update is recorded as no content change: answered_by is server-owned, and
// settlement versions a document for a server-owned attribute no more than it does for its own
// repairs of one, since that version would stale every approval pinned to the version before it.
// The document's versions keep the name they were written with.
func (s *Service) RenameAskAnswerers(ctx context.Context, artifactIDs []string, rename func(string) string) []AnswererRename {
	ctx = store.WithTransactionTracking(ctx)
	reports := make([]AnswererRename, 0, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		_, skipped, err := s.repairDocument(ctx, artifactID, func(doc *crdt.Doc, origin any) (int, bool, error) {
			_, renamed, err := rewriteTree(doc, origin, func(tree *pmdoc.Node) int {
				renamed := 0
				pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
					by, ok := askAnsweredBy(node)
					if !ok {
						return true
					}
					if next := rename(by); next != by {
						node.Attrs["answered_by"] = next
						renamed++
					}
					return true
				})
				return renamed
			})
			return renamed, false, err
		})
		reports = append(reports, AnswererRename{ArtifactID: artifactID, Skipped: skipped, Err: err})
	}
	return reports
}

// askAnsweredBy is the answered_by attribute node carries when it is an answered ask block.
func askAnsweredBy(node *pmdoc.Node) (string, bool) {
	if node.Type != "ask" {
		return "", false
	}
	by, ok := node.Attrs["answered_by"].(string)
	return by, ok && by != ""
}

// documentIDs lists every document artifact, in id order.
func (s *Service) documentIDs(ctx context.Context) ([]string, error) {
	rows, err := s.store.Pool.Query(ctx, `select id::text from artifacts where kind = 'doc' order by id`)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	artifactIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	return artifactIDs, nil
}

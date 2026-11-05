package docs

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// FirstScreenHTML is the HTML of artifactID's opening blocks, for the page a reader opens the
// document at (routes.servePage, dispatch://INFRA-341 R11): up to pmdoc.FirstScreenBlocks of them
// and no more than pmdoc.FirstScreenBytes, with complete saying whether that is the whole
// document.
//
// It draws from the tree this server holds - a write's own fork, or the resident room - and,
// where no room holds the document, from a fold of its stored state (storedTree), which is a
// load of the history rather than a rendering: the cold reads' cache holds renderings, not trees,
// so a hit there has no tree to draw. The fold does not fail the room, as storedTree says: a page
// that cannot draw the document sends the shell, and the reader's own editor connects as ever.
//
// A document with no stored state draws nothing, and so does one the reader's own address names
// before it has any content.
func (s *Service) FirstScreenHTML(ctx context.Context, artifactID string) (string, bool, error) {
	tree, held, err := s.heldTree(ctx, artifactID)
	if err != nil {
		return "", false, err
	}
	if !held {
		tree, err = s.storedTree(ctx, artifactID)
		if err != nil {
			return "", false, err
		}
	}
	html, complete := pmdoc.FirstScreenHTML(tree)
	return html, complete, nil
}

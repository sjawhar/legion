package api

import (
	"context"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// FirstScreenAddress names the document a browser address opens: an issue's own spec, where Slug
// is empty (`/issues/{key}`), and the issue document at Slug otherwise
// (`/issues/{key}/artifacts/{slug}`). No other address carries a document into its page.
type FirstScreenAddress struct {
	IssueKey string
	Slug     string
}

// FirstScreen is what the page handler puts into the page a reader opens a document at: the HTML
// of the document's opening blocks, and whether that is the whole document (routes.servePage,
// dispatch://INFRA-341 R11). Drawn is false where the page carries no document at all - the
// reader has no session or a revoked one, the address names no document, it names something
// that is not one, the document cannot be loaded, or the read ran out of the time the page may
// spend on it - and the page is then the static shell it has always been.
type FirstScreen struct {
	HTML     string
	Complete bool
	Drawn    bool
}

// firstScreen answers the document address's opening blocks for a page request.
//
// The gate is the document's own read, and nothing beside it. `GET
// /api/v1/issues/{key}/artifacts/{slug}/text` asks two things and no more: that the request
// names a signed-in caller (requireAuthenticated, which is optionalActor), and that the address
// resolves to an artifact whose kind is `doc`. This asks exactly those two, through the same
// calls, so no reader is refused here who is answered there and none is answered here who is
// refused there. Its refusals - no session, a session since revoked, ARTIFACT_NOT_FOUND,
// NOT_DOCUMENT, a document that will not load - are each answered the same way, by drawing
// nothing.
//
// Every refusal is silent: the page is the one surface where a document's absence is not an
// error to show but a page to send, and the client reads the document through its own API call
// moments later, where the refusal reaches the reader with its reason.
func (s *server) firstScreen(ctx context.Context, r *http.Request, address FirstScreenAddress) FirstScreen {
	if s.deps.Store == nil || s.deps.Docs == nil {
		return FirstScreen{}
	}
	if _, _, err := s.optionalActor(r); err != nil {
		return FirstScreen{}
	}
	artifact, err := s.firstScreenArtifact(ctx, address)
	if err != nil || artifact.Kind != "doc" {
		return FirstScreen{}
	}
	html, complete, err := s.deps.Docs.FirstScreenHTML(ctx, artifact.ID)
	if err != nil || html == "" {
		return FirstScreen{}
	}
	return FirstScreen{Complete: complete, Drawn: true, HTML: html}
}

// firstScreenArtifact resolves a browser address to the document behind it, as the read routes
// resolve their own: an issue's spec is the artifact the issue names as its primary one, which is
// what the page's Spec tab reads, and any other slug is resolved as
// `GET /api/v1/issues/{key}/artifacts/{slug}` resolves it, by reference key and then by filename.
func (s *server) firstScreenArtifact(ctx context.Context, address FirstScreenAddress) (model.Artifact, error) {
	pool := s.deps.Store.Pool
	if address.Slug != "" {
		key := address.IssueKey
		return s.loadArtifactByDocumentReference(ctx, pool, artifactTarget{IssueKey: &key}, address.Slug)
	}
	issue, err := s.loadIssue(ctx, pool, address.IssueKey)
	if err != nil {
		return model.Artifact{}, err
	}
	if issue.PrimaryArtifactID == "" {
		return model.Artifact{}, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "the issue has no spec")
	}
	return s.loadArtifact(ctx, pool, issue.PrimaryArtifactID)
}

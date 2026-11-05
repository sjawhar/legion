package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// referenceTargetsMax is how many references one batched read may name. A page draws a chip per
// reference it holds, and the point of this read is that one page's whole first load is one
// request, so the cap is set above the heaviest set a page can hold rather than at a round guess.
// Measured on 2026-11-05 against this repository's own Dispatch, on dispatch://INFRA-1196, its
// heaviest issue: its spec, plan and verification documents write 177 edges, 174 of them neither
// part_of nor attached_to, naming 72 distinct targets; the renders of that issue and of its four
// largest documents name 247 distinct references in all. The tabs this page replaces send one
// GET /api/v1/references per chip, 14 on the heaviest of them. 256 is above all of those, so no
// first load needs a second request, and a set at the cap still costs one statement per node
// kind (refs.TargetNames), not one per reference.
//
// The other bound on a batch is the request line itself, which this server sets no limit of its
// own on: those 247 references encode to about 17 KB of query string, and a load balancer in
// front of Dispatch admits a request line of its own size (16 KB on an AWS one). A caller whose
// references are long enough to meet that reads them in two requests, as it would past the cap.
const referenceTargetsMax = 256

// getReferenceTargets resolves many dispatch:// references in one read. For each ?ref= it answers
// what that reference points at: its kind, the words a reader is shown for its target in place of
// the target's text (refs.TargetNames, the same words GET /api/v1/references shows beside an edge
// at that node), and whether the target is gone. The answer holds one member per ?ref= in the
// request's order, each echoing the reference as the request wrote it, so a caller reads the
// answer by index or by reference; a reference named twice is answered twice.
//
// It is a GET with one repeated parameter, not a POST of a list: a POST of the page goes through
// the write store with an Idempotency-Key (packages/dispatch/AGENTS.md), so a POST read would be
// a second write path, and a GET is what a browser cache, a conditional read and the page's
// request budget are counted by. GET /api/v1/references reads one node's edges and stays as it
// is; this read names no edges at all.
//
// A reference the grammar cannot read is refused by name (parseRequestRef, as the per-reference
// route refuses its own), and so is a set past referenceTargetsMax, which is refused before
// anything is parsed. A reference that reads but names nothing is answered, gone: a chip whose
// target was deleted still has to draw.
func (s *server) getReferenceTargets(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	targets, err := s.referenceTargets(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	etag := referenceTargetsETag(targets)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	WriteJSON(w, http.StatusOK, model.ReferenceTargets{Targets: targets})
}

// referenceTargets reads every ?ref= of the request and resolves them in one query per node kind.
// The kind of each answer is the kind the reference itself names, which is the kind its node
// resolves to (refs.TargetOf stores a block of an ask, a comment or a message under its item's
// block kind, and those resolve to nodes of kind block), so a gone target is answered under its
// kind too.
func (s *server) referenceTargets(r *http.Request) ([]model.ReferenceTarget, error) {
	raw := r.URL.Query()["ref"]
	if len(raw) == 0 {
		return nil, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "ref is required, once per reference to resolve")
	}
	if len(raw) > referenceTargetsMax {
		return nil, countExceededError("CAP_EXCEEDED", "ref", len(raw), referenceTargetsMax)
	}
	answers := make([]model.ReferenceTarget, len(raw))
	named := make([]refs.Target, len(raw))
	targets := make([]refs.Target, 0, len(raw))
	seen := make(map[refs.Target]struct{}, len(raw))
	for index, value := range raw {
		value = strings.TrimSpace(value)
		ref, err := parseRequestRef(value, s.deps.ServerURL)
		if err != nil {
			return nil, err
		}
		// Every kind parseRequestRef admits has a target; only an external URL has none, and
		// that is no reference.
		target, _ := refs.TargetOf(ref)
		answers[index] = model.ReferenceTarget{Ref: value, Kind: ref.Kind, Gone: true}
		named[index] = target
		if _, duplicate := seen[target]; !duplicate {
			seen[target] = struct{}{}
			targets = append(targets, target)
		}
	}
	names, err := refs.TargetNames(r.Context(), s.deps.Store.Pool, targets)
	if err != nil {
		return nil, err
	}
	for index, target := range named {
		if name, found := names[target]; found {
			answers[index].Name, answers[index].Gone = name, false
		}
	}
	return answers, nil
}

// referenceTargetsETag hashes the answer itself, every member in order: its reference, its kind,
// whether it is gone and its name, the name after its length so no name's own line endings can
// read as another member's fields. The answer depends on nothing else, so a set whose members all
// stand as they did is answered 304, and a member renamed, deleted or restored changes the tag.
func referenceTargetsETag(targets []model.ReferenceTarget) string {
	hash := sha256.New()
	for _, target := range targets {
		_, _ = fmt.Fprintf(hash, "%s %s %t %d %s\n", target.Ref, target.Kind, target.Gone, len(target.Name), target.Name)
	}
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
}

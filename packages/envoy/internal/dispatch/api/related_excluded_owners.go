package api

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// relatedExcludedOwners is the exclusion filter the check's statements carry in their kinds CTE
// (searchKindsCTE): true of an arm's row whose owner the check leaves out. Parameter $owners
// holds those owners as relatedInput.excluded writes them, so a statement drops exactly what
// relatedInput.excludes drops - but before the page takes its suggestionSearchDepth rows rather
// than after, which is the whole point of filtering here (INFRA-1160). Those two forms of the
// one rule, the SQL here and the Go below, are in this file together for that reason: the
// statement drops an excluded owner's rows before the page takes its suggestionSearchDepth, and
// excludes applies the same rule again in Go to whatever rows come back (fillRelatedItems).
//
// A row's owner is the issue it belongs to when it carries no owner document and that document
// otherwise, as scanSearchRows reads the same two columns; an ask and a document are named by
// themselves besides, as a source's own ask and own document are. The coalesce leaves a row
// whose owner key is null - an ask on neither an issue nor a document - in the results rather
// than making the whole predicate null, which `where not` would read as a row to leave out.
func relatedExcludedOwners(owners int) string {
	return fmt.Sprintf(`coalesce(case when owner_artifact_id is null then 'issue:' || issue_key
                     else 'document:' || owner_artifact_id end, '') = any($%[1]d::text[])
      or (kind = 'document' and 'document:' || id = any($%[1]d::text[]))
      or (kind = 'ask' and 'ask:' || id = any($%[1]d::text[]))`, owners)
}

// owns reports whether result is the source itself or anything its owner holds: the new issue, or
// the issue or project document the new ask sits on, and everything inside that owner (its spec,
// its other asks, comments and messages). Each is the best match for the source's own words, so
// suggesting one says nothing, and acting on it (a reply on the ask's own issue) must never count
// as acting on a suggestion.
func (source suggestionSource) owns(result model.SearchResult) bool {
	if source.askID != "" && result.Kind == "ask" && result.ID == source.askID {
		return true
	}
	if source.issueKey != "" {
		return ownedByIssue(result, source.issueKey)
	}
	if source.artifactID != "" {
		return (result.Kind == "document" && result.ID == source.artifactID) ||
			(result.Owner.Kind == "document" && result.Owner.ArtifactID == source.artifactID)
	}
	return false
}

// ownedByIssue reports whether result is the issue key or anything that issue owns.
func ownedByIssue(result model.SearchResult, key string) bool {
	return (result.Kind == "issue" && result.ID == key) || (result.Owner.Kind == "issue" && result.Owner.Key == key)
}

// excludes reports whether result is left out of the check's items: the source's own, the
// excluded issue's, or any issue's on the exclusion list.
func (input relatedInput) excludes(result model.SearchResult) bool {
	if input.source.owns(result) {
		return true
	}
	if input.excludeIssue != "" && ownedByIssue(result, input.excludeIssue) {
		return true
	}
	for _, key := range input.excludeIssues {
		if ownedByIssue(result, key) {
			return true
		}
	}
	return false
}

// excluded is those same owners as the check's statements read them (relatedExcludedOwners):
// the source's own ask, issue or document, the excluded issue and every issue on the exclusion
// list, each written as the kind of owner it is. It is nil when the check leaves nothing out,
// and the check then runs the statement without the filter.
func (input relatedInput) excluded() []string {
	owners := make([]string, 0, len(input.excludeIssues)+3)
	if input.source.askID != "" {
		owners = append(owners, "ask:"+input.source.askID)
	}
	if input.source.issueKey != "" {
		owners = append(owners, "issue:"+input.source.issueKey)
	}
	if input.source.artifactID != "" {
		owners = append(owners, "document:"+input.source.artifactID)
	}
	if input.excludeIssue != "" {
		owners = append(owners, "issue:"+input.excludeIssue)
	}
	for _, key := range input.excludeIssues {
		owners = append(owners, "issue:"+key)
	}
	if len(owners) == 0 {
		return nil
	}
	return owners
}

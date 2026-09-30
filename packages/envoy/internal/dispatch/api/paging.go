package api

import (
	"net/http"
	"slices"
)

// pagingParameters are the query parameters a caller asks for a page with.
var pagingParameters = [...]string{"limit", "offset", "cursor"}

// pagingRule is how one GET route pages: the paging parameters its handler reads itself, and
// what the refusal of any other says pages the route.
type pagingRule struct {
	served []string
	pager  string
}

// routePaging names the GET routes that read a paging parameter, or whose refusal has more to
// say than that the route does not page. Every other GET route reads none.
var routePaging = map[string]pagingRule{
	"/api/v1/issues": {
		pager: ": the listing is unpaginated and answers every matching issue; the dispatch_issues tool pages it with its limit and offset",
	},
	"/api/v1/issues/{key}/events":   eventLogPaging,
	"/api/v1/artifacts/{id}/events": eventLogPaging,
	"/api/v1/search": {
		served: []string{"limit"},
		pager:  ": it answers the best limit results, at most 50, and has no next page",
	},
}

var eventLogPaging = pagingRule{
	served: []string{"limit"},
	pager:  ", which pages with after (oldest first) or before (newest first), and limit",
}

// refuseUnservedPaging answers 400 INVALID_QUERY for a paging parameter the GET route at pattern
// does not read, naming the parameter and the route, before the handler runs. A route that
// ignored it would answer 200 with rows the caller cannot tell from the page it asked for
// (LEGION-406).
func refuseUnservedPaging(pattern string, handler http.HandlerFunc) http.HandlerFunc {
	rule, named := routePaging[pattern]
	if !named {
		rule.pager = ", which does not page"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			query := r.URL.Query()
			for _, parameter := range pagingParameters {
				if query.Has(parameter) && !slices.Contains(rule.served, parameter) {
					writeError(w, "INVALID_QUERY", http.StatusBadRequest,
						parameter+" is not a query parameter of GET "+pattern+rule.pager)
					return
				}
			}
		}
		handler(w, r)
	}
}

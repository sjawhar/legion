package api

import (
	"math"
	"net/http"
	"net/url"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// issuePage is the page a GET /api/v1/issues caller asked for with limit and offset.
type issuePage struct {
	limit  int
	offset int
}

// parseIssuePage reads the listing's paging parameters. With neither limit nor offset the
// caller asked for no page (nil), and the listing answers every matching issue. offset alone
// pages at contracts.DefaultIssuePageLimit. cursor is refused rather than ignored: the listing
// pages with limit and offset, and answering a cursor request with the first page would hand the
// caller rows it cannot tell from the ones it asked for.
func parseIssuePage(query url.Values) (*issuePage, error) {
	if query.Has("cursor") {
		return nil, errorf(http.StatusBadRequest, "INVALID_QUERY",
			"cursor is not a query parameter of GET /api/v1/issues, which pages with limit and offset")
	}
	if !query.Has("limit") && !query.Has("offset") {
		return nil, nil
	}
	page := &issuePage{limit: contracts.DefaultIssuePageLimit}
	if limit, present, valid := parseQueryInt(query, "limit", 1, contracts.MaxIssuePageLimit); present {
		if !valid {
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"limit must be one integer from 1 to %d", contracts.MaxIssuePageLimit)
		}
		page.limit = limit
	}
	if offset, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "offset must be one non-negative integer")
		}
		page.offset = offset
	}
	return page, nil
}

// of cuts the page out of the whole filtered listing, which is in the listing's total order, so
// consecutive offsets cover every matching issue once while the listing does not change between
// reads: an issue that enters or leaves what the filters match, or whose status or rank changes,
// shifts rows across a page boundary, and one issue is served twice and another never. Only the
// unpaged listing is an exact set in one read. It is the cut a route_status request takes, whose
// filter runs here rather than in SQL; every other paged request is cut by its own statement and
// answers through holding.
func (p issuePage) of(issues []model.IssueSummary) model.IssueSummaryPage {
	start := min(p.offset, len(issues))
	end := min(start+p.limit, len(issues))
	return p.holding(issues[start:end], len(issues))
}

// holding is the page a statement cut itself, with total the count that statement made of the
// same snapshot, so the rows and the count cannot come from two readings of a moving table.
func (p issuePage) holding(issues []model.IssueSummary, total int) model.IssueSummaryPage {
	return model.IssueSummaryPage{Issues: issues, Total: total, Limit: p.limit, Offset: p.offset}
}

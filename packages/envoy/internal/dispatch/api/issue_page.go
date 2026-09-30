package api

import (
	"net/http"
	"net/url"
	"strconv"

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
	if values, ok := query["limit"]; ok {
		limit, err := strconv.Atoi(values[0])
		if len(values) != 1 || err != nil || limit < 1 || limit > contracts.MaxIssuePageLimit {
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"limit must be one integer from 1 to %d", contracts.MaxIssuePageLimit)
		}
		page.limit = limit
	}
	if values, ok := query["offset"]; ok {
		offset, err := strconv.Atoi(values[0])
		if len(values) != 1 || err != nil || offset < 0 {
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "offset must be one non-negative integer")
		}
		page.offset = offset
	}
	return page, nil
}

// of cuts the page out of the whole filtered listing, which is in the listing's total order, so
// consecutive offsets cover every matching issue once.
func (p issuePage) of(issues []model.IssueSummary) model.IssueSummaryPage {
	start := min(p.offset, len(issues))
	end := min(start+p.limit, len(issues))
	return model.IssueSummaryPage{Issues: issues[start:end], Total: len(issues), Limit: p.limit, Offset: p.offset}
}

package api

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// parseDeliveryLiveView resolves a subscribed view as the matching GET route reads its query, or
// answers the error frame refusing it: INVALID_FRAME for a view of no known shape, INVALID_QUERY
// for one the route would refuse.
func parseDeliveryLiveView(view DeliveryLiveView) (deliveryLiveSpec, *LiveError) {
	invalid := func(code, message string) (deliveryLiveSpec, *LiveError) {
		refusal := liveRefusal("", code, message)
		return deliveryLiveSpec{}, &refusal
	}
	spec := deliveryLiveSpec{kind: view.Kind}
	if view.Project != nil && view.Kind != legionBoardView {
		return invalid("INVALID_FRAME", "only a legion view names a project")
	}
	switch view.Kind {
	case deliveryLiveTimeline, deliveryLiveMeasures:
		if view.At != nil {
			return invalid("INVALID_FRAME", "a "+view.Kind+" view names a window, not an at")
		}
		if view.Window == nil {
			return invalid("INVALID_FRAME", "a "+view.Kind+" view names its window")
		}
		spec.query = url.Values{}
		if view.Query != nil {
			query, err := url.ParseQuery(*view.Query)
			if err != nil {
				return invalid("INVALID_QUERY", "query does not parse: "+err.Error())
			}
			spec.query = query
		}
		// The window names from and to; the query is everything else the GET route reads.
		spec.query.Del("from")
		spec.query.Del("to")
		named, refusal := spec.readWindow(view.Window)
		if refusal != nil {
			return deliveryLiveSpec{}, refusal
		}
		spec.key = view.Kind + "|" + named + "|" + spec.query.Encode()
	case deliveryLivePipeline:
		// A pipeline view names its window as a timeline view does, or an at, which is the seven
		// days ending on its day, or neither, which is the seven days ending at the present. It
		// reads no query: the Pipeline page follows none of the timeline's facets.
		if view.Query != nil {
			return invalid("INVALID_FRAME", "a pipeline view names a window or an at, not a query")
		}
		switch {
		case view.Window != nil && view.At != nil:
			return invalid("INVALID_FRAME", "a pipeline view names a window or an at, not both")
		case view.Window != nil:
			named, refusal := spec.readWindow(view.Window)
			if refusal != nil {
				return deliveryLiveSpec{}, refusal
			}
			spec.key = "pipeline|" + named + "|"
		case view.At != nil:
			at, err := parseOptionalRFC3339(url.Values{"at": {*view.At}}, "at", time.Time{})
			if err != nil {
				return invalid("INVALID_QUERY", err.Error())
			}
			at = at.UTC()
			spec.at = &at
			spec.key = "pipeline|" + at.Format(time.RFC3339Nano) + "|"
		default:
			spec.key = "pipeline|now|"
		}
	case legionBoardView:
		// A legion view names the project whose board it shows and nothing else: the board has no
		// window, and the project is the only thing GET /api/v1/legion/board reads.
		if view.Window != nil || view.At != nil || view.Query != nil {
			return invalid("INVALID_FRAME", "a legion view names a project, not a window, an at or a query")
		}
		if view.Project == nil {
			return invalid("INVALID_FRAME", "a legion view names its project")
		}
		project := strings.TrimSpace(*view.Project)
		if !projectKeyPattern.MatchString(project) {
			return invalid("INVALID_QUERY", projectKeyProblem("project", project))
		}
		spec.query = url.Values{"project": {project}}
		spec.key = legionBoardView + "|" + project + "|"
	default:
		return invalid("INVALID_FRAME", "view.kind must be timeline, measures, pipeline or legion")
	}
	return spec, nil
}

// project is the project a legion view's board shows, carried in the spec's query as the GET
// route reads it; every other kind names none.
func (spec deliveryLiveSpec) project() string {
	return spec.query.Get("project")
}

// readWindow reads a view's window into spec, as the GET routes read from and to, and answers how
// the key names it: a preset by its days, whose end stays at the present, or a custom range by its
// exact ends.
func (spec *deliveryLiveSpec) readWindow(window *DeliveryLiveWindow) (string, *LiveError) {
	refuse := func(code, message string) (string, *LiveError) {
		refusal := liveRefusal("", code, message)
		return "", &refusal
	}
	switch {
	case window.Preset != nil && window.From == nil && window.To == nil:
		if !slices.Contains(deliveryLivePresets, *window.Preset) {
			return refuse("INVALID_FRAME", "window.preset must be 7, 14, 28 or 84")
		}
		spec.preset = *window.Preset
		return "preset:" + strconv.Itoa(spec.preset), nil
	case window.Preset == nil && window.From != nil && window.To != nil:
		from, to, err := parseDeliveryWindow(url.Values{"from": {*window.From}, "to": {*window.To}})
		if err != nil {
			return refuse("INVALID_QUERY", err.Error())
		}
		spec.from, spec.to = from.UTC(), to.UTC()
		return spec.from.Format(time.RFC3339Nano) + "," + spec.to.Format(time.RFC3339Nano), nil
	default:
		return refuse("INVALID_FRAME", "a window is a preset or a from and to")
	}
}

// pipelineWindowAt is a pipeline view's window at now: its preset's days or its pinned at's seven,
// ending at the present, or the custom range it named, which never moves.
func (spec deliveryLiveSpec) pipelineWindowAt(now time.Time) pipelineWindow {
	switch {
	case !spec.to.IsZero():
		return pipelineWindowBetween(spec.from, spec.to)
	case spec.at != nil:
		return pipelineWindowEndingAt(*spec.at, measures.ShownDayCount)
	case spec.preset > 0:
		return pipelineWindowEndingAt(now, spec.preset)
	default:
		return pipelineWindowEndingAt(now, measures.ShownDayCount)
	}
}

// pipelineRolls is whether a pipeline view's window ends at the present, so it moves at midnight
// and as a result leaves the failing-jobs table; a pinned at and a custom range never move.
func (spec deliveryLiveSpec) pipelineRolls() bool {
	return spec.at == nil && spec.to.IsZero()
}

package metrics

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// base is the moment every fixture's hour is counted from, and `hour` is a whole number of hours
// after it: a fixture reads as the timeline it describes.
var base = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func hour(after float64) time.Time {
	return base.Add(time.Duration(after * float64(time.Hour)))
}

// newIssue is an issue of a fixture, with the maps the builders below write into.
func newIssue(key string) *Issue {
	return &Issue{Key: key, Title: key + " title", FirstRoleEvent: map[string]time.Time{}, Observed: map[string][]Mark{}}
}

// under names the issue's parent.
func (i *Issue) under(parent string) *Issue {
	i.Parent = parent
	return i
}

// closedAt closes the issue.
func (i *Issue) closedAt(at time.Time) *Issue {
	i.ClosedAt = &at
	return i
}

// worked records the first event of a session titled `Legion <role> · <this issue>`.
func (i *Issue) worked(role string, at time.Time) *Issue {
	i.FirstRoleEvent[role] = at
	return i
}

// handedOff records a handoff of the phase word.
func (i *Issue) handedOff(word string, at time.Time) *Issue {
	i.Handoffs = append(i.Handoffs, Handoff{Phase: word, Version: len(i.Handoffs) + 1, At: at})
	return i
}

// statusAt records a status snapshot.
func (i *Issue) statusAt(at time.Time, status string) *Issue {
	i.StatusMarks = append(i.StatusMarks, Mark{At: at, Value: status})
	return i
}

// observe records one value a read found changed, the JSON text as legion_observations holds it.
func (i *Issue) observe(field string, at time.Time, value string) *Issue {
	i.Observed[field] = append(i.Observed[field], Mark{At: at, Value: value})
	return i
}

// observeString records an observation whose value is a JSON string.
func (i *Issue) observeString(field string, at time.Time, value string) *Issue {
	return i.observe(field, at, fmt.Sprintf("%q", value))
}

// asked records a span an ask of this issue stood on a person's turn.
func (i *Issue) asked(from, to time.Time) *Issue {
	i.AskTurns = append(i.AskTurns, Span{From: from, To: to})
	return i
}

// fixture is a population of hand-built issues over [from, to), with its trees linked as Read
// links them.
func fixture(from, to time.Time, issues ...*Issue) *Population {
	population := &Population{Project: "INFRA", From: from, To: to, Issues: map[string]*Issue{}}
	for _, issue := range issues {
		population.Issues[issue.Key] = issue
		population.Keys = append(population.Keys, issue.Key)
	}
	slices.Sort(population.Keys)
	population.linkTrees()
	return population
}

// barOf is one role's bar of a report.
func barOf(t *testing.T, report Report, role string) RoleBar {
	t.Helper()
	for _, bar := range report.PhaseTime.Bars {
		if bar.Role == role {
			return bar
		}
	}
	t.Fatalf("the report has no %s bar: %+v", role, report.PhaseTime.Bars)
	return RoleBar{}
}

// timelineOf is an issue's phase timeline written as "<phase> <from>-<to>" in hours after base,
// which is what a phase test compares.
func timelineOf(population *Population, issue *Issue) []string {
	spans, _ := population.Phases(issue)
	written := make([]string, 0, len(spans))
	for _, span := range spans {
		written = append(written, fmt.Sprintf("%s %g-%g",
			span.Phase, span.Span.From.Sub(base).Hours(), span.Span.To.Sub(base).Hours()))
	}
	return written
}

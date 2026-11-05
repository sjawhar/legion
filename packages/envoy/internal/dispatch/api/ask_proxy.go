package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// askProxySkills maps a person, by canonical login, to the skill that holds their recorded advice.
// An agent reads that skill instead of spending the person's time, so a new ask whose target is
// one of these people carries the skill's address and what the person already answered nearby
// (model.AskProxy). A second person with such a skill is one more row here.
//
// It is a table in the server rather than a setting because the advisory is worth nothing until
// something names a person, and today exactly one person has a proxy skill. Nothing here refuses
// a write or asks for another ask to be opened.
var askProxySkills = map[string]string{
	"sami@theorem.dev": "skill://sami-proxy",
}

const (
	// askProxyAnswerLimit is how many of the target's earlier answers the advisory lists: enough
	// to show the issue's decided ground, few enough that the three related items stay on the
	// same screen. `dispatch answers` prints each of them whole.
	askProxyAnswerLimit = 5
	// askProxyLineMax bounds one listed line, in runes, with an ellipsis where it cut. An answer
	// has no length of its own worth trusting here: a one-line answer of a few hundred thousand
	// characters is a legal ask answer, and five of them would make this response larger than the
	// most a keyed write's answer may be kept at (maxKeptAnswerBytes), so a repeat under the same
	// key could not be replayed from it. A line is a pointer to the answer, not the answer.
	askProxyLineMax = 200
	// askProxyTimeout bounds the two indexed reads the advisory makes. They run after the ask is
	// committed and answered to nobody but this response, so a slow database costs the advisory
	// and never the ask.
	askProxyTimeout = time.Second
)

// askProxyAssignee reads the person an issue's asks reach: the issue's assignee, which an issue
// created under a parent inherits from it.
const askProxyAssignee = `select coalesce(assignee, '') from issues where key = $1`

// askProxyAnswers reads what one person has already answered on an issue and on that issue's
// parent: each answered ask of the two, newest answer first. The answer's `at` is an RFC 3339
// string in the row's jsonb and is compared as an instant, not as text: Go writes a whole second
// without a fraction, so "12:00:00Z" sorts above "12:00:00.1Z" as text and below it as a time.
//
// An approval Dispatch carried forward is left out. The carry answers an open approval request
// in the person's name, with the person as the answer's user and as the ask.answered event's
// actor: words the person never wrote, on a question nobody asked them, and left in, a run of
// them would push the person's real answers out of the few rows the advisory lists. What tells
// them apart is the mark the carry writes on the answer itself (model.AskAnswer.Carried, written
// by docs.answerApprovalAsk alone), never the wording of its text: the carry's sentence is a
// sentence a person may write too, and an answer of theirs that opens with it is theirs. An
// answer stored before the mark existed has none and is read as the person's, as it was then.
const askProxyAnswers = `
with scope as (
	select i.key from issues i where i.key = $1
	union
	select p.key from issues i join issues p on p.key = i.parent_key where i.key = $1
)
select a.issue_key, a.id::text, coalesce(a.answer->>'text', '')
  from asks a join scope on scope.key = a.issue_key
 where a.state = 'answered' and lower(a.answer->>'user') = $2
   and coalesce(a.answer->>'carried', '') <> 'true'
 order by (a.answer->>'at')::timestamptz desc nulls last, a.id desc
 limit $3`

// askProxyAdvice is the advisory a new ask on issueKey carries when the person it reaches has a
// proxy skill, and nil when they do not, when the issue has no assignee, or when the assignee
// could not be read. A failed answer read leaves the advisory with the answers it had, since the
// skill is worth naming on its own. Every failure is logged and costs the advisory alone: the ask
// is written and answered either way.
func (s *server) askProxyAdvice(ctx context.Context, issueKey string) *model.AskProxy {
	ctx, cancel := context.WithTimeout(ctx, askProxyTimeout)
	defer cancel()
	var assignee string
	if err := s.deps.Store.Pool.QueryRow(ctx, askProxyAssignee, issueKey).Scan(&assignee); err != nil {
		slog.Warn("dispatch: ask proxy advisory read the issue's assignee", "issue", issueKey, "error", err)
		return nil
	}
	target := model.CanonicalLogin(assignee)
	skill, isProxy := askProxySkills[target]
	if !isProxy {
		return nil
	}
	advice := &model.AskProxy{Skill: skill, Target: target, Answers: []model.AskProxyAnswer{}}
	rows, err := s.deps.Store.Pool.Query(ctx, askProxyAnswers, issueKey, target, askProxyAnswerLimit)
	if err != nil {
		slog.Warn("dispatch: ask proxy advisory read the target's answers", "issue", issueKey, "error", err)
		return advice
	}
	defer rows.Close()
	for rows.Next() {
		var answerIssue, askID, text string
		if err := rows.Scan(&answerIssue, &askID, &text); err != nil {
			slog.Warn("dispatch: ask proxy advisory read an answer", "issue", issueKey, "error", err)
			return advice
		}
		advice.Answers = append(advice.Answers, model.AskProxyAnswer{
			Ref:  "dispatch://" + answerIssue + "/ask/" + askID,
			Line: askProxyFirstLine(text),
		})
	}
	if err := rows.Err(); err != nil {
		slog.Warn("dispatch: ask proxy advisory read the target's answers", "issue", issueKey, "error", err)
	}
	return advice
}

// askProxyFirstLine is an answer's first line with no blank line in front of it, cut at
// askProxyLineMax runes with an ellipsis, as the advisory lists it; an answer that chose options
// and wrote no words has none.
func askProxyFirstLine(text string) string {
	for line := range strings.Lines(text) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if runes := []rune(trimmed); len(runes) > askProxyLineMax {
			return string(runes[:askProxyLineMax]) + "…"
		}
		return trimmed
	}
	return ""
}

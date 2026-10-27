package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// parseIssuePriority decodes the tri-state `priority` field of an issue write. Absent →
// (nil, false): leave it alone. JSON null → (nil, true): clear it. An integer 0..3 → its
// value. Anything else → 400 INVALID_PRIORITY.
func parseIssuePriority(raw json.RawMessage) (priority *int, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 || value > 3 {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_PRIORITY", "priority must be an integer from 0 to 3 or null")
	}
	return &value, true, nil
}

func (s *server) createIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Project    string          `json:"project"`
		Title      string          `json:"title"`
		Parent     *string         `json:"parent"`
		BlockedBy  []string        `json:"blocked_by"`
		External   string          `json:"external"`
		Force      bool            `json:"force"`
		Spec       *string         `json:"spec"`
		Labels     []string        `json:"labels"`
		Priority   json.RawMessage `json:"priority"`
		Assignee   json.RawMessage `json:"assignee"`
		Components json.RawMessage `json:"components"`
		Repo       json.RawMessage `json:"repo"`
		Actor      *model.Actor    `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	blockers, err := normalizeIssueBlockers(input.BlockedBy)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	input.Project = strings.TrimSpace(input.Project)
	input.Title = strings.TrimSpace(input.Title)
	parentKey := ""
	if input.Parent != nil {
		parentKey = strings.TrimSpace(*input.Parent)
	}

	var externalRepo, externalNumber string
	usingDefaultProject := false
	if input.External != "" {
		var err error
		externalRepo, externalNumber, err = externalRef(input.External)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		project, err := s.repoProject(r.Context(), externalRepo)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if project == "" {
			project = s.deps.DefaultProject
			if project == "" {
				writeError(w, "PROJECT_UNMAPPED", http.StatusBadRequest,
					"repository is not mapped in repository settings and DISPATCH_DEFAULT_PROJECT is not configured")
				return
			}
			usingDefaultProject = true
		}
		if input.Project != "" && input.Project != project {
			writeError(
				w,
				"EXTERNAL_PROJECT_MISMATCH",
				http.StatusBadRequest,
				fmt.Sprintf(
					"external issue project %s must match repository settings project %s",
					input.Project,
					project,
				),
			)
			return
		}
		input.Project = project
		existingKey, err := s.resolveIssueRef(r.Context(), input.External)
		if err == nil {
			var existingProject string
			if err := s.deps.Store.Pool.QueryRow(r.Context(), `
				select project_key from issues where key = $1
			`, existingKey).Scan(&existingProject); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if existingProject != project {
				writeError(
					w,
					"EXTERNAL_PROJECT_MISMATCH",
					http.StatusBadRequest,
					fmt.Sprintf(
						"external issue project %s must match repository settings project %s",
						existingProject,
						project,
					),
				)
				return
			}
			s.respondWithExistingExternalIssue(w, r, existingKey, project, actor)
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			s.writeHandlerError(w, err)
			return
		}
		if input.Title == "" {
			input.Title = input.External
		}
	}
	if !projectKeyPattern.MatchString(input.Project) {
		writeError(w, "INVALID_ISSUE", http.StatusBadRequest, projectKeyProblem("project", input.Project))
		return
	}
	if input.Title == "" {
		writeError(w, "INVALID_ISSUE", http.StatusBadRequest, "title is required")
		return
	}
	// Before the duplicate check, which reads the new title against every title in the project.
	if length := len16(input.Title); length > contracts.IssueTitleMax {
		capExceeded(w, "title", length, contracts.IssueTitleMax)
		return
	}
	if usingDefaultProject {
		input.Labels = append(input.Labels, repoLabelPrefix+externalRepo)
	}
	labels, err := normalizeIssueLabels(input.Labels)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	priority, _, err := parseIssuePriority(input.Priority)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	assignee, assigneeProvided, err := s.parseIssueAssignee(r.Context(), input.Assignee)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	components, componentsProvided, err := parseIssueComponents(input.Components)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo, _, err := parseIssueRepo(input.Repo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The title-overlap refusal, and under force the same overlaps named in the result instead
	// (INFRA-568): a forced create is told what it forced past, but a slow or failing overlap read
	// must not refuse a create its caller already insisted on, so it is bounded on its own and its
	// failure only logged.
	var titleMatches []model.DuplicateCandidate
	if input.External == "" && input.Force {
		matchCtx, cancel := context.WithTimeout(r.Context(), forcedTitleMatchTimeout)
		candidates, err := s.duplicateCandidates(matchCtx, s.deps.Store.Pool, input.Project, input.Title, parentKey)
		cancel()
		if err != nil {
			slog.Warn("dispatch: forced issue create names no title overlaps", "project", input.Project, "error", err)
		} else {
			titleMatches = candidates
		}
	}
	if input.External == "" && !input.Force {
		candidates, err := s.duplicateCandidates(r.Context(), s.deps.Store.Pool, input.Project, input.Title, parentKey)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if len(candidates) > 0 {
			WriteJSON(w, http.StatusConflict, map[string]any{
				"error":      fmt.Sprintf("possible duplicate of %s: %s", candidates[0].Key, candidates[0].Title),
				"code":       "POSSIBLE_DUPLICATE",
				"candidates": candidates,
			})
			return
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// A create with blockers can close a wait cycle, and a create with a parent adds a link the
	// depth cap counts (refuseParentTooDeep): both serialize on the project's dependency lock with
	// every other blocker and parent write. A new issue with neither takes no dependency lock.
	if len(blockers) > 0 || parentKey != "" {
		if err := lockProjectIssueDependencies(r.Context(), tx, input.Project); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var parentAssignee *string
	if parentKey != "" {
		var parentProject string
		err := tx.QueryRow(r.Context(), `select project_key, assignee from issues where key = $1`, parentKey).Scan(&parentProject, &parentAssignee)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "PARENT_INPUT", http.StatusBadRequest, fmt.Sprintf("parent issue %s not found", parentKey))
			return
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if parentProject != input.Project {
			writeError(w, "PARENT_INPUT", http.StatusBadRequest, "parent must be in the same project")
			return
		}
		if err := refuseParentTooDeep(r.Context(), tx, "", parentKey); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var number int
	var projectDefaultAssignee *string
	if err := tx.QueryRow(r.Context(), `
		update projects
		set next_number = next_number + 1
		where key = $1
		-- The default_assignee subquery is not the raw stored value: it yields the stored default
		-- only when that email still names a person who has signed in (people.email is the primary
		-- key, so this is one index probe inside a statement the create runs anyway), and null for
		-- a project with no default or one whose default has gone stale.
		returning next_number - 1, (select email from people where email = projects.default_assignee)
	`, input.Project).Scan(&number, &projectDefaultAssignee); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The project's stored default is the chain's last step, and this update already holds the
	// project row: the decision moves below it so a create in a project with no default costs no
	// round trip and takes no lock of its own.
	if !assigneeProvided {
		assignee = defaultAssignee(actor, parentAssignee, projectDefaultAssignee)
	}
	// The project row is locked from here; the mapping check takes its key-share lock only now, in
	// the project-then-mapping order every mapping write follows (lockRepoProjectWrite).
	if repo != nil {
		if err := checkIssueRepoMapped(r.Context(), tx, *repo, input.Project); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	key := fmt.Sprintf("%s-%d", input.Project, number)
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var parent any
	if parentKey != "" {
		parent = parentKey
	}
	if err := lockProjectRankAllocation(r.Context(), tx, input.Project); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	lastRank, err := lastProjectRank(r.Context(), tx, input.Project, "")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issueRank := rank.Between(lastRank, "")

	if _, err := tx.Exec(r.Context(), `
		insert into issues (key, project_key, number, title, parent_key, created_by, labels, priority, rank, assignee, repo)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, key, input.Project, number, input.Title, parent, actorJSON, labels, priority, issueRank, assignee, repo); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := writeBlockedBy(r.Context(), tx, key, input.Project, blockers); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if componentsProvided {
		if err := writeIssueComponents(r.Context(), tx, key, input.Project, *components); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.External != "" {
		if _, err := tx.Exec(r.Context(), `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github_issue')
		`, key, externalURL(externalRepo, externalNumber)); err != nil {
			if !isUniqueViolation(err) {
				s.writeHandlerError(w, err)
				return
			}
			if err := tx.Rollback(r.Context()); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			existingKey, err := s.resolveIssueRef(r.Context(), input.External)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			s.respondWithExistingExternalIssue(w, r, existingKey, input.Project, actor)
			return
		}
	}

	var artifactID string
	if err := tx.QueryRow(r.Context(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'spec', 'spec.md', 'doc', true, $3)
		returning id::text
	`, key, input.Project, actorJSON).Scan(&artifactID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// An issue created without a spec gets an empty primary document.
	var markdown string
	if input.Spec != nil && strings.TrimSpace(*input.Spec) != "" {
		markdown = *input.Spec
	}
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	markdown, err = s.deps.Docs.SeedText(documentCtx, artifactID, markdown, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	authors, err := encodeJSON([]model.Actor{actor})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		insert into artifact_versions (artifact_id, number, markdown, authors, doc_update_version)
		values ($1, 1, $2, $3, coalesce((select max(version) from doc_updates where artifact_id = $1), 0))
	`, artifactID, markdown, authors); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The version row exists now, so the count names it (LEGION-542).
	if err := docs.RecordTaskProgressMarkdown(r.Context(), tx, artifactID, markdown); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	specChanges, err := s.deps.Docs.IndexDocumentReferences(documentCtx, artifactID, 1, markdown)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue.LastSeq++
	event, err := s.appendEvent(r.Context(), tx, issueOwner(key).event(
		"issue.created",
		actor,
		model.NewIssueEventPayload(issue, specChanges),
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "artifact", artifactID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	advice := s.writeAdvice(
		r.Context(), tx, "POST /api/v1/issues", key, actor, "", issue.Status,
	)
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A seeded spec never passed through a live edit, so nothing has queued the document
	// closer that indexes its ask blocks and repairs block ids; queue it now that the
	// transaction is durable.
	s.deps.Docs.ScheduleSettlement(artifactID)
	s.publish(event)
	if advice != nil && input.Spec != nil && strings.TrimSpace(*input.Spec) != "" {
		advice.documentBlocks = readDocumentBlocks(markdown)
	}
	// INFRA-568: the related-item check runs after the write committed, whatever writeAdvice
	// answered, and waits at most relatedAllowance; its result rides the advice, or advice of its
	// own when the write-advice read failed.
	suggestions := s.relatedCheck(r.Context(), relatedInput{
		project: input.Project,
		text:    input.Title + "\n" + markdown,
		source:  suggestionSource{kind: "issue", issueKey: key, actor: actor},
		record:  true,
	})
	suggestions.TitleMatches = titleMatches
	WriteJSON(w, http.StatusCreated, withSuggestions(issue, advice, suggestions))
}

// forcedTitleMatchTimeout bounds the title-overlap read a forced issue create runs only to name
// what it forced past: the create is not refused on it, so it must not wait long on it either.
const forcedTitleMatchTimeout = time.Second

// respondWithExistingExternalIssue answers a create naming a GitHub issue Dispatch already holds
// with that issue. Nothing was created, so no related-item check is made, and the result says so
// (and the check is recorded as not made, reason already_exists).
func (s *server) respondWithExistingExternalIssue(
	w http.ResponseWriter,
	r *http.Request,
	existingKey string,
	project string,
	actor model.Actor,
) {
	adviceReadTx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer adviceReadTx.Rollback(r.Context())
	var existingStatus string
	if err := adviceReadTx.QueryRow(r.Context(), `
		select status from issues where key = $1 for no key update
	`, existingKey).Scan(&existingStatus); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	existing, err := s.loadIssue(r.Context(), adviceReadTx, existingKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	advice := s.writeAdvice(
		r.Context(), adviceReadTx, "POST /api/v1/issues", existingKey, actor, "", existingStatus,
	)
	if err := adviceReadTx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	suggestions := s.relatedCheckNotRun(r.Context(), relatedInput{
		project: project,
		source:  suggestionSource{kind: "issue", issueKey: existingKey, actor: actor},
		record:  true,
	}, relatedReasonAlreadyExists, "Dispatch already holds this issue; nothing was created")
	WriteJSON(w, http.StatusOK, withSuggestions(existing, advice, suggestions))
}

func isUniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}

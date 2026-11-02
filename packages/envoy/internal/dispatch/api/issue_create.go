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
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
	}
	// The one write whose project its address does not name, so withinProjectScope leaves it
	// here by its class (scopeProjectInBody). It runs the moment that project is settled —
	// read from the body above, or from the repository the external ref names — and so before
	// the ref is resolved to an issue already imported: a scoped caller naming one imported
	// into another project is refused rather than answered that project's issue, which
	// respondWithExistingExternalIssue would also record a related-check outcome on. Nothing
	// is written before this point.
	if covers, scoped := s.callerProject(r); scoped && covers != input.Project {
		refuseWriteOutsideProjectScope(w, covers, input.Project)
		return
	}
	if input.External != "" {
		existingKey, err := s.resolveIssueRef(r.Context(), input.External)
		if err == nil {
			var existingProject string
			if err := s.deps.Store.Pool.QueryRow(r.Context(), `
				select project_key from issues where key = $1
			`, existingKey).Scan(&existingProject); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if existingProject != input.Project {
				writeError(
					w,
					"EXTERNAL_PROJECT_MISMATCH",
					http.StatusBadRequest,
					fmt.Sprintf(
						"external issue project %s must match repository settings project %s",
						existingProject,
						input.Project,
					),
				)
				return
			}
			s.respondWithExistingExternalIssue(w, r, existingKey, input.Project, actor)
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
	// A components field that parsed is non-nil exactly when the create named one.
	components, _, err := parseIssueComponents(input.Components)
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
	// a forced create is told what it forced past, but a slow or failing overlap read
	// must not refuse a create its caller already insisted on, so it is bounded on its own and its
	// failure only logged.
	var titleMatches []model.DuplicateCandidate
	if input.External == "" {
		if input.Force {
			matchCtx, cancel := context.WithTimeout(r.Context(), forcedTitleMatchTimeout)
			candidates, err := s.duplicateCandidates(matchCtx, s.deps.Store.Pool, input.Project, input.Title, parentKey)
			cancel()
			if err != nil {
				slog.Warn("dispatch: forced issue create names no title overlaps", "project", input.Project, "error", err)
			} else {
				titleMatches = candidates
			}
		} else {
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
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	var spec string
	if input.Spec != nil {
		spec = *input.Spec
	}
	inserted, err := s.insertIssue(r.Context(), tx, documentCtx, ledger, issueInsert{
		project:          input.Project,
		title:            input.Title,
		parentKey:        parentKey,
		blockers:         blockers,
		labels:           labels,
		priority:         priority,
		assignee:         assignee,
		assigneeProvided: assigneeProvided,
		components:       components,
		repo:             repo,
		externalRepo:     externalRepo,
		externalNumber:   externalNumber,
		spec:             spec,
		actor:            actor,
	})
	if errors.Is(err, errExternalIssueExists) {
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
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	key, issue := inserted.issue.Key, inserted.issue
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
	s.deps.Docs.ScheduleSettlement(inserted.specID)
	s.publish(inserted.event)
	if advice != nil && strings.TrimSpace(spec) != "" {
		advice.documentBlocks = documentBlocksFor(inserted.specReadBack, inserted.specMarkdown)
	}
	// The related-item check runs after the write committed, whatever writeAdvice
	// answered, and waits at most relatedAllowance; its result rides the advice, or advice of its
	// own when the write-advice read failed.
	suggestions := s.relatedCheck(r.Context(), relatedInput{
		project: input.Project,
		text:    input.Title + "\n" + inserted.specMarkdown,
		title:   input.Title,
		source:  suggestionSource{kind: "issue", issueKey: key, actor: actor},
		record:  true,
	})
	suggestions.TitleMatches = titleMatches
	WriteJSON(w, http.StatusCreated, withSuggestions(issue, advice, suggestions))
}

// issueInsert is a new native issue as its route has checked it: what insertIssue writes.
type issueInsert struct {
	project          string
	title            string
	parentKey        string
	blockers         []string
	labels           []string
	priority         *int
	assignee         *string
	assigneeProvided bool
	// assigneeIsProjectDefault gives the issue to the project's default assignee, or nobody,
	// whoever created it: a report's reporter is not the one who fixes it (reports.go).
	assigneeIsProjectDefault bool
	components               *componentsInput
	repo                     *string
	// route is the issue's route from its first event, nil for none: the route a report is given
	// when its reporter asks an agent to fix it (reports.go).
	route *string
	// externalRepo and externalNumber name the GitHub issue a create imports, empty for none.
	externalRepo   string
	externalNumber string
	spec           string
	// beforeSpec, when set, runs once the issue's row and its spec's row exist, and answers the
	// spec's text in place of spec: a report writes its pictures as the issue's own files there,
	// so the spec it seeds can show them.
	beforeSpec func(ctx context.Context, tx pgx.Tx, key string) (string, error)
	actor      model.Actor
}

// insertedIssue is what insertIssue wrote: the issue as read back, its issue.created event,
// appended but not published, and its spec's id, text and read-back.
type insertedIssue struct {
	issue        model.Issue
	event        model.Event
	specID       string
	specMarkdown string
	specReadBack *pmdoc.Node
}

// errExternalIssueExists is insertIssue finding the GitHub issue it imports already linked to an
// issue, which another create imported meanwhile: its caller rolls back and answers that issue.
var errExternalIssueExists = errors.New("the external issue is already imported")

// insertIssue writes a new native issue inside tx, whose document operations documentCtx and
// ledger record (Docs.Join): its number and rank, its row, its blockers, components and external
// link, its spec, and its issue.created event. The caller has checked every field and commits.
func (s *server) insertIssue(
	ctx context.Context,
	tx pgx.Tx,
	documentCtx context.Context,
	ledger *docs.Ledger,
	in issueInsert,
) (insertedIssue, error) {
	// A create with blockers can close a wait cycle, and a create with a parent adds a link the
	// depth cap counts (refuseParentTooDeep): both serialize on the project's dependency lock with
	// every other blocker and parent write. A new issue with neither takes no dependency lock.
	if len(in.blockers) > 0 || in.parentKey != "" {
		if err := lockProjectIssueDependencies(ctx, tx, in.project); err != nil {
			return insertedIssue{}, err
		}
	}
	var parentAssignee *string
	if in.parentKey != "" {
		var parentProject string
		err := tx.QueryRow(ctx, `select project_key, assignee from issues where key = $1`, in.parentKey).Scan(&parentProject, &parentAssignee)
		if errors.Is(err, pgx.ErrNoRows) {
			return insertedIssue{}, errorf(http.StatusBadRequest, "PARENT_INPUT", "parent issue %s not found", in.parentKey)
		}
		if err != nil {
			return insertedIssue{}, err
		}
		if parentProject != in.project {
			return insertedIssue{}, errorf(http.StatusBadRequest, "PARENT_INPUT", "parent must be in the same project")
		}
		if err := refuseParentTooDeep(ctx, tx, "", in.parentKey); err != nil {
			return insertedIssue{}, err
		}
	}
	var number int
	var projectDefaultAssignee *string
	if err := tx.QueryRow(ctx, `
		update projects
		set next_number = next_number + 1
		where key = $1
		-- The default_assignee subquery is not the raw stored value: it yields the stored default
		-- only when that email still names a person who has signed in (people.email is the primary
		-- key, so this is one index probe inside a statement the create runs anyway), and null for
		-- a project with no default or one whose default has gone stale.
		returning next_number - 1, (select email from people where email = projects.default_assignee)
	`, in.project).Scan(&number, &projectDefaultAssignee); err != nil {
		return insertedIssue{}, err
	}
	// The project's stored default is the chain's last step, and this update already holds the
	// project row: the decision moves below it so a create in a project with no default costs no
	// round trip and takes no lock of its own.
	assignee := in.assignee
	switch {
	case in.assigneeIsProjectDefault:
		assignee = projectDefaultAssignee
	case !in.assigneeProvided:
		assignee = defaultAssignee(in.actor, parentAssignee, projectDefaultAssignee)
	}
	// The project row is locked from here; the mapping check takes its key-share lock only now, in
	// the project-then-mapping order every mapping write follows (lockRepoProjectWrite).
	if in.repo != nil {
		if err := checkIssueRepoMapped(ctx, tx, *in.repo, in.project); err != nil {
			return insertedIssue{}, err
		}
	}
	key := fmt.Sprintf("%s-%d", in.project, number)
	actorJSON, err := encodeJSON(in.actor)
	if err != nil {
		return insertedIssue{}, err
	}
	var parent any
	if in.parentKey != "" {
		parent = in.parentKey
	}
	if err := lockProjectRankAllocation(ctx, tx, in.project); err != nil {
		return insertedIssue{}, err
	}

	lastRank, err := lastProjectRank(ctx, tx, in.project, "")
	if err != nil {
		return insertedIssue{}, err
	}
	issueRank := rank.Between(lastRank, "")

	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, created_by, labels, priority, rank, assignee, repo, route)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, key, in.project, number, in.title, parent, actorJSON, in.labels, in.priority, issueRank, assignee, in.repo, in.route); err != nil {
		return insertedIssue{}, err
	}
	if err := writeBlockedBy(ctx, tx, key, in.project, in.blockers); err != nil {
		return insertedIssue{}, err
	}
	if in.components != nil {
		if err := writeIssueComponents(ctx, tx, key, in.project, *in.components); err != nil {
			return insertedIssue{}, err
		}
	}
	if in.externalRepo != "" {
		if _, err := tx.Exec(ctx, `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github_issue')
		`, key, externalURL(in.externalRepo, in.externalNumber)); err != nil {
			if isUniqueViolation(err) {
				return insertedIssue{}, errExternalIssueExists
			}
			return insertedIssue{}, err
		}
	}

	var artifactID string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'spec', 'spec.md', 'doc', true, $3)
		returning id::text
	`, key, in.project, actorJSON).Scan(&artifactID); err != nil {
		return insertedIssue{}, err
	}
	spec := in.spec
	if in.beforeSpec != nil {
		if spec, err = in.beforeSpec(ctx, tx, key); err != nil {
			return insertedIssue{}, err
		}
	}
	// An issue created without a spec gets an empty primary document.
	var markdown string
	if strings.TrimSpace(spec) != "" {
		markdown = spec
	}
	markdown, err = s.deps.Docs.SeedText(documentCtx, artifactID, markdown, in.actor)
	if err != nil {
		return insertedIssue{}, err
	}
	authors, err := encodeJSON([]model.Actor{in.actor})
	if err != nil {
		return insertedIssue{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, doc_update_version)
		values ($1, 1, $2, $3, coalesce((select max(version) from doc_updates where artifact_id = $1), 0))
	`, artifactID, markdown, authors); err != nil {
		return insertedIssue{}, err
	}
	// documentReadBack is the rendering the seed stored, read back by its own parse (docs'
	// Ledger.ReadBackOf), which the task count and the advice's blocks read in place of parsing
	// the same markdown again (recordDocumentTaskProgress, documentBlocksFor).
	documentReadBack := ledger.ReadBackOf(artifactID, markdown)
	// The version row exists now, so the count names it (LEGION-542).
	err = recordDocumentTaskProgress(ctx, tx, artifactID, documentReadBack, markdown)
	if err != nil {
		return insertedIssue{}, err
	}
	specChanges, err := s.deps.Docs.IndexDocumentReferences(documentCtx, artifactID, 1, markdown)
	if err != nil {
		return insertedIssue{}, err
	}
	issue, err := s.loadIssue(ctx, tx, key)
	if err != nil {
		return insertedIssue{}, err
	}
	issue.LastSeq++
	// A new issue's blocked_by edges move each blocker's count, beside what its spec cites.
	blockerChanges, err := issueEdgeChanges(ctx, tx, unsetIssue, issue, false)
	if err != nil {
		return insertedIssue{}, err
	}
	// A new issue changed every field it holds a value in, measured from an issue with none.
	payload, err := newIssueEventPayload(ctx, tx, issue, refs.MergeChanges(specChanges, blockerChanges), issueChangedFields(unsetIssue, issue))
	if err != nil {
		return insertedIssue{}, err
	}
	event, err := s.appendEvent(ctx, tx, issueOwner(key).event("issue.created", in.actor, payload))
	if err != nil {
		return insertedIssue{}, err
	}
	if err := refs.Stamp(ctx, tx, "artifact", artifactID, event.ID); err != nil {
		return insertedIssue{}, err
	}
	return insertedIssue{
		issue:        issue,
		event:        event,
		specID:       artifactID,
		specMarkdown: markdown,
		specReadBack: documentReadBack,
	}, nil
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

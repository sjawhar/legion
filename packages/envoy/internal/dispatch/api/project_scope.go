package api

// project_scope.go answers one question — which project does the object a write addresses
// belong to? — and refuses a caller whose credential names another project. owner.go is not
// this: it resolves collaboration ownership, the issue or unlinked document a comment, an ask or
// an event hangs from and whether that owner is still open, and decides no authority at all.
// These resolvers decide authority.
//
// The check runs in one place, withinProjectScope, which register wraps around every mutating
// route after the caller is resolved and before the handler, rather than in each of the seventy
// routes that write. Two things make a pre-handler read sound, and both were read out of the
// tree rather than assumed:
//
//   - Nothing moves an object between projects. An issue's, an artifact's, an ask's, a comment's
//     and a message's project is written when the row is inserted and updated by no route: no
//     `update ... set project_key` and no `update ... set issue_key` exists outside the
//     migrations and the tests, and the two writes that take another issue's project into
//     account (a parent, a rank neighbour) refuse one outside the issue's own project rather
//     than move anything. So the project this check reads before the handler runs is the project
//     the handler will write, and the check needs no transaction of its own.
//   - The check is a no-op for a credential that carries no project: a browser cookie, a personal
//     agent token, a Kubernetes service-account token and the shared token name none, and so does
//     an access token the broker scoped to a person rather than to one project — a dev box
//     session, whose `project_scope` is `person`, reaches brokerTokenActor with no Project and is
//     unscoped here, with no second mechanism of its own. That is the rule and not a transitional
//     measure: every caller that exists today writes exactly what it wrote before, which is what
//     lets this deploy before anything presents a broker token.
//
// A target that does not exist resolves to no project and is left to its handler, which answers
// the 404 it has always answered: this check never turns an unknown id into an answer about
// which projects exist, and never refuses a caller for addressing an object that is not there.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/brokertoken"
)

const (
	// codeProjectScope refuses a write to an object of a project the caller's credential does
	// not name.
	codeProjectScope = "PROJECT_SCOPE"
	// codeProjectScopeDeployment refuses a project-scoped caller a write that belongs to the
	// deployment rather than to any one project.
	codeProjectScopeDeployment = "PROJECT_SCOPE_DEPLOYMENT"
)

// issueCreateRoute is the one row classed as a project's write whose address names no object:
// POST /api/v1/issues takes its project from the body, or from the repository an external ref
// names, so createIssue makes this check itself once that project is settled
// (refuseWriteOutsideProjectScope). Every other project row is decided by withinProjectScope.
const issueCreateRoute = http.MethodPost + " /api/v1/issues"

// callerProject is the one project this request's credential lets the caller write, and whether
// it names one at all. Only an access token the secrets broker issued for a named project
// carries one (model.Actor.Project, set by brokerTokenActor from the token and never from a
// body), so a request presenting no DPoP-bound token is unscoped without resolving anything. A
// credential that does not resolve is unscoped too: its own refusal is the handler's to write.
func (s *server) callerProject(r *http.Request) (string, bool) {
	if _, presented := brokertoken.Presented(r); !presented {
		return "", false
	}
	actor, _, err := s.optionalActor(r)
	if err != nil || actor.Project == nil || *actor.Project == "" {
		return "", false
	}
	return *actor.Project, true
}

// projectResolver answers the project a request's addressed object belongs to. An empty project
// with no error means the address named no project: a target that does not exist, an id that is
// not an id, or a message on no issue.
type projectResolver func(ctx context.Context, q queryer, r *http.Request) (string, error)

// projectResolverFor is the resolver a route's address calls for, read from the shape of its
// pattern, and whether the pattern names an object at all. Every row the table classes
// scopeProject has one, except issueCreateRoute; unclassedWrites holds the table to that and
// register refuses to mount a table that breaks it.
func (s *server) projectResolverFor(pattern string) (projectResolver, bool) {
	switch {
	case addresses(pattern, "/api/v1/issues/{key}"):
		return s.projectOfIssue, true
	case addresses(pattern, "/api/v1/projects/{key}"):
		return s.projectOfProject, true
	case addresses(pattern, "/api/v1/artifacts/{id}"):
		return s.projectOfArtifact, true
	case addresses(pattern, "/api/v1/asks/{id}"):
		return s.projectOfAsk, true
	case addresses(pattern, "/api/v1/comments/{id}"):
		return s.projectOfComment, true
	case addresses(pattern, "/api/v1/messages/{id}"):
		return s.projectOfMessage, true
	}
	return nil, false
}

// addresses answers whether pattern addresses the object prefix names: that object itself, or
// anything under it.
func addresses(pattern, prefix string) bool {
	return pattern == prefix || strings.HasPrefix(pattern, prefix+"/")
}

// projectOfIssue reads the project column of the issue the path names, the same one-row read
// every handler that needs an issue's project makes (issue_patch.go, issue_parent.go).
func (s *server) projectOfIssue(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `select project_key from issues where key = $1`, r.PathValue("key")))
}

// projectOfProject is loadProject: a project's own key is the project it belongs to, and a key
// no project has resolves to none.
func (s *server) projectOfProject(ctx context.Context, q queryer, r *http.Request) (string, error) {
	project, err := s.loadProject(ctx, q, r.PathValue("key"))
	if unaddressed(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return project.Key, nil
}

// projectOfArtifact reads the project_key every artifact carries (artifactColumns), which is
// its issue's for an issue document and its own for a project document. An agent conversation's
// artifact carries none and resolves to no project; no row classed scopeProject addresses one.
func (s *server) projectOfArtifact(ctx context.Context, q queryer, r *http.Request) (string, error) {
	id, err := parseArtifactID(r.PathValue("id"))
	if err != nil {
		return "", nil
	}
	return scanProjectKey(q.QueryRow(ctx, `select coalesce(project_key, '') from artifacts where id = $1`, id))
}

// projectOfAsk resolves an ask through the join askSnoozeEvent already makes: an ask hangs from
// an issue or from a document, and takes that owner's project.
func (s *server) projectOfAsk(ctx context.Context, q queryer, r *http.Request) (string, error) {
	id, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		return "", nil
	}
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key, '')
		from asks a
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		where a.id = $1
	`, id))
}

// projectOfComment is one hop to the comment's issue or document, since the comments table holds
// no project of its own (store/migrations/0001_init.up.sql, 0009_project_artifacts.up.sql).
func (s *server) projectOfComment(ctx context.Context, q queryer, r *http.Request) (string, error) {
	id, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		return "", nil
	}
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key, '')
		from comments c
		left join issues i on i.key = c.issue_key
		left join artifacts ar on ar.id = c.artifact_id
		where c.id = $1
	`, id))
}

// projectOfMessage is the same one hop for a message, which hangs from an issue or from nothing
// at all: a direct message between a person and a session belongs to no project, and the two
// routes a session answers its own delivery on are classed scopeSession rather than this.
func (s *server) projectOfMessage(ctx context.Context, q queryer, r *http.Request) (string, error) {
	id, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		return "", nil
	}
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, '')
		from messages m
		left join issues i on i.key = m.issue_key
		where m.id = $1
	`, id))
}

// scanProjectKey reads a resolver's one column, answering no project for a target that does not
// exist rather than an error, so the handler keeps the answer it has today.
func scanProjectKey(row pgx.Row) (string, error) {
	var project string
	if err := row.Scan(&project); err != nil {
		if unaddressed(err) {
			return "", nil
		}
		return "", err
	}
	return project, nil
}

// unaddressed answers whether err means the target does not exist, which is the handler's answer
// to make and not a refusal to make here.
func unaddressed(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var refusal *apiError
	return errors.As(err, &refusal) && refusal.status == http.StatusNotFound
}

// canonicalUUID is the form Postgres takes, as every handler that reads a uuid path value sends
// it (state.go): an id that is not a uuid addresses nothing.
func canonicalUUID(id string) (string, bool) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

// withinProjectScope runs the project check in front of one mutating route's handler. For a
// caller whose credential names no project — every caller that exists today — it costs one
// header read and calls the handler.
func (s *server) withinProjectScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	resolve, _ := s.projectResolverFor(route.Pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		covers, scoped := s.callerProject(r)
		if !scoped {
			handler(w, r)
			return
		}
		if route.Scope == scopeDeployment {
			writeError(w, codeProjectScopeDeployment, http.StatusForbidden, fmt.Sprintf(
				"this request's credential may write project %s and nothing else; %s %s belongs to the deployment rather than to a project",
				covers, route.Method, route.Pattern))
			return
		}
		if route.Scope != scopeProject || resolve == nil {
			// A write that belongs to the calling session, and the issue create, whose project
			// its handler reads from the body.
			handler(w, r)
			return
		}
		belongsTo, err := resolve(r.Context(), s.deps.Store.Pool, r)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if belongsTo == "" || belongsTo == covers {
			handler(w, r)
			return
		}
		refuseWriteOutsideProjectScope(w, covers, belongsTo)
	}
}

// refuseWriteOutsideProjectScope names the project the caller's credential covers and the
// project the write belongs to, and nothing of the credential itself.
func refuseWriteOutsideProjectScope(w http.ResponseWriter, covers, belongsTo string) {
	writeError(w, codeProjectScope, http.StatusForbidden, fmt.Sprintf(
		"this request's credential may write project %s; this write belongs to project %s", covers, belongsTo))
}

// unclassedWrites names every mutating row the project check cannot decide: one writeClasses
// gives no class, and one classed scopeProject whose address names no object to resolve.
// TestEveryMutatingRouteIsClassed walks the table through it, and register refuses to mount a
// table it names anything in, so a row added later cannot be unclassed. The other direction, a
// writeClasses name that is no row of the table, is the same test's (nothing is served by it,
// so it cannot admit a write).
func (s *server) unclassedWrites(routes []apiRoute) []string {
	var unclassed []string
	for _, route := range routes {
		if route.Method == http.MethodGet {
			continue
		}
		name := route.Method + " " + route.Pattern
		switch route.Scope {
		case scopeSession, scopeDeployment:
		case scopeProject:
			if _, addressed := s.projectResolverFor(route.Pattern); !addressed && name != issueCreateRoute {
				unclassed = append(unclassed, name+": classed as a project's write, but its address names no object whose project can be resolved")
			}
		default:
			unclassed = append(unclassed, name+": no write class (writeClasses)")
		}
	}
	return unclassed
}

package api

// project_scope.go answers what a write is about to touch — which project does the object it
// addresses belong to, and which session does its path name? — and refuses a caller whose
// credential covers another project, or proves another session. owner.go is not this: it
// resolves collaboration ownership, the issue or unlinked document a comment, an ask or an
// event hangs from and whether that owner is still open, and decides no authority at all.
// These resolvers decide authority.
//
// Each check runs in one place — withinProjectScope and withinSessionScope, which register
// wraps around every mutating route after the caller is resolved and before the handler —
// rather than in each of the seventy routes that write. Two things make a pre-handler read
// sound, and both were read out of the tree rather than assumed:
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
// For a credential that does name a project, the check fails closed: only a target that
// resolves to that credential's own project reaches the handler. A target that resolves to
// another project, to no project at all, or to nothing at all — no such row, or an id Postgres
// will not read as a uuid — is refused. Each resolver sends the raw path value to Postgres, as
// the handler behind it does, so the parser that decides what this check resolved is the parser
// the handler then addresses its row by, and the two cannot disagree about which object a path
// names: Go's uuid.Parse refuses spellings Postgres accepts (a hyphen after any group of four
// digits), and a check that resolved nothing where the handler resolves a row would be a write
// nothing judged.
//
// Failing closed does not close the existence oracle, and does not claim to. The two refusals
// still read differently — a target of another project is told which project that is, and one
// that resolved to nothing is told the write resolves to no project of that credential's — so a
// scoped caller can tell an object of another project from an id nobody holds. Nothing is read
// out that way: no GET route is project-scoped, so every answer this check can colour is a
// refusal of the caller's own write, and what it learns is which of its own refused writes
// addressed something that exists.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	// codeProjectScope refuses a write the caller's credential does not cover: an object of
	// another project, or one this check could not resolve to the caller's own.
	codeProjectScope = "PROJECT_SCOPE"
	// codeProjectScopeDeployment refuses a project-scoped caller a write that belongs to the
	// deployment rather than to any one project.
	codeProjectScopeDeployment = "PROJECT_SCOPE_DEPLOYMENT"
	// codeSessionScope refuses a caller whose credential proved one session a write addressed
	// to another session's own rows.
	codeSessionScope = "SESSION_SCOPE"
)

// brokerCredentialActor is the caller an access token the secrets broker issued decided, and
// whether this request presents one that resolved at all. Every other credential answers false
// without resolving anything: a browser cookie, a personal agent token, a Kubernetes
// service-account token and the shared token each name no project and prove no session, so a
// request presenting no DPoP-bound token is outside both checks. A credential that does not
// resolve is outside them too: its own refusal is the handler's to write.
func (s *server) brokerCredentialActor(r *http.Request) (model.Actor, bool) {
	if _, presented := brokertoken.Presented(r); !presented {
		return model.Actor{}, false
	}
	actor, _, err := s.optionalActor(r)
	if err != nil {
		return model.Actor{}, false
	}
	return actor, true
}

// callerProject is the one project this request's credential lets the caller write, and whether
// it names one at all. Only an access token the secrets broker issued for a named project
// carries one (model.Actor.Project, set by brokerTokenActor from the token and never from a
// body).
func (s *server) callerProject(r *http.Request) (string, bool) {
	actor, resolved := s.brokerCredentialActor(r)
	if !resolved || actor.Project == nil || *actor.Project == "" {
		return "", false
	}
	return *actor.Project, true
}

// callerSession is the session this request's credential proved, and whether it proved one at
// all. A broker-issued token is the only credential that does: the broker binds it to one
// session and brokerTokenActor takes that session from the token, never from a body. Every
// other credential proves none — a cookie, a personal token, a Kubernetes service-account token
// and the shared token each leave the session to the body — and is outside this check.
func (s *server) callerSession(r *http.Request) (string, bool) {
	actor, resolved := s.brokerCredentialActor(r)
	if !resolved || actor.Kind != "session" || actor.ID == "" {
		return "", false
	}
	return actor.ID, true
}

// projectResolver answers the project a request's addressed object belongs to. An empty project
// with no error means the address resolved to no project: an object that belongs to none, a
// target that does not exist, or an id Postgres will not read as a uuid.
type projectResolver func(ctx context.Context, q queryer, r *http.Request) (string, error)

// projectResolverFor is the resolver a route's address calls for, read from the shape of its
// pattern, and whether the pattern names an object at all. Every row the table classes
// scopeProject has one; undecidableWrites holds the table to that and register refuses to
// mount a table that breaks it, so a mounted scopeProject row always resolves.
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

// projectOfProject is the one-row read the other resolvers make, against the projects table
// itself: a project's own key is the project it belongs to, and a key no project has resolves
// to none. It reads the key alone rather than going through loadProject, whose 404 this would
// only have to undo.
func (s *server) projectOfProject(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `select key from projects where key = $1`, r.PathValue("key")))
}

// projectOfArtifact reads the project_key every artifact carries (artifactColumns), which is
// its issue's for an issue document and its own for a project document. An agent conversation's
// artifact carries none, so it resolves to no project and a project-scoped caller is refused
// it.
func (s *server) projectOfArtifact(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(project_key, '') from artifacts where id = $1
	`, r.PathValue("id")))
}

// projectOfAsk resolves an ask through the join askSnoozeEvent already makes: an ask hangs from
// an issue or from a document, and takes that owner's project.
func (s *server) projectOfAsk(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key, '')
		from asks a
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		where a.id = $1
	`, r.PathValue("id")))
}

// projectOfComment is one hop to the comment's issue or document, since the comments table holds
// no project of its own (store/migrations/0001_init.up.sql, 0009_project_artifacts.up.sql).
func (s *server) projectOfComment(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key, '')
		from comments c
		left join issues i on i.key = c.issue_key
		left join artifacts ar on ar.id = c.artifact_id
		where c.id = $1
	`, r.PathValue("id")))
}

// projectOfMessage is the same one hop for a message, which hangs from an issue or from nothing
// at all: a direct message between a person and a session belongs to no project, so it resolves
// to none here, and the two routes a session answers its own delivery on are classed
// scopeSession rather than this.
func (s *server) projectOfMessage(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, '')
		from messages m
		left join issues i on i.key = m.issue_key
		where m.id = $1
	`, r.PathValue("id")))
}

// scanProjectKey reads a resolver's one column, answering no project for a target that does not
// resolve rather than an error: no such row, or a path value Postgres will not read as a uuid
// (SQLSTATE 22P02, which it reports as it binds the parameter). Those are the ids a handler
// answers 404 or 400 for, and withinProjectScope refuses them to a project-scoped caller
// rather than letting that answer out.
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

// unaddressed answers whether err means the target resolves to no row at all, which is the
// handler's answer to make for an unscoped caller and a refusal for a scoped one. Every
// resolver reads its one column itself, so what reaches this is Postgres's own answer and never
// a refusal some loader already wrote a status onto.
func unaddressed(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var malformed *pgconn.PgError
	return errors.As(err, &malformed) && malformed.Code == "22P02"
}

// withinProjectScope runs the project check in front of one mutating route's handler. The row's
// class is decided once here, at mount, as withinSessionScope decides its own: a class this
// check does not cover returns the handler unchanged, so no route it leaves alone pays for it.
//
// For a caller whose credential names no project — every caller that exists today — a covered
// route costs one header read and calls the handler. For one that names a project it fails
// closed: only a target this check resolved to that same project reaches the handler.
func (s *server) withinProjectScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	switch route.Scope {
	case scopeProject:
		resolve, addressed := s.projectResolverFor(route.Pattern)
		if !addressed {
			// Nothing mounts such a row: undecidableWrites names it and register refuses the
			// table holding it. A project-scoped caller is refused it here all the same, so
			// what stands between that caller and a write nothing judged is this check rather
			// than a panic somewhere else.
			return func(w http.ResponseWriter, r *http.Request) {
				covers, scoped := s.callerProject(r)
				if !scoped {
					handler(w, r)
					return
				}
				refuseWriteOutsideProjectScope(w, covers, "")
			}
		}
		return func(w http.ResponseWriter, r *http.Request) {
			covers, scoped := s.callerProject(r)
			if !scoped {
				handler(w, r)
				return
			}
			belongsTo, err := resolve(r.Context(), s.deps.Store.Pool, r)
			if err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
			if belongsTo != covers {
				// Including a target that resolved to no project at all: an object belonging
				// to none, a row that is not there, and an id Postgres will not read.
				// Refusing those too is what makes the check fail closed — not what hides
				// which ids exist, which this does not claim to do (the file comment).
				refuseWriteOutsideProjectScope(w, covers, belongsTo)
				return
			}
			handler(w, r)
		}
	case scopeDeployment:
		return func(w http.ResponseWriter, r *http.Request) {
			covers, scoped := s.callerProject(r)
			if !scoped {
				handler(w, r)
				return
			}
			writeError(w, codeProjectScopeDeployment, http.StatusForbidden, fmt.Sprintf(
				"this request's credential may write project %s and nothing else; %s %s belongs to the deployment rather than to a project",
				covers, route.Method, route.Pattern))
		}
	default:
		// A write that belongs to the calling session (scopeSession, held to its own session by
		// withinSessionScope), and the issue create, whose project its handler reads from the
		// body (scopeProjectInBody).
		return handler
	}
}

// refuseWriteOutsideProjectScope names the project the caller's credential covers and the
// project the write belongs to where one resolved, and nothing of the credential itself. It
// names no project at all for a write that resolved to none, since there is none to name and
// an invented one would say something about what exists.
func refuseWriteOutsideProjectScope(w http.ResponseWriter, covers, belongsTo string) {
	if belongsTo == "" {
		writeError(w, codeProjectScope, http.StatusForbidden, fmt.Sprintf(
			"this request's credential may write project %s; this write resolves to no project of that credential's",
			covers))
		return
	}
	writeError(w, codeProjectScope, http.StatusForbidden, fmt.Sprintf(
		"this request's credential may write project %s; this write belongs to project %s", covers, belongsTo))
}

// withinSessionScope runs the session check in front of one mutating route's handler. A row
// classed scopeSession whose pattern addresses a session by id belongs to that session alone,
// so a caller whose credential proved a session — a broker-issued token, the one credential
// that proves one — may address only its own. Every other credential proves no session and
// names one in the body where a route needs it, so the check leaves it to the handler, which is
// the rule those routes have always kept.
//
// Every other row returns the handler unchanged, so no route it does not cover pays for it.
func (s *server) withinSessionScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	if !addressesOneSession(route) {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		proved, provedOne := s.callerSession(r)
		if !provedOne {
			handler(w, r)
			return
		}
		if addressed := r.PathValue("session_id"); addressed != proved {
			writeError(w, codeSessionScope, http.StatusForbidden, fmt.Sprintf(
				"this request's credential acts for session %s; %s %s writes into session %s",
				proved, route.Method, route.Pattern, addressed))
			return
		}
		handler(w, r)
	}
}

// addressesOneSession answers whether route is a write to the rows of the one session its path
// names: the scopeSession rows whose pattern carries {session_id}. The other scopeSession rows
// address no session of their own — the `me` token rows are the calling person's, and the two
// message rows are held to the session their delivery attempt names.
func addressesOneSession(route apiRoute) bool {
	return route.Scope == scopeSession && strings.Contains(route.Pattern, "{session_id}")
}

// undecidableWrites names every mutating row the project check cannot decide: one writeClasses
// gives no class, and one classed scopeProject whose address names no object to resolve. Every
// other class is decided: scopeDeployment and scopeProjectInBody by the class itself, and
// scopeSession by withinSessionScope and the handlers behind it.
// TestEveryMutatingRouteIsClassed walks the table through it, and register refuses to mount a
// table it names anything in, so a row added later cannot be left undecided. The other direction,
// a writeClasses name that is no row of the table, is the same test's (nothing is served by it,
// so it cannot admit a write).
func (s *server) undecidableWrites(routes []apiRoute) []string {
	var undecidable []string
	for _, route := range routes {
		if route.Method == http.MethodGet {
			continue
		}
		name := route.Method + " " + route.Pattern
		switch route.Scope {
		case scopeSession, scopeDeployment, scopeProjectInBody:
		case scopeProject:
			if _, addressed := s.projectResolverFor(route.Pattern); !addressed {
				undecidable = append(undecidable, name+": classed as a project's write, but its address names no object whose project can be resolved")
			}
		default:
			undecidable = append(undecidable, name+": no write class (writeClasses)")
		}
	}
	return undecidable
}

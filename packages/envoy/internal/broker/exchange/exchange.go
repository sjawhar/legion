// Package exchange decides what a verified subject token entitles its holder to write in
// Dispatch: the claims a minted Dispatch access token carries — the session that is writing, the
// person it acts for, and which projects it may write, either the one it names or every one its
// person may write — and, beside them, the platform identity the session name is to be bound to.
//
// It is the policy half of the RFC 8693 token exchange and nothing else. It verifies no token,
// reads no configuration, takes no clock and makes no call: it is handed entries someone else
// parsed and the already-verified claims of a subject token, and it answers Claims or refuses with
// a sentinel the caller maps onto an OAuth error. A Set is built once and is then immutable, so
// the same subject token decides the same way however many exchanges run at once.
//
// Three kinds of subject token reach it, and what decides each one's claims differs:
//
//   - a pod's Kubernetes service-account token, where an entry keyed on the account subject
//     (system:serviceaccount:<namespace>:<name>) gives the project and the person, and the token
//     must carry the pod it is bound to;
//   - a continuous-integration workflow run's GitHub token, where an entry keyed on the
//     repository, the workflow's path and the ref it ran on gives the project and the person;
//   - a dev box session's machine credential, which no entry lists, because the person is the
//     approver the credential already records.
package exchange

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Why a subject token is refused. Each stands for one answer an operator can act on: list the
// identity (ErrNoEntry), mint the token in a pod rather than by hand (ErrNoPodBinding), ask under
// the session the entry fixes (ErrSessionMismatch), name a session (ErrSessionRequired), name a
// usable one (ErrSessionInvalid), or present a token that carries the claims its kind must
// (ErrClaimMissing).
var (
	ErrNoEntry         = errors.New("exchange: no entry lists this identity")
	ErrNoPodBinding    = errors.New("exchange: the subject token is bound to no pod")
	ErrSessionMismatch = errors.New("exchange: the request names a session the entry does not fix")
	ErrSessionRequired = errors.New("exchange: the request names no session and the entry fixes none")
	ErrSessionInvalid  = errors.New("exchange: the request names an unusable session")
	ErrClaimMissing    = errors.New("exchange: the subject token carries no such claim")
)

// MaxSessionBytes is the longest session name a request may carry. A session name is written into
// every record the token's writes produce and read back in a dashboard, so it is bounded; 200
// bytes is past any name a Legion session or a person's tool has reason to use.
const MaxSessionBytes = 200

// accountPattern is a Kubernetes service account's subject, the one a pod's projected token
// carries: system:serviceaccount:<namespace>:<name>, the namespace a DNS label and the name a DNS
// subdomain. It is the grammar the broker's configuration states for the same accounts, repeated
// here because this package reads no configuration and so cannot import the one that parses it.
var accountPattern = regexp.MustCompile(`^system:serviceaccount:[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?:[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?$`)

// repositoryPattern is a GitHub repository as its token's `repository` claim names it,
// <owner>/<name>: the owner up to 39 characters of letters, digits and hyphens, the name up to
// 100 of letters, digits, hyphens, underscores and dots.
var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// ProjectScope says what a minted token's project claim means. Its zero value is no scope at
// all, so a Claims that was not built by this package carries one that every reader refuses:
// the absence of a project must be a decision this package made, never a field nobody filled in.
type ProjectScope int

const (
	// ProjectScopeUnset is the zero value, which no decision answers. A project check handed it
	// refuses the token: a Claims built by anything but this package's constructors has decided
	// nothing, and 'no project named' must not read as 'no oversight'.
	ProjectScopeUnset ProjectScope = iota
	// ProjectScopeNamed is a token that may write the one project Claims.Project names.
	ProjectScopeNamed
	// ProjectScopePerson is a token that may write every project the person it acts for may
	// write, and names none. It is what a person's own credential gets, because a person reaches
	// whichever projects they may reach.
	ProjectScopePerson
)

// String names the scope as a refusal or a log line spells it, the zero value included, so a
// Claims that decided nothing does not read as a number.
func (s ProjectScope) String() string {
	switch s {
	case ProjectScopeNamed:
		return "one named project"
	case ProjectScopePerson:
		return "every project the person may write"
	default:
		return "no project scope"
	}
}

// Claims are what an exchange writes into the Dispatch access token it mints, and the binding it
// makes before it does.
type Claims struct {
	// Session is who the token's writes are recorded as.
	Session string
	// Person is who the session acts for: the human answerable for what the token writes.
	Person string
	// Scope is what Project means, and is always one of ProjectScopeNamed or ProjectScopePerson:
	// every constructor sets it, so a reader never has to read an empty Project as either.
	Scope ProjectScope
	// Project is the one project key the token may write, and is empty for a
	// ProjectScopePerson token, which names no project.
	Project string
	// Binding is the platform identity the session name is to be bound to, and is not a claim of
	// the minted token: it is what the broker records as the holder of the session name, so a
	// caller that may exchange cannot mint a token naming a session another identity is already
	// writing under. It is the pod uid for a pod, the repository and run id for a workflow run,
	// and the machine credential's id for a dev box session.
	Binding string
}

// AccountEntry lists one Kubernetes service account and what a pod running as it may write. Every
// field but Session is required.
type AccountEntry struct {
	// Account is the subject a pod's projected token carries,
	// system:serviceaccount:<namespace>:<name>, named exactly: there is no wildcard.
	Account string
	// Project is the one project key the minted token may write.
	Project string
	// Person is who the account's pods act for.
	Person string
	// Session, when set, is the session name every token minted for this account carries, and a
	// request naming any other is refused. It is how a long-running service keeps writing under
	// one name it does not get to choose per request.
	Session string
}

// WorkflowEntry lists one GitHub workflow of one repository on one ref, and what a run of it may
// write. Every field but Session is required: nothing is wildcarded by leaving a field out, so an
// entry that names no ref matches no run rather than every one of them.
type WorkflowEntry struct {
	// Repository is <owner>/<name>, the repository the run belongs to.
	Repository string
	// Workflow is the workflow file's path inside the repository, such as
	// .github/workflows/release.yaml, compared exactly.
	Workflow string
	// Ref is the git ref the run is on, such as refs/heads/main, compared exactly.
	Ref string
	// Project is the one project key the minted token may write.
	Project string
	// Person is who the run acts for.
	Person string
	// Session, when set, is the session name every token minted for this workflow carries, as
	// AccountEntry.Session is for an account.
	Session string
}

// workflowKey is the triple a workflow run is looked up by: every field of an entry is required,
// so the key is the whole identity and no partial match exists.
type workflowKey struct{ repository, workflow, ref string }

// Set is the listed identities as one build of the configuration read them. It is immutable once
// built, so every Pod, Workflow and Machine on it is a pure function of its arguments.
type Set struct {
	accounts  map[string]AccountEntry
	workflows map[workflowKey]WorkflowEntry
}

// NewSet validates every entry and builds the set the exchange decides against. It refuses,
// naming the entry it refuses: an empty or blank required field, an account or repository that is
// not of its grammar, a fixed session name that is not usable, and an identity listed twice, since
// two answers for one identity is a configuration whose meaning depends on order.
func NewSet(accounts []AccountEntry, workflows []WorkflowEntry) (*Set, error) {
	set := &Set{
		accounts:  make(map[string]AccountEntry, len(accounts)),
		workflows: make(map[workflowKey]WorkflowEntry, len(workflows)),
	}
	for i, entry := range accounts {
		where := fmt.Sprintf("account entry %d, %q", i+1, entry.Account)
		if !accountPattern.MatchString(entry.Account) {
			return nil, fmt.Errorf("exchange: %s: an account is system:serviceaccount:<namespace>:<name>", where)
		}
		if err := checkGrant(where, entry.Project, entry.Person, entry.Session); err != nil {
			return nil, err
		}
		if _, listed := set.accounts[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: the account is listed twice, and one account writes as one identity", where)
		}
		set.accounts[entry.Account] = entry
	}
	for i, entry := range workflows {
		where := fmt.Sprintf("workflow entry %d, %q", i+1, entry.Repository+"/"+entry.Workflow+"@"+entry.Ref)
		if !repositoryPattern.MatchString(entry.Repository) {
			return nil, fmt.Errorf("exchange: %s: a repository is <owner>/<name>", where)
		}
		if strings.TrimSpace(entry.Workflow) == "" {
			return nil, fmt.Errorf("exchange: %s: the workflow's path is required, since an entry with no path matches no run", where)
		}
		if strings.TrimSpace(entry.Ref) == "" {
			return nil, fmt.Errorf("exchange: %s: the ref is required, since an entry with no ref matches no run", where)
		}
		if err := checkGrant(where, entry.Project, entry.Person, entry.Session); err != nil {
			return nil, err
		}
		key := workflowKey{entry.Repository, entry.Workflow, entry.Ref}
		if _, listed := set.workflows[key]; listed {
			return nil, fmt.Errorf("exchange: %s: the workflow is listed twice, and one workflow writes as one identity", where)
		}
		set.workflows[key] = entry
	}
	return set, nil
}

// checkGrant refuses what every entry shares: a blank project or person, which would mint a token
// answerable to nobody, and a fixed session name no request could name back.
func checkGrant(where, project, person, session string) error {
	if strings.TrimSpace(project) == "" {
		return fmt.Errorf("exchange: %s: the project is required, since the minted token may write one project", where)
	}
	if strings.TrimSpace(person) == "" {
		return fmt.Errorf("exchange: %s: the person is required, since every token acts for someone", where)
	}
	if session != "" && !validSession(session) {
		return fmt.Errorf("exchange: %s: %s", where, sessionRule)
	}
	return nil
}

// Pod answers the claims for a pod's Kubernetes service-account token. The token's own shape is
// read first — the account it was minted for, and the pod it is bound to — and the entry for that
// account second, so an operator reading the refusal learns which of the two to fix.
//
// A token carrying no pod uid is refused however the account is listed: Kubernetes mints such a
// token to anyone holding the account's credentials, and it proves only that the account exists,
// where a bound one proves a pod of this cluster is running right now. That uid is also what the
// session name is bound to, so one pod cannot write under the session name another pod of the
// same account is writing under.
func (s *Set) Pod(token map[string]any, session string) (Claims, error) {
	account, err := claimString(token, "sub")
	if err != nil {
		return Claims{}, err
	}
	uid := podUID(token)
	if uid == "" {
		return Claims{}, fmt.Errorf("%w: %s presented a token bound to no pod, which proves only that the account exists", ErrNoPodBinding, account)
	}
	entry, listed := s.accounts[account]
	if !listed {
		return Claims{}, fmt.Errorf("%w: no entry lists the service account %s", ErrNoEntry, account)
	}
	name, err := resolveSession(entry.Session, session, account)
	if err != nil {
		return Claims{}, err
	}
	return Claims{Session: name, Person: entry.Person, Scope: ProjectScopeNamed, Project: entry.Project, Binding: uid}, nil
}

// Workflow answers the claims for a continuous-integration workflow run's GitHub token, from the
// claims GitHub puts in it: `repository`, `workflow_ref`, `job_workflow_ref`, `ref` and `run_id`.
//
// The identity is the repository, the workflow file's path and the ref, each compared exactly. A
// run whose job comes from a reusable workflow other than the one the run started in is refused:
// `job_workflow_ref` names the workflow file the job is written in and `workflow_ref` the one the
// run entered through, and where they differ the code that is running is not the code the entry
// names.
//
// The session name is bound to the run, which is `run_id` under its repository: a run id is
// unique only within the repository that numbered it, so two repositories' runs would otherwise
// collide on one binding.
func (s *Set) Workflow(token map[string]any, session string) (Claims, error) {
	repository, err := claimString(token, "repository")
	if err != nil {
		return Claims{}, err
	}
	workflowRef, err := claimString(token, "workflow_ref")
	if err != nil {
		return Claims{}, err
	}
	jobRef, err := claimString(token, "job_workflow_ref")
	if err != nil {
		return Claims{}, err
	}
	ref, err := claimString(token, "ref")
	if err != nil {
		return Claims{}, err
	}
	run, err := claimString(token, "run_id")
	if err != nil {
		return Claims{}, err
	}
	if jobRef != workflowRef {
		return Claims{}, fmt.Errorf("%w: %s ran %s, whose job is written in %s; an entry lists the workflow a job runs in", ErrNoEntry, repository, workflowRef, jobRef)
	}
	workflow, ok := workflowPath(workflowRef, repository)
	if !ok {
		return Claims{}, fmt.Errorf("%w: the subject token's workflow_ref is not <owner>/<name>/<path>@<ref> under its repository %s", ErrClaimMissing, repository)
	}
	entry, listed := s.workflows[workflowKey{repository, workflow, ref}]
	if !listed {
		return Claims{}, fmt.Errorf("%w: no entry lists the workflow %s of %s on %s", ErrNoEntry, workflow, repository, ref)
	}
	name, err := resolveSession(entry.Session, session, repository+"/"+workflow)
	if err != nil {
		return Claims{}, err
	}
	return Claims{
		Session: name,
		Person:  entry.Person,
		Scope:   ProjectScopeNamed,
		Project: entry.Project,
		Binding: repository + "/actions/runs/" + run,
	}, nil
}

// Machine answers the claims for a dev box session's machine credential, credential being the
// credential's own id. No entry lists one: the person is approver, the one the credential already
// records, and the session is the one the caller names and is bound to that credential, so one
// person's dev box cannot write under another's session name.
//
// The token names no project, and says so: its scope is ProjectScopePerson. A machine credential
// is a person's own identity on their own machine, as their browser cookie and their personal
// token are, and neither of those carries a project either: the person reaches whichever projects
// they may reach, and an exchange that pinned one here would hand a dev box more standing than
// the person has, not less. Naming the scope is what keeps a reader of an introspection answer
// from mistaking that for a token no project check applies to.
func (s *Set) Machine(credential, approver, session string) (Claims, error) {
	if strings.TrimSpace(credential) == "" {
		return Claims{}, fmt.Errorf("%w: the machine credential has no id to bind the session name to", ErrClaimMissing)
	}
	if strings.TrimSpace(approver) == "" {
		return Claims{}, fmt.Errorf("%w: the machine credential records no approver to act for", ErrClaimMissing)
	}
	name, err := resolveSession("", session, approver)
	if err != nil {
		return Claims{}, err
	}
	return Claims{Session: name, Person: approver, Scope: ProjectScopePerson, Binding: credential}, nil
}

// sessionRule is the one sentence that says what a session name may be, so an entry refused at
// build time and a request refused at exchange time are refused in the same words, and the cap it
// quotes is the cap validSession applies.
var sessionRule = fmt.Sprintf("a session name is printable, carries no whitespace, and is at most %d bytes", MaxSessionBytes)

// resolveSession answers the session claim for an entry whose fixed session is fixed and a request
// that named named, where identity names who is asking. An entry that fixes a session answers it
// and refuses a request that names another, rather than quietly writing under the fixed one; an
// entry that fixes none has nothing to fall back on, so the request must name a usable session.
//
// A refused name is described and never repeated: it comes from the request, so its length and
// its bytes are whatever the caller sent.
func resolveSession(fixed, named, identity string) (string, error) {
	if fixed != "" {
		if named != "" && named != fixed {
			return "", fmt.Errorf("%w: %s writes as %s", ErrSessionMismatch, identity, fixed)
		}
		return fixed, nil
	}
	if named == "" {
		return "", fmt.Errorf("%w: %s is listed with no session of its own, so the request names one", ErrSessionRequired, identity)
	}
	if !validSession(named) {
		return "", fmt.Errorf("%w: %s named a session of %d bytes; %s", ErrSessionInvalid, identity, len(named), sessionRule)
	}
	return named, nil
}

// validSession reports whether name is usable as a session claim: UTF-8, printable, with no
// whitespace and at most MaxSessionBytes.
func validSession(name string) bool {
	if name == "" || len(name) > MaxSessionBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// claimString reads one claim the decision needs as a non-empty string. A claim that is absent,
// null, or of another JSON type is the same fault to a caller: the subject token is not the kind
// of token this decision is for.
func claimString(token map[string]any, name string) (string, error) {
	value, present := token[name]
	if !present {
		return "", fmt.Errorf("%w: the subject token carries no %s claim", ErrClaimMissing, name)
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "", fmt.Errorf("%w: the subject token's %s claim is not a non-empty string", ErrClaimMissing, name)
	}
	return text, nil
}

// podUID is the uid of the pod a projected service-account token is bound to (kubernetes.io → pod
// → uid), and "" for a token bound to no pod, such as one kubectl create token mints.
func podUID(token map[string]any) string {
	kubernetes, _ := token["kubernetes.io"].(map[string]any)
	pod, _ := kubernetes["pod"].(map[string]any)
	uid, _ := pod["uid"].(string)
	return uid
}

// workflowPath is the workflow file's path inside repository that workflowRef names, a GitHub
// workflow_ref being "<owner>/<name>/<path>@<ref>". It answers false for a ref that is not of that
// form or not under repository.
func workflowPath(workflowRef, repository string) (string, bool) {
	rest, under := strings.CutPrefix(workflowRef, repository+"/")
	if !under {
		return "", false
	}
	at := strings.LastIndex(rest, "@")
	if at <= 0 {
		return "", false
	}
	return rest[:at], true
}

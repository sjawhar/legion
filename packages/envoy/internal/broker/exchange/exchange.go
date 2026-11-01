// Package exchange decides what a verified subject token entitles its holder to write in
// Dispatch: the claims a minted Dispatch access token carries — the session that is writing, the
// person it acts for or the service it is, and which projects it may write, either the one it
// names or every one its person may write — and, beside them, the principal the session name
// belongs to.
//
// It is the policy half of the RFC 8693 token exchange and nothing else. It verifies no token,
// reads no configuration, takes no clock and makes no call: it is handed entries someone else
// parsed and the already-verified claims of a subject token, and it answers Claims or refuses with
// a sentinel the caller maps onto an OAuth error. A Set is built once and is then immutable, so
// the same subject token decides the same way however many exchanges run at once.
//
// Four kinds of subject token reach it, and what decides each one's claims differs:
//
//   - a pod's Kubernetes service-account token, where an entry keyed on the account subject
//     (system:serviceaccount:<namespace>:<name>) gives the project and the person, and the token
//     must carry the pod it is bound to;
//   - a continuous-integration workflow run's GitHub token, where an entry keyed on the
//     repository, the workflow's path and the ref it ran on gives the project and the person;
//   - a machine's Google identity token, where an entry keyed on the account's email address
//     gives the service, may pin the Compute Engine project and instance names the machine may
//     carry, and may give a person and a project as a pod's entry does;
//   - a dev box session's machine credential, which no entry lists, because the person is the
//     approver the credential already records.
//
// The principal is the caller's standing identity, never the instance asking: the listed account
// for a pod, the listed workflow for a run, the listed Google account for a machine, the person
// for a dev box. A session name belongs to one principal, so a pod's replacement and a workflow's
// next run keep writing under the name
// their predecessor held, and a caller of any other principal is refused it. Instance is the
// second rule beside it, and the only thing that separates two callers of one principal: the pod
// uid a registration has to name for a pod of a free-session entry to write under a session name.
package exchange

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Why a subject token is refused. Each stands for one answer an operator can act on: list the
// identity (ErrNoEntry), mint the token in a pod rather than by hand (ErrNoPodBinding), ask under
// the session the entry fixes (ErrSessionMismatch), name a session (ErrSessionRequired), name a
// usable one (ErrSessionInvalid), present a token that carries the claims its kind must
// (ErrClaimMissing), verify the machine's address with its provider (ErrEmailNotVerified), mint
// the machine's token on a Compute Engine instance or unpin its entry (ErrNoComputeEngine), run
// the machine where its entry allows (ErrMachineNotAllowed), or, of a registration, leave an
// entry that fixes a session alone (ErrSessionFixed).
var (
	ErrNoEntry           = errors.New("exchange: no entry lists this identity")
	ErrNoPodBinding      = errors.New("exchange: the subject token is bound to no pod")
	ErrSessionMismatch   = errors.New("exchange: the request names a session the entry does not fix")
	ErrSessionRequired   = errors.New("exchange: the request names no session and the entry fixes none")
	ErrSessionInvalid    = errors.New("exchange: the request names an unusable session")
	ErrClaimMissing      = errors.New("exchange: the subject token carries no such claim")
	ErrEmailNotVerified  = errors.New("exchange: the subject token's email address is not verified")
	ErrNoComputeEngine   = errors.New("exchange: the subject token carries no Compute Engine claims")
	ErrMachineNotAllowed = errors.New("exchange: the machine is not one the entry allows")
	ErrSessionFixed      = errors.New("exchange: the entry fixes the session name it writes under")
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

// googleAccountPattern is a Google service account's email address as a Google identity token
// carries it in its `email` claim: lowercase, since that is the one spelling a token holds. It is
// the grammar the broker's configuration states for the same accounts, repeated here for the
// reason accountPattern is.
var googleAccountPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._%+-]*@[a-z0-9][a-z0-9.-]*\.[a-z]{2,}$`)

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

// ComputeEngine is the instance a Google identity token was minted on, the `google` →
// `compute_engine` claims of a token asked for with format=full. Every field is empty for a token
// minted anywhere else: one a workload identity pool minted, and the one a pod's metadata server
// on Google Kubernetes Engine answers, neither of which runs on an instance of the caller's.
type ComputeEngine struct {
	// Project is the Google Cloud project the instance runs in (project_id).
	Project string
	// Zone is the zone it runs in.
	Zone string
	// Instance is the instance's name (instance_name), the one an entry's pattern matches.
	Instance string
	// InstanceID is the instance's numeric id, unique within a project and a zone.
	InstanceID string
}

// Whole reports whether the token carried a complete Compute Engine identity. A token carries all
// four of these claims or none, so anything between the two is a token this package will not read
// an identity out of.
func (c ComputeEngine) Whole() bool {
	return c.Project != "" && c.Zone != "" && c.Instance != "" && c.InstanceID != ""
}

// Empty reports whether the token carried no Compute Engine claim at all, which is what a token
// minted through a workload identity pool and a pod's own metadata token carry.
func (c ComputeEngine) Empty() bool {
	return c == ComputeEngine{}
}

// GoogleIdentity is what a Google identity token says about the machine presenting it: the
// account it was minted for, whether the provider holds that address verified, and the Compute
// Engine instance it was minted on, when it was minted on one.
type GoogleIdentity struct {
	// Account is the `email` claim, the service account the token was minted for.
	Account string
	// EmailVerified is the `email_verified` claim. Google writes it as a JSON boolean and some
	// of its endpoints as the string "true", so both are read.
	EmailVerified bool
	// Compute is the instance the token was minted on, every field empty for a token minted off
	// one.
	Compute ComputeEngine
}

// ReadGoogleIdentity reads a Google identity token's own claims out of the claim map of a token
// something else has already verified. It is the one reading of those claim shapes: this package
// decides on them, the broker's VM token verifier reads the same token (enroll.VerifyGoogle), and
// a caller authenticated by such a token reads the account off it, so all three read one
// declaration rather than three spellings of Google's claim names. It refuses nothing: a claim
// that is absent or of another JSON type reads as empty, and which of those is a refusal is the
// reader's own rule.
func ReadGoogleIdentity(token map[string]any) GoogleIdentity {
	identity := GoogleIdentity{EmailVerified: isVerified(token["email_verified"])}
	identity.Account, _ = token["email"].(string)
	google, _ := token["google"].(map[string]any)
	compute, _ := google["compute_engine"].(map[string]any)
	identity.Compute.Project, _ = compute["project_id"].(string)
	identity.Compute.Zone, _ = compute["zone"].(string)
	identity.Compute.Instance, _ = compute["instance_name"].(string)
	identity.Compute.InstanceID, _ = compute["instance_id"].(string)
	return identity
}

// isVerified reads an `email_verified` claim, which is a JSON boolean on a token from Google's
// metadata server and the string "true" from some of its other endpoints.
func isVerified(claim any) bool {
	switch value := claim.(type) {
	case bool:
		return value
	case string:
		return value == "true"
	}
	return false
}

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

// Claims are what an exchange writes into the Dispatch access token it mints, and the principal
// the session name it writes under belongs to.
type Claims struct {
	// Session is who the token's writes are recorded as.
	Session string
	// Person is who the session acts for: the human answerable for what the token writes. A
	// token names a person or a service and never both, so this is empty exactly where Service
	// is set.
	Person string
	// Service is the service a token that names no person is: the fleet a machine belongs to,
	// which is what a machine whose entry names no person gets instead of a person. It is empty
	// for every other decision, each of which acts for a person.
	Service string
	// Scope is what Project means, and is always one of ProjectScopeNamed or ProjectScopePerson:
	// every constructor sets it, so a reader never has to read an empty Project as either.
	Scope ProjectScope
	// Project is the one project key the token may write, and is empty for a
	// ProjectScopePerson token, which names no project.
	Project string
	// Principal is who the session name belongs to, and is not a claim of the minted token: it is
	// what the broker records as the name's holder, so a caller that may exchange cannot mint a
	// token naming a session another principal writes under. It is the listed account for a pod,
	// the listed workflow (its repository, path and ref) for a run, the listed Google account for
	// a machine, and the person for a dev box session, each spelled with its kind first
	// (account:, workflow:, google:, person:) so principals of two kinds never compare equal.
	// Every pod of one account, every run of one workflow, every machine of one Google account
	// and every machine of one person is therefore one holder.
	Principal string
	// Instance is which caller of that principal is asking, and is not a claim of the minted
	// token either: it is the pod uid a registration has to name for this exchange to be made
	// (oauthstore.Store.PodSessionRegistered), and is empty for every caller no registration
	// separates from the others of its principal — a pod whose entry fixes the session, a
	// workflow run, and a dev box session, each of which writes under a name its configuration or
	// its person already decides.
	Instance string
}

// Reservation is a session name an entry fixes and the principal it belongs to. The broker holds
// each one for its entry's principal before it makes any exchange, so a name an entry fixes is
// refused to every other caller by the same rule that refuses any held name, whether or not the
// entry's own caller has exchanged yet.
type Reservation struct {
	Session   string
	Principal string
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
	// one name it does not get to choose per request. The name is one of the set's Reservations,
	// held for the account however many of its pods exchange and refused to every other caller.
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

// GoogleEntry lists one Google service account and what a machine holding its identity token may
// write. Account and Service are required; the rest is what the entry chooses to pin and to
// grant.
//
// The two pins go together: an entry that pins both holds the machine to one Compute Engine
// project and to the instance names its pattern matches, and refuses a token carrying no Compute
// Engine claims at all. An entry that pins neither takes such a token, which is what a token
// minted through a workload identity pool and a token a pod's metadata server answers both carry.
//
// Project and Person go together too: an entry that names them mints a token that acts for that
// person and writes that one project, exactly as an AccountEntry does for a pod, and an entry
// that names neither mints a token that names its service and no person at all.
type GoogleEntry struct {
	// Account is the service account's email address, the `email` claim of its identity tokens,
	// named exactly: there is no wildcard.
	Account string
	// Service is the service a machine of this account is, which the minted token names when the
	// entry names no person.
	Service string
	// CloudProject, when set, is the Google Cloud project the machine must run in, compared
	// exactly with the token's google.compute_engine.project_id.
	CloudProject string
	// Instance, when set, is the pattern the machine's instance name must match, in path.Match's
	// syntax, such as example-vm-*.
	Instance string
	// Project, when set, is the one Dispatch project key the minted token may write. It is set
	// with Person and with nothing else.
	Project string
	// Person, when set, is who the machine's token acts for. An entry that names none mints a
	// token that names Service and no person.
	Person string
	// Session, when set, is the session name every token minted for this account carries, as
	// AccountEntry.Session is for an account.
	Session string
}

// workflowKey is the triple a workflow run is looked up by: every field of an entry is required,
// so the key is the whole identity and no partial match exists.
type workflowKey struct{ repository, workflow, ref string }

// Set is the listed identities as one build of the configuration read them. It is immutable once
// built, so every Pod, Workflow, Google and Machine on it is a pure function of its arguments.
type Set struct {
	accounts     map[string]AccountEntry
	workflows    map[workflowKey]WorkflowEntry
	google       map[string]GoogleEntry
	reservations []Reservation
}

// NewSet validates every entry and builds the set the exchange decides against. It refuses,
// naming the entry it refuses: an empty or blank required field, an account or repository that is
// not of its grammar, a fixed session name that is not usable, an identity listed twice, since
// two answers for one identity is a configuration whose meaning depends on order, two workflows
// whose principals spell alike, and a session name two entries fix, since one name belongs to one
// principal. A Google entry is refused on top of that for half a pin and for half a grant: one
// that pins a Compute Engine project without an instance pattern or the other way round says
// nothing clear about which machines it means, and one that names a person without a project, or
// a project without a person, says nothing clear about what they may write.
func NewSet(accounts []AccountEntry, workflows []WorkflowEntry, machines []GoogleEntry) (*Set, error) {
	set := &Set{
		accounts:  make(map[string]AccountEntry, len(accounts)),
		workflows: make(map[workflowKey]WorkflowEntry, len(workflows)),
		google:    make(map[string]GoogleEntry, len(machines)),
	}
	fixedBy, spelledBy := map[string]string{}, map[string]string{}
	reserve := func(where, session, principal string) error {
		if session == "" {
			return nil
		}
		if other, fixed := fixedBy[session]; fixed {
			return fmt.Errorf("exchange: %s: %s fixes the same session name, and one name belongs to one identity", where, other)
		}
		fixedBy[session] = where
		set.reservations = append(set.reservations, Reservation{Session: session, Principal: principal})
		return nil
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
		if err := reserve(where, entry.Session, accountPrincipal(entry.Account)); err != nil {
			return nil, err
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
		principal := workflowPrincipal(key)
		if other, spelled := spelledBy[principal]; spelled {
			return nil, fmt.Errorf("exchange: %s: %s spells the same identity, so the two could not be told apart as holders of a session name", where, other)
		}
		spelledBy[principal] = where
		if err := reserve(where, entry.Session, principal); err != nil {
			return nil, err
		}
		set.workflows[key] = entry
	}
	for i, entry := range machines {
		where := fmt.Sprintf("google entry %d, %q", i+1, entry.Account)
		if !googleAccountPattern.MatchString(entry.Account) {
			return nil, fmt.Errorf("exchange: %s: a Google account is the lowercase email address its identity tokens carry", where)
		}
		if strings.TrimSpace(entry.Service) == "" {
			return nil, fmt.Errorf("exchange: %s: the service is required, since a machine whose entry names no person is its service and nothing else", where)
		}
		if (entry.CloudProject == "") != (entry.Instance == "") {
			return nil, fmt.Errorf("exchange: %s: an entry pins the Compute Engine project and the instance names together or neither (project %q, instance names %q)", where, entry.CloudProject, entry.Instance)
		}
		if _, err := path.Match(entry.Instance, ""); err != nil {
			return nil, fmt.Errorf("exchange: %s: the instance-name pattern %q is not a pattern: %w", where, entry.Instance, err)
		}
		if (entry.Project == "") != (entry.Person == "") {
			return nil, fmt.Errorf("exchange: %s: an entry names the person and the project together or neither (project %q, person %q)", where, entry.Project, entry.Person)
		}
		if entry.Person != "" {
			if err := checkGrant(where, entry.Project, entry.Person, entry.Session); err != nil {
				return nil, err
			}
		} else if entry.Session != "" && !validSession(entry.Session) {
			return nil, fmt.Errorf("exchange: %s: %s", where, sessionRule)
		}
		if _, listed := set.google[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: the account is listed twice, and one account writes as one identity", where)
		}
		if err := reserve(where, entry.Session, googlePrincipal(entry.Account)); err != nil {
			return nil, err
		}
		set.google[entry.Account] = entry
	}
	return set, nil
}

// Reservations are the session names the entries fix, each with the principal it belongs to, in
// the order the entries were listed: accounts first, then workflows, then Google accounts. The
// slice is the caller's own, so the set stays immutable.
func (s *Set) Reservations() []Reservation {
	return slices.Clone(s.reservations)
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
// where a bound one proves Kubernetes minted it for one pod of this cluster. It does not prove
// that pod still runs: the broker's exchange verifies the token offline against the issuer's
// published keys and asks the cluster nothing, so a token whose pod has been deleted is exchanged
// until its own expiry, which the Kubernetes subject token's lifetime tolerance
// (oauth.kubernetesTolerance) allows up to 24 hours ahead. What bounds the reach that follows is
// the access token each exchange mints, which lives BROKER_ACCESS_TOKEN_SECONDS (15 minutes by
// default). That trades immediate revocation of a pod's token for needing no access to the
// cluster's own interface.
//
// The principal is the account, not the pod: every pod of one account is one holder, so the
// replacement pod of a rolling update, or a Legion session resumed in a relaunched pod, keeps
// writing under the name the pod before it held. What keeps the pods of one account apart from
// one another is therefore not the claim on the name but Instance, the pod uid, which this
// answers for an entry that fixes no session: the exchange mints nothing under that name unless a
// registration its deployment made says that pod uid may use it, and a relaunch is a second pod
// uid registered for the same name rather than a refusal. An entry that fixes a session answers
// no Instance and needs no registration, because the name it writes under is the configuration's
// and every pod of the account writes under it.
func (s *Set) Pod(token map[string]any, session string) (Claims, error) {
	account, err := claimString(token, "sub")
	if err != nil {
		return Claims{}, err
	}
	uid := podUID(token)
	entry, err := s.podEntry(account, uid)
	if err != nil {
		return Claims{}, err
	}
	name, err := resolveSession(entry.Session, session, account)
	if err != nil {
		return Claims{}, err
	}
	claims := Claims{Session: name, Person: entry.Person, Scope: ProjectScopeNamed, Project: entry.Project, Principal: accountPrincipal(account)}
	if entry.Session == "" {
		claims.Instance = uid
	}
	return claims, nil
}

// podEntry is the reading of a pod's identity that Pod and Registration both begin with: the
// pod uid the token must carry, refused first because a token bound to no pod proves only that
// the account exists, and then the entry that lists the account. Both refuse in this order, in
// these words, because they are this one helper: a registrar and the exchange that reads its
// row refuse the same pods for the same reasons, which Registration's doc promises.
func (s *Set) podEntry(account, uid string) (AccountEntry, error) {
	if uid == "" {
		return AccountEntry{}, fmt.Errorf("%w: %s presented a token bound to no pod, which proves only that the account exists", ErrNoPodBinding, account)
	}
	entry, listed := s.accounts[account]
	if !listed {
		return AccountEntry{}, fmt.Errorf("%w: no entry lists the service account %s", ErrNoEntry, account)
	}
	return entry, nil
}

// Registration refuses what a pod-session registration must refuse before it is written: a token
// bound to no pod, an account no entry lists, and an entry that fixes a session, which needs no
// registration because its name is the configuration's. The first two refusals are podEntry,
// the one reading of a pod's identity Pod makes too, so a registrar and the exchange that reads
// its row refuse the same pods for the same reasons; what it does not decide is whether the
// name belongs to the account, which only the claim on the name answers
// (oauthstore.Store.BindSession).
func (s *Set) Registration(account, podUID, session string) error {
	entry, err := s.podEntry(account, podUID)
	if err != nil {
		return err
	}
	if entry.Session != "" {
		return fmt.Errorf("%w: %s writes as %s, which every pod of it may do, so its pods are registered for no name", ErrSessionFixed, account, entry.Session)
	}
	if !validSession(session) {
		return fmt.Errorf("%w: the registration of %s names a session of %d bytes; %s", ErrSessionInvalid, account, len(session), sessionRule)
	}
	return nil
}

// Workflow answers the claims for a continuous-integration workflow run's GitHub token, from the
// claims GitHub puts in it: `repository`, `workflow_ref`, `job_workflow_ref` and `ref`.
//
// The identity is the repository, the workflow file's path and the ref, each compared exactly. A
// run whose job comes from a reusable workflow other than the one the run started in is refused:
// `job_workflow_ref` names the workflow file the job is written in and `workflow_ref` the one the
// run entered through, and where they differ the code that is running is not the code the entry
// names.
//
// The principal is that listed workflow, not the run: every run of it is one holder, so the next
// run of a release workflow keeps writing under the name the run before it held, however soon
// after it starts.
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
	if jobRef != workflowRef {
		return Claims{}, fmt.Errorf("%w: %s ran %s, whose job is written in %s; an entry lists the workflow a job runs in", ErrNoEntry, repository, workflowRef, jobRef)
	}
	workflow, ok := workflowPath(workflowRef, repository)
	if !ok {
		return Claims{}, fmt.Errorf("%w: the subject token's workflow_ref is not <owner>/<name>/<path>@<ref> under its repository %s", ErrClaimMissing, repository)
	}
	key := workflowKey{repository, workflow, ref}
	entry, listed := s.workflows[key]
	if !listed {
		return Claims{}, fmt.Errorf("%w: no entry lists the workflow %s of %s on %s", ErrNoEntry, workflow, repository, ref)
	}
	name, err := resolveSession(entry.Session, session, repository+"/"+workflow)
	if err != nil {
		return Claims{}, err
	}
	return Claims{Session: name, Person: entry.Person, Scope: ProjectScopeNamed, Project: entry.Project, Principal: workflowPrincipal(key)}, nil
}

// Google answers the claims for a machine's Google identity token, from the claims Google puts in
// it: `email`, `email_verified` and, for a token minted on a Compute Engine instance with
// format=full, the `google` → `compute_engine` members.
//
// The identity is the account the token was minted for, which an entry lists by its email
// address. Google's signature, the issuer, the audience and the expiry are checked before this is
// reached (oauth.ExchangeConfig); what is left to decide is that Google holds the address
// verified, that the machine is one the entry allows, and what its token may write.
//
// Where a machine runs is the entry's to pin. An entry that pins the Compute Engine project and
// an instance-name pattern holds the token to a VM of that project whose name the pattern
// matches, and refuses a token carrying no Compute Engine claims, which proves nothing about
// where it was minted. An entry that pins neither takes such a token: that is what a token minted
// through a workload identity pool carries, and what a token from the metadata server of a pod on
// Google Kubernetes Engine carries, neither of which runs on an instance of its own.
//
// The principal is the listed account, not the machine: every machine of one account is one
// holder, so a VM rebuilt under another instance id keeps writing under the name the machine
// before it held, and a machine of any other account is refused that name. Nothing separates two
// machines of one account the way a registration separates two pods, so each names a session of
// its own and the claim on the name keeps them apart.
//
// An entry that names a person and a project mints a token that acts for that person and writes
// that project, as a pod's entry does. An entry that names neither mints a token that names its
// service and no person at all: it names no project either, and says so with the person scope,
// the wire's word for a token held to no one project. Nothing it can write follows from that
// alone — a resource server holds a token naming no person to what a service may do — which is
// why the service is a claim of its own rather than an absence to be read.
func (s *Set) Google(token map[string]any, session string) (Claims, error) {
	identity := ReadGoogleIdentity(token)
	if identity.Account == "" {
		return Claims{}, fmt.Errorf("%w: the subject token carries no email claim, so it names no account (ask for it with format=full)", ErrClaimMissing)
	}
	entry, listed := s.google[identity.Account]
	if !listed {
		return Claims{}, fmt.Errorf("%w: no entry lists the Google account %s", ErrNoEntry, identity.Account)
	}
	if !identity.EmailVerified {
		return Claims{}, fmt.Errorf("%w: %s presented a token whose email_verified is not true", ErrEmailNotVerified, identity.Account)
	}
	if err := s.allowedMachine(entry, identity); err != nil {
		return Claims{}, err
	}
	name, err := resolveSession(entry.Session, session, identity.Account)
	if err != nil {
		return Claims{}, err
	}
	claims := Claims{Session: name, Scope: ProjectScopePerson, Principal: googlePrincipal(identity.Account)}
	if entry.Person != "" {
		claims.Person, claims.Scope, claims.Project = entry.Person, ProjectScopeNamed, entry.Project
	} else {
		claims.Service = entry.Service
	}
	return claims, nil
}

// allowedMachine holds a token to the Compute Engine instance its entry pins, and takes every
// machine of an entry that pins nothing. The two refusals are told apart because an operator acts
// on each differently: a token with no Compute Engine claims was minted somewhere the pins cannot
// describe, and a token with them from another project or another instance name is a machine the
// entry does not list.
func (s *Set) allowedMachine(entry GoogleEntry, identity GoogleIdentity) error {
	if entry.CloudProject == "" && entry.Instance == "" {
		return nil
	}
	if !identity.Compute.Whole() {
		return fmt.Errorf("%w: %s presented a token carrying no Compute Engine identity, and its entry takes machines of the project %s named %s",
			ErrNoComputeEngine, identity.Account, entry.CloudProject, entry.Instance)
	}
	if identity.Compute.Project != entry.CloudProject {
		return fmt.Errorf("%w: %s presented a token minted in the project %s, and its entry takes machines of %s",
			ErrMachineNotAllowed, identity.Account, identity.Compute.Project, entry.CloudProject)
	}
	// The pattern was checked at NewSet, so a match error here is impossible; a pattern that did
	// not match is the refusal either way.
	if matched, _ := path.Match(entry.Instance, identity.Compute.Instance); !matched {
		return fmt.Errorf("%w: %s presented a token minted on the instance %s, and its entry takes the names %s",
			ErrMachineNotAllowed, identity.Account, identity.Compute.Instance, entry.Instance)
	}
	return nil
}

// Machine answers the claims for a dev box session's machine credential, credential being the
// credential's own id. No entry lists one: the person is approver, the one the credential already
// records, and the session is the one the caller names. A machine login's token names the
// credential it was issued under, so a token naming none is refused as not being one.
//
// The principal is the person, not the credential: every machine and every session of one person
// is one holder, so a person's session resumed on another of their machines, or after their
// machine logged in again, keeps its name, and one person's dev box cannot write under another's
// session name or under one an entry fixes.
//
// The token names no project, and says so: its scope is ProjectScopePerson. A machine credential
// is a person's own identity on their own machine, as their browser cookie and their personal
// token are, and neither of those carries a project either: the person reaches whichever projects
// they may reach, and an exchange that pinned one here would hand a dev box more standing than
// the person has, not less. Naming the scope is what keeps a reader of an introspection answer
// from mistaking that for a token no project check applies to.
func (s *Set) Machine(credential, approver, session string) (Claims, error) {
	if strings.TrimSpace(credential) == "" {
		return Claims{}, fmt.Errorf("%w: the subject token names no machine credential id, so it is not a machine login's token", ErrClaimMissing)
	}
	if strings.TrimSpace(approver) == "" {
		return Claims{}, fmt.Errorf("%w: the machine credential records no approver to act for", ErrClaimMissing)
	}
	name, err := resolveSession("", session, approver)
	if err != nil {
		return Claims{}, err
	}
	return Claims{Session: name, Person: approver, Scope: ProjectScopePerson, Principal: personPrincipal(approver)}, nil
}

// The four spellings of a principal, each led by its kind so that no account, workflow, Google
// account and person ever spell the same string. A workflow's is <repository>/<path>@<ref>, as an
// operator reads one elsewhere; a path or ref carrying "@" could make two entries spell one
// string, and NewSet refuses that pair rather than let one's runs hold the other's names.
func accountPrincipal(account string) string { return "account:" + account }

func workflowPrincipal(key workflowKey) string {
	return "workflow:" + key.repository + "/" + key.workflow + "@" + key.ref
}

func googlePrincipal(account string) string { return "google:" + account }

func personPrincipal(person string) string { return "person:" + person }

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

package exchange_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/exchange"
)

const (
	workerAccount = "system:serviceaccount:example-ns:example-worker"
	daemonAccount = "system:serviceaccount:example-ns:example-daemon"
	repository    = "example-org/example-repo"
	workflow      = ".github/workflows/release.yaml"
	nightly       = ".github/workflows/nightly.yaml"
	ref           = "refs/heads/main"
	project       = "ACME"
	person        = "ada@example.com"
	// fixedSession is the session name the daemon account's entry fixes, and nightlySession the
	// one the nightly workflow's entry fixes: one name belongs to one principal, so two entries
	// never fix the same one.
	fixedSession   = "legion-daemon:ACME"
	nightlySession = "ci-nightly"
	podUID         = "11111111-2222-3333-4444-555555555555"
	credentialID   = "66666666-7777-8888-9999-aaaaaaaaaaaa"
)

// The principals the listed entries' session names belong to, as the exchange spells them.
const (
	workerPrincipal  = "account:" + workerAccount
	daemonPrincipal  = "account:" + daemonAccount
	releasePrincipal = "workflow:" + repository + "/" + workflow + "@" + ref
	nightlyPrincipal = "workflow:" + repository + "/" + nightly + "@" + ref
	personPrincipal  = "person:" + person
)

// listed is the set every decision test asks: one account whose sessions name themselves, one
// account whose session the entry fixes, and the same pair for one workflow of one repository on
// one ref.
func listed(t *testing.T) *exchange.Set {
	t.Helper()
	set, err := exchange.NewSet(
		[]exchange.AccountEntry{
			{Account: workerAccount, Project: project, Person: person},
			{Account: daemonAccount, Project: project, Person: person, Session: fixedSession},
		},
		[]exchange.WorkflowEntry{
			{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
			{Repository: repository, Workflow: nightly, Ref: ref, Project: project, Person: person, Session: nightlySession},
		},
	)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return set
}

// podToken is a projected service-account token's claims as the exchange is handed them, bound to
// podUID unless uid is empty.
func podToken(account, uid string) map[string]any {
	token := map[string]any{"sub": account}
	if uid != "" {
		token["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
	}
	return token
}

// workflowToken is a GitHub workflow run's token claims. jobRef is the workflow the job itself is
// written in, which differs from workflowRef only for a run that called a reusable workflow.
func workflowToken(repo, workflowRef, jobRef, gitRef string) map[string]any {
	return map[string]any{
		"repository":       repo,
		"workflow_ref":     workflowRef,
		"job_workflow_ref": jobRef,
		"ref":              gitRef,
		"run_id":           "4242",
	}
}

// runRef is the workflow_ref a run of path in repository on ref carries.
func runRef(path string) string { return repository + "/" + path + "@" + ref }

func TestNewSetRefusesAnEntryItCannotServe(t *testing.T) {
	account := exchange.AccountEntry{Account: workerAccount, Project: project, Person: person}
	flow := exchange.WorkflowEntry{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person}
	with := func(change func(*exchange.AccountEntry)) exchange.AccountEntry {
		entry := account
		change(&entry)
		return entry
	}
	withFlow := func(change func(*exchange.WorkflowEntry)) exchange.WorkflowEntry {
		entry := flow
		change(&entry)
		return entry
	}
	for _, c := range []struct {
		name      string
		accounts  []exchange.AccountEntry
		workflows []exchange.WorkflowEntry
		names     string
	}{
		{name: "an account that is not a service account subject",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Account = "example-worker" })}, names: "example-worker"},
		{name: "an account with no namespace",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Account = "system:serviceaccount:example-worker" })}, names: "system:serviceaccount:example-worker"},
		{name: "an empty account",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Account = "" })}, names: "account entry 1"},
		{name: "an account with no project",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Project = "  " })}, names: workerAccount},
		{name: "an account with no person",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Person = "" })}, names: workerAccount},
		{name: "an account whose fixed session carries whitespace",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = "legion daemon" })}, names: workerAccount},
		{name: "an account whose fixed session is too long",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = strings.Repeat("s", exchange.MaxSessionBytes+1) })}, names: workerAccount},
		{name: "the same account twice",
			accounts: []exchange.AccountEntry{account, with(func(e *exchange.AccountEntry) { e.Project = "OTHER" })}, names: "account entry 2"},
		{name: "a repository that is not <owner>/<name>",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Repository = "example-repo" })}, names: "example-repo"},
		{name: "a workflow with no path",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Workflow = "" })}, names: repository},
		{name: "a workflow with no ref",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Ref = "" })}, names: workflow},
		{name: "a workflow with no project",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Project = "" })}, names: workflow},
		{name: "a workflow with no person",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Person = " " })}, names: workflow},
		{name: "a workflow whose fixed session is unprintable",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Session = "legion\x00daemon" })}, names: workflow},
		{name: "the same workflow twice",
			workflows: []exchange.WorkflowEntry{flow, withFlow(func(e *exchange.WorkflowEntry) { e.Person = "bob@example.com" })}, names: "workflow entry 2"},
		{name: "two workflows whose principals spell alike",
			workflows: []exchange.WorkflowEntry{
				withFlow(func(e *exchange.WorkflowEntry) { e.Workflow, e.Ref = ".github/workflows/a.yaml@refs/heads/b", "c" }),
				withFlow(func(e *exchange.WorkflowEntry) { e.Workflow, e.Ref = ".github/workflows/a.yaml", "refs/heads/b@c" }),
			}, names: "workflow entry 1"},
		{name: "a session name two accounts fix",
			accounts: []exchange.AccountEntry{
				with(func(e *exchange.AccountEntry) { e.Session = fixedSession }),
				{Account: daemonAccount, Project: project, Person: person, Session: fixedSession},
			}, names: "account entry 1"},
		{name: "a session name an account and a workflow fix",
			accounts:  []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = fixedSession })},
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Session = fixedSession })}, names: workerAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := exchange.NewSet(c.accounts, c.workflows)
			if err == nil {
				t.Fatalf("NewSet(%s) = %v, want a refusal", c.name, set)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("NewSet(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

func TestNewSetTakesTheSameAccountAndWorkflowKeyTwiceApart(t *testing.T) {
	set, err := exchange.NewSet(
		[]exchange.AccountEntry{{Account: workerAccount, Project: project, Person: person}},
		[]exchange.WorkflowEntry{
			{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
			{Repository: repository, Workflow: workflow, Ref: "refs/heads/release", Project: "OTHER", Person: person},
		},
	)
	if err != nil {
		t.Fatalf("NewSet(one workflow on two refs): %v", err)
	}
	for _, c := range []struct {
		gitRef string
		want   string
	}{{ref, project}, {"refs/heads/release", "OTHER"}} {
		claims, err := set.Workflow(workflowToken(repository, repository+"/"+workflow+"@"+c.gitRef, repository+"/"+workflow+"@"+c.gitRef, c.gitRef), "release-bot")
		if err != nil || claims.Project != c.want {
			t.Errorf("Workflow(on %s) = %+v, %v; want project %s", c.gitRef, claims, err, c.want)
		}
	}
}

// TestTheReservationsAreTheNamesTheEntriesFix: every session name an entry fixes is reserved for
// that entry's principal, in the order the entries were listed, and a name no entry fixes is not
// reserved at all. The answer is the caller's own copy, so the set stays as it was built.
func TestTheReservationsAreTheNamesTheEntriesFix(t *testing.T) {
	set := listed(t)
	want := []exchange.Reservation{
		{Session: fixedSession, Principal: daemonPrincipal},
		{Session: nightlySession, Principal: nightlyPrincipal},
	}
	got := set.Reservations()
	if !slices.Equal(got, want) {
		t.Fatalf("Reservations = %+v, want %+v", got, want)
	}
	got[0].Principal = "account:changed"
	if again := set.Reservations(); !slices.Equal(again, want) {
		t.Errorf("Reservations after the caller changed its copy = %+v, want %+v", again, want)
	}
}

func TestPodAnswersTheListedAccount(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "the session the request names", token: podToken(workerAccount, podUID), session: "implementer-g1",
			want: exchange.Claims{Session: "implementer-g1", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: workerPrincipal}},
		{name: "the session the entry fixes, the request naming none", token: podToken(daemonAccount, podUID), session: "",
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: daemonPrincipal}},
		{name: "the session the entry fixes, the request naming the same", token: podToken(daemonAccount, podUID), session: fixedSession,
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: daemonPrincipal}},
		{name: "another pod of the account is the same principal", token: podToken(daemonAccount, "another-pod-uid"), session: "",
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: daemonPrincipal}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Pod(c.token, c.session)
			if err != nil || claims != c.want {
				t.Fatalf("Pod(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

func TestPodRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    error
		names   string
	}{
		{name: "a token with no subject", token: map[string]any{}, session: "implementer-g1", want: exchange.ErrClaimMissing, names: "sub"},
		{name: "a subject that is not a string", token: map[string]any{"sub": 7}, session: "implementer-g1", want: exchange.ErrClaimMissing, names: "sub"},
		{name: "a token bound to no pod", token: podToken(workerAccount, ""), session: "implementer-g1", want: exchange.ErrNoPodBinding, names: workerAccount},
		{name: "a pod binding with no uid", token: map[string]any{"sub": workerAccount, "kubernetes.io": map[string]any{"pod": map[string]any{}}},
			session: "implementer-g1", want: exchange.ErrNoPodBinding, names: workerAccount},
		{name: "an account no entry lists", token: podToken("system:serviceaccount:example-ns:example-other", podUID),
			session: "implementer-g1", want: exchange.ErrNoEntry, names: "system:serviceaccount:example-ns:example-other"},
		{name: "a session the entry does not fix", token: podToken(daemonAccount, podUID), session: "implementer-g1",
			want: exchange.ErrSessionMismatch, names: fixedSession},
		{name: "no session where the entry fixes none", token: podToken(workerAccount, podUID), session: "",
			want: exchange.ErrSessionRequired, names: workerAccount},
		{name: "a session carrying whitespace", token: podToken(workerAccount, podUID), session: "implementer g1",
			want: exchange.ErrSessionInvalid, names: workerAccount},
		{name: "a session longer than the cap", token: podToken(workerAccount, podUID), session: strings.Repeat("s", exchange.MaxSessionBytes+1),
			want: exchange.ErrSessionInvalid, names: workerAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Pod(c.token, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Pod(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Pod(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

func TestPodNeverRepeatsAnUnusableSessionName(t *testing.T) {
	set := listed(t)
	const unusable = "implementer\x00g1"
	_, err := set.Pod(podToken(workerAccount, podUID), unusable)
	if !errors.Is(err, exchange.ErrSessionInvalid) {
		t.Fatalf("Pod(an unprintable session) = %v, want ErrSessionInvalid", err)
	}
	if strings.Contains(err.Error(), unusable) {
		t.Errorf("Pod refused with %q, which repeats what the request sent", err)
	}
}

func TestWorkflowAnswersTheListedRun(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "the session the request names", token: workflowToken(repository, runRef(workflow), runRef(workflow), ref),
			session: "release-bot", want: exchange.Claims{Session: "release-bot", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: releasePrincipal}},
		{name: "the session the entry fixes", token: workflowToken(repository, runRef(nightly), runRef(nightly), ref),
			session: "", want: exchange.Claims{Session: nightlySession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: nightlyPrincipal}},
		{name: "another run of the workflow is the same principal", token: withRun(workflowToken(repository, runRef(nightly), runRef(nightly), ref), "4243"),
			session: "", want: exchange.Claims{Session: nightlySession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: nightlyPrincipal}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Workflow(c.token, c.session)
			if err != nil || claims != c.want {
				t.Fatalf("Workflow(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

func TestWorkflowRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    error
		names   string
	}{
		{name: "a token with no repository", token: workflowToken("", runRef(workflow), runRef(workflow), ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: "repository"},
		{name: "a token with no workflow_ref", token: workflowToken(repository, "", runRef(workflow), ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: "workflow_ref"},
		{name: "a token with no job_workflow_ref", token: workflowToken(repository, runRef(workflow), "", ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: "job_workflow_ref"},
		{name: "a token with no ref", token: workflowToken(repository, runRef(workflow), runRef(workflow), ""),
			session: "release-bot", want: exchange.ErrClaimMissing, names: "ref"},
		{name: "a workflow_ref that names no path", token: workflowToken(repository, repository+"/"+workflow, repository+"/"+workflow, ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: repository},
		{name: "a workflow_ref under another repository", token: workflowToken(repository, "example-org/other-repo/"+workflow+"@"+ref, "example-org/other-repo/"+workflow+"@"+ref, ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: repository},
		{name: "a job written in a reusable workflow", token: workflowToken(repository, runRef(workflow), runRef(".github/workflows/reusable.yaml"), ref),
			session: "release-bot", want: exchange.ErrNoEntry, names: ".github/workflows/reusable.yaml"},
		{name: "a workflow no entry lists", token: workflowToken(repository, runRef(".github/workflows/other.yaml"), runRef(".github/workflows/other.yaml"), ref),
			session: "release-bot", want: exchange.ErrNoEntry, names: ".github/workflows/other.yaml"},
		{name: "a ref no entry lists", token: workflowToken(repository, repository+"/"+workflow+"@refs/heads/topic", repository+"/"+workflow+"@refs/heads/topic", "refs/heads/topic"),
			session: "release-bot", want: exchange.ErrNoEntry, names: "refs/heads/topic"},
		{name: "a repository no entry lists", token: workflowToken("example-org/other-repo", "example-org/other-repo/"+workflow+"@"+ref, "example-org/other-repo/"+workflow+"@"+ref, ref),
			session: "release-bot", want: exchange.ErrNoEntry, names: "example-org/other-repo"},
		{name: "a session the entry does not fix", token: workflowToken(repository, runRef(nightly), runRef(nightly), ref),
			session: "release-bot", want: exchange.ErrSessionMismatch, names: nightlySession},
		{name: "no session where the entry fixes none", token: workflowToken(repository, runRef(workflow), runRef(workflow), ref),
			session: "", want: exchange.ErrSessionRequired, names: workflow},
		{name: "a session carrying whitespace", token: workflowToken(repository, runRef(workflow), runRef(workflow), ref),
			session: "release bot", want: exchange.ErrSessionInvalid, names: workflow},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Workflow(c.token, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Workflow(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Workflow(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// withRun is token as another run of the same workflow carries it.
func withRun(token map[string]any, run string) map[string]any {
	token["run_id"] = run
	return token
}

func TestMachineActsForTheApproverAndWritesEveryProjectThePersonMay(t *testing.T) {
	set := listed(t)
	claims, err := set.Machine(credentialID, person, "ada-devbox")
	if err != nil {
		t.Fatalf("Machine: %v", err)
	}
	want := exchange.Claims{Session: "ada-devbox", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal}
	if claims != want {
		t.Fatalf("Machine = %+v, want %+v: a machine credential names no project and says which absence that is", claims, want)
	}
	// Another machine of the same person is the same principal: the name belongs to the person.
	other, err := set.Machine("77777777-8888-9999-aaaa-bbbbbbbbbbbb", person, "ada-devbox")
	if err != nil || other != want {
		t.Fatalf("Machine(another credential of %s) = %+v, %v; want %+v", person, other, err, want)
	}
}

// TestEveryDecisionScopesItsProject: the zero ProjectScope is what a Claims nobody built carries,
// so no constructor may answer it; a later project check refuses a token that names neither a
// project nor the person's whole reach.
func TestEveryDecisionScopesItsProject(t *testing.T) {
	set := listed(t)
	pod, err := set.Pod(podToken(workerAccount, podUID), "implementer-g1")
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	run, err := set.Workflow(workflowToken(repository, runRef(workflow), runRef(workflow), ref), "release-bot")
	if err != nil {
		t.Fatalf("Workflow: %v", err)
	}
	machine, err := set.Machine(credentialID, person, "ada-devbox")
	if err != nil {
		t.Fatalf("Machine: %v", err)
	}
	for _, c := range []struct {
		name   string
		claims exchange.Claims
		want   exchange.ProjectScope
	}{
		{name: "Pod", claims: pod, want: exchange.ProjectScopeNamed},
		{name: "Workflow", claims: run, want: exchange.ProjectScopeNamed},
		{name: "Machine", claims: machine, want: exchange.ProjectScopePerson},
	} {
		if c.claims.Scope != c.want {
			t.Errorf("%s scope = %v, want %v", c.name, c.claims.Scope, c.want)
		}
		if (c.claims.Project != "") != (c.want == exchange.ProjectScopeNamed) {
			t.Errorf("%s = %+v: a named scope names a project and no other scope does", c.name, c.claims)
		}
	}
	if got := exchange.ProjectScopeUnset.String(); !strings.Contains(got, "no project scope") {
		t.Errorf("ProjectScopeUnset.String() = %q, want it to say the scope was never set", got)
	}
}

func TestMachineRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name       string
		credential string
		approver   string
		session    string
		want       error
		names      string
	}{
		{name: "a credential with no id", credential: " ", approver: person, session: "ada-devbox", want: exchange.ErrClaimMissing, names: "id"},
		{name: "a credential with no approver", credential: credentialID, approver: " ", session: "ada-devbox", want: exchange.ErrClaimMissing, names: "approver"},
		{name: "a request naming no session", credential: credentialID, approver: person, session: "", want: exchange.ErrSessionRequired, names: person},
		{name: "a request naming a session with whitespace", credential: credentialID, approver: person, session: "ada devbox", want: exchange.ErrSessionInvalid, names: person},
		{name: "a request naming a session past the cap", credential: credentialID, approver: person, session: strings.Repeat("s", exchange.MaxSessionBytes+1), want: exchange.ErrSessionInvalid, names: person},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Machine(c.credential, c.approver, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Machine(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Machine(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

func TestSessionAtTheCapIsUsable(t *testing.T) {
	set := listed(t)
	name := strings.Repeat("s", exchange.MaxSessionBytes)
	claims, err := set.Pod(podToken(workerAccount, podUID), name)
	if err != nil || claims.Session != name {
		t.Fatalf("Pod(a session of exactly %d bytes) = %+v, %v; want it answered", exchange.MaxSessionBytes, claims, err)
	}
}

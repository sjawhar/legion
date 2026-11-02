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
	// fixedSession is the session name the daemon account's entry fixes, nightlySession the one
	// the nightly workflow's entry fixes, and machineSession the one the fixed fleet's entry
	// fixes: one name belongs to one principal, so two entries never fix the same one.
	fixedSession   = "legion-daemon:ACME"
	nightlySession = "ci-nightly"
	machineSession = "example-fleet-1"
	podUID         = "11111111-2222-3333-4444-555555555555"
	credentialID   = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	// The three Google accounts the set lists, each a placeholder: pinnedAccount is held to one
	// Compute Engine project and one instance-name pattern and writes for a person, poolAccount
	// is pinned to nothing and names no person, so its machines are its service and nothing
	// else, and fixedAccount is pinned to nothing and fixes the session its machines write
	// under. strangerAccount is listed nowhere.
	pinnedAccount   = "example-fleet@example-project.iam.gserviceaccount.com"
	poolAccount     = "example-pool@example-project.iam.gserviceaccount.com"
	fixedAccount    = "example-fixed@example-project.iam.gserviceaccount.com"
	strangerAccount = "example-stranger@example-project.iam.gserviceaccount.com"
	cloudProject    = "example-project"
	instanceGlob    = "example-vm-*"
	instanceName    = "example-vm-7"
	pinnedService   = "example-fleet"
	poolService     = "example-pool"
	// signInDomain is the Google Workspace domain BROKER_SIGNIN_DOMAIN names in these tests, the
	// one a person signing in on a machine must belong to; otherDomain is any other Workspace,
	// and stranger a person of it.
	signInDomain = "example.com"
	otherDomain  = "other.example"
	stranger     = "mallory@other.example"
)

// The principals the listed entries' session names belong to, as the exchange spells them.
const (
	workerPrincipal  = "account:" + workerAccount
	daemonPrincipal  = "account:" + daemonAccount
	releasePrincipal = "workflow:" + repository + "/" + workflow + "@" + ref
	nightlyPrincipal = "workflow:" + repository + "/" + nightly + "@" + ref
	personPrincipal  = "person:" + person
	pinnedPrincipal  = "google:" + pinnedAccount
	poolPrincipal    = "google:" + poolAccount
	fixedPrincipal   = "google:" + fixedAccount
)

// listed is the set every decision test asks: one account whose sessions name themselves, one
// account whose session the entry fixes, the same pair for one workflow of one repository on one
// ref, and three Google accounts — one pinned to a project and an instance-name pattern that
// writes for a person, one pinned to nothing that names no person, and one that fixes the session
// its machines write under.
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
		[]exchange.GoogleEntry{
			{Account: pinnedAccount, Service: pinnedService, CloudProject: cloudProject, Instance: instanceGlob, Project: project, Person: person},
			{Account: poolAccount, Service: poolService},
			{Account: fixedAccount, Service: poolService, Session: machineSession},
		},
		signInDomain,
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
	machine := exchange.GoogleEntry{Account: pinnedAccount, Service: pinnedService}
	withMachine := func(change func(*exchange.GoogleEntry)) exchange.GoogleEntry {
		entry := machine
		change(&entry)
		return entry
	}
	for _, c := range []struct {
		name      string
		machines  []exchange.GoogleEntry
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
		{name: "a Google account that is not an email address",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Account = "example-fleet" })}, names: "example-fleet"},
		{name: "a Google account written with capitals, which no token carries",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Account = "Example-Fleet@example-project.iam.gserviceaccount.com" })},
			names:    "google entry 1"},
		{name: "a Google account with no service",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Service = " " })}, names: pinnedAccount},
		{name: "a machine pinned to a project and no instance pattern",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.CloudProject = cloudProject })}, names: pinnedAccount},
		{name: "a machine pinned to an instance pattern and no project",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Instance = instanceGlob })}, names: pinnedAccount},
		{name: "a machine whose instance pattern is not a pattern",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.CloudProject, e.Instance = cloudProject, "example-[vm" })},
			names:    "example-[vm"},
		{name: "a machine named a person and no project",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Person = person })}, names: pinnedAccount},
		{name: "a machine named a project and no person",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Project = project })}, names: pinnedAccount},
		{name: "a machine whose fixed session carries whitespace",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Session = "example fleet" })}, names: pinnedAccount},
		{name: "the same Google account twice",
			machines: []exchange.GoogleEntry{machine, withMachine(func(e *exchange.GoogleEntry) { e.Service = poolService })}, names: "google entry 2"},
		{name: "a session name a machine and an account fix",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = fixedSession })},
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Session = fixedSession })}, names: pinnedAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := exchange.NewSet(c.accounts, c.workflows, c.machines, signInDomain)
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
		nil, signInDomain,
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
		{Session: machineSession, Principal: fixedPrincipal},
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

// TestPodAnswersTheListedAccount pins what each kind of entry answers, Instance included: an
// entry that fixes no session answers the pod uid, which a registration has to name for the
// exchange to go through, and an entry that fixes one answers none, since every pod of that
// account writes under the configuration's name and is registered for nothing.
func TestPodAnswersTheListedAccount(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "the session the request names", token: podToken(workerAccount, podUID), session: "implementer-g1",
			want: exchange.Claims{Session: "implementer-g1", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: workerPrincipal, Instance: podUID}},
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

// TestRegistrationRefusesWhatNeedsNoRegistration pins what a pod-session registration may be
// written for: the pod of an entry that fixes no session, and nothing else. A token bound to no
// pod proves only that the account exists, an account no entry lists exchanges nothing to begin
// with, and an entry that fixes a session writes under the configuration's name, so registering
// one of its pods would say something no exchange reads.
func TestRegistrationRefusesWhatNeedsNoRegistration(t *testing.T) {
	set := listed(t)
	if err := set.Registration(workerAccount, podUID, "implementer-g1"); err != nil {
		t.Fatalf("Registration of a pod of %s for a name of its own = %v, want it accepted", workerAccount, err)
	}
	for _, c := range []struct {
		name         string
		account, uid string
		session      string
		want         error
		names        string
	}{
		{name: "a token bound to no pod", account: workerAccount, uid: "", session: "implementer-g1",
			want: exchange.ErrNoPodBinding, names: workerAccount},
		{name: "an account no entry lists", account: "system:serviceaccount:example-ns:example-other", uid: podUID,
			session: "implementer-g1", want: exchange.ErrNoEntry, names: "system:serviceaccount:example-ns:example-other"},
		{name: "an entry that fixes a session", account: daemonAccount, uid: podUID, session: fixedSession,
			want: exchange.ErrSessionFixed, names: fixedSession},
		{name: "a session carrying whitespace", account: workerAccount, uid: podUID, session: "implementer g1",
			want: exchange.ErrSessionInvalid, names: workerAccount},
		{name: "no session at all", account: workerAccount, uid: podUID, session: "",
			want: exchange.ErrSessionInvalid, names: workerAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := set.Registration(c.account, c.uid, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Registration(%s) = %v; want %v", c.name, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Registration(%s) refused with %q, which does not name %q", c.name, err, c.names)
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

// googleToken is a Google identity token's claims as the exchange is handed them: the account it
// was minted for, whether Google holds the address verified, and the Compute Engine instance it
// was minted on — none when instance is empty, which is what a token minted through a workload
// identity pool and a pod's own metadata token carry.
func googleToken(account string, verified any, cloud, instance string) map[string]any {
	token := map[string]any{"email": account, "email_verified": verified}
	if instance != "" {
		token["google"] = map[string]any{"compute_engine": map[string]any{
			"project_id":    cloud,
			"zone":          "example-zone-a",
			"instance_name": instance,
			"instance_id":   "1234567890",
		}}
	}
	return token
}

// TestGoogleAnswersTheListedMachine pins what each kind of Google entry answers: a pinned entry
// that names a person answers that person and its one project, an entry that pins nothing answers
// its service and no person at all, and a token carrying no Compute Engine claims — a workload
// identity pool's, and the one a pod's metadata server answers — is taken by an entry that pins
// nothing. No Google entry ever answers an Instance: nothing registers a machine, so the claim on
// the session name is what keeps two machines of one account apart. Every one of them answers
// its Fleet, the service its account is listed for, whether or not the token names that service:
// it is what the launcher credential the exchange mints records.
func TestGoogleAnswersTheListedMachine(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "a pinned machine of the project and a name the pattern matches",
			token: googleToken(pinnedAccount, true, cloudProject, instanceName), session: "example-machine",
			want: exchange.Claims{Session: "example-machine", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: pinnedPrincipal, Fleet: pinnedService}},
		{name: "another machine of that account is the same principal",
			token: googleToken(pinnedAccount, true, cloudProject, "example-vm-8"), session: "example-other",
			want: exchange.Claims{Session: "example-other", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: pinnedPrincipal, Fleet: pinnedService}},
		{name: "email_verified as the string Google writes on some of its tokens",
			token: googleToken(pinnedAccount, "true", cloudProject, instanceName), session: "example-machine",
			want: exchange.Claims{Session: "example-machine", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: pinnedPrincipal, Fleet: pinnedService}},
		{name: "an entry naming no person answers its service and no project",
			token: googleToken(poolAccount, true, "", ""), session: "example-pool-1",
			want: exchange.Claims{Session: "example-pool-1", Service: poolService, Scope: exchange.ProjectScopePerson, Principal: poolPrincipal, Fleet: poolService}},
		{name: "an entry pinning nothing takes a machine that does run on an instance",
			token: googleToken(poolAccount, true, "other-project", "anything"), session: "example-pool-2",
			want: exchange.Claims{Session: "example-pool-2", Service: poolService, Scope: exchange.ProjectScopePerson, Principal: poolPrincipal, Fleet: poolService}},
		{name: "the session the entry fixes, the request naming none",
			token: googleToken(fixedAccount, true, "", ""), session: "",
			want: exchange.Claims{Session: machineSession, Service: poolService, Scope: exchange.ProjectScopePerson, Principal: fixedPrincipal, Fleet: poolService}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Google(c.token, c.session)
			if err != nil || claims != c.want {
				t.Fatalf("Google(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

func TestGoogleRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    error
		names   string
	}{
		{name: "a token with no email", token: map[string]any{"email_verified": true}, session: "example-machine",
			want: exchange.ErrClaimMissing, names: "email"},
		{name: "an email that is not a string", token: map[string]any{"email": 7, "email_verified": true}, session: "example-machine",
			want: exchange.ErrClaimMissing, names: "email"},
		{name: "an account no entry lists", token: googleToken(strangerAccount, true, cloudProject, instanceName), session: "example-machine",
			want: exchange.ErrNoEntry, names: strangerAccount},
		{name: "an email Google has not verified", token: googleToken(pinnedAccount, false, cloudProject, instanceName), session: "example-machine",
			want: exchange.ErrEmailNotVerified, names: pinnedAccount},
		{name: "an email_verified of the string false", token: googleToken(pinnedAccount, "false", cloudProject, instanceName), session: "example-machine",
			want: exchange.ErrEmailNotVerified, names: pinnedAccount},
		{name: "a token with no Compute Engine claims against a pinned entry", token: googleToken(pinnedAccount, true, "", ""), session: "example-machine",
			want: exchange.ErrNoComputeEngine, names: pinnedAccount},
		{name: "a token with half a Compute Engine identity against a pinned entry",
			token:   map[string]any{"email": pinnedAccount, "email_verified": true, "google": map[string]any{"compute_engine": map[string]any{"project_id": cloudProject}}},
			session: "example-machine", want: exchange.ErrNoComputeEngine, names: pinnedAccount},
		{name: "a machine of another project", token: googleToken(pinnedAccount, true, "other-project", instanceName), session: "example-machine",
			want: exchange.ErrMachineNotAllowed, names: "other-project"},
		{name: "an instance name the pattern does not match", token: googleToken(pinnedAccount, true, cloudProject, "example-other-1"), session: "example-machine",
			want: exchange.ErrMachineNotAllowed, names: "example-other-1"},
		{name: "no session where the entry fixes none", token: googleToken(pinnedAccount, true, cloudProject, instanceName), session: "",
			want: exchange.ErrSessionRequired, names: pinnedAccount},
		{name: "a session the entry does not fix", token: googleToken(fixedAccount, true, "", ""), session: "example-other",
			want: exchange.ErrSessionMismatch, names: machineSession},
		{name: "a session carrying whitespace", token: googleToken(pinnedAccount, true, cloudProject, instanceName), session: "example machine",
			want: exchange.ErrSessionInvalid, names: pinnedAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Google(c.token, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Google(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Google(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestGoogleSaysWhichRefusalItIs: the five refusals a machine can meet are five sentences, each
// naming what an operator would change. They are compared against one another here, so that
// collapsing two of them into one message fails.
func TestGoogleSaysWhichRefusalItIs(t *testing.T) {
	set := listed(t)
	said := map[string]string{}
	for name, token := range map[string]map[string]any{
		"an account no entry lists":   googleToken(strangerAccount, true, cloudProject, instanceName),
		"an unverified email":         googleToken(pinnedAccount, false, cloudProject, instanceName),
		"no Compute Engine claims":    googleToken(pinnedAccount, true, "", ""),
		"another Compute project":     googleToken(pinnedAccount, true, "other-project", instanceName),
		"an instance name not listed": googleToken(pinnedAccount, true, cloudProject, "example-other-1"),
	} {
		_, err := set.Google(token, "example-machine")
		if err == nil {
			t.Fatalf("Google(%s) was answered, want a refusal", name)
		}
		said[name] = err.Error()
	}
	for name, text := range said {
		for other, otherText := range said {
			if name != other && text == otherText {
				t.Errorf("the refusals of %s and of %s read alike: %q", name, other, text)
			}
		}
	}
}

// personToken is a person's own Google identity token's claims as the exchange is handed them:
// the subject Google gives the account, the address it was minted for, whether Google holds that
// address verified, and the Workspace domain in `hd`.
func personToken(login string, verified any, domain string) map[string]any {
	token := map[string]any{"sub": "109" + login, "email": login, "email_verified": verified}
	if domain != "" {
		token["hd"] = domain
	}
	return token
}

// TestPersonAnswersTheSignedInPersonAndTheMachineActingForThem pins what a sign-in's trade
// decides: the person is the subject of the token and the machine only acts for them, so the
// claims name the person, the machine's listed account as the actor, and no project at all —
// a person's sign-in on their own box reaches whichever projects that person reaches, as their
// dev box's machine credential does. The principal is the person, so one person's boxes are one
// holder and their session names follow them from box to box. The machine's fleet decides
// nothing else: a fleet whose entry names a person and one whose entry names none answer alike,
// because what the token names is the person who signed in, never the fleet.
func TestPersonAnswersTheSignedInPersonAndTheMachineActingForThem(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name         string
		token, actor map[string]any
		session      string
		want         exchange.Claims
	}{
		{name: "a person of the domain on a pinned machine",
			token: personToken(person, true, signInDomain), actor: googleToken(pinnedAccount, true, cloudProject, instanceName), session: "example-box-1",
			want: exchange.Claims{Session: "example-box-1", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal, Actor: pinnedAccount}},
		{name: "the same person on a second box of a fleet that names nobody",
			token: personToken(person, true, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "example-box-2",
			want: exchange.Claims{Session: "example-box-2", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal, Actor: poolAccount}},
		{name: "an address Google spells with capitals is the lowercase login",
			token: personToken("Ada@example.com", true, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "example-box-3",
			want: exchange.Claims{Session: "example-box-3", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal, Actor: poolAccount}},
		{name: "email_verified as the string Google writes on some of its tokens",
			token: personToken(person, "true", signInDomain), actor: googleToken(poolAccount, "true", "", ""), session: "example-box-4",
			want: exchange.Claims{Session: "example-box-4", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal, Actor: poolAccount}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Person(c.token, c.actor, c.session)
			if err != nil || claims != c.want {
				t.Fatalf("Person(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

// TestPersonRefusesWhatItCannotDecide walks every way a sign-in's trade is refused, each by the
// sentinel an operator acts on and naming what to fix. The machine is read first and the person
// second, so a box no entry lists is refused whoever is signed in on it.
func TestPersonRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name         string
		token, actor map[string]any
		session      string
		want         error
		names        string
	}{
		{name: "no actor token at all", token: personToken(person, true, signInDomain), actor: nil, session: "example-box",
			want: exchange.ErrNoActorToken, names: "machine-google-identity"},
		{name: "an actor token naming no account", token: personToken(person, true, signInDomain), actor: map[string]any{"email_verified": true}, session: "example-box",
			want: exchange.ErrClaimMissing, names: "email"},
		{name: "an actor token of an account no entry lists", token: personToken(person, true, signInDomain), actor: googleToken(strangerAccount, true, "", ""), session: "example-box",
			want: exchange.ErrNoEntry, names: strangerAccount},
		{name: "a machine whose own address Google has not verified", token: personToken(person, true, signInDomain), actor: googleToken(pinnedAccount, false, cloudProject, instanceName), session: "example-box",
			want: exchange.ErrEmailNotVerified, names: pinnedAccount},
		{name: "a machine running where its entry does not allow", token: personToken(person, true, signInDomain), actor: googleToken(pinnedAccount, true, "other-project", instanceName), session: "example-box",
			want: exchange.ErrMachineNotAllowed, names: "other-project"},
		{name: "a person's token naming no subject", token: map[string]any{"email": person, "email_verified": true, "hd": signInDomain}, actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrClaimMissing, names: "sub"},
		{name: "a person's token naming no address", token: map[string]any{"sub": "109", "email_verified": true, "hd": signInDomain}, actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrClaimMissing, names: "email"},
		{name: "a person of another Workspace domain", token: personToken(stranger, true, otherDomain), actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrNotInDomain, names: stranger},
		{name: "a person's token carrying no hd at all", token: personToken(person, true, ""), actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrNotInDomain, names: person},
		{name: "a person whose address Google has not verified", token: personToken(person, false, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrEmailNotVerified, names: person},
		{name: "no session, which no entry fixes for a person", token: personToken(person, true, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "",
			want: exchange.ErrSessionRequired, names: person},
		{name: "a session carrying whitespace", token: personToken(person, true, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "example box",
			want: exchange.ErrSessionInvalid, names: person},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Person(c.token, c.actor, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Person(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Person(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestPersonSaysWhichRefusalItIs: the refusals the spec names each read differently, since what
// the person does about each one differs — sign in as an account of the domain, verify the
// address, present the machine's token, have the fleet listed, or run where the entry allows.
func TestPersonSaysWhichRefusalItIs(t *testing.T) {
	set := listed(t)
	said := map[string]string{}
	for name, c := range map[string]struct{ token, actor map[string]any }{
		"another domain":             {personToken(stranger, true, otherDomain), googleToken(poolAccount, true, "", "")},
		"an unverified person":       {personToken(person, false, signInDomain), googleToken(poolAccount, true, "", "")},
		"an unverified machine":      {personToken(person, true, signInDomain), googleToken(pinnedAccount, false, cloudProject, instanceName)},
		"a machine no entry lists":   {personToken(person, true, signInDomain), googleToken(strangerAccount, true, "", "")},
		"no machine acting for them": {personToken(person, true, signInDomain), nil},
		"a machine where it may not": {personToken(person, true, signInDomain), googleToken(pinnedAccount, true, "other-project", instanceName)},
	} {
		_, err := set.Person(c.token, c.actor, "example-box")
		if err == nil {
			t.Fatalf("Person(%s) was answered, want a refusal", name)
		}
		said[name] = err.Error()
	}
	for name, text := range said {
		for other, otherText := range said {
			if name != other && text == otherText {
				t.Errorf("the refusals of %s and of %s read alike: %q", name, other, text)
			}
		}
	}
}

// TestAnEmptySignInDomainAdmitsNobody: a set built with no domain refuses every person, whatever
// their token says, rather than taking every account outside any Workspace. It is the rule
// oidc.WorkspaceLogin states, and the one a broker told about no domain has to follow.
func TestAnEmptySignInDomainAdmitsNobody(t *testing.T) {
	set, err := exchange.NewSet(nil, nil, []exchange.GoogleEntry{{Account: poolAccount, Service: poolService}}, "")
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	for _, token := range []map[string]any{
		personToken(person, true, signInDomain),
		personToken(person, true, ""),
	} {
		if _, err := set.Person(token, googleToken(poolAccount, true, "", ""), "example-box"); !errors.Is(err, exchange.ErrNotInDomain) {
			t.Errorf("Person(hd %v) = %v, want %v: an empty configured domain admits nobody", token["hd"], err, exchange.ErrNotInDomain)
		}
	}
}

// TestReadGoogleIdentityReadsGooglesClaimShapes: the one reading of a Google identity token's own
// claims, which this package decides on and the broker's VM token verifier reads the same token
// through. A claim of another shape reads as absent, and a token minted off a Compute Engine
// instance reads as carrying no instance rather than as a malformed one.
func TestReadGoogleIdentityReadsGooglesClaimShapes(t *testing.T) {
	whole := exchange.ReadGoogleIdentity(googleToken(pinnedAccount, true, cloudProject, instanceName))
	if whole.Account != pinnedAccount || !whole.EmailVerified || !whole.Compute.Whole() || whole.Compute.Empty() {
		t.Fatalf("ReadGoogleIdentity(a VM's token) = %+v, want the account, a verified email and the whole instance", whole)
	}
	if whole.Compute.Project != cloudProject || whole.Compute.Instance != instanceName ||
		whole.Compute.Zone != "example-zone-a" || whole.Compute.InstanceID != "1234567890" {
		t.Errorf("ReadGoogleIdentity read the instance as %+v", whole.Compute)
	}
	none := exchange.ReadGoogleIdentity(googleToken(poolAccount, "true", "", ""))
	if none.Account != poolAccount || !none.EmailVerified || !none.Compute.Empty() || none.Compute.Whole() {
		t.Fatalf("ReadGoogleIdentity(a pool's token) = %+v, want the account, a verified email and no instance", none)
	}
	odd := exchange.ReadGoogleIdentity(map[string]any{"email": 7, "email_verified": "yes", "google": "not an object"})
	if odd != (exchange.GoogleIdentity{}) {
		t.Errorf("ReadGoogleIdentity(claims of other shapes) = %+v, want every field empty", odd)
	}
}

// TestMachineSubjectNamesOneMachineAndReadsItsAccountBack: the subject the broker keys a
// machine's launcher credential by. A machine minted on a Compute Engine instance is named by
// that instance, so a new key keeps the one credential; a machine minted anywhere else is named
// by the key its proof presented, so two machines of one account are two credentials and never
// one. Both read their account back, and a Kubernetes ServiceAccount reads as no machine at all.
func TestMachineSubjectNamesOneMachineAndReadsItsAccountBack(t *testing.T) {
	onInstance := exchange.ReadGoogleIdentity(googleToken(pinnedAccount, true, cloudProject, instanceName))
	onNone := exchange.ReadGoogleIdentity(googleToken(poolAccount, true, "", ""))

	instanceSubject := exchange.MachineSubject(onInstance, "thumbprint-one")
	if want := pinnedAccount + "@" + cloudProject + "/example-zone-a/1234567890"; instanceSubject != want {
		t.Fatalf("MachineSubject(a Compute Engine machine) = %q, want %q", instanceSubject, want)
	}
	if other := exchange.MachineSubject(onInstance, "thumbprint-two"); other != instanceSubject {
		t.Errorf("MachineSubject of the same instance with another key = %q, want the same subject %q: a new key moves the one credential", other, instanceSubject)
	}
	first := exchange.MachineSubject(onNone, "thumbprint-one")
	if want := poolAccount + "#thumbprint-one"; first != want {
		t.Fatalf("MachineSubject(a machine minted on no instance) = %q, want %q", first, want)
	}
	if second := exchange.MachineSubject(onNone, "thumbprint-two"); second == first {
		t.Errorf("two machines of %s with keys of their own share the subject %q; each must hold a credential of its own", poolAccount, first)
	}
	for _, c := range []struct {
		subject, account string
		machine          bool
	}{
		{subject: instanceSubject, account: pinnedAccount, machine: true},
		{subject: first, account: poolAccount, machine: true},
		{subject: "system:serviceaccount:legion-system:legion-daemon", machine: false},
		{subject: pinnedAccount, machine: false},
		{subject: "", machine: false},
	} {
		account, ok := exchange.MachineAccount(c.subject)
		if ok != c.machine || account != c.account {
			t.Errorf("MachineAccount(%q) = %q, %t; want %q, %t", c.subject, account, ok, c.account, c.machine)
		}
	}
}

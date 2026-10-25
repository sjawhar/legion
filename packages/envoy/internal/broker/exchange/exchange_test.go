package exchange_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/exchange"
)

const (
	workerAccount = "system:serviceaccount:example-ns:example-worker"
	daemonAccount = "system:serviceaccount:example-ns:example-daemon"
	repository    = "example-org/example-repo"
	workflow      = ".github/workflows/release.yaml"
	ref           = "refs/heads/main"
	project       = "ACME"
	person        = "ada@example.com"
	fixedSession  = "legion-daemon:ACME"
	podUID        = "11111111-2222-3333-4444-555555555555"
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
			{Repository: repository, Workflow: ".github/workflows/nightly.yaml", Ref: ref, Project: project, Person: person, Session: fixedSession},
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

func TestPodAnswersTheListedAccount(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "the session the request names", token: podToken(workerAccount, podUID), session: "implementer-g1",
			want: exchange.Claims{Session: "implementer-g1", Person: person, Project: project}},
		{name: "the session the entry fixes, the request naming none", token: podToken(daemonAccount, podUID), session: "",
			want: exchange.Claims{Session: fixedSession, Person: person, Project: project}},
		{name: "the session the entry fixes, the request naming the same", token: podToken(daemonAccount, podUID), session: fixedSession,
			want: exchange.Claims{Session: fixedSession, Person: person, Project: project}},
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
			session: "release-bot", want: exchange.Claims{Session: "release-bot", Person: person, Project: project}},
		{name: "the session the entry fixes", token: workflowToken(repository, runRef(".github/workflows/nightly.yaml"), runRef(".github/workflows/nightly.yaml"), ref),
			session: "", want: exchange.Claims{Session: fixedSession, Person: person, Project: project}},
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
		{name: "a session the entry does not fix", token: workflowToken(repository, runRef(".github/workflows/nightly.yaml"), runRef(".github/workflows/nightly.yaml"), ref),
			session: "release-bot", want: exchange.ErrSessionMismatch, names: fixedSession},
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

func TestMachineActsForTheApproverAndWritesNoProject(t *testing.T) {
	set := listed(t)
	claims, err := set.Machine(person, "ada-devbox")
	if err != nil {
		t.Fatalf("Machine: %v", err)
	}
	if want := (exchange.Claims{Session: "ada-devbox", Person: person}); claims != want {
		t.Fatalf("Machine = %+v, want %+v: a machine credential carries no project", claims, want)
	}
}

func TestMachineRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name     string
		approver string
		session  string
		want     error
		names    string
	}{
		{name: "a credential with no approver", approver: " ", session: "ada-devbox", want: exchange.ErrClaimMissing, names: "approver"},
		{name: "a request naming no session", approver: person, session: "", want: exchange.ErrSessionRequired, names: person},
		{name: "a request naming a session with whitespace", approver: person, session: "ada devbox", want: exchange.ErrSessionInvalid, names: person},
		{name: "a request naming a session past the cap", approver: person, session: strings.Repeat("s", exchange.MaxSessionBytes+1), want: exchange.ErrSessionInvalid, names: person},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Machine(c.approver, c.session)
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

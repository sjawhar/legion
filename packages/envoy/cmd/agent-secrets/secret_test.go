// packages/envoy/cmd/agent-secrets/secret_test.go
//
// The secret forms run in-process here, against a secrets.Local standing in for Secrets Manager,
// a fakeSTS answering one sign-in, and a fake broker answering GET /v1/settings and the reread.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// testAccount is the account policytest.KeyARN, and so every test secret, is in.
const testAccount = "111122223333"

// adaSignIn is ada@example.com's Identity Center sign-in in testAccount.
const adaSignIn = "arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com"

// secretBroker is a fake broker for the secret forms: GET /v1/settings answers policytest's
// namespace in testAccount, and each reread is answered by answer and recorded, in order, or
// refused with the status fail names when it is set.
type secretBroker struct {
	mu      sync.Mutex
	rereads []string
	answers []Reread
	answer  func(name string) Reread
	fail    int
}

// failRereads makes the broker refuse every reread from now on with status.
func (b *secretBroker) failRereads(status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = status
}

// startSecretBroker serves a secretBroker on AGENT_SECRETS_URL for the rest of t.
func startSecretBroker(t *testing.T, answer func(name string) Reread) *secretBroker {
	t.Helper()
	b := &secretBroker{answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/settings":
			writeJSON(w, Settings{SecretsPrefix: policytest.Prefix, KMSKeyARN: policytest.KeyARN, AWSAccountID: testAccount, AWSRegion: "us-east-1"})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/secrets/") && strings.HasSuffix(r.URL.Path, "/reread"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/secrets/"), "/reread")
			b.mu.Lock()
			fail := b.fail
			b.mu.Unlock()
			if fail != 0 {
				w.WriteHeader(fail)
				writeJSON(w, map[string]string{"code": "RATE_LIMITED", "error": "too many secret rereads; try again later"})
				return
			}
			reread := b.answer(name)
			b.mu.Lock()
			b.rereads = append(b.rereads, name)
			b.answers = append(b.answers, reread)
			b.mu.Unlock()
			writeJSON(w, reread)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AGENT_SECRETS_URL", srv.URL)
	return b
}

// recorded is every reread the broker answered, and its answers, in order.
func (b *secretBroker) recorded() ([]string, []Reread) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.rereads...), append([]Reread(nil), b.answers...)
}

// servedBy answers a reread as the broker would from what sm holds: absent for a secret it does
// not hold or one scheduled for deletion, no-current-value for one with no AWSCURRENT version,
// served otherwise.
func servedBy(sm secretsAPI) func(name string) Reread {
	return func(name string) Reread {
		out, err := sm.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID(name))})
		switch {
		case err != nil || out.DeletedDate != nil:
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		case !policy.HasCurrentVersion(out.VersionIdsToStages):
			return Reread{Name: name, Reason: policy.ReasonNoCurrentValue}
		}
		return Reread{Name: name, Served: true}
	}
}

// useAWS makes the secret forms' AWS clients sm and a fakeSTS signed in as arn in account, for the
// rest of t.
func useAWS(t *testing.T, sm secretsAPI, account, arn string) {
	t.Helper()
	restore := awsClients
	awsClients = func(context.Context, string, string) (secretsAPI, stsAPI, error) {
		return sm, fakeSTS{account: account, arn: arn}, nil
	}
	t.Cleanup(func() { awsClients = restore })
}

// useStdin makes value the secret forms' standard input for the rest of t.
func useStdin(t *testing.T, value string) {
	t.Helper()
	restore := secretStdin
	secretStdin = strings.NewReader(value)
	t.Cleanup(func() { secretStdin = restore })
}

// runSecret runs `agent-secrets secret <args>` in-process.
func runSecret(args ...string) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer
	code = cmdSecret(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// describe is what local holds for the secret a session asks for as name.
func describe(t *testing.T, local *secrets.Local, name string) *secretsmanager.DescribeSecretOutput {
	t.Helper()
	out, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID(name))})
	if err != nil {
		t.Fatalf("describe %s: %v", name, err)
	}
	return out
}

// valueOf is local's current value of the secret a session asks for as name.
func valueOf(t *testing.T, local *secrets.Local, name string) string {
	t.Helper()
	out, err := local.GetSecretValue(context.Background(), &secretsmanager.GetSecretValueInput{SecretId: aws.String(policytest.ID(name))})
	if err != nil {
		t.Fatalf("value of %s: %v", name, err)
	}
	return aws.ToString(out.SecretString)
}

// fakeSTS answers GetCallerIdentity with one sign-in.
type fakeSTS struct{ account, arn string }

func (f fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account), Arn: aws.String(f.arn)}, nil
}

func TestSecretCreateRefusesAMachinesOwnRole(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/settings" {
			json.NewEncoder(w).Encode(Settings{SecretsPrefix: "example/agent-secrets/", KMSKeyARN: policytest.KeyARN, AWSAccountID: "111122223333", AWSRegion: "us-east-1"})
		}
	}))
	defer broker.Close()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	restore := awsClients
	awsClients = func(ctx context.Context, profile, region string) (secretsAPI, stsAPI, error) {
		return secrets.NewLocal(), fakeSTS{account: "111122223333", arn: "arn:aws:sts::111122223333:assumed-role/example-devbox-admin-instance-role/i-0abc"}, nil
	}
	t.Cleanup(func() { awsClients = restore })
	stdin := secretStdin
	secretStdin = strings.NewReader("v1")
	t.Cleanup(func() { secretStdin = stdin })
	var out, errOut bytes.Buffer
	if code := cmdSecret([]string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, stderr %q; want 1", code, errOut.String())
	}
	for _, want := range []string{"not a person's Identity Center sign-in", "example-devbox-admin-instance-role"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr %q must name %q", errOut.String(), want)
		}
	}
}

// TestSecretCreateResolvesOwnerMeFromTheSessionName: --owner me is the sign-in's session name, the
// person's email, lowercased; the secret is created on the broker's key with the value read from
// standard input less its one trailing newline, and the broker is asked to serve it.
func TestSecretCreateResolvesOwnerMeFromTheSessionName(t *testing.T) {
	local := secrets.NewLocal()
	broker := startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, "arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/Ada@Example.com")
	useStdin(t, "v1\n")
	stdout, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	got := describe(t, local, "NEW_KEY")
	if tags := tagMap(got.Tags); tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent {
		t.Fatalf("tags = %v, want owner ada@example.com and tier agent", tags)
	}
	if aws.ToString(got.KmsKeyId) != policytest.KeyARN {
		t.Fatalf("key = %q, want the broker's %q", aws.ToString(got.KmsKeyId), policytest.KeyARN)
	}
	if v := valueOf(t, local, "NEW_KEY"); v != "v1" {
		t.Fatalf("value = %q, want %q", v, "v1")
	}
	if rereads, _ := broker.recorded(); len(rereads) != 1 || rereads[0] != "NEW_KEY" {
		t.Fatalf("rereads = %v, want one of NEW_KEY", rereads)
	}
	if !strings.Contains(stdout, "broker: serving NEW_KEY") {
		t.Fatalf("stdout %q must say the broker serves NEW_KEY", stdout)
	}
}

// TestSecretCreateRefusesAnotherAccount: a person's own sign-in in an account other than the
// broker's is refused before anything is written, naming both accounts.
func TestSecretCreateRefusesAnotherAccount(t *testing.T) {
	local := secrets.NewLocal()
	broker := startSecretBroker(t, servedBy(local))
	useAWS(t, local, "999999999999", "arn:aws:sts::999999999999:assumed-role/AWSReservedSSO_User_abc123/ada@example.com")
	useStdin(t, "v1")
	_, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
	}
	for _, want := range []string{"in account 999999999999", "account " + testAccount} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q must name %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "not a person's Identity Center sign-in") {
		t.Fatalf("stderr %q calls a person's own sign-in a machine's", stderr)
	}
	if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
		t.Fatal("NEW_KEY was created under a sign-in in another account")
	}
	if rereads, _ := broker.recorded(); len(rereads) != 0 {
		t.Fatalf("rereads = %v, want none", rereads)
	}
}

// TestSecretFormsRefuseBadUsageWithExitTwo: a missing or malformed argument, or an empty value, is a
// usage error that writes nothing.
func TestSecretFormsRefuseBadUsageWithExitTwo(t *testing.T) {
	for _, tc := range []struct {
		name, stdin, want string
		args              []string
	}{
		{"empty value", "", "standard input, which was empty", []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent"}},
		{"a newline alone", "\n", "standard input, which was empty", []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent"}},
		{"malformed name", "v1", "not an agent secret name", []string{"create", "new-key", "--owner", "me", "--tier", "agent"}},
		{"no name", "v1", "exactly one secret NAME", []string{"create", "--owner", "me", "--tier", "agent"}},
		{"no tier", "v1", "--tier", []string{"create", "NEW_KEY", "--owner", "me"}},
		{"an owner that is neither", "v1", "--owner", []string{"create", "NEW_KEY", "--owner", "bob@example.com", "--tier", "agent"}},
		{"a tier that is neither", "v1", "--tier", []string{"create", "NEW_KEY", "--owner", "me", "--tier", "robot"}},
		{"no form", "", "a form is required", nil},
		{"an unknown form", "", "unknown form", []string{"rename"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := secrets.NewLocal()
			startSecretBroker(t, servedBy(local))
			useAWS(t, local, testAccount, adaSignIn)
			useStdin(t, tc.stdin)
			_, stderr, code := runSecret(tc.args...)
			if code != exitUsageError || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit %d, stderr %q; want %d naming %q", code, stderr, exitUsageError, tc.want)
			}
			if out, _ := local.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{}); len(out.SecretList) != 0 {
				t.Fatalf("a usage error wrote %d secrets", len(out.SecretList))
			}
		})
	}
}

// TestSecretRetagSendsBothTags: a retag naming one tag sends both, the other re-sent as the secret
// holds it, since the deployment repository's IAM statement that lets a person tag their own secret
// conditions on both request tags.
func TestSecretRetagSendsBothTags(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("OWNED_KEY", "ada@example.com", policy.TierAgent, "v1"))
	sent := &recordingTags{Local: local}
	broker := startSecretBroker(t, servedBy(local))
	useAWS(t, sent, testAccount, adaSignIn)
	stdout, stderr, code := runSecret("retag", "OWNED_KEY", "--tier", "human")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	if tags := tagMap(describe(t, local, "OWNED_KEY").Tags); tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierHuman {
		t.Fatalf("tags = %v, want owner ada@example.com unchanged and tier human", tags)
	}
	if len(sent.requests) != 1 {
		t.Fatalf("TagResource requests = %d, want 1", len(sent.requests))
	}
	if got := tagMap(sent.requests[0].Tags); len(got) != 2 || got[policy.TagOwner] != "ada@example.com" || got[policy.TagTier] != policy.TierHuman {
		t.Fatalf("TagResource sent %v, want both tags: owner ada@example.com and tier human", got)
	}
	if rereads, _ := broker.recorded(); len(rereads) != 1 || rereads[0] != "OWNED_KEY" {
		t.Fatalf("rereads = %v, want one of OWNED_KEY", rereads)
	}
	if !strings.Contains(stdout, "broker: serving OWNED_KEY") {
		t.Fatalf("stdout %q must say the broker serves OWNED_KEY", stdout)
	}
}

// recordingTags is a secrets.Local that records every TagResource request it is sent.
type recordingTags struct {
	*secrets.Local
	requests []*secretsmanager.TagResourceInput
}

func (r *recordingTags) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	r.requests = append(r.requests, in)
	return r.Local.TagResource(ctx, in, o...)
}

// refusingTags is a secrets.Local whose TagResource IAM refuses, as it refuses a person changing
// a shared secret's owner or tier: the SDK answers AccessDeniedException as a generic API error.
type refusingTags struct{ *secrets.Local }

// accessDenied is the refusal refusingTags answers.
const accessDenied = "User: arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc/ada@example.com is not authorized to perform: secretsmanager:TagResource"

func (refusingTags) TagResource(context.Context, *secretsmanager.TagResourceInput, ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: accessDenied}
}

// TestSecretRetagOfASharedSecretSaysAnAdministratorChangesIt: IAM refuses a person's change to a
// shared secret's owner or tier, and a person making their own secret shared; the CLI prints AWS's
// refusal and says whose change it is, and nothing changes. Any other refused retag says only what
// AWS said.
func TestSecretRetagOfASharedSecretSaysAnAdministratorChangesIt(t *testing.T) {
	local := secrets.NewLocal(
		policytest.Secret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, "v1"),
		policytest.Secret("OWNED_KEY", "ada@example.com", policy.TierHuman, "v1"),
	)
	broker := startSecretBroker(t, servedBy(local))
	useAWS(t, refusingTags{local}, testAccount, adaSignIn)
	for _, args := range [][]string{{"retag", "SHARED_KEY", "--tier", "agent"}, {"retag", "OWNED_KEY", "--owner", "shared"}} {
		_, stderr, code := runSecret(args...)
		if code != 1 {
			t.Fatalf("%v: exit %d, stderr %q; want 1", args, code, stderr)
		}
		for _, want := range []string{accessDenied, "a shared secret's owner and tier are an administrator's to change"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("%v: stderr %q must contain %q", args, stderr, want)
			}
		}
	}
	if tags := tagMap(describe(t, local, "SHARED_KEY").Tags); tags[policy.TagOwner] != policy.OwnerShared || tags[policy.TagTier] != policy.TierHuman {
		t.Fatalf("tags = %v, want shared and human unchanged", tags)
	}
	if tags := tagMap(describe(t, local, "OWNED_KEY").Tags); tags[policy.TagOwner] != "ada@example.com" {
		t.Fatalf("tags = %v, want OWNED_KEY's owner unchanged", tags)
	}
	_, stderr, code := runSecret("retag", "OWNED_KEY", "--tier", "agent")
	if code != 1 || !strings.Contains(stderr, accessDenied) || strings.Contains(stderr, "administrator") {
		t.Fatalf("an owned secret's refused retag: exit %d, stderr %q; want 1 with AWS's refusal alone", code, stderr)
	}
	if rereads, _ := broker.recorded(); len(rereads) != 0 {
		t.Fatalf("rereads = %v, want none after refused writes", rereads)
	}
}

// TestSecretReadsRefuseAnotherAccount: list and show under a sign-in in an account other than the
// broker's are refused, naming both accounts, rather than reading that account's secrets as if
// they were the agent secrets. Reads still take a machine's role in the broker's account.
func TestSecretReadsRefuseAnotherAccount(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, "999999999999", "arn:aws:sts::999999999999:assumed-role/AWSReservedSSO_User_abc123/ada@example.com")
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"show", "HELD_KEY"}} {
		stdout, stderr, code := runSecret(args...)
		if code != 1 || stdout != "" {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want 1 and nothing on stdout", args, code, stdout, stderr)
		}
		for _, want := range []string{"in account 999999999999", "account " + testAccount} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("%v: stderr %q must name %q", args, stderr, want)
			}
		}
	}
}

// failingStdin fails t if a form reads it: no value may be asked for before the sign-in is checked.
type failingStdin struct{ t *testing.T }

func (f failingStdin) Read([]byte) (int, error) {
	f.t.Error("the form read the value from standard input before refusing the sign-in")
	return 0, io.EOF
}

// TestSecretWritesCheckTheSignInBeforeReadingTheValue: create and set refuse a machine's role or
// another account before they read the value, so nobody pastes a secret only to be refused.
func TestSecretWritesCheckTheSignInBeforeReadingTheValue(t *testing.T) {
	restore := secretStdin
	secretStdin = failingStdin{t}
	t.Cleanup(func() { secretStdin = restore })
	for _, signIn := range []struct{ account, arn, refusal string }{
		{testAccount, "arn:aws:sts::111122223333:assumed-role/example-service-role/session", "not a person's Identity Center sign-in"},
		{"999999999999", "arn:aws:sts::999999999999:assumed-role/AWSReservedSSO_User_abc123/ada@example.com", "in account 999999999999"},
	} {
		for _, args := range [][]string{{"create", "NEW_KEY", "--owner", "me", "--tier", "agent"}, {"set", "HELD_KEY"}} {
			local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
			startSecretBroker(t, servedBy(local))
			useAWS(t, local, signIn.account, signIn.arn)
			_, stderr, code := runSecret(args...)
			if code != 1 || !strings.Contains(stderr, signIn.refusal) {
				t.Fatalf("%v as %s: exit %d, stderr %q; want 1 naming %q", args, signIn.arn, code, stderr, signIn.refusal)
			}
			if valueOf(t, local, "HELD_KEY") != "v1" {
				t.Fatalf("%v as %s changed HELD_KEY's value", args, signIn.arn)
			}
			if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
				t.Fatalf("%v as %s created NEW_KEY", args, signIn.arn)
			}
		}
	}
}

// terminalStdin stands for a terminal on standard input (useTerminal): it fails t if a form reads
// it to its end instead of prompting for the value.
type terminalStdin struct{ t *testing.T }

func (s terminalStdin) Read([]byte) (int, error) {
	s.t.Error("the form read standard input to its end at a terminal instead of prompting for the value")
	return 0, io.EOF
}

// terminalFD is the file descriptor of useTerminal's terminal.
const terminalFD = 7

// useTerminal makes standard input a terminal for the rest of t, at which each value a form reads
// with echo off is the next of typed; it answers the descriptor of each such read, in order.
func useTerminal(t *testing.T, typed ...string) *[]int {
	t.Helper()
	restoreStdin, restoreTerminal, restoreHidden := secretStdin, stdinTerminal, readHidden
	t.Cleanup(func() { secretStdin, stdinTerminal, readHidden = restoreStdin, restoreTerminal, restoreHidden })
	stdin := terminalStdin{t}
	secretStdin = stdin
	stdinTerminal = func(r io.Reader) (int, bool) {
		if r != io.Reader(stdin) {
			return 0, false
		}
		return terminalFD, true
	}
	reads := &[]int{}
	readHidden = func(fd int) ([]byte, error) {
		*reads = append(*reads, fd)
		if len(*reads) > len(typed) {
			t.Fatalf("read %d values at the terminal, want %d", len(*reads), len(typed))
		}
		return []byte(typed[len(*reads)-1]), nil
	}
	return reads
}

// TestSecretCreateAndSetPromptForTheValueAtATerminal: with a terminal on standard input, create
// and set print a prompt naming the secret on stderr and read one line with echo off, never
// printing the value; a refused sign-in prompts for nothing.
func TestSecretCreateAndSetPromptForTheValueAtATerminal(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	reads := useTerminal(t, "typed-new", "typed-held")
	for _, tc := range []struct {
		name, value string
		args        []string
	}{
		{"NEW_KEY", "typed-new", []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent"}},
		{"HELD_KEY", "typed-held", []string{"set", "HELD_KEY"}},
	} {
		stdout, stderr, code := runSecret(tc.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d, stderr %q; want 0", tc.args, code, stderr)
		}
		if want := "Value for " + tc.name + ": \n"; stderr != want {
			t.Fatalf("%v: stderr %q, want exactly the prompt %q", tc.args, stderr, want)
		}
		if strings.Contains(stdout+stderr, tc.value) {
			t.Fatalf("%v printed the value: stdout %q, stderr %q", tc.args, stdout, stderr)
		}
		if v := valueOf(t, local, tc.name); v != tc.value {
			t.Fatalf("%v: value = %q, want %q", tc.args, v, tc.value)
		}
	}
	if len(*reads) != 2 || (*reads)[0] != terminalFD || (*reads)[1] != terminalFD {
		t.Fatalf("hidden reads = %v, want two at the terminal's descriptor %d", *reads, terminalFD)
	}
	useAWS(t, local, testAccount, "arn:aws:sts::111122223333:assumed-role/example-service-role/session")
	_, stderr, code := runSecret("set", "HELD_KEY")
	if code != 1 || strings.Contains(stderr, "Value for") || len(*reads) != 2 {
		t.Fatalf("set under a machine's role: exit %d, stderr %q, hidden reads %v; want 1 with no prompt and no read", code, stderr, *reads)
	}
}

// TestSecretAnEmptyValueAtATerminalIsAUsageError: Enter alone, or the end of input, at the prompt
// is a usage error that writes nothing.
func TestSecretAnEmptyValueAtATerminalIsAUsageError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"enter alone", nil}, {"end of input", io.EOF}} {
		t.Run(tc.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
			startSecretBroker(t, servedBy(local))
			useAWS(t, local, testAccount, adaSignIn)
			useTerminal(t)
			readHidden = func(int) ([]byte, error) { return nil, tc.err }
			_, stderr, code := runSecret("set", "HELD_KEY")
			if code != exitUsageError || !strings.Contains(stderr, "no value was entered") {
				t.Fatalf("exit %d, stderr %q; want %d saying no value was entered", code, stderr, exitUsageError)
			}
			if v := valueOf(t, local, "HELD_KEY"); v != "v1" {
				t.Fatalf("value = %q, want v1 unchanged", v)
			}
		})
	}
}

// TestStdinIsATerminalOnlyAtATerminal: stdinTerminal answers false for a pipe and for a reader that
// is no file, so a value piped in is read to its end, with no prompt.
func TestStdinIsATerminalOnlyAtATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for _, in := range []io.Reader{r, strings.NewReader("v1")} {
		if _, ok := stdinTerminal(in); ok {
			t.Fatalf("stdinTerminal(%T) = true, want false", in)
		}
	}
}

// TestSecretListShowsValueAndDeletion: list shows every secret under the prefix, a secret
// scheduled for deletion included with when it was deleted and the earliest Secrets Manager can
// purge it (DeletedDate plus AWS's 7-day minimum window, never the 30 days delete schedules, since
// the listing does not say which window the delete used), whether each has a value, and a missing
// date or tag as absent; nothing outside the prefix, and never a value.
func TestSecretListShowsValueAndDeletion(t *testing.T) {
	deletedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	gone := policytest.Secret("GONE_KEY", "ada@example.com", policy.TierAgent, "gone-value")
	gone.DeletedAt = &deletedAt
	untagged := policytest.Secret("UNTAGGED_KEY", "", "", "untagged-value")
	untagged.Tags = nil
	local := secrets.NewLocal(
		policytest.Secret("LIVE_KEY", "ada@example.com", policy.TierHuman, "live-value"),
		policytest.Secret("EMPTY_KEY", policy.OwnerShared, policy.TierAgent, ""),
		gone, untagged,
		secrets.LocalSecret{Name: "example/other/outside", KmsKeyID: policytest.KeyARN, Value: "outside-value"},
	)
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	stdout, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	rows := map[string][]string{}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "NAME OWNER TIER VALUE DELETED EARLIEST_PURGE" {
		t.Fatalf("header = %q", lines[0])
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		rows[fields[0]] = fields
	}
	want := map[string][]string{
		"EMPTY_KEY":    {"EMPTY_KEY", "shared", "agent", "no", "-", "-"},
		"GONE_KEY":     {"GONE_KEY", "ada@example.com", "agent", "yes", "2026-10-01T12:00:00Z", "2026-10-08T12:00:00Z"},
		"LIVE_KEY":     {"LIVE_KEY", "ada@example.com", "human", "yes", "-", "-"},
		"UNTAGGED_KEY": {"UNTAGGED_KEY", "-", "-", "yes", "-", "-"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want exactly %v", rows, want)
	}
	for name, fields := range want {
		if strings.Join(rows[name], " ") != strings.Join(fields, " ") {
			t.Fatalf("row %s = %v, want %v", name, rows[name], fields)
		}
	}
	for _, never := range []string{"live-value", "gone-value", "untagged-value", "outside", "2026-10-31"} {
		if strings.Contains(stdout, never) {
			t.Fatalf("list printed %q: %s", never, stdout)
		}
	}
	if !strings.Contains(stderr, "EARLIEST_PURGE is the deletion plus Secrets Manager's 7-day minimum recovery window") {
		t.Fatalf("stderr %q must say what EARLIEST_PURGE is", stderr)
	}

	stdout, stderr, code = runSecret("list", "--json")
	if code != 0 {
		t.Fatalf("list --json: exit %d, stderr %q", code, stderr)
	}
	var views []struct {
		Name          string     `json:"name"`
		HasValue      bool       `json:"has_value"`
		Created       *time.Time `json:"created"`
		Deleted       *time.Time `json:"deleted"`
		EarliestPurge *time.Time `json:"earliest_purge"`
	}
	if err := json.Unmarshal([]byte(stdout), &views); err != nil {
		t.Fatalf("list --json is not a JSON array of secrets: %v: %s", err, stdout)
	}
	if len(views) != 4 || views[1].Name != "GONE_KEY" || views[0].HasValue || views[1].Created != nil ||
		views[1].Deleted == nil || !views[1].Deleted.Equal(deletedAt) ||
		views[1].EarliestPurge == nil || !views[1].EarliestPurge.Equal(deletedAt.AddDate(0, 0, 7)) ||
		views[0].Deleted != nil || views[0].EarliestPurge != nil {
		t.Fatalf("list --json = %s", stdout)
	}
}

// TestSecretShowOfADeletedSecretNamesTheEarliestPurge: show of a secret scheduled for deletion
// prints when it was deleted and the earliest Secrets Manager can purge it, DeletedDate plus 7
// days, labelled as the earliest rather than the exact purge, and never DeletedDate plus 30.
func TestSecretShowOfADeletedSecretNamesTheEarliestPurge(t *testing.T) {
	deletedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	gone := policytest.Secret("GONE_KEY", "ada@example.com", policy.TierAgent, "gone-value")
	gone.DeletedAt = &deletedAt
	local := secrets.NewLocal(gone)
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	stdout, stderr, code := runSecret("show", "GONE_KEY")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	for _, want := range []string{"deleted: 2026-10-01T12:00:00Z\n", "earliest_purge: 2026-10-08T12:00:00Z (", "at least"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout %q must contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "2026-10-31") {
		t.Fatalf("stdout %q names DeletedDate plus 30 days, which may be after the secret is purged", stdout)
	}
}

// TestEarliestPurgeIsTheOnePlaceDeletedDateIsRead pins earliestPurge's reading of DeletedDate: the
// moment the delete ran (measured against Secrets Manager) plus AWS's 7-day minimum recovery
// window, the earliest the secret can be purged whatever window its delete used; never plus 30.
func TestEarliestPurgeIsTheOnePlaceDeletedDateIsRead(t *testing.T) {
	if earliestPurge(nil) != nil {
		t.Fatal("earliestPurge(nil) must be nil: the secret is not scheduled for deletion")
	}
	deletedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if got := earliestPurge(&deletedAt); !got.Equal(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("earliestPurge = %v, want the delete's time plus 7 days", got)
	}
}

// TestSecretShowNeverPrintsTheValue: show prints a secret's owner, tier, dates and versions, a
// date Secrets Manager answers none for as absent, and never the value, as text or as JSON.
func TestSecretShowNeverPrintsTheValue(t *testing.T) {
	const value = "show-must-not-print-this"
	local := secrets.NewLocal(policytest.Secret("SHOWN_KEY", "ada@example.com", policy.TierHuman, value))
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	stdout, stderr, code := runSecret("show", "SHOWN_KEY")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	for _, want := range []string{
		"name: SHOWN_KEY\n", "secret_name: " + policytest.ID("SHOWN_KEY") + "\n",
		"owner: ada@example.com\n", "tier: human\n", "created: -\n", "last_changed: -\n",
		"version: local-current AWSCURRENT\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout %q must contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "deleted:") || strings.Contains(stdout, "earliest_purge:") {
		t.Fatalf("stdout %q names a deletion for a secret not scheduled for one", stdout)
	}
	jsonOut, stderr, code := runSecret("show", "SHOWN_KEY", "--json")
	if code != 0 {
		t.Fatalf("show --json: exit %d, stderr %q", code, stderr)
	}
	for _, out := range []string{stdout, jsonOut} {
		if strings.Contains(out, value) {
			t.Fatalf("show printed the value: %s", out)
		}
	}
	if _, _, code := runSecret("show", "MISSING_KEY"); code != 1 {
		t.Fatalf("show of a secret Secrets Manager does not hold: exit %d, want 1", code)
	}
}

// TestSecretDeleteThenRestoreRereads: delete schedules a 30-day deletion and the broker's reread
// stops serving the secret; restore brings it back and the broker serves it again. Each write
// rereads once, in order.
func TestSecretDeleteThenRestoreRereads(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("DOOMED_KEY", "ada@example.com", policy.TierAgent, "v1"))
	broker := startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	stdout, stderr, code := runSecret("delete", "DOOMED_KEY")
	if code != 0 {
		t.Fatalf("delete: exit %d, stderr %q; want 0", code, stderr)
	}
	held := describe(t, local, "DOOMED_KEY")
	if held.DeletedDate == nil {
		t.Fatal("DOOMED_KEY is not scheduled for deletion")
	}
	// delete prints DeleteSecret's own DeletionDate, the exact end of the 30-day window it
	// scheduled, which a later read back cannot know.
	want := "broker: DOOMED_KEY is deleted (restorable until " + formatTime(aws.Time(held.DeletedDate.AddDate(0, 0, recoveryWindowDays))) + ")"
	if !strings.Contains(stdout, want) {
		t.Fatalf("delete stdout %q must contain %q", stdout, want)
	}
	stdout, stderr, code = runSecret("restore", "DOOMED_KEY")
	if code != 0 {
		t.Fatalf("restore: exit %d, stderr %q; want 0", code, stderr)
	}
	if describe(t, local, "DOOMED_KEY").DeletedDate != nil {
		t.Fatal("DOOMED_KEY is still scheduled for deletion after restore")
	}
	if !strings.Contains(stdout, "broker: serving DOOMED_KEY") {
		t.Fatalf("restore stdout %q must say the broker serves it", stdout)
	}
	rereads, answers := broker.recorded()
	if len(rereads) != 2 || rereads[0] != "DOOMED_KEY" || rereads[1] != "DOOMED_KEY" || answers[0].Served || !answers[1].Served {
		t.Fatalf("rereads = %v answered %+v; want DOOMED_KEY absent after delete, then served after restore", rereads, answers)
	}
}

// TestSecretDeleteNeverForces: delete schedules the deletion with the longest recovery window and
// never forces it.
func TestSecretDeleteNeverForces(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("DOOMED_KEY", "ada@example.com", policy.TierAgent, "v1"))
	sent := &recordingDeletes{Local: local}
	startSecretBroker(t, servedBy(local))
	useAWS(t, sent, testAccount, adaSignIn)
	if _, stderr, code := runSecret("delete", "DOOMED_KEY"); code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	if len(sent.requests) != 1 || aws.ToInt64(sent.requests[0].RecoveryWindowInDays) != 30 || aws.ToBool(sent.requests[0].ForceDeleteWithoutRecovery) {
		t.Fatalf("DeleteSecret requests = %+v, want one with a 30-day window and no force", sent.requests)
	}
}

// recordingDeletes is a secrets.Local that records every DeleteSecret request it is sent.
type recordingDeletes struct {
	*secrets.Local
	requests []*secretsmanager.DeleteSecretInput
}

func (r *recordingDeletes) DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	r.requests = append(r.requests, in)
	return r.Local.DeleteSecret(ctx, in, o...)
}

// TestSecretWriteExitsOneWhenTheBrokerRefuses: a write the broker's reread contradicts exits 1
// naming the broker's reason: a set it refuses (no-current-value), a delete it still serves.
func TestSecretWriteExitsOneWhenTheBrokerRefuses(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("REFUSED_KEY", "ada@example.com", policy.TierAgent, "v1"))
	startSecretBroker(t, func(name string) Reread { return Reread{Name: name, Reason: policy.ReasonNoCurrentValue} })
	useAWS(t, local, testAccount, adaSignIn)
	useStdin(t, "v2")
	stdout, stderr, code := runSecret("set", "REFUSED_KEY")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
	}
	if !strings.Contains(stderr, "no-current-value") || !strings.Contains(stdout, "broker: refusing REFUSED_KEY (reason=no-current-value)") {
		t.Fatalf("stdout %q stderr %q must name the broker's reason", stdout, stderr)
	}
	if v := valueOf(t, local, "REFUSED_KEY"); v != "v2" {
		t.Fatalf("value = %q, want the write to stand at %q", v, "v2")
	}

	startSecretBroker(t, func(name string) Reread { return Reread{Name: name, Served: true} })
	if _, stderr, code := runSecret("delete", "REFUSED_KEY"); code != 1 || !strings.Contains(stderr, "still serves REFUSED_KEY") {
		t.Fatalf("a delete the broker still serves: exit %d, stderr %q; want 1 saying so", code, stderr)
	}
}

// TestSecretWriteExitsOneWhenTheBrokerCannotBeAsked: a write whose reread the broker refuses (here
// rate-limited) exits 1 saying the write stands and is served only from the broker's next reload.
func TestSecretWriteExitsOneWhenTheBrokerCannotBeAsked(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("SET_KEY", "ada@example.com", policy.TierAgent, "v1"))
	broker := startSecretBroker(t, servedBy(local))
	broker.failRereads(http.StatusTooManyRequests)
	useAWS(t, local, testAccount, adaSignIn)
	useStdin(t, "v2")
	_, stderr, code := runSecret("set", "SET_KEY")
	if code != 1 || !strings.Contains(stderr, "the write to Secrets Manager stands") || !strings.Contains(stderr, "RATE_LIMITED") {
		t.Fatalf("exit %d, stderr %q; want 1 saying the write stands and naming the refusal", code, stderr)
	}
	if v := valueOf(t, local, "SET_KEY"); v != "v2" {
		t.Fatalf("value = %q, want %q", v, "v2")
	}
}

// TestSecretWritesRefuseAMachinesOwnRoleBeforeWriting: every write form refuses a machine's own
// role before it calls AWS to write; the reads take a machine's role in the broker's account (IAM
// answers them).
func TestSecretWritesRefuseAMachinesOwnRoleBeforeWriting(t *testing.T) {
	const machine = "arn:aws:sts::111122223333:assumed-role/example-service-role/session"
	for _, args := range [][]string{
		{"set", "HELD_KEY"}, {"retag", "HELD_KEY", "--tier", "human"}, {"delete", "HELD_KEY"}, {"restore", "HELD_KEY"},
	} {
		t.Run(args[0], func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
			broker := startSecretBroker(t, servedBy(local))
			useAWS(t, local, testAccount, machine)
			useStdin(t, "v2")
			_, stderr, code := runSecret(args...)
			if code != 1 || !strings.Contains(stderr, "not a person's Identity Center sign-in") || !strings.Contains(stderr, "example-service-role") {
				t.Fatalf("exit %d, stderr %q; want 1 naming the role", code, stderr)
			}
			got := describe(t, local, "HELD_KEY")
			if got.DeletedDate != nil || tagMap(got.Tags)[policy.TagTier] != policy.TierAgent || valueOf(t, local, "HELD_KEY") != "v1" {
				t.Fatalf("HELD_KEY changed under a machine's role: %+v", got)
			}
			if rereads, _ := broker.recorded(); len(rereads) != 0 {
				t.Fatalf("rereads = %v, want none", rereads)
			}
		})
	}
	local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, machine)
	for _, args := range [][]string{{"list"}, {"show", "HELD_KEY"}} {
		if _, stderr, code := runSecret(args...); code != 0 {
			t.Fatalf("%s under a machine's role: exit %d, stderr %q; want 0 (IAM decides reads)", args[0], code, stderr)
		}
	}
}

// TestSecretSetOrCreateWithASharedOwner: create --owner shared tags the secret shared, whoever is
// signed in, and set replaces the value of a secret already held.
func TestSecretSetOrCreateWithASharedOwner(t *testing.T) {
	local := secrets.NewLocal()
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	useStdin(t, "first")
	if _, stderr, code := runSecret("create", "TEAM_KEY", "--owner", "shared", "--tier", "human"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	if tags := tagMap(describe(t, local, "TEAM_KEY").Tags); tags[policy.TagOwner] != policy.OwnerShared || tags[policy.TagTier] != policy.TierHuman {
		t.Fatalf("tags = %v, want shared and human", tags)
	}
	useStdin(t, "second\n")
	if _, stderr, code := runSecret("set", "TEAM_KEY"); code != 0 {
		t.Fatalf("set: exit %d, stderr %q", code, stderr)
	}
	if v := valueOf(t, local, "TEAM_KEY"); v != "second" {
		t.Fatalf("value = %q, want %q", v, "second")
	}
	useStdin(t, "third")
	if _, stderr, code := runSecret("create", "TEAM_KEY", "--owner", "shared", "--tier", "human"); code != 1 || !strings.Contains(stderr, "ResourceExistsException") {
		t.Fatalf("a second create of TEAM_KEY: exit %d, stderr %q; want 1 with AWS's ResourceExistsException", code, stderr)
	}
}

// TestSecretFormsSignInWithTheProfileInTheBrokersRegion: every form loads the person's AWS sign-in
// with the profile --profile names ("" leaves it to AWS_PROFILE and the SDK's default chain),
// pinned to the region the broker's settings answer.
func TestSecretFormsSignInWithTheProfileInTheBrokersRegion(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
	startSecretBroker(t, servedBy(local))
	type load struct{ profile, region string }
	var loads []load
	restore := awsClients
	awsClients = func(_ context.Context, profile, region string) (secretsAPI, stsAPI, error) {
		loads = append(loads, load{profile, region})
		return local, fakeSTS{account: testAccount, arn: adaSignIn}, nil
	}
	t.Cleanup(func() { awsClients = restore })
	useStdin(t, "v2")
	for _, args := range [][]string{{"list", "--profile", "work"}, {"set", "HELD_KEY", "--profile", "work"}, {"show", "HELD_KEY"}} {
		if _, stderr, code := runSecret(args...); code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
	want := []load{{"work", "us-east-1"}, {"work", "us-east-1"}, {"", "us-east-1"}}
	if len(loads) != len(want) || loads[0] != want[0] || loads[1] != want[1] || loads[2] != want[2] {
		t.Fatalf("loads = %+v, want %+v", loads, want)
	}
}

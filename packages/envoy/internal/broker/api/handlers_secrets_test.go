package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/ratelimit"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// wireSettings is GET /v1/settings's answer as the CLI reads it.
type wireSettings struct {
	Store         string `json:"store"`
	SecretsPrefix string `json:"secrets_prefix"`
	KMSKeyARN     string `json:"kms_key_arn"`
	AWSAccountID  string `json:"aws_account_id"`
	AWSRegion     string `json:"aws_region"`
	KMSKeyName    string `json:"kms_key_name"`
	GCPProject    string `json:"gcp_project"`
}

// wireReread is POST /v1/secrets/{name}/reread's answer.
type wireReread struct {
	Name   string `json:"name"`
	Served bool   `json:"served"`
	Reason string `json:"reason"`
	Kind   string `json:"kind"`
}

func TestSettingsAnswersTheNamespaceAndKey(t *testing.T) {
	ts := newTestServer(t)
	status, body := ts.req(t, http.MethodGet, "/v1/settings", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	got := decode[wireSettings](t, body)
	want := wireSettings{Store: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, KMSKeyARN: policytest.KeyARN, AWSAccountID: "111122223333", AWSRegion: "us-east-1"}
	if got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	// The AWS fields are omitted, not empty, so a client reading the raw body sees no AWS key.
	if strings.Contains(string(body), "kms_key_name") || strings.Contains(string(body), "gcp_project") {
		t.Fatalf("an AWS broker's settings carry GCP fields: %s", body)
	}
}

// TestSettingsAnswersTheGCPStore pins GET /v1/settings for a broker on Google Secret Manager: the
// store's name, the prefix, the Cloud KMS key's name and the project number cut from the prefix,
// with none of the AWS fields.
func TestSettingsAnswersTheGCPStore(t *testing.T) {
	const prefix = "projects/000000000000/secrets/agent-secrets-"
	const key = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
	mux := http.NewServeMux()
	cur, err := policy.NewCurrent(t.Context(), policytest.Loader(secrets.NewLocal()), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	api.Register(mux, api.Deps{Policy: cur, SecretsStore: secrets.StoreGCP, SecretsPrefix: prefix, SecretsKMSKey: key})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/v1/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	got := decode[wireSettings](t, body)
	want := wireSettings{Store: secrets.StoreGCP, SecretsPrefix: prefix, KMSKeyName: key, GCPProject: "000000000000"}
	if got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	for _, field := range []string{"kms_key_arn", "aws_account_id", "aws_region"} {
		if strings.Contains(string(body), field) {
			t.Fatalf("a GCP broker's settings carry %s: %s", field, body)
		}
	}
}

// rereadsFreely is the broker-wide bucket a row about what a reread *answers* mounts, so that
// row never meets the production sizing of a bucket it is not about. The production figure
// (api.DefaultRereadOverallLimit) is a burst of ten refilled one a second, sized for the fleet
// rather than for a test: a row driving several rereads back to back, and any row that later
// drives one more, would be answered by a limiter rather than by the handler it is testing, and
// would go red on a sizing change that was correct. The rows that *are* about the limiter inject
// a bucket of their own in the same way, with no tokens left in it.
var rereadsFreely = ratelimit.Limit{Every: time.Millisecond, Burst: 100}

func TestRereadServesANewSecretAndDropsADeletedOne(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) { d.RereadOverallLimit = &rereadsFreely })
	ts.Secrets.Put(policytest.Secret("NEW_KEY", testApprover, policy.TierAgent, "v1"))
	status, body := ts.req(t, http.MethodPost, "/v1/secrets/NEW_KEY/reread", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("reread: %d %s", status, body)
	}
	if r := decode[wireReread](t, body); !r.Served || r.Reason != "" {
		t.Fatalf("reread = %+v, want served", r)
	}
	ts.Secrets.Delete(policytest.ID("NEW_KEY"))
	_, body = ts.req(t, http.MethodPost, "/v1/secrets/NEW_KEY/reread", nil, nil)
	if r := decode[wireReread](t, body); r.Served || r.Reason != policy.ReasonAbsent {
		t.Fatalf("after delete = %+v, want absent", r)
	}
	if status, body = ts.req(t, http.MethodPost, "/v1/secrets/not_a_name/reread", nil, nil); status != http.StatusBadRequest || decode[wireError](t, body).Code != "SECRET_NAME_INVALID" {
		t.Fatalf("malformed name: %d %s, want 400 SECRET_NAME_INVALID", status, body)
	}
	// A name whose Secrets Manager name would pass the 512-character limit names no secret.
	tooLong := "A" + strings.Repeat("B", 512-len(policytest.Prefix))
	if status, body = ts.req(t, http.MethodPost, "/v1/secrets/"+tooLong+"/reread", nil, nil); status != http.StatusBadRequest || decode[wireError](t, body).Code != "SECRET_NAME_INVALID" {
		t.Fatalf("over-long name: %d %s, want 400 SECRET_NAME_INVALID", status, body)
	}
}

// TestARereadNamesTheKindItServes: a reread's answer names the kind the broker serves the secret
// as, and none for a stored secret, so a person who retags a secret to a kind can tell a reread
// that read the new tags from one that read them stale. It carries nothing else of the tags.
func TestARereadNamesTheKindItServes(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) { d.RereadOverallLimit = &rereadsFreely })
	ts.Secrets.Put(policytest.Secret("PLAIN_KEY", testApprover, policy.TierAgent, "v1"))
	_, body := ts.req(t, http.MethodPost, "/v1/secrets/PLAIN_KEY/reread", nil, nil)
	if r := decode[wireReread](t, body); !r.Served || r.Kind != "" || strings.Contains(string(body), `"kind"`) {
		t.Fatalf("reread of a stored secret = %s, want served with no kind", body)
	}
	for _, kind := range policy.Kinds {
		ts.Secrets.Put(policytest.KindSecret("PLAIN_KEY", testApprover, policy.TierAgent, kind, "v1"))
		_, body = ts.req(t, http.MethodPost, "/v1/secrets/PLAIN_KEY/reread", nil, nil)
		if r := decode[wireReread](t, body); !r.Served || r.Kind != kind {
			t.Fatalf("reread once the kind is %s = %s, want served as %s", kind, body, kind)
		}
		for _, private := range []string{testApprover, policy.TierAgent, "github-owners", "owner"} {
			if strings.Contains(string(body), private) {
				t.Fatalf("reread answer %s carries %q; it names only the kind", body, private)
			}
		}
	}
}

// TestARereadWhoseRequestEndedLogsNoError pins that a reread whose request ended before it
// finished - its caller went away, or its deadline passed - is answered without the ERROR line
// "broker: reread secret failed" that an operator's alert counts, as the miss path logs nothing for
// one, while a reread Secrets Manager itself fails for a caller still waiting is still that 500 and
// that line.
func TestARereadWhoseRequestEndedLogsNoError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local := secrets.NewLocal()
	loader := policytest.Loader(local)
	loader.Store = policytest.StoreThrough(local, abandoningDescriber{SecretsManagerAPI: local, cancel: cancel})
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.Register(mux, api.Deps{Policy: cur, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN})
	logged := policytest.CaptureLog(t)

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/secrets/NEW_KEY/reread", nil))
	if strings.Contains(logged.String(), "reread secret failed") {
		t.Fatalf("a reread whose caller went away logged:\n%s", logged.String())
	}

	failed := httptest.NewRecorder()
	mux.ServeHTTP(failed, httptest.NewRequest(http.MethodPost, "/v1/secrets/NEW_KEY/reread", nil))
	if failed.Code != http.StatusInternalServerError || !strings.Contains(logged.String(), "ERROR broker: reread secret failed") {
		t.Fatalf("a reread Secrets Manager failed for a live caller: %d, logged %q; want 500 and the ERROR line", failed.Code, logged.String())
	}
}

// abandoningDescriber is a caller that goes away while the reread's DescribeSecret is in flight:
// it ends that caller's context and fails as the AWS SDK does once a context has ended. For a
// caller whose context it does not hold, it fails as a throttled Secrets Manager does.
type abandoningDescriber struct {
	secrets.SecretsManagerAPI
	cancel context.CancelFunc
}

func (d abandoningDescriber) DescribeSecret(ctx context.Context, _ *secretsmanager.DescribeSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	d.cancel()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("operation error Secrets Manager: DescribeSecret, %w", err)
	}
	return nil, errors.New("operation error Secrets Manager: DescribeSecret, ThrottlingException")
}

// postReread sends one reread of name with headers, answering the status, headers and body: the
// 429 each reread limit answers carries a Retry-After, which testServer.req does not give back.
func postReread(t *testing.T, ts *testServer, name string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/secrets/"+name+"/reread", nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, body
}

func TestRereadIsRateLimitedPerAddress(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) { d.RereadLimit = &ratelimit.Limit{Every: time.Hour, Burst: 1} })
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/WORKER_TOKEN/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("first reread: %d %s", status, body)
	}
	status, headers, body := postReread(t, ts, "WORKER_TOKEN", nil)
	if status != http.StatusTooManyRequests || headers.Get("Retry-After") != "3600" || decode[wireError](t, body).Code != "RATE_LIMITED" {
		t.Fatalf("second reread: %d Retry-After=%q %s, want 429 RATE_LIMITED with Retry-After 3600", status, headers.Get("Retry-After"), body)
	}
}

// TestOneAddressNeverOutlastsTheSharedRereadBucket pins the ordering the whole fan-out leans on,
// as a relation between the two buckets rather than as two numbers.
//
// POST /v1/secrets/{name}/reread takes no credential (routes_table.go: public), and
// ratelimit.Keyed.AllowBothAt charges a refusal to neither bucket, so a caller that never floods
// and merely keeps succeeding at the rate the broker-wide bucket refills holds that bucket at
// zero for ever and is never refused itself. While it is empty no narrowing retag reaches any
// pod before that pod's own five-minute reload, which is the one thing this fan-out exists to
// prevent. So one address's own sustained rate has to be strictly less than the shared bucket's,
// and by enough that holding it down takes more than one address: the rule is that the address
// bucket refills at half the rate of the shared bucket THIS PROCESS MOUNTS.
//
// It has to be the mounted bucket and not a constant, because cmd/broker mounts
// api.GCPRereadOverallLimit only under gcp-secret-manager and api.DefaultRereadOverallLimit
// otherwise: an address limit derived from the Google figure alone makes every AWS deployment
// pay a fourfold tightening for a Secret Manager quota it never touches, and one derived from
// the default figure alone leaves a single address able to hold a Secret Manager fleet's budget
// at zero.
//
// Retry-After is the observable: it carries the refusing bucket's own Every
// (api/limits.go's retryAfterSeconds), so it says which bucket refused and at what rate,
// which nothing else in the answer does. The mounted shared bucket is given a depth no row here
// can reach, so the only refusal this can meet is the address's own.
func TestOneAddressNeverOutlastsTheSharedRereadBucket(t *testing.T) {
	for _, shared := range []time.Duration{time.Second, 4 * time.Second} {
		t.Run(shared.String(), func(t *testing.T) {
			ts := newTestServerWith(t, func(d *api.Deps) {
				d.RereadOverallLimit = &ratelimit.Limit{Every: shared, Burst: 1000}
				d.TrustedProxyHeader = "X-Forwarded-For"
			})
			want := fmt.Sprintf("%d", int(2*shared/time.Second))
			from := map[string]string{"X-Forwarded-For": "203.0.113.7"}
			for sent := 1; sent <= 400; sent++ {
				status, headers, body := postReread(t, ts, "WORKER_TOKEN", from)
				if status == http.StatusOK {
					continue
				}
				if status != http.StatusTooManyRequests || decode[wireError](t, body).Code != "RATE_LIMITED" {
					t.Fatalf("reread %d from one address against a shared bucket of %s = %d %s; want 200 until that address's own bucket refuses",
						sent, shared, status, body)
				}
				if got := headers.Get("Retry-After"); got != want {
					t.Fatalf("one address's own reread bucket refuses with Retry-After %q against a shared bucket refilling every %s; want %q — the address bucket refills at half the shared bucket this process mounts, so that no single unauthenticated address can hold the fleet's reread budget at zero and so that a deployment is not tightened by a bucket it does not mount",
						got, shared, want)
				}
				return
			}
			t.Fatalf("400 rereads from one address and its own bucket never refused; the row cannot read a rate it never meets")
		})
	}
}

// TestRereadIsRateLimitedAcrossEveryAddress pins the broker-wide bound. Every reread takes the
// policy's writer lock, so a flood spread over addresses that are each on their first reread —
// well inside their own limit — is refused all the same, and names the broker-wide bucket's own
// Retry-After.
func TestRereadIsRateLimitedAcrossEveryAddress(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.RereadOverallLimit = &ratelimit.Limit{Every: time.Hour, Burst: 2}
		d.TrustedProxyHeader = "X-Forwarded-For"
	})
	for _, address := range []string{"198.51.100.1", "198.51.100.2"} {
		if status, _, body := postReread(t, ts, "WORKER_TOKEN", map[string]string{"X-Forwarded-For": address}); status != http.StatusOK {
			t.Fatalf("reread from %s: %d %s, want 200 (its own first reread)", address, status, body)
		}
	}
	status, headers, body := postReread(t, ts, "WORKER_TOKEN", map[string]string{"X-Forwarded-For": "198.51.100.3"})
	if status != http.StatusTooManyRequests || headers.Get("Retry-After") != "3600" || decode[wireError](t, body).Code != "RATE_LIMITED" {
		t.Fatalf("third address: %d Retry-After=%q %s, want 429 RATE_LIMITED with Retry-After 3600 past the broker-wide burst", status, headers.Get("Retry-After"), body)
	}
}

// TestOneAddressFloodingItsLimitLeavesOthersTheirRereads pins the order the two buckets are taken
// in: a reread its own address's bucket refuses spends nothing of the broker-wide one, so one
// address flooding past its own limit does not refuse another address's first reread.
func TestOneAddressFloodingItsLimitLeavesOthersTheirRereads(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.RereadLimit = &ratelimit.Limit{Every: time.Hour, Burst: 1}
		d.RereadOverallLimit = &ratelimit.Limit{Every: time.Hour, Burst: 2}
		d.TrustedProxyHeader = "X-Forwarded-For"
	})
	flooder := map[string]string{"X-Forwarded-For": "198.51.100.1"}
	if status, _, body := postReread(t, ts, "WORKER_TOKEN", flooder); status != http.StatusOK {
		t.Fatalf("flooder's first reread: %d %s", status, body)
	}
	for i := range 5 {
		if status, headers, body := postReread(t, ts, "WORKER_TOKEN", flooder); status != http.StatusTooManyRequests || headers.Get("Retry-After") != "3600" {
			t.Fatalf("flooder's reread %d: %d Retry-After=%q %s, want 429 from its own bucket", i+2, status, headers.Get("Retry-After"), body)
		}
	}
	if status, _, body := postReread(t, ts, "WORKER_TOKEN", map[string]string{"X-Forwarded-For": "198.51.100.2"}); status != http.StatusOK {
		t.Fatalf("another address's first reread after the flood: %d %s, want 200", status, body)
	}
}

// recordingNotifier stands in for the Postgres fan-out cmd/broker wires into Deps.NotifyReread
// (internal/broker/reread's Notifier), recording the names it was asked to send. The handler
// calls it on a context of its own rather than the request's, from the server's own goroutine, so
// the names are taken under a mutex; a context already ended when the call arrives is recorded as
// its own entry, so a fan-out wrongly riding the request's context fails the comparison rather
// than passing unnoticed.
type recordingNotifier struct {
	mu    sync.Mutex
	names []string
}

func (n *recordingNotifier) notify(ctx context.Context, name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		name = name + " (on an ended context: " + err.Error() + ")"
	}
	n.names = append(n.names, name)
}

func (n *recordingNotifier) recorded() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.names)
}

// TestRereadNotifiesEveryOtherPodOnEvery200 pins the fan-out's one call site and the decision that
// it fires on every 200: a name this broker now serves and a name it does not are both changes a
// peer must learn of, and the handler cannot tell "unchanged" from "changed" without a verdict
// RefreshOne does not report. A name no secret can carry rereads nothing, so it notifies nobody.
func TestRereadNotifiesEveryOtherPodOnEvery200(t *testing.T) {
	notifier := &recordingNotifier{}
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.NotifyReread = notifier.notify
		d.RereadOverallLimit = &rereadsFreely
	})
	ts.Secrets.Put(policytest.Secret("NEW_KEY", testApprover, policy.TierAgent, "v1"))

	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/NEW_KEY/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("reread of a name the broker now serves: %d %s, want 200", status, body)
	}
	// MISSING_KEY is a well-formed name no secret under the prefix carries: still a 200, with
	// served=false, and still a notification, since a peer that does serve it must drop it.
	status, body := ts.req(t, http.MethodPost, "/v1/secrets/MISSING_KEY/reread", nil, nil)
	if r := decode[wireReread](t, body); status != http.StatusOK || r.Served {
		t.Fatalf("reread of a name the broker does not serve: %d %+v, want 200 served=false", status, r)
	}
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/not_a_name/reread", nil, nil); status != http.StatusBadRequest || decode[wireError](t, body).Code != "SECRET_NAME_INVALID" {
		t.Fatalf("malformed name: %d %s, want 400 SECRET_NAME_INVALID", status, body)
	}
	if got := notifier.recorded(); !slices.Equal(got, []string{"NEW_KEY", "MISSING_KEY"}) {
		t.Fatalf("notified %q, want exactly one notification per 200, in order, and none for the refused name", got)
	}
}

// TestARereadTheLimiterRefusesNotifiesNobody pins that the 429 a drained bucket writes before any
// work sends nothing either: no reread happened on this pod, so there is nothing for a peer to
// copy, and a flood the limiter exists to stop must not reach the other pods through the fan-out.
func TestARereadTheLimiterRefusesNotifiesNobody(t *testing.T) {
	notifier := &recordingNotifier{}
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.NotifyReread = notifier.notify
		d.RereadOverallLimit = &ratelimit.Limit{Every: time.Hour, Burst: 1}
	})
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/WORKER_TOKEN/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("first reread: %d %s, want 200", status, body)
	}
	for i := range 3 {
		if status, _, body := postReread(t, ts, "WORKER_TOKEN", nil); status != http.StatusTooManyRequests {
			t.Fatalf("reread %d past the drained bucket: %d %s, want 429", i+2, status, body)
		}
	}
	if got := notifier.recorded(); !slices.Equal(got, []string{"WORKER_TOKEN"}) {
		t.Fatalf("notified %q, want only the one reread that happened", got)
	}
}

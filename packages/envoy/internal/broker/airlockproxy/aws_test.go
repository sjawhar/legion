// packages/envoy/internal/broker/airlockproxy/aws_test.go
//
// The aws kind, end to end on the rig every other row runs on: a command signs with the
// placeholder key pair the broker handed it, and what reaches the stand-in service is the same
// call signed with the real key and carrying no placeholder anywhere. The stand-in verifies the
// signature from the Signature Version 4 specification itself - the canonical request, the
// string to sign, the derived key, the HMAC - rather than through the signer the proxy signs
// with, so a signature this row accepts is one AWS would accept and not one the same code
// agreed with itself about.
package airlockproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// The key pair behind the airlock: what the broker holds, and what the command holds in its
// place. The real key id is in the shape AWS mints them, since the stand-in reads it out of the
// Credential of each call.
const (
	awsName          = "AWS_KEY"
	awsRealKeyID     = "AKIAREALKEYID0000000"
	awsRealSecret    = "the-real-aws-secret-access-key"
	awsRealSession   = "the-real-aws-session-token"
	awsRegion        = "us-west-2"
	awsOtherKeyID    = "AKIASOMEBODYELSES000"
	awsUnsignedBody  = "UNSIGNED-PAYLOAD"
	awsStandInAnswer = "the stand-in service verified the signature"
)

var (
	awsKeyPlaceholder     = requests.PlaceholderPrefix + "awsKeyIdAwsKeyIdAwsKeyIdAwsKey01"
	awsSecretPlaceholder  = requests.PlaceholderPrefix + "awsSecretAwsSecretAwsSecretAws01"
	awsSessionPlaceholder = requests.PlaceholderPrefix + "awsSessionAwsSessionAwsSession01"
)

// awsRules is the rules file of a key pair: one entry of kind aws, bound to the one host, with
// no allow list, since what bounds an AWS call is the key the broker signs it with.
func awsRules() airlock.Rules {
	return airlock.Rules{Secrets: []airlock.SecretRule{{Name: awsName, Hosts: []string{boundHost}, Kind: "aws"}}}
}

// newAWSRig is the rig with the key pair as the grant's one name. session says whether the pair
// has a session token, which is the slot a stored pair fills only when its value holds one.
func newAWSRig(t *testing.T, session bool) *rig {
	t.Helper()
	r := newRig(t, Options{Rules: awsRules()})
	awsGrant(r, session)
	return r
}

// awsGrant puts the key pair in a rig's grant: the three placeholders the command holds and
// the real key behind them. It is apart from newAWSRig for the one row that builds its rig
// with an option of its own.
func awsGrant(r *rig, session bool) {
	slots := map[string]string{
		requests.SlotAWSAccessKeyID:     awsKeyPlaceholder,
		requests.SlotAWSSecretAccessKey: awsSecretPlaceholder,
	}
	values := map[string]string{
		requests.SlotAWSAccessKeyID:     awsRealKeyID,
		requests.SlotAWSSecretAccessKey: awsRealSecret,
	}
	if session {
		slots[requests.SlotAWSSessionToken] = awsSessionPlaceholder
		values[requests.SlotAWSSessionToken] = awsRealSession
	}
	r.grants.connections[awsKeyPlaceholder] = requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{awsName: {Slots: slots}},
	}
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{awsName: {Slots: values}})
}

// verifyingStandIn makes the stand-in service verify every call it is sent with the real key and
// answer 403 when the signature is not the real key's or a placeholder reached it. The check
// itself is the stand-in package's, built from the Signature Version 4 specification rather
// than from the signer the proxy signs with, and the same one the local stack's AWS stand-in
// runs.
func verifyingStandIn(r *rig, session bool) {
	sessionToken := ""
	if session {
		sessionToken = awsRealSession
	}
	r.answerWith(airlocktest.NewAWSStand(airlocktest.AWSOptions{
		KeyID:        awsRealKeyID,
		SecretKey:    awsRealSecret,
		SessionToken: sessionToken,
		Body:         awsStandInAnswer,
	}).Answer)
}

// awsRequest is one request as a command's own AWS client makes it: signed with the
// placeholder key pair, through the same signer, since that is what the measured clients use.
func awsRequest(t *testing.T, method, target, body, service string, session bool, headers ...string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	// What the command signs the body with: its own hash header when it sends one - a hex hash
	// or UNSIGNED-PAYLOAD - and otherwise the hash of the body, which the proxy then has to
	// read the body to make for itself.
	payload := request.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		payload = hashOf(body)
	}
	credentials := aws.Credentials{AccessKeyID: awsKeyPlaceholder, SecretAccessKey: awsSecretPlaceholder}
	if session {
		credentials.SessionToken = awsSessionPlaceholder
	}
	if err := v4.NewSigner().SignHTTP(t.Context(), credentials, request, payload, service, awsRegion, time.Now().UTC()); err != nil {
		t.Fatalf("sign the request with the placeholder key: %v", err)
	}
	return request
}

// awsCall is one such call sent through the proxy, and what came back.
func awsCall(t *testing.T, r *rig, client *http.Client, method, target, body, service string, session bool, headers ...string) (*http.Response, string) {
	t.Helper()
	return sendAWS(t, client, awsRequest(t, method, target, body, service, session, headers...))
}

// hashOf is the hex SHA-256 a client puts in X-Amz-Content-Sha256 when it hashes its own body.
func hashOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// TestAnAWSCallIsSignedAgainWithTheRealKey: the command signs with the key pair the airlock
// handed it, which signs nothing anywhere, and the service receives the same call signed with
// the real key - a GET with a query, an upload whose hash the proxy reads the body to make, and
// an upload that carries its own hash header and so streams through.
func TestAnAWSCallIsSignedAgainWithTheRealKey(t *testing.T) {
	for _, c := range []struct {
		name, method, target, body, service string
		headers                             []string
		// hashed says the proxy had to read the body to hash it, which is what a call that
		// carries no X-Amz-Content-Sha256 costs.
		hashed bool
	}{{
		name:    "a GET with a query",
		method:  http.MethodGet,
		target:  "https://" + boundHost + "/v1/things?Action=GetCallerIdentity&Version=2011-06-15",
		service: "sts",
		hashed:  true,
	}, {
		name:    "a PUT with a body and no hash header",
		method:  http.MethodPut,
		target:  "https://" + boundHost + "/v1/upload",
		body:    "the artifact this command is uploading",
		service: "s3",
		hashed:  true,
	}, {
		name:    "a PUT whose hash header carries the hash",
		method:  http.MethodPut,
		target:  "https://" + boundHost + "/v1/upload",
		body:    "the artifact this command is uploading",
		service: "s3",
		headers: []string{"X-Amz-Content-Sha256", hashOf("the artifact this command is uploading")},
	}, {
		name:    "a PUT whose hash header says the body is unsigned",
		method:  http.MethodPut,
		target:  "https://" + boundHost + "/v1/upload",
		body:    "the artifact this command is uploading",
		service: "s3",
		headers: []string{"X-Amz-Content-Sha256", awsUnsignedBody},
	}} {
		t.Run(c.name, func(t *testing.T) {
			for _, session := range []bool{false, true} {
				t.Run(fmt.Sprintf("session token=%v", session), func(t *testing.T) {
					r := newAWSRig(t, session)
					verifyingStandIn(r, session)
					client := r.client(awsKeyPlaceholder)
					answer, body := awsCall(t, r, client, c.method, c.target, c.body, c.service, session, c.headers...)
					if answer.StatusCode != http.StatusOK || body != awsStandInAnswer {
						t.Fatalf("the call answered %d %q; want 200 %q", answer.StatusCode, body, awsStandInAnswer)
					}
					seen := r.requests()
					if len(seen) != 1 {
						t.Fatalf("the service saw %d calls; want one", len(seen))
					}
					if seen[0].Body != c.body {
						t.Errorf("the service saw the body %q; want %q", seen[0].Body, c.body)
					}
					if c.hashed && seen[0].Header.Get("X-Amz-Content-Sha256") != "" {
						t.Errorf("the proxy added X-Amz-Content-Sha256 %q; want the hash in the signature alone", seen[0].Header.Get("X-Amz-Content-Sha256"))
					}
					if line := r.logs.callsAtLeast(t, 1); len(line) != 1 || line[0].fields["secret"] != awsName || line[0].fields["allowed"] != true {
						t.Errorf("the call logged %v; want one allowed line for %s", line, awsName)
					}
				})
			}
		})
	}
}

// TestAnAWSCallThatIsNotThisGrantsIsRefused: the signature the command made is the proof that
// the call is the one the broker released this key for, so a Credential that names another key
// id and a session token that is not this grant's placeholder are refused before anything is
// signed and before anything is sent.
func TestAnAWSCallThatIsNotThisGrantsIsRefused(t *testing.T) {
	t.Run("another key id", func(t *testing.T) {
		r := newAWSRig(t, false)
		client := r.client(awsKeyPlaceholder)
		request, err := http.NewRequest(http.MethodGet, "https://"+boundHost+"/v1/things", nil)
		if err != nil {
			t.Fatal(err)
		}
		credentials := aws.Credentials{AccessKeyID: awsOtherKeyID, SecretAccessKey: "somebody-elses-secret"}
		if err := v4.NewSigner().SignHTTP(t.Context(), credentials, request, hashOf(""), "sts", awsRegion, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		answer, body := sendAWS(t, client, request)
		if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "another key id") {
			t.Fatalf("the call answered %d %q; want 403 naming the key id it refused", answer.StatusCode, body)
		}
		if seen := r.requests(); len(seen) != 0 {
			t.Fatalf("the service was sent %d calls; want none", len(seen))
		}
	})

	t.Run("another session token", func(t *testing.T) {
		r := newAWSRig(t, true)
		client := r.client(awsKeyPlaceholder)
		request, err := http.NewRequest(http.MethodGet, "https://"+boundHost+"/v1/things", nil)
		if err != nil {
			t.Fatal(err)
		}
		credentials := aws.Credentials{AccessKeyID: awsKeyPlaceholder, SecretAccessKey: awsSecretPlaceholder, SessionToken: awsSessionPlaceholder}
		if err := v4.NewSigner().SignHTTP(t.Context(), credentials, request, hashOf(""), "sts", awsRegion, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		// The key id is this grant's and the session token is not: a command that signed with
		// the pair it was given and then put somebody else's token beside it.
		request.Header.Set("X-Amz-Security-Token", "a session token of somebody else")
		answer, body := sendAWS(t, client, request)
		if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "X-Amz-Security-Token is not this grant's placeholder") {
			t.Fatalf("the call answered %d %q; want 403 naming the session token it refused", answer.StatusCode, body)
		}
		if seen := r.requests(); len(seen) != 0 {
			t.Fatalf("the service was sent %d calls; want none", len(seen))
		}
	})
}

// TestTheThreeAWSShapesTheAirlockRefuses: each of the three the broker cannot sign again - a
// chunk-signed upload, a presigned URL and Signature Version 4a - is refused with an error that
// names it, so a command reads what it has to change rather than a bare 403.
func TestTheThreeAWSShapesTheAirlockRefuses(t *testing.T) {
	for _, c := range []struct {
		name, want string
		// build is the request as the command sends it, signed or not.
		build func(t *testing.T) *http.Request
	}{{
		name: "a chunk-signed upload",
		want: "chunk-signed upload (X-Amz-Content-Sha256: " + streamingPayload + ")",
		build: func(t *testing.T) *http.Request {
			t.Helper()
			request, err := http.NewRequest(http.MethodPut, "https://"+boundHost+"/v1/upload", strings.NewReader("a chunked body"))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Amz-Content-Sha256", streamingPayload)
			credentials := aws.Credentials{AccessKeyID: awsKeyPlaceholder, SecretAccessKey: awsSecretPlaceholder}
			if err := v4.NewSigner().SignHTTP(t.Context(), credentials, request, streamingPayload, "s3", awsRegion, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			return request
		},
	}, {
		name: "a chunk-signed upload with a trailer",
		want: "chunk-signed upload (X-Amz-Content-Sha256: " + streamingPayloadTrailer + ")",
		build: func(t *testing.T) *http.Request {
			t.Helper()
			request, err := http.NewRequest(http.MethodPut, "https://"+boundHost+"/v1/upload", strings.NewReader("a chunked body"))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Amz-Content-Sha256", streamingPayloadTrailer)
			credentials := aws.Credentials{AccessKeyID: awsKeyPlaceholder, SecretAccessKey: awsSecretPlaceholder}
			if err := v4.NewSigner().SignHTTP(t.Context(), credentials, request, streamingPayloadTrailer, "s3", awsRegion, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			return request
		},
	}, {
		name: "a presigned URL",
		want: "presigned URL",
		build: func(t *testing.T) *http.Request {
			t.Helper()
			query := url.Values{
				"X-Amz-Algorithm":     {sigV4Scheme},
				"X-Amz-Credential":    {awsKeyPlaceholder + "/20260101/" + awsRegion + "/s3/aws4_request"},
				"X-Amz-Date":          {"20260101T000000Z"},
				"X-Amz-SignedHeaders": {"host"},
				"X-Amz-Signature":     {strings.Repeat("0", 64)},
			}
			request, err := http.NewRequest(http.MethodGet, "https://"+boundHost+"/v1/thing?"+query.Encode(), nil)
			if err != nil {
				t.Fatal(err)
			}
			return request
		},
	}, {
		name: "Signature Version 4a",
		want: "Signature Version 4a (" + sigV4aScheme + ")",
		build: func(t *testing.T) *http.Request {
			t.Helper()
			request, err := http.NewRequest(http.MethodGet, "https://"+boundHost+"/v1/things", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", sigV4aScheme+" Credential="+awsKeyPlaceholder+"/20260101/"+awsRegion+"/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature="+strings.Repeat("0", 96))
			request.Header.Set("X-Amz-Region-Set", "*")
			request.Header.Set("X-Amz-Date", "20260101T000000Z")
			return request
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			r := newAWSRig(t, false)
			client := r.client(awsKeyPlaceholder)
			answer, body := sendAWS(t, client, c.build(t))
			if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, c.want) {
				t.Fatalf("the call answered %d %q; want 403 naming %q", answer.StatusCode, body, c.want)
			}
			if seen := r.requests(); len(seen) != 0 {
				t.Fatalf("the service was sent %d calls; want none", len(seen))
			}
			if line := r.logs.callsAtLeast(t, 1); len(line) != 1 || line[0].fields["allowed"] != false {
				t.Errorf("the refusal logged %v; want one refused line", line)
			}
		})
	}
}

// TestTheAirlockHoldsOnlyItsBudgetOfBodiesToHash: a call with no hash header has its whole body
// held in memory from the read until the round trip ends, and one container serves every
// command's calls, so what may be held at once is a budget of the whole proxy's. A call whose
// bytes do not fit waits for room rather than being held beside the one that has it, it goes
// through once that one ends - were the budget not given back, it never would - and a call
// that carries its own X-Amz-Content-Sha256 holds no body and takes nothing from the budget.
func TestTheAirlockHoldsOnlyItsBudgetOfBodiesToHash(t *testing.T) {
	const body = "a body of thirty-two bytes, this"
	// A budget of exactly one such body, so the second call of that size has to wait for it.
	r := newRig(t, Options{Rules: awsRules(), HashBudget: int64(len(body))})
	awsGrant(r, false)

	// The stand-in holds every call it is sent until the row lets them all answer, so a call
	// that reached it is a call still in flight, holding whatever of the budget it took.
	arrived := make(chan string, 3)
	answer := make(chan struct{})
	r.answerWith(func(w http.ResponseWriter, _ *http.Request, seen airlocktest.Request) bool {
		arrived <- seen.Path
		<-answer
		_, _ = w.Write([]byte(awsStandInAnswer))
		return true
	})
	// Two of the three calls are in flight at once, so each is sent from a goroutine of its
	// own and reports its status here.
	client := r.client(awsKeyPlaceholder)
	status := make(chan int, 3)
	send := func(request *http.Request) {
		go func() {
			sent, err := client.Do(request)
			if err != nil {
				t.Errorf("PUT %s through the proxy: %v", request.URL.Path, err)
				status <- 0
				return
			}
			defer sent.Body.Close()
			io.Copy(io.Discard, sent.Body)
			status <- sent.StatusCode
		}()
	}
	put := func(path string, headers ...string) *http.Request {
		return awsRequest(t, http.MethodPut, "https://"+boundHost+path, body, "s3", false, headers...)
	}

	send(put("/first"))
	if path := <-arrived; path != "/first" {
		t.Fatalf("the service was sent %q first; want the call that takes the whole budget", path)
	}
	send(put("/waiting"))
	send(put("/hashed", "X-Amz-Content-Sha256", hashOf(body)))
	// The call carrying its own hash reaches the service although the whole budget is held,
	// which is what proves it takes none of it, and the one that has to wait has not reached
	// the service by the time that one has.
	if path := <-arrived; path != "/hashed" {
		t.Fatalf("the service was sent %q next; want the call that carries its own hash, which holds no body", path)
	}
	select {
	case path := <-arrived:
		t.Fatalf("the service was sent %q while the whole budget was held; want it to wait for room", path)
	case <-time.After(100 * time.Millisecond):
	}

	// Both calls in flight answer, which gives the first one's bytes back, and the waiting
	// call goes through on them.
	close(answer)
	if path := <-arrived; path != "/waiting" {
		t.Fatalf("the service was sent %q once the budget was free; want the call that was waiting", path)
	}
	for range 3 {
		if got := <-status; got != http.StatusOK {
			t.Fatalf("a call answered %d; want 200 from each of the three", got)
		}
	}
}

// sendAWS sends a request a row built itself through the proxy and reads the answer.
func sendAWS(t *testing.T, client *http.Client, request *http.Request) (*http.Response, string) {
	t.Helper()
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s through the proxy: %v", request.Method, request.URL, err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return answer, string(body)
}

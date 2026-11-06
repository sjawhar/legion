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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/sjawhar/envoy/internal/broker/airlock"
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
	return r
}

// verifyingStandIn makes the stand-in service verify every call it is sent with the real key and
// answer 403 when the signature is not the real key's or a placeholder reached it.
func verifyingStandIn(r *rig, session bool) {
	sessionToken := ""
	if session {
		sessionToken = awsRealSession
	}
	r.answerWith(func(w http.ResponseWriter, _ *http.Request) {
		seen := r.requests()
		if err := verifySigV4(seen[len(seen)-1], awsRealKeyID, awsRealSecret, sessionToken); err != nil {
			http.Error(w, "the stand-in service refused this call: "+err.Error(), http.StatusForbidden)
			return
		}
		fmt.Fprint(w, awsStandInAnswer)
	})
}

// awsCall is one call a command makes: the request signed with the placeholder key pair, as the
// command's own AWS client signs it - through the same signer, since that is what the measured
// clients use - and sent through the proxy.
func awsCall(t *testing.T, r *rig, client *http.Client, method, target, body, service string, session bool, headers ...string) (*http.Response, string) {
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
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s through the proxy: %v", method, target, err)
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return answer, string(read)
}

// verifySigV4 is the stand-in service's own check of one call: the Signature Version 4
// specification, carried out here rather than through an AWS library, and the refusal of any
// placeholder anywhere in the request.
func verifySigV4(seen seenRequest, keyID, secretKey, sessionToken string) error {
	if dump := fmt.Sprint(seen.method, seen.path, seen.query, seen.host, seen.header, seen.body); strings.Contains(dump, requests.PlaceholderPrefix) {
		return fmt.Errorf("this call carries a placeholder: %s", dump)
	}
	authorization := seen.header.Get("Authorization")
	rest, isV4 := strings.CutPrefix(authorization, sigV4Scheme+" ")
	if !isV4 {
		return fmt.Errorf("the Authorization is no %s credential: %q", sigV4Scheme, authorization)
	}
	var credential, signedHeaders, signature string
	for _, part := range strings.Split(rest, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch name {
		case "Credential":
			credential = value
		case "SignedHeaders":
			signedHeaders = value
		case "Signature":
			signature = value
		}
	}
	scope := strings.Split(credential, "/")
	if len(scope) != 5 {
		return fmt.Errorf("the Credential is no KEY/date/region/service/aws4_request scope: %q", credential)
	}
	if scope[0] != keyID {
		return fmt.Errorf("the Credential names the key id %q; want the real key id %q", scope[0], keyID)
	}
	if got := seen.header.Get("X-Amz-Security-Token"); got != sessionToken {
		return fmt.Errorf("X-Amz-Security-Token = %q; want %q", got, sessionToken)
	}
	date, region, service := scope[1], scope[2], scope[3]
	payload := seen.header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		sum := sha256.Sum256([]byte(seen.body))
		payload = hex.EncodeToString(sum[:])
	}
	var canonicalHeaders strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		var value string
		switch name {
		case "host":
			value = seen.host
		case "content-length":
			if value = seen.header.Get("Content-Length"); value == "" {
				value = strconv.Itoa(len(seen.body))
			}
		default:
			value = strings.Join(seen.header.Values(http.CanonicalHeaderKey(name)), ",")
		}
		canonicalHeaders.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}
	query, err := url.ParseQuery(seen.query)
	if err != nil {
		return fmt.Errorf("the query %q: %w", seen.query, err)
	}
	for name := range query {
		slices.Sort(query[name])
	}
	canonical := strings.Join([]string{
		seen.method,
		seen.path,
		strings.ReplaceAll(query.Encode(), "+", "%20"),
		canonicalHeaders.String(),
		signedHeaders,
		payload,
	}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	credentialScope := strings.Join([]string{date, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{sigV4Scheme, seen.header.Get("X-Amz-Date"), credentialScope, hex.EncodeToString(sum[:])}, "\n")
	signing := hmacOf(hmacOf(hmacOf(hmacOf([]byte("AWS4"+secretKey), date), region), service), "aws4_request")
	if want := hex.EncodeToString(hmacOf(signing, stringToSign)); want != signature {
		return fmt.Errorf("the signature is %q; want %q, this call signed with the real key. canonical request:\n%s", signature, want, canonical)
	}
	return nil
}

func hmacOf(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
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
					if seen[0].body != c.body {
						t.Errorf("the service saw the body %q; want %q", seen[0].body, c.body)
					}
					if c.hashed && seen[0].header.Get("X-Amz-Content-Sha256") != "" {
						t.Errorf("the proxy added X-Amz-Content-Sha256 %q; want the hash in the signature alone", seen[0].header.Get("X-Amz-Content-Sha256"))
					}
					if line := r.logs.calls(); len(line) != 1 || line[0].fields["secret"] != awsName || line[0].fields["allowed"] != true {
						t.Errorf("the call logged %v; want one allowed line for %s", line, awsName)
					}
				})
			}
		})
	}
}

// hashOf is the hex SHA-256 a client puts in X-Amz-Content-Sha256 when it hashes its own body.
func hashOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
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
			if line := r.logs.calls(); len(line) != 1 || line[0].fields["allowed"] != false {
				t.Errorf("the refusal logged %v; want one refused line", line)
			}
		})
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

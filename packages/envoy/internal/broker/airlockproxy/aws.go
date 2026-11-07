// packages/envoy/internal/broker/airlockproxy/aws.go
// The aws kind: a stored AWS key pair, whose value is no string a header carries but a key the
// call itself is signed with. The command signs with the grant's placeholder key id and
// placeholder secret key; this file takes that signature off, puts the real session token in
// place of the placeholder one, and signs the request again with the real key - AWS Signature
// Version 4, through the official AWS SDK for Go signer - so what leaves the broker is a
// signature the service accepts and what the command holds is a key that signs nothing
// anywhere.
//
// This path is outside the Lean model, which covers the header swap alone: the model's Rewrite
// swaps a placeholder for a value in the headers an entry names, and no signature is a
// substitution of that shape. What is inside the model is the decision that lets this path run.
// airlock.Decide runs first, on the request as it came, so the bound host, the TLS name, the
// Host header, the entry's allow list and the refusal of a request that already carries the
// real value hold here exactly as they hold for a header swap; this file adds the signature and
// nothing else.
package airlockproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/sjawhar/envoy/internal/broker/requests"
)

// The two signature schemes an Authorization header of an AWS call opens with: Signature
// Version 4, which this file signs again, and Signature Version 4a, which it refuses. 4a signs
// with an ECDSA key derived from the secret key and covers several regions at once, so signing
// it again is neither the same algorithm nor the same scope as the pair the broker holds.
const (
	sigV4Scheme  = "AWS4-HMAC-SHA256"
	sigV4aScheme = "AWS4-ECDSA-P256-SHA256"
)

// The two X-Amz-Content-Sha256 values of a chunk-signed upload, which this file refuses: the
// body is aws-chunked and every chunk carries a signature chained from the seed signature in
// the header, so a new header signature would leave every chunk signed by the placeholder key.
const (
	streamingPayload        = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingPayloadTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
)

// maxHashedBody is how much of a body the proxy reads to hash when no header carries the
// payload hash. 64 MiB: the calls that send no hash header are the small ones - STS, and the
// JSON and query APIs - while S3's own clients, which send the large bodies, carry the hash,
// UNSIGNED-PAYLOAD or STREAMING-UNSIGNED-PAYLOAD-TRAILER in a header and stream through
// untouched. A larger body with no hash header is refused rather than half-read, since a
// signature over a truncated body is a signature over something other than what is sent.
const maxHashedBody = 64 << 20

// hashedBodyBudget is how many such bytes the whole proxy holds at once. One body is held from
// the read until the upstream round trip ends, since it is what r.Body serves upstream, and
// nothing else bounds how many requests with no hash header run at once: sixteen of the
// largest of them is the whole 1 GiB the container has, and the pod is killed. 256 MiB is four
// of that largest size, which is as much of the container as this proxy will ever hold in
// bodies; a request past it waits for room rather than being refused, because a call that
// waits a moment is one the command still makes.
const hashedBodyBudget = 256 << 20

// connectionHeaders belong to the connection the command opened to this proxy and not to the
// service: httputil.ReverseProxy drops them from what it sends, so a signature that covered
// them would cover headers the service never sees.
var connectionHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
	"Te", "Trailer", "Upgrade",
}

// signAWS answers the headers and the query the request goes upstream with, signed with the
// grant's real key, or the reason the airlock will not sign it. A request with no payload-hash
// header has its body read to hash it, and r.Body is replaced by what was read.
//
// On that success it answers a release of the byte budget a read body is held against, which
// the caller defers: the bytes are held until the upstream round trip ends, not until the hash
// is made, because they are what r.Body serves upstream. A refusal gives whatever it took back
// here and answers no release, so a refusal added after the acquire below cannot leave the
// budget held for the life of the pod.
//
// host is the Host the request leaves with, which is the host the signature covers; its
// placeholders are the grant's three slots and values the real key behind them.
func (s *Server) signAWS(ctx context.Context, r *http.Request, host string, placeholders, values map[string]string) (http.Header, string, func(), string) {
	keyID, secretKey := values[requests.SlotAWSAccessKeyID], values[requests.SlotAWSSecretAccessKey]
	if keyID == "" || secretKey == "" {
		return nil, "", nil, "the broker released no AWS key id and secret key for this name, so there is no key to sign this request with"
	}
	authorization := r.Header.Get("Authorization")
	query := r.URL.Query()
	switch {
	case query.Has("X-Amz-Signature") || query.Has("X-Amz-Credential") || authorization == "":
		return nil, "", nil, "refusing a presigned URL: a presigned request carries its credential and its signature in the query, and the airlock signs a request as it leaves rather than minting a URL somebody else can replay; sign the request with an Authorization header instead"
	case strings.HasPrefix(authorization, sigV4aScheme):
		return nil, "", nil, "refusing Signature Version 4a (" + sigV4aScheme + "): the airlock signs again with Signature Version 4 alone"
	}
	rest, isV4 := strings.CutPrefix(authorization, sigV4Scheme+" ")
	if !isV4 {
		return nil, "", nil, "refusing an Authorization that is no " + sigV4Scheme + " credential"
	}
	_, credential, named := strings.Cut(rest, "Credential=")
	if !named {
		return nil, "", nil, "refusing a " + sigV4Scheme + " Authorization that names no Credential"
	}
	credential, _, _ = strings.Cut(credential, ",")
	// KEY/date/region/service/aws4_request: the key id the command signed with, and the scope
	// it signed in, which the new signature is made in too - the region and the service are
	// the command's own choice of endpoint and the broker reads them rather than guessing.
	scope := strings.Split(credential, "/")
	if len(scope) != 5 {
		return nil, "", nil, "refusing a Credential that is no KEY/date/region/service/aws4_request scope"
	}
	if scope[0] != placeholders[requests.SlotAWSAccessKeyID] {
		return nil, "", nil, "refusing a request signed with another key id: the Credential of this request does not name this grant's placeholder key id"
	}
	region, service := scope[2], scope[3]
	sessionPlaceholder, hasSession := placeholders[requests.SlotAWSSessionToken]
	if hasSession {
		if r.Header.Get("X-Amz-Security-Token") != sessionPlaceholder {
			return nil, "", nil, "refusing a request whose X-Amz-Security-Token is not this grant's placeholder: this key pair has a session token, and the call must carry the placeholder the command was given"
		}
		if values[requests.SlotAWSSessionToken] == "" {
			return nil, "", nil, "the broker released no AWS session token for this name, whose placeholder this request carries"
		}
	}

	payload := r.Header.Get("X-Amz-Content-Sha256")
	// release gives back whatever of the budget this request took, and is the answer of the
	// success path alone. A request that carries its own hash, and one refused before the
	// read, hold no body and so take none of it.
	release := func() {}
	switch payload {
	case streamingPayload, streamingPayloadTrailer:
		return nil, "", nil, "refusing a chunk-signed upload (X-Amz-Content-Sha256: " + payload + "): every chunk of such a body carries a signature chained from the one in the header, so signing again would mean rewriting the whole body"
	case "":
		// What a request takes from the budget is what it may hold: its own length when it
		// says one this path would read, and the whole of maxHashedBody when it says none or
		// says more than that. The calls that carry no hash header are the small ones - STS,
		// the JSON and query APIs - so each takes its own few kilobytes and hundreds of them
		// are under the budget at once, where one weight for all of them would hold four.
		weight := int64(maxHashedBody)
		if 0 <= r.ContentLength && r.ContentLength <= maxHashedBody {
			weight = r.ContentLength
		}
		if err := s.hashBudget.Acquire(ctx, weight); err != nil {
			return nil, "", nil, "this request waited for room in the airlock's budget for the bodies it hashes and ended before there was room: " + err.Error() + "; a call that carries its own X-Amz-Content-Sha256 needs no room in that budget at all"
		}
		release = func() { s.hashBudget.Release(weight) }
		body, err := io.ReadAll(io.LimitReader(r.Body, maxHashedBody+1))
		if err != nil {
			release()
			return nil, "", nil, "reading this request's body to hash it: " + err.Error()
		}
		if len(body) > maxHashedBody {
			release()
			return nil, "", nil, fmt.Sprintf("refusing a body of more than %d bytes that carries no X-Amz-Content-Sha256: the airlock reads a body only to hash it, and an upload this large has to carry its own hash header", maxHashedBody)
		}
		sum := sha256.Sum256(body)
		payload = hex.EncodeToString(sum[:])
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	// The signature has to cover exactly the headers the service receives, so what is signed is
	// a request built from what will be sent: the command's own headers, without the ones this
	// connection owns and without the two the signer writes itself.
	header := r.Header.Clone()
	for _, name := range connectionHeaders {
		header.Del(name)
	}
	// And every header the command's own Connection header names, which net/http drops with
	// it for the same reason.
	for _, value := range r.Header.Values("Connection") {
		for _, named := range strings.Split(value, ",") {
			if named = strings.TrimSpace(named); named != "" {
				header.Del(named)
			}
		}
	}
	header.Del("Authorization")
	header.Del("X-Amz-Date")
	// Whatever the command put there, the identity of the call that leaves is this grant's key
	// pair: the signer writes the real session token back when the pair has one.
	header.Del("X-Amz-Security-Token")
	signed := &http.Request{
		Method:        r.Method,
		URL:           &url.URL{Scheme: "https", Host: host, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery},
		Host:          host,
		Header:        header,
		ContentLength: r.ContentLength,
	}
	credentials := aws.Credentials{AccessKeyID: keyID, SecretAccessKey: secretKey, SessionToken: values[requests.SlotAWSSessionToken]}
	signer := v4.NewSigner(func(o *v4.SignerOptions) {
		// S3 canonicalizes the path as the client wrote it, where every other service escapes
		// it a second time. It is the one difference between S3's canonical request and the
		// rest, and the signer carries it as this switch.
		o.DisableURIPathEscaping = service == "s3"
	})
	if err := signer.SignHTTP(ctx, credentials, signed, payload, service, region, s.now().UTC()); err != nil {
		release()
		return nil, "", nil, "signing this request again with the real key: " + err.Error()
	}
	// The signer writes the canonical query - sorted, and each value encoded one way - back
	// into the request it signed, so that is the query this request leaves with: the signature
	// covers it and not the order the command happened to write its parameters in.
	return header, signed.URL.RawQuery, release, ""
}

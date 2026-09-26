// Package githubapptest is an httptest stand-in for the GitHub App webhook delivery API: the
// delivery log GitHub keeps for an App's webhook, its paged listing, and its redelivery request.
package githubapptest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Key returns a fresh RSA key and its PEM, the credential an App client under test signs with.
func Key(t testing.TB) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate App key: %v", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// Delivery is one attempt in the fake's delivery log, in the shape GitHub's list endpoint serves.
type Delivery struct {
	ID             int64     `json:"id"`
	GUID           string    `json:"guid"`
	DeliveredAt    time.Time `json:"delivered_at"`
	Redelivery     bool      `json:"redelivery"`
	Duration       float64   `json:"duration"`
	Status         string    `json:"status"`
	StatusCode     int       `json:"status_code"`
	Event          string    `json:"event"`
	Action         *string   `json:"action"`
	InstallationID *int64    `json:"installation_id"`
	RepositoryID   *int64    `json:"repository_id"`
}

// Attempt is what a redelivery re-sends: the original request's event, delivery GUID and body.
type Attempt struct {
	GUID    string
	Event   string
	Payload []byte
}

// Webhook serves the App webhook delivery API over an httptest server. It checks every call
// carries an App JWT signed by the key it was given, for the client id it was given.
type Webhook struct {
	t         testing.TB
	server    *httptest.Server
	publicKey *rsa.PublicKey
	clientID  string

	mu        sync.Mutex
	log       []Delivery // in the order GitHub recorded them
	payloads  map[int64]Attempt
	nextID    int64
	redeliver func(Attempt) int
	refusals  map[int64]int
	answers   map[int64]int    // a redelivery request carried out, then answered with this status
	limits    map[string]limit // by method: the listing is GET, a redelivery request POST
	requests  []int64
	listings  int
	now       func() time.Time
}

// NewWebhook starts the fake. redeliver answers a redelivery request: it re-sends the attempt to
// the receiver and returns the HTTP status the receiver answered, which the fake records as the
// redelivery's outcome under the original GUID. A nil redeliver answers 200.
func NewWebhook(t testing.TB, publicKey *rsa.PublicKey, clientID string, redeliver func(Attempt) int) *Webhook {
	t.Helper()
	w := &Webhook{
		t:         t,
		publicKey: publicKey,
		clientID:  clientID,
		payloads:  map[int64]Attempt{},
		nextID:    3_844_000_000_000_000_000,
		redeliver: redeliver,
		refusals:  map[int64]int{},
		answers:   map[int64]int{},
		limits:    map[string]limit{},
		now:       time.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/hook/deliveries", w.list)
	mux.HandleFunc("POST /app/hook/deliveries/{id}/attempts", w.attempt)
	w.server = httptest.NewServer(mux)
	t.Cleanup(w.server.Close)
	return w
}

// URL is the API origin an App client is pointed at.
func (w *Webhook) URL() string { return w.server.URL }

// Record appends an attempt to the log as GitHub records one it made, with the status code the
// receiver answered, and returns it with its id.
func (w *Webhook) Record(attempt Attempt, action string, repositoryID int64, statusCode int, at time.Time) Delivery {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recordLocked(attempt, action, repositoryID, statusCode, at, false)
}

func (w *Webhook) recordLocked(attempt Attempt, action string, repositoryID int64, statusCode int, at time.Time, redelivery bool) Delivery {
	w.nextID += 17
	installation := int64(135459902)
	repo := repositoryID
	delivery := Delivery{
		ID:             w.nextID,
		GUID:           attempt.GUID,
		DeliveredAt:    at.UTC(),
		Redelivery:     redelivery,
		Duration:       0.3,
		Status:         statusText(statusCode),
		StatusCode:     statusCode,
		Event:          attempt.Event,
		InstallationID: &installation,
		RepositoryID:   &repo,
	}
	if action != "" {
		delivery.Action = &action
	}
	w.log = append(w.log, delivery)
	w.payloads[delivery.ID] = attempt
	return delivery
}

// statusText is the attempt's status: OK for a 2xx answer, and for any other the failure GitHub
// names. A status code of 0 is an attempt that got no HTTP answer (GitHub's own listing filter,
// status=failure, covers only 400-599, so it leaves such an attempt out).
func statusText(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "OK"
	case code == 0:
		return "timed out"
	}
	return fmt.Sprintf("Invalid HTTP Response: %d", code)
}

// Refuse makes GitHub answer a redelivery request for id with status instead of 202; status 0
// accepts it again.
func (w *Webhook) Refuse(id int64, status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if status == 0 {
		delete(w.refusals, id)
		return
	}
	w.refusals[id] = status
}

// AnswerAfterRedelivering makes GitHub carry out a redelivery request for id, recording the
// redelivery as usual, and then answer the request with status instead of 202: the request did
// its work, and its answer says otherwise.
func (w *Webhook) AnswerAfterRedelivering(id int64, status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.answers[id] = status
}

type limit struct {
	status int
	header http.Header
}

// Limit makes GitHub answer every request of method (GET, the listing; POST, a redelivery
// request) with status and header, as it answers a request over a rate limit; status 0 lifts it.
func (w *Webhook) Limit(method string, status int, header http.Header) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if status == 0 {
		delete(w.limits, method)
		return
	}
	w.limits[method] = limit{status: status, header: header}
}

// limited answers r with its method's limit and reports whether it did.
func (w *Webhook) limited(rw http.ResponseWriter, r *http.Request) bool {
	w.mu.Lock()
	current, found := w.limits[r.Method]
	w.mu.Unlock()
	if !found {
		return false
	}
	for name, values := range current.header {
		rw.Header()[name] = values
	}
	http.Error(rw, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`, current.status)
	return true
}

// SetClock sets the time GitHub stamps on the attempts a redelivery makes.
func (w *Webhook) SetClock(now func() time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = now
}

// SetRedeliver replaces the function that answers a redelivery request.
func (w *Webhook) SetRedeliver(redeliver func(Attempt) int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.redeliver = redeliver
}

// Requests returns the delivery ids a redelivery was requested for, in order.
func (w *Webhook) Requests() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.requests)
}

// Log returns every attempt recorded, in the order GitHub recorded them.
func (w *Webhook) Log() []Delivery {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.log)
}

// Listings counts list requests served.
func (w *Webhook) Listings() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.listings
}

// list pages the log newest first, as GitHub does: per_page (default 30, at most 100), an
// opaque cursor carried in the Link header's rel="next" URL, and the optional status filter
// (success is 200-399, failure 400-599).
func (w *Webhook) list(rw http.ResponseWriter, r *http.Request) {
	w.verifyAppJWT(r)
	w.mu.Lock()
	w.listings++
	w.mu.Unlock()
	if w.limited(rw, r) {
		return
	}
	query := r.URL.Query()
	perPage := 30
	if raw := query.Get("per_page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			http.Error(rw, "bad per_page", http.StatusBadRequest)
			return
		}
		perPage = min(parsed, 100)
	}
	status := query.Get("status")
	if status != "" && status != "success" && status != "failure" {
		http.Error(rw, "bad status", http.StatusUnprocessableEntity)
		return
	}
	w.mu.Lock()
	rows := make([]Delivery, 0, len(w.log))
	for i := len(w.log) - 1; i >= 0; i-- {
		row := w.log[i]
		switch status {
		case "success":
			if row.StatusCode < 200 || row.StatusCode > 399 {
				continue
			}
		case "failure":
			if row.StatusCode < 400 || row.StatusCode > 599 {
				continue
			}
		}
		rows = append(rows, row)
	}
	w.mu.Unlock()

	start := 0
	if cursor := query.Get("cursor"); cursor != "" {
		id, err := strconv.ParseInt(strings.TrimPrefix(cursor, "v1_"), 10, 64)
		if err != nil {
			http.Error(rw, "bad cursor", http.StatusBadRequest)
			return
		}
		start = slices.IndexFunc(rows, func(row Delivery) bool { return row.ID == id })
		if start < 0 {
			start = len(rows)
		}
	}
	end := min(start+perPage, len(rows))
	if end < len(rows) {
		next := url.Values{"per_page": {strconv.Itoa(perPage)}, "cursor": {"v1_" + strconv.FormatInt(rows[end].ID, 10)}}
		if status != "" {
			next.Set("status", status)
		}
		rw.Header().Set("Link", fmt.Sprintf(`<%s/app/hook/deliveries?%s>; rel="next"`, w.server.URL, next.Encode()))
	}
	rw.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(rw).Encode(rows[start:end]); err != nil {
		w.t.Errorf("encode deliveries: %v", err)
	}
}

// attempt answers a redelivery request: 202, then the fake re-sends the original attempt through
// redeliver and records the outcome as a new attempt under the same GUID, with redelivery set.
func (w *Webhook) attempt(rw http.ResponseWriter, r *http.Request) {
	w.verifyAppJWT(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(rw, "bad id", http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	w.requests = append(w.requests, id)
	w.mu.Unlock()
	if w.limited(rw, r) {
		return
	}
	w.mu.Lock()
	if status, refused := w.refusals[id]; refused {
		w.mu.Unlock()
		http.Error(rw, `{"message":"refused"}`, status)
		return
	}
	original, found := w.payloads[id]
	var action string
	var repo int64
	for _, row := range w.log {
		if row.ID == id {
			if row.Action != nil {
				action = *row.Action
			}
			if row.RepositoryID != nil {
				repo = *row.RepositoryID
			}
		}
	}
	redeliver := w.redeliver
	w.mu.Unlock()
	if !found {
		http.Error(rw, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	code := http.StatusOK
	if redeliver != nil {
		code = redeliver(original)
	}
	w.mu.Lock()
	w.recordLocked(original, action, repo, code, w.now(), true)
	answer, lost := w.answers[id]
	w.mu.Unlock()
	if lost {
		http.Error(rw, `{"message":"Server Error"}`, answer)
		return
	}
	rw.WriteHeader(http.StatusAccepted)
	_, _ = rw.Write([]byte("{}"))
}

// verifyAppJWT checks the bearer is an RS256 JWT that verifies against the fake's public key and
// carries the claims GitHub requires of an App JWT.
func (w *Webhook) verifyAppJWT(r *http.Request) {
	w.t.Helper()
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		w.t.Errorf("%s %s called with Authorization %q, want an App JWT", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		return
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		w.t.Errorf("App JWT has %d segments, want 3", len(parts))
		return
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		w.t.Errorf("decode App JWT signature: %v", err)
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(w.publicKey, crypto.SHA256, digest[:], signature); err != nil {
		w.t.Errorf("App JWT does not verify against the App key: %v", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		w.t.Errorf("decode App JWT claims: %v", err)
		return
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		w.t.Errorf("parse App JWT claims: %v", err)
		return
	}
	if claims.Iss != w.clientID {
		w.t.Errorf("App JWT iss %q, want %q", claims.Iss, w.clientID)
	}
	if now := time.Now().Unix(); claims.Exp <= now || claims.Exp > now+10*60 {
		w.t.Errorf("App JWT exp %d outside (now, now+10m]", claims.Exp)
	}
}

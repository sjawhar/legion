package brokertest

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The broker's own OpenID Connect client at a test's provider (BROKER_SIGNIN_CLIENT_ID and the
// secret its BROKER_SIGNIN_CLIENT_SECRET_FILE holds), and the Workspace domain a person signing in
// must belong to (BROKER_SIGNIN_DOMAIN).
const (
	SignInClientID     = "broker-client"
	SignInClientSecret = "broker-client-secret"
	SignInDomain       = "example.com"
)

// SignIn arms issuer's code flow for the broker's own client, signing ID tokens with key, and
// answers the sign-in configuration naming it, with sessions of an hour: what BROKER_SIGNIN_* and
// the provider give a deployed broker. The issuer answers a code exchange with no refresh token,
// as Google answers the broker's online sign-in.
func SignIn(issuer *oidctest.Issuer, key *oidctest.Key) *config.SignIn {
	issuer.EnableCodeFlow(key, SignInClientID, SignInClientSecret)
	issuer.OmitRefreshTokens()
	return &config.SignIn{Issuer: issuer.URL(), ClientID: SignInClientID, ClientSecret: SignInClientSecret, Domain: SignInDomain, SessionLifetime: time.Hour}
}

// NewApprovalPage builds the broker's approval page as cmd/broker/main.go does, for a broker at
// publicURL whose records m reads and decides and whose sessions st keeps. A nil signIn builds it
// with sign-in off.
func NewApprovalPage(t *testing.T, signIn *config.SignIn, publicURL string, st *store.Store, m *requests.Machine) *approval.Page {
	t.Helper()
	page, err := approval.New(approval.Config{
		SignIn: signIn, PublicURL: publicURL, MachineLoginURL: MachineLoginURL, Store: st, Machine: m,
	})
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}
	return page
}

// Person is the claims a member of SignInDomain with a verified email signs in to the provider
// with.
func Person(email string) map[string]any {
	return map[string]any{"sub": "sub-" + email, "email": email, "email_verified": true, "hd": SignInDomain}
}

// Decide decides recordID as login on the approval page of the broker at brokerURL, as that person
// does: a fresh browser opens the record's page, signs in at issuer as login and returns to it,
// then posts the page's Approve or Deny for the record. It fails t unless the page offers login
// that decision and takes it; what became of the record is the broker's to say.
func Decide(t *testing.T, brokerURL string, issuer *oidctest.Issuer, login, recordID string, approve bool) {
	t.Helper()
	b := NewBrowser(t, brokerURL, http.DefaultTransport, issuer)
	b.SignIn(t, recordID, Person(login))
	b.Decide(t, recordID, approve)
}

// Response is one answer as a browser received it.
type Response struct {
	Status int
	Header http.Header
	Body   string
}

// Location is the answer's Location, failing t when it has none.
func (r Response) Location(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(r.Header.Get("Location"))
	if err != nil || r.Header.Get("Location") == "" {
		t.Fatalf("answer %d has no Location: %v\n%s", r.Status, err, r.Body)
	}
	return u
}

// Cookie is the answer's Set-Cookie naming name, or nil.
func (r Response) Cookie(name string) *http.Cookie {
	for _, c := range (&http.Response{Header: r.Header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Browser is a person's browser on a broker's approval page: a cookie jar, following no redirect
// by itself so a test sees each hop.
type Browser struct {
	url    string
	issuer *oidctest.Issuer
	jar    *cookiejar.Jar
	client *http.Client
	// Check, when set, is run on every answer from the broker, for a test holding each to a rule.
	Check func(t *testing.T, req *http.Request, res Response)
}

// NewBrowser is a browser with no cookies for the broker at brokerURL, reached through transport,
// whose sign-in provider is issuer.
func NewBrowser(t *testing.T, brokerURL string, transport http.RoundTripper, issuer *oidctest.Issuer) *Browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Browser{
		url: brokerURL, issuer: issuer, jar: jar,
		client: &http.Client{
			Transport:     transport,
			Jar:           loopbackJar{jar},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// loopbackJar keeps and sends a Secure cookie over plain http too, as a browser does for a
// loopback origin, so the page's __Host- cookies travel to a broker on an httptest.Server without
// TLS as they do to one behind it.
type loopbackJar struct{ jar *cookiejar.Jar }

func (j loopbackJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.jar.SetCookies(secure(u), cookies)
}

func (j loopbackJar) Cookies(u *url.URL) []*http.Cookie { return j.jar.Cookies(secure(u)) }

// secure is u with an http scheme read as https.
func secure(u *url.URL) *url.URL {
	if u.Scheme != "http" {
		return u
	}
	https := *u
	https.Scheme = "https"
	return &https
}

// target is path on the broker, or an absolute URL as given.
func (b *Browser) target(path string) string {
	if strings.HasPrefix(path, "http") {
		return path
	}
	return b.url + path
}

// send sends req, adding cookies beside the jar's, and runs Check on the broker's answer.
func (b *Browser) send(t *testing.T, req *http.Request, cookies []*http.Cookie) Response {
	t.Helper()
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", req.Method, req.URL, err)
	}
	out := Response{Status: res.StatusCode, Header: res.Header, Body: string(body)}
	if b.Check != nil && strings.HasPrefix(req.URL.String(), b.url) {
		b.Check(t, req, out)
	}
	return out
}

// Get opens path on the broker, or an absolute URL, sending cookies beside the jar's.
func (b *Browser) Get(t *testing.T, path string, cookies ...*http.Cookie) Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, b.target(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	return b.send(t, req, cookies)
}

// Post posts form to path with headers, sending cookies beside the jar's.
func (b *Browser) Post(t *testing.T, path string, form url.Values, headers map[string]string, cookies ...*http.Cookie) Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.target(path), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return b.send(t, req, cookies)
}

// FromPage posts form to path as the broker's own page does, with the broker's Origin.
func (b *Browser) FromPage(t *testing.T, path string, form url.Values) Response {
	t.Helper()
	return b.Post(t, path, form, map[string]string{"Origin": b.url})
}

// Cookie is the value of the jar's cookie name for the broker, or "".
func (b *Browser) Cookie(t *testing.T, name string) string {
	t.Helper()
	u, err := url.Parse(b.url)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range b.jar.Cookies(secure(u)) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// StartSignIn opens path with no live session and answers the provider URL the broker sends the
// browser to.
func (b *Browser) StartSignIn(t *testing.T, path string) *url.URL {
	t.Helper()
	res := b.Get(t, path)
	if res.Status != http.StatusFound {
		t.Fatalf("GET %s = %d, want 302 to the provider:\n%s", path, res.Status, res.Body)
	}
	return res.Location(t)
}

// Authorize signs in at the provider as claims and answers the callback URL it returns to.
func (b *Browser) Authorize(t *testing.T, authURL *url.URL, claims map[string]any) string {
	t.Helper()
	b.issuer.SignInAs(claims)
	res := b.Get(t, authURL.String())
	if res.Status != http.StatusFound {
		t.Fatalf("the provider answered %d, want 302 to the callback: %s", res.Status, res.Body)
	}
	return res.Header.Get("Location")
}

// SignIn signs the browser in as claims through recordID's page, the way a person arrives.
func (b *Browser) SignIn(t *testing.T, recordID string, claims map[string]any) {
	t.Helper()
	res := b.Get(t, b.Authorize(t, b.StartSignIn(t, "/approve/"+recordID), claims))
	if res.Status != http.StatusSeeOther || res.Header.Get("Location") != "/approve/"+recordID {
		t.Fatalf("callback = %d to %q, want 303 to /approve/%s:\n%s", res.Status, res.Header.Get("Location"), recordID, res.Body)
	}
}

// Page opens recordID's page with a live session.
func (b *Browser) Page(t *testing.T, recordID string) Response {
	t.Helper()
	res := b.Get(t, "/approve/"+recordID)
	if res.Status != http.StatusOK {
		t.Fatalf("GET /approve/%s = %d, want the page:\n%s", recordID, res.Status, res.Body)
	}
	return res
}

// Decide opens recordID's page with a live session and posts its Approve or Deny for the record,
// failing t unless the page offers that form and answers it with a 303 back to the record.
func (b *Browser) Decide(t *testing.T, recordID string, approve bool) {
	t.Helper()
	form := DecisionForm(t, b.Page(t, recordID).Body, recordID)
	if form == nil {
		t.Fatalf("the page for %s offers no decision", recordID)
	}
	value := "deny"
	if approve {
		value = "approve"
	}
	res := b.FromPage(t, "/approve", Decision(form, value))
	if want := "/approve/" + recordID + "?decided=" + recordID; res.Status != http.StatusSeeOther || res.Header.Get("Location") != want {
		t.Fatalf("%s %s = %d to %q, want 303 to %s:\n%s", value, recordID, res.Status, res.Header.Get("Location"), want, res.Body)
	}
}

// Form is one form of a page: its action, its hidden fields and its buttons.
type Form struct {
	Action  string
	Fields  url.Values
	Buttons []Button
}

// Button is one of a form's buttons.
type Button struct{ Name, Value, Text string }

func parse(t *testing.T, body string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse the page: %v", err)
	}
	return doc
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// text is every text node under n, with runs of whitespace as one space. The templates put a
// newline between block elements, so their texts stay apart.
func text(n *html.Node) string {
	var sb strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			sb.WriteString(d.Data)
		}
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// PageText is a page's text as a person reads it.
func PageText(t *testing.T, body string) string {
	t.Helper()
	return text(parse(t, body))
}

// Forms is every form of a page.
func Forms(t *testing.T, body string) []Form {
	t.Helper()
	var out []Form
	for n := range parse(t, body).Descendants() {
		if n.Type != html.ElementNode || n.Data != "form" {
			continue
		}
		f := Form{Action: attr(n, "action"), Fields: url.Values{}}
		for d := range n.Descendants() {
			switch {
			case d.Type == html.ElementNode && d.Data == "input" && attr(d, "type") == "hidden":
				f.Fields.Add(attr(d, "name"), attr(d, "value"))
			case d.Type == html.ElementNode && d.Data == "button":
				f.Buttons = append(f.Buttons, Button{Name: attr(d, "name"), Value: attr(d, "value"), Text: text(d)})
			}
		}
		out = append(out, f)
	}
	return out
}

// DecisionForm is the hidden fields of the page's Approve/Deny form for recordID, nil when the
// page offers none.
func DecisionForm(t *testing.T, body, recordID string) url.Values {
	t.Helper()
	for _, f := range Forms(t, body) {
		if f.Action == "/approve" && len(f.Fields["record"]) == 1 && f.Fields.Get("record") == recordID {
			return f.Fields
		}
	}
	return nil
}

// Decision is a copy of form with its decision set to value, "approve" or "deny".
func Decision(form url.Values, value string) url.Values {
	out := url.Values{}
	for k, v := range form {
		out[k] = append([]string(nil), v...)
	}
	out.Set("decision", value)
	return out
}

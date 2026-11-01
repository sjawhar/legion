package main

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTheApprovalPagesRoutesAreReadInTheirOwnPackage reads the broker's own source, as the docs
// build does. A route whose handler is a method of *approval.Page is read in the approval package,
// where it answers HTML pages and redirects by status and no JSON; and the account approve route's
// DECIDE_ON_BROKER_PAGE refusal names the body it answers, which carries approve_url.
func TestTheApprovalPagesRoutesAreReadInTheirOwnPackage(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	route := func(method, pattern string) int {
		t.Helper()
		i := slices.IndexFunc(a.routes, func(r route) bool { return r.Method == method && r.Pattern == pattern })
		if i < 0 {
			t.Fatalf("no route %s %s", method, pattern)
		}
		return i
	}

	decide := route(http.MethodPost, "/approve")
	if got, want := a.routes[decide].Dir, envoyDir+"/internal/broker/approval"; got != want {
		t.Errorf("POST /approve's handler is read in %s, want %s", got, want)
	}
	pages := map[int]bool{}
	for _, o := range a.outcomes[decide] {
		if o.Code != "" || o.RespType != "" {
			t.Errorf("POST /approve answers %d %q with a JSON body %q, want an HTML page", o.Status, o.Code, o.RespType)
		}
		if o.Page {
			pages[o.Status] = true
		}
	}
	if !pages[http.StatusSeeOther] || !pages[http.StatusForbidden] {
		t.Errorf("POST /approve answers %v, want its 303 back to the page and its 403 refusals", pages)
	}

	approve := route(http.MethodPost, "/v1/credential-requests/{record}/approve")
	var refusal *outcome
	for _, o := range refusals(a.outcomes[approve]) {
		if o.Code == "DECIDE_ON_BROKER_PAGE" {
			refusal = &o
		}
	}
	if refusal == nil || refusal.Status != http.StatusForbidden || refusal.RespType == "" {
		t.Fatalf("the approve route's DECIDE_ON_BROKER_PAGE refusal = %+v, want a 403 naming its body", refusal)
	}
	ref, err := a.g.localStruct(refusal.RespType)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := a.g.fields(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(fields, func(f jsonField) bool { return f.Name == "approve_url" }) {
		t.Errorf("the refusal's body %s = %+v, want an approve_url field", refusal.RespType, fields)
	}
}

// TestTheSignInClassRefusesPastTheRateLimit reads the broker's own source, as the docs build
// does. authenticate's authSignIn case spends the source address's bucket before any route of
// the approval page runs, and refuses past it with the page's own 429, written by a method of
// the approval package whose receiver is a field expression. refgen has to read that refusal
// through to the page it generates, or the class reads as refusing nothing while its own doc
// text says every one of its routes spends that bucket first.
func TestTheSignInClassRefusesPastTheRateLimit(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(a.classes, func(c authClass) bool { return c.Wrapper == "signInAuth" })
	if i < 0 {
		t.Fatalf("no signInAuth class among %+v", a.classes)
	}
	class := a.classes[i]
	if !slices.ContainsFunc(class.Refusals, func(o outcome) bool {
		return o.Status == http.StatusTooManyRequests && o.Page
	}) {
		t.Errorf("the %s class refuses %+v, want the approval page's 429", class.Label, class.Refusals)
	}

	page, err := apiPage(a)
	if err != nil {
		t.Fatal(err)
	}
	row := ""
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "| "+class.Label+" |") {
			row = line
		}
	}
	if !strings.Contains(row, "429") {
		t.Errorf("the %s row of the Authentication table is %q, want its 429 in the refusals column", class.Label, row)
	}
}

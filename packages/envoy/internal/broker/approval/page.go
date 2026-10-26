package approval

import (
	"html/template"
	"log/slog"
	"net/http"
	"strings"
)

// Page is one of the broker's approval pages: the one a person reads before approving, and every
// answer it can end in. A page carries no script, no style and no image — nothing but its own
// markup — so it needs no asset of the broker's and none of anyone else's, which is what lets
// Write serve it under a content-security policy that allows none.
type Page struct {
	// Title is the browser's title and the page's one heading.
	Title string
	// Message is the paragraph under that heading: what the person is being asked, or what went
	// wrong and what to do about it.
	Message string
	// Facts are what is being approved, each a label and its value, as the broker's own record
	// states them. The page shows no more than the record holds.
	Facts []Fact
	// Form is the path the page's one button posts to; empty on a page with nothing to do.
	Form string
	// Button is that button's label.
	Button string
	// FormRedirectOrigin is the origin (scheme://host[:port]) the form's own answer redirects the
	// browser to — the issuer's authorization endpoint, for the page whose button starts a
	// sign-in. form-action allows it beside 'self', because Chromium and WebKit check form-action
	// on every redirect that follows a form submission, not only on the form's own target: without
	// it the button posts to this origin, the redirect to the issuer is blocked, and nothing
	// happens. Empty on every other page, which then allows this origin alone.
	FormRedirectOrigin string
}

// Fact is one line of what is being approved.
type Fact struct {
	Label, Value string
}

// contentSecurityPolicy is what an approval page is served under: nothing loads, nothing runs, its
// one form posts to this origin and redirects nowhere but formRedirect (empty for none), and no
// page of anyone else's may frame it. A page carrying no asset of its own is what makes
// default-src 'none' possible rather than a policy with holes in it; frame-ancestors is the modern
// half of the X-Frame-Options header Write also sends, and base-uri 'none' keeps an injected
// <base> from re-pointing the form.
func contentSecurityPolicy(formRedirect string) string {
	formAction := "'self'"
	if formRedirect != "" {
		formAction += " " + formRedirect
	}
	return "default-src 'none'; form-action " + formAction + "; base-uri 'none'; frame-ancestors 'none'"
}

// Write answers w with p and status.
//
// Every answer carries the same headers: its policy (above), X-Frame-Options for a browser that
// reads no frame-ancestors, no-store so neither the browser nor anything between keeps a page that
// names a pending approval, no referrer (the challenge id is in the URL and must not travel in a
// Referer header), and nosniff.
func Write(w http.ResponseWriter, status int, p Page) {
	var body strings.Builder
	if err := pageTemplate.Execute(&body, p); err != nil {
		// Unreachable: the template is parsed at build and every field it reads is a string. A
		// failure here means the page is not what the broker meant to show, so it says so plainly
		// rather than serving half of one.
		slog.Error("broker: render the approval page failed", "error", err)
		http.Error(w, "the approval page could not be rendered", http.StatusInternalServerError)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Content-Security-Policy", contentSecurityPolicy(p.FormRedirectOrigin))
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body.String()))
}

// pageTemplate renders a Page. html/template escapes every value for where it stands, so a secret
// name, a host or an email the record holds is shown and never read as markup.
var pageTemplate = template.Must(template.New("approval").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .Facts}}<dl>
{{range .Facts}}<dt>{{.Label}}</dt><dd>{{.Value}}</dd>
{{end}}</dl>
{{end}}{{if .Form}}<form method="post" action="{{.Form}}">
<button type="submit">{{.Button}}</button>
</form>
{{end}}</main>
</body>
</html>
`))

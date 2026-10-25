package approval

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

const (
	// tokenLifetime is how long a rendered page's decision forms stay good.
	tokenLifetime = time.Hour
	// maxRecords bounds the records one page prints with a button, and so the records one POST
	// may name and the outcomes one page reports.
	maxRecords = 100
	// timeLayout is how the page prints an instant.
	timeLayout = "2006-01-02 15:04:05 UTC"
)

// recordView is one record as the page prints it, every field from the broker's own record.
type recordView struct {
	ID string
	// MachineLogin is a launcher_credential record, decided by its typed code on Dispatch's
	// machine-login page and never here.
	MachineLogin bool
	// Identifiers are the secret names an agent_secret record asks for, or a machine login's host.
	Identifiers []string
	// Enrollment is the requesting enrollment the broker verified; nil for a machine login.
	Enrollment *record.Enrollment
	// EnrollmentSession is the session id that enrollment stated when it enrolled.
	EnrollmentSession string
	// RequestSession is the session id the request itself named, its own unsigned claim, set only
	// when it differs from EnrollmentSession.
	RequestSession string
	Reason         string
	Lifetime       string
	RequestedAt    string
	ExpiresAt      string
	Approver       string
	// State is the record's state, "expired" too for one past its expiry the sweeper has not
	// ended yet; DecidedBy, DecidedAt and Detail are its decision's.
	State, DecidedBy, DecidedAt, Detail string
	// Pending is a record still waiting for a decision.
	Pending bool
	// MayDecide is whether the signed-in person may decide it (record.MayDecide).
	MayDecide bool
	// Buttons is whether the page offers Approve and Deny for it: a pending agent_secret record
	// the person may decide.
	Buttons bool
}

// tokenFields are the hidden fields every decision form carries: the records the page printed with
// a button, the token binding them to the session, and its expiry.
type tokenFields struct {
	Printed []string
	Token   string
	Expires string
}

// pageView is the record page.
type pageView struct {
	chrome
	// Outcomes are the lines a decision's redirect reports, one per record it decided.
	Outcomes []string
	// Records are the record the URL names, first, and then the other pending agent_secret
	// records the person may decide.
	Records []recordView
	// Unshown counts the pending records left off the page for maxRecords.
	Unshown int
	// Form is the decision forms' hidden fields; ApproveAll is every record Form.Printed names,
	// in page order, for the one "Approve all N shown" form, offered when there are two or more.
	Form            tokenFields
	ApproveAll      []string
	MachineLoginURL string
}

// ServeRecord is GET /approve/{record}: the page for one record. With no live session, or with
// ?switch=1 (a person signing in as someone else), it starts a sign-in that returns here. The page
// prints that record and every other pending agent_secret record the person may decide, with
// Approve and Deny for each the person may decide and one "Approve all N shown" when N is two or
// more, and, after a decision, one outcome line per decided record, read from the broker's own
// record now.
func (p *Page) ServeRecord(w http.ResponseWriter, r *http.Request, sess Session) {
	id := r.PathValue("record")
	if !record.ValidID(id) {
		writeMessage(w, http.StatusBadRequest, say("Not a request", "Credential-request record ids are lowercase-hex SHA-256 hashes."))
		return
	}
	detail, err := p.cfg.Machine.ReadRecord(r.Context(), id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		m := say("No such request", "The secrets broker has no credential request "+id+".")
		m.Login = sess.Login
		writeMessage(w, http.StatusNotFound, m)
		return
	case err != nil:
		writeUnavailable(w, "read the request", err)
		return
	}
	if switching := r.URL.Query().Get("switch") == "1"; switching || !sess.live() {
		p.beginSignIn(w, r, detail, switching)
		return
	}

	now := time.Now()
	main := p.recordView(detail, sess.Login, now)
	view := pageView{
		chrome:          chrome{Title: "Approve a secret request", Login: sess.Login, Record: id},
		Records:         []recordView{main},
		MachineLoginURL: p.cfg.MachineLoginURL,
	}
	if view.Outcomes, err = p.outcomes(r.Context(), r.URL.Query().Get("decided"), sess.Login, now, detail); err != nil {
		writeUnavailable(w, "read the decided requests", err)
		return
	}
	var printed []string
	if main.Buttons {
		printed = append(printed, id)
	}
	pending, err := p.cfg.Machine.PendingForApprover(r.Context(), sess.Login)
	if err != nil {
		writeUnavailable(w, "list the pending requests", err)
		return
	}
	for _, s := range pending {
		if s.Kind != record.KindAgentSecret || s.RecordID == id {
			continue
		}
		if len(printed) == maxRecords {
			view.Unshown++
			continue
		}
		other, err := p.cfg.Machine.ReadRecord(r.Context(), s.RecordID)
		if err != nil {
			writeUnavailable(w, "read a pending request", err)
			return
		}
		if v := p.recordView(other, sess.Login, now); v.Buttons {
			view.Records = append(view.Records, v)
			printed = append(printed, v.ID)
		}
	}
	if len(printed) > 0 {
		expires := now.Add(tokenLifetime).Unix()
		view.Form = tokenFields{Printed: printed, Token: pageToken(sess.secret, expires, printed), Expires: strconv.FormatInt(expires, 10)}
	}
	if len(printed) > 1 {
		view.ApproveAll = printed
	}
	writePage(w, http.StatusOK, "record.html", view)
}

// recordView is detail as the page prints it to login at now.
func (p *Page) recordView(d requests.RecordDetail, login string, now time.Time) recordView {
	v := recordView{
		ID: d.RecordID, MachineLogin: d.Kind == record.KindLauncherCredential,
		Identifiers: d.Identifiers, Enrollment: d.Enrollment,
		EnrollmentSession: d.Session.Enrollment,
		Reason:            d.Reason, Lifetime: "until the session ends",
		RequestedAt: d.RequestedAt.UTC().Format(timeLayout), ExpiresAt: d.ExpiresAt.UTC().Format(timeLayout),
		Approver: approverText(d.Approver), State: d.State,
		MayDecide: record.MayDecide(d.Kind, d.Approver, d.Service, login),
	}
	if d.Session.Request != "" && d.Session.Request != d.Session.Enrollment {
		v.RequestSession = d.Session.Request
	}
	if d.LifetimeSeconds != nil {
		v.Lifetime = (time.Duration(*d.LifetimeSeconds) * time.Second).String()
	}
	if d.Decided != nil {
		v.DecidedBy, v.DecidedAt, v.Detail = d.Decided.Login, d.Decided.At.UTC().Format(timeLayout), d.Decided.Detail
	}
	v.Pending = d.State == "pending" && d.ExpiresAt.After(now)
	if d.State == "pending" && !v.Pending {
		v.State = "expired"
	}
	v.Buttons = v.Pending && !v.MachineLogin && v.MayDecide
	return v
}

// approverText is who a record's approver field says may decide it.
func approverText(approver string) string {
	if approver == record.AnyoneApprover {
		return "anyone signed in to this broker"
	}
	return approver
}

// outcomes is one line for each record id the comma-separated decided names, at most maxRecords
// of them, each said from the broker's own record now and never from the URL: an id that is not a
// record id, or names no record, gets none. read is the record the page has just read, which a
// decision's own page always names, so it is not read again.
func (p *Page) outcomes(ctx context.Context, decided, login string, now time.Time, read requests.RecordDetail) ([]string, error) {
	var lines []string
	var seen []string
	for id := range strings.SplitSeq(decided, ",") {
		if !record.ValidID(id) || slices.Contains(seen, id) {
			continue
		}
		if len(seen) == maxRecords {
			break
		}
		seen = append(seen, id)
		d := read
		if id != read.RecordID {
			var err error
			d, err = p.cfg.Machine.ReadRecord(ctx, id)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				continue
			case err != nil:
				return nil, err
			}
		}
		lines = append(lines, outcome(d, login, now))
	}
	return lines, nil
}

// outcome says what became of a record a decision named, from its state now.
func outcome(d requests.RecordDetail, login string, now time.Time) string {
	what := strings.Join(d.Identifiers, ", ") + ": "
	switch {
	case d.Kind != record.KindAgentSecret:
		return what + "a machine login, which is decided by typing its code on Dispatch's machine-login page."
	case d.Decided == nil && !record.MayDecide(d.Kind, d.Approver, d.Service, login):
		return what + "not yours to decide; only " + approverText(d.Approver) + " may."
	case d.Decided == nil && !d.ExpiresAt.After(now):
		return what + "expired before anyone decided it."
	case d.Decided == nil:
		return what + "still pending; it was not decided."
	}
	by, at := d.Decided.Login, d.Decided.At.UTC().Format(timeLayout)
	if by != "" && by != login {
		what += "already decided: "
	}
	switch d.Decided.Event {
	case "approved":
		if d.Decided.Detail != "" {
			return what + "recorded as denied because the requesting session ended (approved by " + by + " at " + at + ")."
		}
		if d.State == "revoked" {
			return what + "granted by " + by + " at " + at + ", and revoked since."
		}
		return what + "granted (approved by " + by + " at " + at + ")."
	case "denied":
		return what + "denied by " + by + " at " + at + "."
	case "expired":
		return what + "expired at " + at + " before anyone decided it."
	case "cancelled":
		return what + "cancelled at " + at + ": the requesting session withdrew it or ended."
	}
	return what + d.Decided.Event + " at " + at + "."
}

// pageMAC binds a page's decision forms to the session whose secret it is and to exactly the
// records printed with a button: HMAC-SHA256 under the raw session secret, which only the browser
// holding the cookie has (Postgres keeps its hash), over "approve-page", the expiry and the sorted
// record ids, each line-separated. Record ids are fixed-length hex, so the message is unambiguous.
func pageMAC(secret []byte, expires int64, printed []string) []byte {
	sorted := slices.Sorted(slices.Values(printed))
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("approve-page\n" + strconv.FormatInt(expires, 10) + "\n" + strings.Join(sorted, "\n")))
	return mac.Sum(nil)
}

// pageToken is pageMAC hex-encoded, as a page's decision forms carry it.
func pageToken(secret []byte, expires int64, printed []string) string {
	return hex.EncodeToString(pageMAC(secret, expires, printed))
}

// recordIDs reads a form's record ids, in order and each once, refusing any that is not a record
// id and more than maxRecords.
func recordIDs(values []string) ([]string, bool) {
	var ids []string
	for _, id := range values {
		if !record.ValidID(id) {
			return nil, false
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, len(ids) <= maxRecords
}

// ServeDecide is POST /approve: decide the records a page's form names. It refuses, writing
// nothing, a POST from any other origin (sameOrigin), one with no live session, and one whose
// token does not bind it to this session and to a printed set holding every record it names, or
// whose token has expired. Each record is then decided on its own by requests.Machine.ApplyDecision
// as the session's person, so who may decide, the current policy and RECORD_TERMINAL stay that
// function's: a replayed form decides nothing again. It answers 303 to the first record's page
// with the decided ids, whose outcomes that page reads from the records themselves.
func (p *Page) ServeDecide(w http.ResponseWriter, r *http.Request, sess Session) {
	if !p.sameOrigin(r) {
		refuseOrigin(w)
		return
	}
	if !sess.live() {
		writeMessage(w, http.StatusForbidden, say("Not signed in",
			"Your sign-in to the secrets broker has ended, so nothing was decided. Open the request again to sign in."))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeMessage(w, http.StatusBadRequest, say("Not a decision", "The form could not be read, so nothing was decided."))
		return
	}
	form := r.PostForm
	var approve bool
	switch form.Get("decision") {
	case "approve":
		approve = true
	case "deny":
	default:
		writeMessage(w, http.StatusBadRequest, say("Not a decision", "A decision is approve or deny, so nothing was decided."))
		return
	}
	printed, printedOK := recordIDs(form["printed"])
	decide, decideOK := recordIDs(form["record"])
	expires, err := strconv.ParseInt(form.Get("expires"), 10, 64)
	token, tokenErr := hex.DecodeString(form.Get("token"))
	if !printedOK || !decideOK || len(decide) == 0 || err != nil || tokenErr != nil {
		writeMessage(w, http.StatusBadRequest, say("Not a decision", "The form names no request it may decide, so nothing was decided."))
		return
	}
	switch {
	case !hmac.Equal(token, pageMAC(sess.secret, expires, printed)):
		writeMessage(w, http.StatusForbidden, say("Refused",
			"This form was not made for your sign-in, so nothing was decided. Open the request again."))
		return
	case time.Now().Unix() >= expires:
		writeMessage(w, http.StatusForbidden, say("Page expired",
			"This page is more than an hour old, so nothing was decided. Open the request again."))
		return
	}
	for _, id := range decide {
		if !slices.Contains(printed, id) {
			writeMessage(w, http.StatusForbidden, say("Refused",
				"This form names a request its page did not print, so nothing was decided. Open the request again."))
			return
		}
	}
	for _, id := range decide {
		slog.Info("approval page decision", "login", sess.Login, "record", id, "decision", form.Get("decision"),
			"outcome", p.decide(r.Context(), id, approve, sess.Login))
	}
	redirect(w, http.StatusSeeOther, "/approve/"+decide[0]+"?decided="+strings.Join(decide, ","))
}

// decide decides one record as login and answers what became of it, for the log. A machine login
// is never decided here, whatever a form says: its typed code is decided on Dispatch's page.
func (p *Page) decide(ctx context.Context, id string, approve bool, login string) string {
	kind, err := p.cfg.Machine.RecordKind(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "no such record"
	case err != nil:
		slog.Error("approval page: read the record's kind", "record", id, "error", err)
		return "error"
	case kind != record.KindAgentSecret:
		return "not decided: a machine login"
	}
	decision, err := p.cfg.Machine.ApplyDecision(ctx, id, approve, login)
	switch {
	case err == nil && decision.State == "denied" && approve:
		return "denied: the requesting enrollment is no longer live"
	case err == nil:
		return decision.State
	case errors.Is(err, requests.ErrExpired):
		return "expired"
	case errors.Is(err, requests.ErrTerminal):
		return "already decided"
	case errors.Is(err, record.ErrNotApprover), errors.Is(err, requests.ErrGrantChainInvalid):
		return "refused: " + err.Error()
	}
	slog.Error("approval page: decide the record", "record", id, "error", err)
	return "error"
}

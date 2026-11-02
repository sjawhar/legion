package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const reportTestCommit = "0123456789abcdef0123456789abcdef01234567"

type reportTestPicture struct {
	name        string
	contentType string
	content     []byte
	// encodedName, when set, names the file in RFC 2231's filename* form instead, which carries
	// a byte a raw header cannot.
	encodedName string
}

func reportFields(askAgent bool) map[string]any {
	return map[string]any{
		"body":       "The Send button overlaps the box\nIt happens on every issue page.",
		"page":       "https://dispatch.example/issues/CORE-1",
		"page_build": "1730000000000",
		"user_agent": "Mozilla/5.0 Test",
		"viewport":   map[string]any{"width": 1280, "height": 800},
		"ask_agent":  askAgent,
	}
}

// reportRequest posts a report as multipart, its fields in the `report` part and each picture a
// `picture` part, as the report form sends it; key, when set, is its Idempotency-Key.
func reportRequest(t *testing.T, handler http.Handler, fields map[string]any, pictures []reportTestPicture, login, key string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("report", string(encoded)); err != nil {
		t.Fatal(err)
	}
	for _, picture := range pictures {
		header := make(map[string][]string)
		header["Content-Disposition"] = []string{`form-data; name="picture"; filename="` + picture.name + `"`}
		if picture.encodedName != "" {
			header["Content-Disposition"] = []string{`form-data; name="picture"; filename*=UTF-8''` + picture.encodedName}
		}
		header["Content-Type"] = []string{picture.contentType}
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(picture.content)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/reports", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Dispatch-User", login)
	if key != "" {
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func newReportServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{commit: reportTestCommit})
	createDefaultAssigneeProject(t, handler, "CORE", "Core")
	return handler, database
}

func reportSpecMarkdown(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(), `
		select v.markdown from artifact_versions v join artifacts a on a.id = v.artifact_id
		where a.issue_key = $1 and a.is_primary
		order by v.number desc limit 1
	`, key).Scan(&markdown); err != nil {
		t.Fatalf("read %s's spec: %v", key, err)
	}
	return markdown
}

func countIssues(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from issues`).Scan(&count); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	return count
}

// Until a person names the project reports go to, a report is refused and nothing is written:
// the form says reports are not set up.
func TestReportRefusedUntilAProjectIsSet(t *testing.T) {
	handler, database := newReportServer(t)
	response := reportRequest(t, handler, reportFields(false), nil, "alice", "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"REPORTS_NOT_CONFIGURED"`) {
		t.Fatalf("status=%d body=%s, want 409 REPORTS_NOT_CONFIGURED", response.Code, response.Body.String())
	}
	if count := countIssues(t, database); count != 0 {
		t.Fatalf("a refused report wrote %d issues", count)
	}
}

// A report files an issue in the reports project, created by the reporter, whose spec holds the
// person's words, the picture shown inline, and where it happened: the page, the deployed
// version, the page's build, the browser, the window and the reporter. The picture is a file of
// the new issue.
func TestReportFilesAnIssueWithEveryDetail(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})

	response := reportRequest(t, handler, reportFields(false),
		[]reportTestPicture{{name: "shot.png", contentType: "image/png", content: pngBytes("pixels")}}, "bob", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	issue := decodeBody[model.Issue](t, response)
	if !strings.HasPrefix(issue.Key, "CORE-") || issue.Title != "The Send button overlaps the box" {
		t.Fatalf("issue = %s %q, want a CORE issue titled from the first line", issue.Key, issue.Title)
	}
	if issue.CreatedBy == nil || issue.CreatedBy.Kind != "user" || issue.CreatedBy.ID != "bob" || issue.Route != nil {
		t.Fatalf("issue created by %#v with route %v, want bob and no route", issue.CreatedBy, issue.Route)
	}
	// The reporter is not the one who fixes it: with no project default, nobody owns it yet.
	if issue.Assignee != nil {
		t.Fatalf("assignee = %q, want nobody: the reporter does not own the report", *issue.Assignee)
	}
	// The issue's own event and its picture's: the answer counts both.
	if issue.LastSeq != 2 {
		t.Fatalf("last_seq = %d, want 2", issue.LastSeq)
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != "report" {
		t.Fatalf("labels = %v, want [report]", issue.Labels)
	}
	spec := reportSpecMarkdown(t, database, issue.Key)
	for _, want := range []string{
		"The Send button overlaps the box",
		"It happens on every issue page.",
		"![shot.png](dispatch://" + issue.Key + "/artifact/shot-png@v1)",
		"https://dispatch.example/issues/CORE-1",
		reportTestCommit,
		"1730000000000",
		"Mozilla/5.0 Test",
		"1280 × 800",
		"bob",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec lacks %q:\n%s", want, spec)
		}
	}
	if strings.Contains(spec, "agent") {
		t.Errorf("a report that asked no agent says one was asked:\n%s", spec)
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts", nil, "alice")
	artifacts := decodeBody[[]model.Artifact](t, listed)
	var picture *model.Artifact
	for index := range artifacts {
		if artifacts[index].Slug == "shot-png" {
			picture = &artifacts[index]
		}
	}
	if picture == nil || picture.Kind != "image" || picture.Name != "shot.png" {
		t.Fatalf("artifacts = %#v, want shot.png as an image", artifacts)
	}
	var created, versions int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from events where issue_key = $1 and type = 'artifact.created'),
		       (select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = $1 and a.slug = 'shot-png')
	`, issue.Key).Scan(&created, &versions); err != nil {
		t.Fatal(err)
	}
	if created != 1 || versions != 1 {
		t.Fatalf("artifact.created events = %d, picture versions = %d, want 1 and 1", created, versions)
	}
}

// Ticking "Ask an agent to fix this" gives the new issue the route Settings names, from its first
// event: the reporter's issue.created notifies, so the outbox publishes it to the role's holder.
func TestReportAsksTheAgentByRoutingTheIssue(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": "role:dispatch-fixer"})

	response := reportRequest(t, handler, reportFields(true), nil, "bob", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	issue := decodeBody[model.Issue](t, response)
	if issue.Route == nil || *issue.Route != "role:dispatch-fixer" {
		t.Fatalf("route = %v, want role:dispatch-fixer", issue.Route)
	}
	var notify bool
	var route *string
	if err := database.Pool.QueryRow(context.Background(), `
		select e.notify, i.route from events e join issues i on i.key = e.issue_key
		where e.issue_key = $1 and e.type = 'issue.created'
	`, issue.Key).Scan(&notify, &route); err != nil {
		t.Fatal(err)
	}
	if !notify || route == nil || *route != "role:dispatch-fixer" {
		t.Fatalf("issue.created notify=%t on route %v, want a notifying event on role:dispatch-fixer", notify, route)
	}
	if spec := reportSpecMarkdown(t, database, issue.Key); !strings.Contains(spec, "role:dispatch-fixer") ||
		!strings.Contains(spec, "open a pull request") {
		t.Fatalf("the spec does not say an agent was asked, and for what:\n%s", spec)
	}
	unticked := reportRequest(t, handler, reportFields(false), nil, "bob", "")
	if unticked.Code != http.StatusCreated || decodeBody[model.Issue](t, unticked).Route != nil {
		t.Fatalf("an unticked report: status=%d body=%s, want no route", unticked.Code, unticked.Body.String())
	}
}

// Asking an agent while Settings names none is refused, and nothing is written.
func TestReportAskingAnAgentWithNoneSetIsRefused(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	response := reportRequest(t, handler, reportFields(true), nil, "bob", "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"REPORTS_AGENT_NOT_CONFIGURED"`) {
		t.Fatalf("status=%d body=%s, want 409 REPORTS_AGENT_NOT_CONFIGURED", response.Code, response.Body.String())
	}
	if count := countIssues(t, database); count != 0 {
		t.Fatalf("a refused report wrote %d issues", count)
	}
}

// A report takes pictures only, at most ten, and needs words within a message's limit.
func TestReportRefusesWhatIsNotAPicture(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})

	text := reportRequest(t, handler, reportFields(false),
		[]reportTestPicture{{name: "notes.txt", contentType: "text/plain", content: []byte("notes")}}, "bob", "")
	if text.Code != http.StatusBadRequest || !strings.Contains(text.Body.String(), `"code":"REPORT_INPUT"`) || !strings.Contains(text.Body.String(), "notes.txt") {
		t.Fatalf("a text file: status=%d body=%s, want 400 REPORT_INPUT naming it", text.Code, text.Body.String())
	}
	many := make([]reportTestPicture, maxReportPictures+1)
	for index := range many {
		many[index] = reportTestPicture{name: "shot.png", contentType: "image/png", content: pngBytes("pixels")}
	}
	if response := reportRequest(t, handler, reportFields(false), many, "bob", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("eleven pictures: status=%d body=%s, want 400", response.Code, response.Body.String())
	}
	blank := reportFields(false)
	blank["body"] = "  \n "
	if response := reportRequest(t, handler, blank, nil, "bob", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("no words: status=%d body=%s, want 400", response.Code, response.Body.String())
	}
	long := reportFields(false)
	long["body"] = strings.Repeat("x", contracts.DispatchBodyMax+1)
	if response := reportRequest(t, handler, long, nil, "bob", ""); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("too many words: status=%d body=%s, want 400 CAP_EXCEEDED", response.Code, response.Body.String())
	}
	if count := countIssues(t, database); count != 0 {
		t.Fatalf("refused reports wrote %d issues", count)
	}
}

// A clipboard names every pasted picture image.png: two of them become two files, each shown.
func TestReportNamesTwoPastedPicturesApart(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	response := reportRequest(t, handler, reportFields(false), []reportTestPicture{
		{name: "image.png", contentType: "image/png", content: pngBytes("first")},
		{name: "image.png", contentType: "image/png", content: pngBytes("second")},
	}, "bob", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	issue := decodeBody[model.Issue](t, response)
	spec := reportSpecMarkdown(t, database, issue.Key)
	for _, want := range []string{
		"![image.png](dispatch://" + issue.Key + "/artifact/image-png@v1)",
		"![image-2.png](dispatch://" + issue.Key + "/artifact/image-2-png@v1)",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec lacks %q:\n%s", want, spec)
		}
	}
}

// A report is a person's: an agent session has no page to report from.
func TestReportIsHumanOnly(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	fields := reportFields(false)
	fields["actor"] = sessionActor()
	response := agentRequest(t, handler, http.MethodPost, "/api/v1/reports", fields, "agent-token")
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("status=%d body=%s, want 403 HUMAN_ONLY", response.Code, response.Body.String())
	}
	if count := countIssues(t, database); count != 0 {
		t.Fatalf("a refused report wrote %d issues", count)
	}
}

// A report with no picture may be plain JSON.
func TestReportWithoutPicturesTakesJSON(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/reports", reportFields(false), "bob")
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	issue := decodeBody[model.Issue](t, response)
	if spec := reportSpecMarkdown(t, database, issue.Key); !strings.Contains(spec, "The Send button overlaps the box") {
		t.Fatalf("spec = %s", spec)
	}
}

// A report sent again under its key, as a page whose answer was lost sends it, files one issue.
func TestReportRepeatUnderOneKeyFilesOnce(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	pictures := []reportTestPicture{
		{name: "shot.png", contentType: "image/png", content: pngBytes("pixels")},
		{name: "after.png", contentType: "image/png", content: pngBytes("later")},
	}
	first := reportRequest(t, handler, reportFields(false), pictures, "bob", "report-1")
	again := reportRequest(t, handler, reportFields(false), pictures, "bob", "report-1")
	if first.Code != http.StatusCreated || again.Code != http.StatusCreated || !replayed(again) {
		t.Fatalf("first=%d again=%d replayed=%v body=%s", first.Code, again.Code, replayed(again), again.Body.String())
	}
	if decodeBody[model.Issue](t, first).Key != decodeBody[model.Issue](t, again).Key {
		t.Fatalf("the repeat answered another issue")
	}
	if count := countIssues(t, database); count != 1 {
		t.Fatalf("issues = %d, want 1", count)
	}
}

// A report goes to the project's default assignee, whoever reported it.
func TestReportGoesToTheProjectDefaultAssignee(t *testing.T) {
	handler, _ := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	putDefaultAssignee(t, handler, "CORE", "alice")
	response := reportRequest(t, handler, reportFields(false), nil, "bob", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if assignee := decodeBody[model.Issue](t, response).Assignee; assignee == nil || *assignee != "alice" {
		t.Fatalf("assignee = %v, want alice", assignee)
	}
}

// Words of exactly a message's limit are a report; a picture's name holding a NUL is refused.
func TestReportTakesWordsAtTheLimitAndRefusesANulPictureName(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	atCap := reportFields(false)
	atCap["body"] = strings.Repeat("a", contracts.DispatchBodyMax)
	if response := reportRequest(t, handler, atCap, nil, "bob", ""); response.Code != http.StatusCreated {
		t.Fatalf("words at the limit: status=%d body=%.300s", response.Code, response.Body.String())
	}
	named := reportRequest(t, handler, reportFields(false),
		[]reportTestPicture{{encodedName: "a%00b.png", contentType: "image/png", content: pngBytes("pixels")}}, "bob", "")
	if named.Code != http.StatusBadRequest || !strings.Contains(named.Body.String(), `"code":"NUL_CHARACTER"`) {
		t.Fatalf("a NUL picture name: status=%d body=%s", named.Code, named.Body.String())
	}
	if count := countIssues(t, database); count != 1 {
		t.Fatalf("issues = %d, want the one report at the limit", count)
	}
}

// Words that leave a code fence open, as a pasted log often does, keep the pictures and the
// details after them: each is shown, and the details are a section of their own. Words the
// document cannot hold (block HTML, a malformed decision block) are kept verbatim as code rather
// than refusing the report.
func TestReportWordsThatLeaveABlockOpenKeepTheDetailsAfterThem(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	for _, words := range []string{
		"It crashes:\n```\npanic: boom",
		"It crashes:\n~~~\npanic: boom",
		"It crashes:\n<!-- left open",
		"It crashes:\n<div>\n\npanic\n\n</div>",
		"It crashes:\n:::ask{#x}\nno options",
	} {
		fields := reportFields(false)
		fields["body"] = words
		response := reportRequest(t, handler, fields,
			[]reportTestPicture{{name: "shot.png", contentType: "image/png", content: pngBytes("pixels")}}, "bob", "")
		if response.Code != http.StatusCreated {
			t.Fatalf("%q: status=%d body=%s", words, response.Code, response.Body.String())
		}
		key := decodeBody[model.Issue](t, response).Key
		spec := reportSpecMarkdown(t, database, key)
		tree, err := pmdoc.Parse(spec)
		if err != nil {
			t.Fatalf("%q: the spec does not parse: %v\n%s", words, err, spec)
		}
		var heading, picture, crashes bool
		var walk func(node *pmdoc.Node, inHeading bool)
		walk = func(node *pmdoc.Node, inHeading bool) {
			switch {
			case node.Type == "heading":
				inHeading = true
			case node.Type == "image":
				picture = true
			case node.Type == "text" && inHeading && strings.Contains(node.Text, "Where it happened"):
				heading = true
			case node.Type == "text" && strings.Contains(node.Text, "panic") || strings.Contains(node.Text, "left open") || strings.Contains(node.Text, "no options"):
				crashes = true
			}
			for _, child := range node.Children {
				walk(child, inHeading)
			}
		}
		walk(tree, false)
		if !heading || !picture || !crashes {
			t.Fatalf("%q: heading=%t picture=%t words kept=%t in\n%s", words, heading, picture, crashes, spec)
		}
	}
}

// A picture named as the spec is stored under another name, and a page address past its bound is
// cut, not refused: the browser supplies it and the person cannot shorten it.
func TestReportKeepsThePictureNamesApartFromTheSpecAndCutsALongAddress(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	fields := reportFields(false)
	fields["page"] = "https://dispatch.example/delivery?" + strings.Repeat("q", maxReportPage)
	response := reportRequest(t, handler, fields,
		[]reportTestPicture{{name: "spec.md", contentType: "image/png", content: pngBytes("pixels")}}, "bob", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	key := decodeBody[model.Issue](t, response).Key
	var names []string
	rows, err := database.Pool.Query(context.Background(), `select name from artifacts where issue_key = $1 order by name`, key)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if len(names) != 2 || names[0] != "spec-2.md" || names[1] != "spec.md" {
		t.Fatalf("names = %v, want the picture as spec-2.md beside spec.md", names)
	}
	spec := reportSpecMarkdown(t, database, key)
	if !strings.Contains(spec, "q…") || strings.Contains(spec, strings.Repeat("q", maxReportPage)) {
		t.Fatalf("the address was not cut to its bound:\n%.400s", spec)
	}
}

// Pictures that together pass one upload's 25 MiB are refused whole, 413, and nothing is filed.
func TestReportRefusesPicturesPastTheUploadLimit(t *testing.T) {
	handler, database := newReportServer(t)
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	half := pngBytes(strings.Repeat("x", maxArtifactBlobSize/2+1))
	response := reportRequest(t, handler, reportFields(false), []reportTestPicture{
		{name: "one.png", contentType: "image/png", content: half},
		{name: "two.png", contentType: "image/png", content: half},
	}, "bob", "")
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("status=%d body=%.300s, want 413 CAP_EXCEEDED", response.Code, response.Body.String())
	}
	if count := countIssues(t, database); count != 0 {
		t.Fatalf("issues = %d, want none", count)
	}
}

func TestReportTitleIsTheFirstLineCutAtAWord(t *testing.T) {
	for _, row := range []struct{ body, want string }{
		{"\n\n  The   Send button\toverlaps  \nmore", "The Send button overlaps"},
		{strings.Repeat("word ", 30), strings.TrimSpace(strings.Repeat("word ", 15)) + "…"},
		{strings.Repeat("é", 100), strings.Repeat("é", reportTitleRunes-1) + "…"},
	} {
		if got := reportTitle(row.body); got != row.want {
			t.Errorf("reportTitle(%q) = %q, want %q", row.body, got, row.want)
		}
		if runes := len([]rune(reportTitle(row.body))); runes > reportTitleRunes {
			t.Errorf("reportTitle(%q) is %d runes, past %d", row.body, runes, reportTitleRunes)
		}
	}
}

func TestReportCodeSpanHoldsAnyValueOnOneLine(t *testing.T) {
	for _, row := range []struct{ value, want string }{
		{"Mozilla/5.0", "`Mozilla/5.0`"},
		{"a`b", "``a`b``"},
		{"`edge``", "``` `edge`` ```"},
		{"line\nbreak\x00", "`line break `"},
	} {
		if got := codeSpan(row.value); got != row.want {
			t.Errorf("codeSpan(%q) = %q, want %q", row.value, got, row.want)
		}
	}
}

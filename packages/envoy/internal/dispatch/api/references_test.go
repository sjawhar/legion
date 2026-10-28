package api

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func createReferenceAPIProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %q: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

func createReferenceAPIIssue(t *testing.T, handler http.Handler, project string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": "Reference issue",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create reference issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

func createReferenceAPIComment(t *testing.T, handler http.Handler, issueKey, body string) model.Comment {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]string{
		"body": body,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create reference comment: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response)
}

func issueReferencesRequest(t *testing.T, handler http.Handler, issueKey, etag string) *httptest.ResponseRecorder {
	t.Helper()
	return issueReferencesQueryRequest(t, handler, issueKey, "", etag)
}

// issueReferencesQueryRequest reads an issue's references as alice with query as the raw query
// string ("" for none) and etag, when set, as If-None-Match.
func issueReferencesQueryRequest(t *testing.T, handler http.Handler, issueKey, query, etag string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/issues/" + issueKey + "/references"
	if query != "" {
		target += "?" + query
	}
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestIssueReferencesClosureDepthViaAndETag(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	diagram := createProjectDocument(t, handler, "OPS", "Diagram PNG", "# Diagram\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\ndispatch://OPS/artifact/diagram-png\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")

	var crossProjectReferences int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'artifact' and to_id = $2
	`, design.ID, diagram.RefKey).Scan(&crossProjectReferences); err != nil {
		t.Fatalf("read cross-project reference: %v", err)
	}
	if crossProjectReferences != 1 {
		t.Fatalf("cross-project reference rows = %d; want 1", crossProjectReferences)
	}

	first := issueReferencesRequest(t, handler, issue.Key, "")
	if first.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", first.Code, first.Body.String())
	}
	firstValue := decodeBody[model.IssueReferences](t, first)
	if firstValue.Truncated || len(firstValue.Members) != 2 {
		t.Fatalf("issue references = %#v; want two untruncated members", firstValue)
	}
	firstMember, secondMember := firstValue.Members[0], firstValue.Members[1]
	if firstMember.Artifact.ID != design.ID || firstMember.Depth != 1 || firstMember.Via != (model.ReferenceVia{Kind: "comment", ID: comment.ID}) {
		t.Fatalf("first reference member = %#v; want Design notes through comment %q", firstMember, comment.ID)
	}
	if secondMember.Artifact.ID != diagram.ID || secondMember.Artifact.Project != "OPS" || secondMember.Depth != 2 || secondMember.Via != (model.ReferenceVia{Kind: "artifact", ID: design.ID}) {
		t.Fatalf("second reference member = %#v; want cross-project Diagram PNG through Design notes", secondMember)
	}
	firstETag := first.Header().Get("ETag")
	if firstETag == "" {
		t.Fatal("issue references response has no ETag")
	}

	notModified := issueReferencesRequest(t, handler, issue.Key, firstETag)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 || notModified.Header().Get("ETag") != firstETag {
		t.Fatalf("conditional issue references: status=%d etag=%q body=%q", notModified.Code, notModified.Header().Get("ETag"), notModified.Body.String())
	}

	updated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/artifacts", map[string]string{
		"name": "Design notes", "content": "# Design\nNo references\n",
	}, "alice")
	if updated.Code != http.StatusCreated {
		t.Fatalf("update Design notes: status=%d body=%s", updated.Code, updated.Body.String())
	}
	second := issueReferencesRequest(t, handler, issue.Key, firstETag)
	if second.Code != http.StatusOK {
		t.Fatalf("read changed issue references: status=%d body=%s", second.Code, second.Body.String())
	}
	secondValue := decodeBody[model.IssueReferences](t, second)
	if secondValue.Truncated || len(secondValue.Members) != 1 || secondValue.Members[0].Artifact.ID != design.ID {
		t.Fatalf("references after removing link = %#v; want only Design notes", secondValue)
	}
	if second.Header().Get("ETag") == firstETag {
		t.Fatal("reference ETag did not change after linked document version changed")
	}
}

func TestIssueReferencesExcludeOwnArtifactsAndDanglingTargets(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://"+issue.Key+"/spec dispatch://CORE/artifact/ghost")

	response := issueReferencesRequest(t, handler, issue.Key, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.IssueReferences](t, response)
	if value.Truncated || len(value.Members) != 0 {
		t.Fatalf("issue references = %#v; want no own or dangling artifacts", value)
	}
}

func TestIssueReferencesTruncateBeyondDepthEight(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	for index := 1; index <= 10; index++ {
		name := "Document " + strconv.Itoa(index)
		content := "# " + name + "\n"
		if index < 10 {
			content += "dispatch://CORE/artifact/document-" + strconv.Itoa(index+1) + "\n"
		}
		createProjectDocument(t, handler, "CORE", name, content)
	}
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/document-1")

	response := issueReferencesRequest(t, handler, issue.Key, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get truncated issue references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.IssueReferences](t, response)
	if !value.Truncated || len(value.Members) != 8 {
		t.Fatalf("truncated issue references = %#v; want eight members with truncation", value)
	}
	for index, member := range value.Members {
		if member.Depth != index+1 {
			t.Fatalf("member %d depth = %d; want %d", index, member.Depth, index+1)
		}
	}
}

// referenceAuthorLists is the cycle of stored author lists the members' versions carry: JSON null
// and an empty list kept apart, several actors in one list, and actors with an origin, an owner
// and a service.
var referenceAuthorLists = []string{
	`null`,
	`[]`,
	`[{"kind":"human","id":"alice"}]`,
	`[{"kind":"human","id":"alice"},{"kind":"session","id":"session-a"}]`,
	`[{"kind":"session","id":"session-b","origin":{"host":"example-host-a","machine":"example-machine-a","cwd":"/work/example","session_title":"Example session"}}]`,
	`[{"kind":"human","id":"bob"}]`,
	`[{"kind":"human","id":"carol","owner":"alice"}]`,
	`[{"kind":"service","id":"service-a","service":"example-service"}]`,
	`[{"kind":"session","id":"session-c"}]`,
	`[{"kind":"human","id":"bob"},{"kind":"human","id":"alice"}]`,
	`[{"kind":"session","id":"session-a"}]`,
	`[{"kind":"human","id":"dave"}]`,
}

// seedReferenceDocumentHistory gives a document versions 2 through versions, rewrites version 1's
// authors, and sets its creator. Version n stores referenceAuthorLists[((n-1)/2+offset) mod len],
// so consecutive versions repeat a list. Every version keeps version 1's markdown, so the live
// document still matches its latest version and a settlement writes no version of its own.
func seedReferenceDocumentHistory(t *testing.T, database *store.Store, artifactID string, versions, offset int, createdBy string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `update artifacts set created_by = $2::jsonb where id = $1`, artifactID, createdBy); err != nil {
		t.Fatalf("set document creator: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		update artifact_versions set authors = (($2::text[])[($3::int % cardinality($2::text[])) + 1])::jsonb
		where artifact_id = $1 and number = 1
	`, artifactID, referenceAuthorLists, offset); err != nil {
		t.Fatalf("set first version authors: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, named, summary, created_at, doc_update_version)
		select first.artifact_id, n, first.markdown,
		       (($2::text[])[(((n - 1) / 2 + $3::int) % cardinality($2::text[])) + 1])::jsonb,
		       n % 7 = 0, case when n % 3 = 0 then 'Summary ' || n end,
		       first.created_at + n * interval '1 millisecond', first.doc_update_version
		from artifact_versions first, generate_series(2, $4::int) n
		where first.artifact_id = $1 and first.number = 1
	`, artifactID, referenceAuthorLists, offset, versions); err != nil {
		t.Fatalf("insert document versions: %v", err)
	}
}

// referenceDocument is one document a referenceFixture cites: its name, how many versions
// seedReferenceDocumentHistory gives it, the offset of its author lists and its creator.
type referenceDocument struct {
	name      string
	versions  int
	offset    int
	createdBy string
}

// referenceFixtureDocuments is how many documents a referenceFixture's comment cites: production's
// median closure is 158 members.
const referenceFixtureDocuments = 160

// referenceFixtureCreators are the creators of a referenceFixture's documents beyond its first three.
var referenceFixtureCreators = []string{
	`{"kind":"human","id":"alice"}`,
	`{"kind":"session","id":"session-d","origin":{"host":"example-host-c","machine":"example-machine-c","cwd":"/work/example-c","session_title":"Writer session"}}`,
	`{"kind":"human","id":"erin","owner":"alice"}`,
	`{"kind":"service","id":"service-c","service":"example-service-c"}`,
	`{"kind":"session","id":"session-e"}`,
}

// referenceFixture is an issue whose one comment cites referenceFixtureDocuments OPS documents,
// each with a seeded history: one of 1 version, one of 40 whose versions 1-2 store JSON null
// authors and 3-4 an empty list, one of 3,217, and the rest of 1 to 40 versions with varied
// author offsets and creators: a full answer of over 1 MB.
type referenceFixture struct {
	handler  http.Handler
	database *store.Store
	served   *server
	issueKey string
	// documents are the cited documents in citation order, as created.
	documents []model.Artifact
	// versions is each cited document's version count, by artifact id.
	versions map[string]int
}

func newReferenceFixture(t *testing.T) referenceFixture {
	t.Helper()
	started := time.Now()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	documents := []referenceDocument{
		{"Single version", 1, 3, `{"kind":"human","id":"creator-human"}`},
		{"Forty versions", 40, 0, `{"kind":"session","id":"creator-session","origin":{"host":"example-host-b","machine":"example-machine-b","cwd":"/work/example-b","session_title":"Creator session"}}`},
		{"Long history", 3217, 5, `{"kind":"service","id":"creator-service","service":"example-service-b"}`},
	}
	for index := 0; len(documents) < referenceFixtureDocuments; index++ {
		documents = append(documents, referenceDocument{
			name:      "Cited document " + strconv.Itoa(index+1),
			versions:  1 + index%40,
			offset:    (index * 5) % len(referenceAuthorLists),
			createdBy: referenceFixtureCreators[index%len(referenceFixtureCreators)],
		})
	}
	fixture := referenceFixture{handler: handler, database: database, served: served, issueKey: issue.Key, versions: map[string]int{}}
	citations := make([]string, 0, len(documents))
	totalVersions := 0
	for _, document := range documents {
		artifact := createProjectDocument(t, handler, "OPS", document.name, "# "+document.name+"\n")
		seedReferenceDocumentHistory(t, database, artifact.ID, document.versions, document.offset, document.createdBy)
		fixture.documents = append(fixture.documents, artifact)
		fixture.versions[artifact.ID] = document.versions
		totalVersions += document.versions
		citations = append(citations, "dispatch://OPS/artifact/"+artifact.Slug)
	}
	createReferenceAPIComment(t, handler, issue.Key, strings.Join(citations, "\n"))
	t.Logf("reference fixture: %d documents, %d versions, built in %s", len(documents), totalVersions, time.Since(started).Round(time.Millisecond))
	return fixture
}

// Every member of an issue's references carries what the per-document read (loadArtifact)
// answers for its document, field for field, but for the approval that read attaches and the
// references answer leaves out: the document's own creator and every version with its authors,
// JSON null and an empty list kept apart.
func TestIssueReferencesMembersCarryTheirDocumentsOwnCreatorAndVersionAuthors(t *testing.T) {
	fixture := newReferenceFixture(t)
	ctx := context.Background()

	started := time.Now()
	response := issueReferencesRequest(t, fixture.handler, fixture.issueKey, "")
	elapsed := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.Bytes()
	var value struct {
		Members []struct {
			Artifact json.RawMessage `json:"artifact"`
		} `json:"members"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode issue references: %v", err)
	}
	if len(value.Members) != len(fixture.versions) {
		t.Fatalf("issue references members = %d; want %d", len(value.Members), len(fixture.versions))
	}
	wantVersions := maps.Clone(fixture.versions)
	totalVersions := 0
	for _, member := range value.Members {
		var got map[string]json.RawMessage
		if err := json.Unmarshal(member.Artifact, &got); err != nil {
			t.Fatalf("decode member artifact: %v", err)
		}
		var id string
		if err := json.Unmarshal(got["id"], &id); err != nil {
			t.Fatalf("decode member artifact id %s: %v", got["id"], err)
		}
		want, found := wantVersions[id]
		if !found {
			t.Fatalf("unexpected or repeated reference member %q", id)
		}
		delete(wantVersions, id)

		oracle, err := fixture.served.loadArtifact(ctx, fixture.database.Pool, id)
		if err != nil {
			t.Fatalf("load artifact %q: %v", id, err)
		}
		if len(oracle.Versions) != want {
			t.Fatalf("stored versions of %q = %d; want %d", id, len(oracle.Versions), want)
		}
		totalVersions += len(oracle.Versions)
		if oracle.Approval == nil {
			t.Fatalf("loadArtifact of %q attached no approval; the check that the references answer leaves it out would pass vacuously", id)
		}
		if approval, carried := got["approval"]; carried {
			t.Fatalf("member %q carries approval %s; the references answer carries none", id, approval)
		}

		wantCreatorJSON, err := json.Marshal(oracle.CreatedBy)
		if err != nil {
			t.Fatalf("encode creator: %v", err)
		}
		gotCreatorJSON, err := json.Marshal(got["created_by"])
		if err != nil {
			t.Fatalf("encode member creator: %v", err)
		}
		if !bytes.Equal(gotCreatorJSON, wantCreatorJSON) {
			t.Fatalf("member %q created_by = %s; want %s", id, gotCreatorJSON, wantCreatorJSON)
		}
		wantVersionsJSON, err := json.Marshal(oracle.Versions)
		if err != nil {
			t.Fatalf("encode versions: %v", err)
		}
		gotVersionsJSON, err := json.Marshal(got["versions"])
		if err != nil {
			t.Fatalf("encode member versions: %v", err)
		}
		if !bytes.Equal(gotVersionsJSON, wantVersionsJSON) {
			t.Fatalf("member %q versions differ from loadArtifact's:\n got %.600s\nwant %.600s", id, gotVersionsJSON, wantVersionsJSON)
		}

		oracle.Approval = nil
		oracleJSON, err := json.Marshal(oracle)
		if err != nil {
			t.Fatalf("encode artifact: %v", err)
		}
		var wantFields map[string]json.RawMessage
		if err := json.Unmarshal(oracleJSON, &wantFields); err != nil {
			t.Fatalf("decode encoded artifact: %v", err)
		}
		for _, field := range slices.Sorted(maps.Keys(wantFields)) {
			if gotField, present := got[field]; !present || !bytes.Equal(gotField, wantFields[field]) {
				t.Fatalf("member %q field %q = %.600s (present %t); loadArtifact answers %.600s", id, field, gotField, present, wantFields[field])
			}
		}
		for field := range got {
			if _, known := wantFields[field]; !known {
				t.Fatalf("member %q carries field %q = %.600s that loadArtifact does not answer", id, field, got[field])
			}
		}
	}
	if len(wantVersions) != 0 {
		t.Fatalf("documents missing from the references answer: %v", slices.Sorted(maps.Keys(wantVersions)))
	}
	if totalVersions < 2205 {
		t.Fatalf("members carry %d versions in all; want at least 2205", totalVersions)
	}
	if len(body) < 1_000_000 {
		t.Fatalf("references answer is %d bytes; want at least 1,000,000", len(body))
	}
	// Stored JSON null authors and a stored empty list are told apart in the answer.
	if !bytes.Contains(body, []byte(`"authors":null`)) || !bytes.Contains(body, []byte(`"authors":[]`)) {
		t.Fatalf("references answer lacks %q or %q", `"authors":null`, `"authors":[]`)
	}
	t.Logf("references answer: %d members, %d versions, %d bytes, served in %s", len(value.Members), totalVersions, len(body), elapsed.Round(time.Millisecond))
}

// ?versions=latest answers the full answer with each member's versions cut to its latest one,
// under an ETag of its own that a conditional read of the trimmed answer matches, and that
// changes, like the full answer's, when a cited document gains a version.
func TestIssueReferencesLatestVersionsAnswerIsTheFullAnswerCutToEachLatestVersion(t *testing.T) {
	fixture := newReferenceFixture(t)

	full := issueReferencesRequest(t, fixture.handler, fixture.issueKey, "")
	if full.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", full.Code, full.Body.String())
	}
	started := time.Now()
	latest := issueReferencesQueryRequest(t, fixture.handler, fixture.issueKey, "versions=latest", "")
	elapsed := time.Since(started)
	if latest.Code != http.StatusOK {
		t.Fatalf("get latest issue references: status=%d body=%s", latest.Code, latest.Body.String())
	}
	t.Logf("full answer %d bytes, latest answer %d bytes, served in %s", full.Body.Len(), latest.Body.Len(), elapsed.Round(time.Millisecond))

	fullValue := decodeBody[model.IssueReferences](t, full)
	latestValue := decodeBody[model.IssueReferences](t, latest)
	if len(fullValue.Members) != len(fixture.versions) || len(latestValue.Members) != len(fullValue.Members) {
		t.Fatalf("members: full %d, latest %d; want %d each", len(fullValue.Members), len(latestValue.Members), len(fixture.versions))
	}
	for index := range fullValue.Members {
		versions := fullValue.Members[index].Artifact.Versions
		fullValue.Members[index].Artifact.Versions = versions[len(versions)-1:]
		if got := len(latestValue.Members[index].Artifact.Versions); got != 1 {
			t.Fatalf("latest member %d (%s) carries %d versions; want 1", index, latestValue.Members[index].Artifact.ID, got)
		}
	}
	encode := func(value model.IssueReferences) []byte {
		var buffer bytes.Buffer
		if err := json.NewEncoder(&buffer).Encode(value); err != nil {
			t.Fatalf("encode issue references: %v", err)
		}
		return buffer.Bytes()
	}
	if cut, trimmed := encode(fullValue), encode(latestValue); !bytes.Equal(cut, trimmed) {
		t.Fatalf("latest answer differs from the full answer cut to each latest version:\n got %.1000s\nwant %.1000s", trimmed, cut)
	}

	fullETag, latestETag := full.Header().Get("ETag"), latest.Header().Get("ETag")
	if latestETag == "" || latestETag == fullETag {
		t.Fatalf("latest ETag = %q, full ETag = %q; want a non-empty latest ETag of its own", latestETag, fullETag)
	}
	notModified := issueReferencesQueryRequest(t, fixture.handler, fixture.issueKey, "versions=latest", latestETag)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 || notModified.Header().Get("ETag") != latestETag {
		t.Fatalf("conditional latest issue references: status=%d etag=%q body=%.200q", notModified.Code, notModified.Header().Get("ETag"), notModified.Body.String())
	}

	// A new version of the forty-version document, which the comment still cites.
	forty := fixture.documents[1]
	var lastNumber int
	for _, member := range latestValue.Members {
		if member.Artifact.ID == forty.ID {
			lastNumber = member.Artifact.Versions[0].Number
		}
	}
	if lastNumber != fixture.versions[forty.ID] {
		t.Fatalf("latest version of %q = %d; want %d", forty.Name, lastNumber, fixture.versions[forty.ID])
	}
	updated := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/projects/OPS/artifacts", map[string]string{
		"name": forty.Name, "content": "# " + forty.Name + "\nA new version\n",
	}, "alice")
	if updated.Code != http.StatusCreated {
		t.Fatalf("update %q: status=%d body=%s", forty.Name, updated.Code, updated.Body.String())
	}
	changedFull := issueReferencesRequest(t, fixture.handler, fixture.issueKey, fullETag)
	if changedFull.Code != http.StatusOK || changedFull.Header().Get("ETag") == fullETag {
		t.Fatalf("full issue references after a new version: status=%d etag=%q (was %q)", changedFull.Code, changedFull.Header().Get("ETag"), fullETag)
	}
	changedLatest := issueReferencesQueryRequest(t, fixture.handler, fixture.issueKey, "versions=latest", latestETag)
	if changedLatest.Code != http.StatusOK || changedLatest.Header().Get("ETag") == latestETag {
		t.Fatalf("latest issue references after a new version: status=%d etag=%q (was %q)", changedLatest.Code, changedLatest.Header().Get("ETag"), latestETag)
	}
	if changedLatest.Header().Get("ETag") == changedFull.Header().Get("ETag") {
		t.Fatalf("latest and full answers share ETag %q after a new version", changedFull.Header().Get("ETag"))
	}
	changedValue := decodeBody[model.IssueReferences](t, changedLatest)
	found := false
	for _, member := range changedValue.Members {
		if member.Artifact.ID != forty.ID {
			continue
		}
		found = true
		if len(member.Artifact.Versions) != 1 || member.Artifact.Versions[0].Number != lastNumber+1 {
			t.Fatalf("latest member %q versions = %#v; want only version %d", forty.Name, member.Artifact.Versions, lastNumber+1)
		}
	}
	if !found {
		t.Fatalf("latest issue references after a new version lack %q", forty.Name)
	}
}

// ?versions=all is the full answer, byte for byte and under the same ETag, as no parameter is;
// any value but all and latest is refused.
func TestIssueReferencesVersionsAllIsTheFullAnswerAndOtherValuesAreRefused(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	seedReferenceDocumentHistory(t, database, design.ID, 4, 0, `{"kind":"human","id":"alice"}`)
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/"+design.Slug)

	plain := issueReferencesRequest(t, handler, issue.Key, "")
	if plain.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", plain.Code, plain.Body.String())
	}
	if members := decodeBody[model.IssueReferences](t, plain).Members; len(members) != 1 || len(members[0].Artifact.Versions) != 4 {
		t.Fatalf("issue references members = %#v; want Design notes with 4 versions", members)
	}
	all := issueReferencesQueryRequest(t, handler, issue.Key, "versions=all", "")
	if all.Code != plain.Code ||
		all.Header().Get("ETag") != plain.Header().Get("ETag") ||
		all.Header().Get("Content-Type") != plain.Header().Get("Content-Type") ||
		!bytes.Equal(all.Body.Bytes(), plain.Body.Bytes()) {
		t.Fatalf("versions=all: status=%d etag=%q type=%q body=%s\nwant status=%d etag=%q type=%q body=%s",
			all.Code, all.Header().Get("ETag"), all.Header().Get("Content-Type"), all.Body.String(),
			plain.Code, plain.Header().Get("ETag"), plain.Header().Get("Content-Type"), plain.Body.String())
	}

	bogus := issueReferencesQueryRequest(t, handler, issue.Key, "versions=bogus", "")
	if bogus.Code != http.StatusBadRequest || !strings.Contains(bogus.Body.String(), `"code":"INVALID_QUERY"`) {
		t.Fatalf("versions=bogus: status=%d body=%s; want 400 INVALID_QUERY", bogus.Code, bogus.Body.String())
	}
}

func TestArtifactReferencesListOutgoingAndReferencedBy(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	diagram := createProjectDocument(t, handler, "OPS", "Diagram PNG", "# Diagram\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\ndispatch://OPS/artifact/diagram-png\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+design.ID+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("get artifact references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.ArtifactReferences](t, response)
	if len(value.Outgoing) != 1 || value.Outgoing[0].Kind != "artifact" || value.Outgoing[0].ToID != diagram.RefKey || value.Outgoing[0].Artifact == nil || value.Outgoing[0].Artifact.ID != diagram.ID {
		t.Fatalf("outgoing references = %#v; want resolved Diagram PNG", value.Outgoing)
	}
	if len(value.ReferencedBy) != 1 || value.ReferencedBy[0].Kind != "comment" || value.ReferencedBy[0].ID != comment.ID || value.ReferencedBy[0].IssueKey == nil || *value.ReferencedBy[0].IssueKey != issue.Key {
		t.Fatalf("referenced by = %#v; want issue comment %q", value.ReferencedBy, comment.ID)
	}
}

func TestIssueReferencesRejectUnauthenticatedRequest(t *testing.T) {
	handler := newTestHandler(t)
	// Authentication comes before the versions parameter: a value the route refuses still
	// answers 401 to a caller with no identity.
	for _, target := range []string{"/api/v1/issues/CORE-1/references", "/api/v1/issues/CORE-1/references?versions=bogus"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"NO_IDENTITY"`) {
			t.Fatalf("unauthenticated %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func graphRequest(t *testing.T, handler http.Handler, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?"+query.Encode(), nil, "alice")
}

func graphEdges(t *testing.T, handler http.Handler, query url.Values) model.GraphReferences {
	t.Helper()
	response := graphRequest(t, handler, query)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references?%s: status=%d body=%s", query.Encode(), response.Code, response.Body.String())
	}
	return decodeBody[model.GraphReferences](t, response)
}

// A message on an ACME issue that cites a LEGION ask is a backlink on that ask: the edge names
// the message with its own issue, project, and dispatch:// address, carries the message body as
// excerpt, and is stamped with the events.id of the message.created write. Read from the
// message, the same edge points out at the ask. Filters narrow it and malformed input is 400.
func TestReferencesReadBacklinksAcrossProjectsWithProvenance(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "LEGION")
	createReferenceAPIProject(t, handler, "ACME")
	legion := createReferenceAPIIssue(t, handler, "LEGION")
	acme := createReferenceAPIIssue(t, handler, "ACME")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+legion.Key+"/asks", map[string]any{
		"question": "Ship the reference graph?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)
	askRef := "dispatch://" + legion.Key + "/ask/" + ask.ID
	messageResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+acme.Key+"/messages", map[string]any{
		"body": "Decided in " + askRef + ", see also https://example.com/notes", "actor": sessionActor(),
	})
	if messageResponse.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", messageResponse.Code, messageResponse.Body.String())
	}
	message := decodeBody[model.Message](t, messageResponse)
	var messageEventID int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events where issue_key = $1 and type = 'message.created'
	`, acme.Key).Scan(&messageEventID); err != nil {
		t.Fatalf("read message.created event: %v", err)
	}

	incoming := graphEdges(t, handler, url.Values{"to": {askRef}})
	legionKey := legion.Key
	if want := (model.GraphNode{
		Kind: "ask", ID: ask.ID, IssueKey: &legionKey, Project: "LEGION", Ref: askRef,
		MadeBy: []model.Actor{ask.Author}, MadeAt: &ask.CreatedAt, ChangedBy: []model.Actor{ask.Author}, ChangedAt: &ask.CreatedAt,
		Parent: &model.NodeParent{Kind: "issue", ID: legion.Key, Ref: "dispatch://" + legion.Key},
	}); !reflect.DeepEqual(incoming.Node, want) {
		t.Fatalf("queried node = %#v; want %#v", incoming.Node, want)
	}
	if len(incoming.Edges) != 1 {
		t.Fatalf("incoming edges = %#v; want the ACME message only", incoming.Edges)
	}
	edge := incoming.Edges[0]
	acmeKey := acme.Key
	wantNode := model.GraphNode{
		Kind: "message", ID: message.ID, IssueKey: &acmeKey, Project: "ACME", Ref: "dispatch://" + acme.Key + "/message/" + message.ID,
		MadeBy: []model.Actor{message.Author}, MadeAt: &message.CreatedAt, ChangedBy: []model.Actor{message.Author}, ChangedAt: &message.CreatedAt,
		Parent: &model.NodeParent{Kind: "issue", ID: acme.Key, Ref: "dispatch://" + acme.Key},
	}
	if edge.Kind != "mentions" || edge.Direction != "in" || !reflect.DeepEqual(edge.Node, wantNode) {
		t.Fatalf("backlink edge = %#v; want mentions in from %#v", edge, wantNode)
	}
	if edge.Excerpt == nil || edge.Excerpt.BlockID != "" || edge.Excerpt.Text != message.Body {
		t.Fatalf("backlink excerpt = %#v; want the message body", edge.Excerpt)
	}
	if edge.SourceSeq == nil || *edge.SourceSeq != messageEventID || !edge.CreatedAt.Equal(message.CreatedAt) {
		t.Fatalf("backlink provenance = seq %v at %v; want event %d at %v", edge.SourceSeq, edge.CreatedAt, messageEventID, message.CreatedAt)
	}

	outgoing := graphEdges(t, handler, url.Values{"from": {"dispatch://" + acme.Key + "/message/" + message.ID}})
	if len(outgoing.Edges) != 1 || outgoing.Edges[0].Direction != "out" || outgoing.Edges[0].Kind != "mentions" || outgoing.Edges[0].Node.Ref != askRef || outgoing.Edges[0].Node.Kind != "ask" {
		t.Fatalf("outgoing edges = %#v; want one mention of the ask", outgoing.Edges)
	}
	if outgoing.Edges[0].Excerpt == nil || outgoing.Edges[0].Excerpt.Text != ask.Question {
		t.Fatalf("outgoing excerpt = %#v; want the ask question", outgoing.Edges[0].Excerpt)
	}

	if filtered := graphEdges(t, handler, url.Values{"to": {askRef}, "kind": {"replies_to,followed_by"}}); len(filtered.Edges) != 0 {
		t.Fatalf("kind filter kept %#v; want no edges", filtered.Edges)
	}
	if since := graphEdges(t, handler, url.Values{"to": {askRef}, "since": {strconv.FormatInt(messageEventID, 10)}}); len(since.Edges) != 0 {
		t.Fatalf("since the introducing event kept %#v; want no edges", since.Edges)
	}
	if since := graphEdges(t, handler, url.Values{"to": {askRef}, "since": {strconv.FormatInt(messageEventID-1, 10)}}); len(since.Edges) != 1 {
		t.Fatalf("since the prior event = %#v; want the backlink", since.Edges)
	}

	for name, tc := range map[string]struct {
		query  url.Values
		status int
		code   string
	}{
		"neither to nor from": {url.Values{}, http.StatusBadRequest, "INVALID_REFERENCE"},
		"both to and from":    {url.Values{"to": {askRef}, "from": {askRef}}, http.StatusBadRequest, "INVALID_REFERENCE"},
		"not a reference":     {url.Values{"to": {"LEGION-1 spec"}}, http.StatusBadRequest, "INVALID_REFERENCE"},
		"unknown kind":        {url.Values{"to": {askRef}, "kind": {"mentions,linked"}}, http.StatusBadRequest, "INVALID_KIND"},
		"since not an id":     {url.Values{"to": {askRef}, "since": {"yesterday"}}, http.StatusBadRequest, "INVALID_SINCE"},
		"missing node":        {url.Values{"to": {"dispatch://" + legion.Key + "/ask/00000000-0000-0000-0000-000000000000"}}, http.StatusNotFound, "NOT_FOUND"},
		"missing document":    {url.Values{"from": {"dispatch://LEGION/artifact/ghost"}}, http.StatusNotFound, "NOT_FOUND"},
	} {
		response := graphRequest(t, handler, tc.query)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%s: status=%d body=%s; want %d %s", name, response.Code, response.Body.String(), tc.status, tc.code)
		}
	}
}

// An issue's backlinks are its mentions and its structure in one list: the child issue
// (child_of), its own documents (attached_to), and a document elsewhere that cites it, listed
// from the block holding the mention, whose excerpt is that block. Read from the citing document
// the same mention points out at the issue; read from the issue's spec the structural edge points
// out at it.
func TestReferencesIncludeStructuralEdgesAndDocumentBlockExcerpts(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	parent := createReferenceAPIIssue(t, handler, "CORE")
	childResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Child work", "parent": parent.Key,
	}, "alice")
	if childResponse.Code != http.StatusCreated {
		t.Fatalf("create child issue: status=%d body=%s", childResponse.Code, childResponse.Body.String())
	}
	child := decodeBody[model.Issue](t, childResponse)
	design := createProjectDocument(t, handler, "OPS", "Design notes", "# Design\n\nIntroduction paragraph.\n\nThe plan lives in dispatch://"+parent.Key+" and nowhere else.\n")

	incoming := graphEdges(t, handler, url.Values{"to": {"dispatch://" + parent.Key}})
	byKind := make(map[string]model.GraphEdge, len(incoming.Edges))
	for _, edge := range incoming.Edges {
		byKind[edge.Kind] = edge
	}
	if len(incoming.Edges) != 3 || len(byKind) != 3 {
		t.Fatalf("issue edges = %#v; want one mention, one child_of, one attached_to", incoming.Edges)
	}
	for index := 1; index < len(incoming.Edges); index++ {
		if incoming.Edges[index].CreatedAt.After(incoming.Edges[index-1].CreatedAt) {
			t.Fatalf("edges are not newest first: %#v", incoming.Edges)
		}
	}
	mention := byKind["mentions"]
	if mention.Node.Kind != "block" || mention.Node.Document != "dispatch://OPS/artifact/design-notes" || mention.Node.Project != "OPS" {
		t.Fatalf("mention node = %#v; want a block of the OPS design document", mention.Node)
	}
	if mention.Excerpt == nil || mention.Excerpt.BlockID == "" || mention.Excerpt.Text != "The plan lives in dispatch://"+parent.Key+" and nowhere else." {
		t.Fatalf("mention excerpt = %#v; want the containing paragraph block with its id", mention.Excerpt)
	}
	if mention.SourceSeq == nil {
		t.Fatalf("document mention has no source_seq: %#v", mention)
	}
	childOf := byKind["child_of"]
	if childOf.Node.Kind != "issue" || childOf.Node.ID != child.Key || childOf.Node.Ref != "dispatch://"+child.Key || childOf.Excerpt == nil || childOf.Excerpt.Text != "Child work" || childOf.SourceSeq != nil {
		t.Fatalf("child_of edge = %#v; want the child issue with its title and no source_seq", childOf)
	}
	attached := byKind["attached_to"]
	if attached.Node.Kind != "artifact" || attached.Node.ID != parent.PrimaryArtifactID || attached.Node.Ref != "dispatch://"+parent.Key+"/spec" || attached.Excerpt == nil || attached.Excerpt.Text != "spec.md" {
		t.Fatalf("attached_to edge = %#v; want the issue's spec", attached)
	}

	outgoing := graphEdges(t, handler, url.Values{"from": {"dispatch://OPS/artifact/design-notes"}})
	if outgoing.Node.Kind != "artifact" || outgoing.Node.ID != design.ID {
		t.Fatalf("document node = %#v; want %s resolved from its ref_key", outgoing.Node, design.ID)
	}
	if len(outgoing.Edges) != 1 || outgoing.Edges[0].Kind != "mentions" || outgoing.Edges[0].Node.Ref != "dispatch://"+parent.Key || outgoing.Edges[0].Excerpt == nil || outgoing.Edges[0].Excerpt.Text != "Reference issue" {
		t.Fatalf("document links = %#v; want one mention of the parent issue with its title", outgoing.Edges)
	}

	spec := graphEdges(t, handler, url.Values{"from": {"dispatch://" + parent.Key + "/spec"}})
	if len(spec.Edges) != 1 || spec.Edges[0].Kind != "attached_to" || spec.Edges[0].Direction != "out" || spec.Edges[0].Node.Ref != "dispatch://"+parent.Key {
		t.Fatalf("spec links = %#v; want attached_to the issue", spec.Edges)
	}
	specBacklinks := graphEdges(t, handler, url.Values{"to": {"dispatch://" + parent.Key + "/spec"}})
	if specBacklinks.Node.ID != parent.PrimaryArtifactID || len(specBacklinks.Edges) != 0 {
		t.Fatalf("spec backlinks = %#v; want the spec node with no edges", specBacklinks)
	}
}

// insertUnknownKindEdges plants refs rows whose other end is a node kind this build does not
// know, as a newer build sharing the database would write them.
func insertUnknownKindEdges(t *testing.T, database *store.Store, edges [][4]string) {
	t.Helper()
	for _, edge := range edges {
		if _, err := database.Pool.Exec(context.Background(), `
			insert into refs (from_kind, from_id, to_kind, to_id) values ($1, $2, $3, $4)
		`, edge[0], edge[1], edge[2], edge[3]); err != nil {
			t.Fatalf("insert reference %v: %v", edge, err)
		}
	}
}

// An edge whose other end is a node kind this build cannot address is one a newer build wrote:
// the graph read skips it in both directions instead of answering 500, and the issue header's
// count is the number of edges its panel lists, not one more.
func TestReferencesAnswerAroundAnEdgeOfAKindThisBuildCannotAddress(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	source := createReferenceAPIIssue(t, handler, "OPS")
	comment := createReferenceAPIComment(t, handler, source.Key, "see dispatch://"+target.Key)
	commentRef := "dispatch://" + source.Key + "/comment/" + comment.ID
	insertUnknownKindEdges(t, database, [][4]string{
		{"future", "x#y", "issue", target.Key},
		{"comment", comment.ID, "future", "x#y"},
	})

	// The target's own spec is attached to it and the comment cites it: two edges this build
	// can address, and the header counts the same edges the panel lists.
	incoming := graphRequest(t, handler, url.Values{"to": {"dispatch://" + target.Key}})
	if incoming.Code != http.StatusOK {
		t.Errorf("GET references to %s: status=%d body=%s", target.Key, incoming.Code, incoming.Body.String())
	} else {
		edges := decodeBody[model.GraphReferences](t, incoming).Edges
		byKind := make(map[string]model.GraphEdge, len(edges))
		for _, edge := range edges {
			byKind[edge.Kind] = edge
		}
		mention, attached := byKind["mentions"], byKind["attached_to"]
		if len(edges) != 2 || mention.Node.Kind != "comment" || mention.Node.ID != comment.ID || attached.Node.Kind != "artifact" || attached.Node.ID != target.PrimaryArtifactID {
			t.Errorf("backlinks of %s = %#v; want the comment's mention and the attached spec, nothing of kind future", target.Key, edges)
		}
	}

	outgoing := graphRequest(t, handler, url.Values{"from": {commentRef}})
	if outgoing.Code != http.StatusOK {
		t.Errorf("GET references from %s: status=%d body=%s", commentRef, outgoing.Code, outgoing.Body.String())
	} else {
		edges := decodeBody[model.GraphReferences](t, outgoing).Edges
		if len(edges) != 1 || edges[0].Kind != "mentions" || edges[0].Node.Kind != "issue" || edges[0].Node.ID != target.Key {
			t.Errorf("links of %s = %#v; want only the mention of %s, nothing of kind future", commentRef, edges, target.Key)
		}
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+target.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	if count := decodeBody[struct {
		ReferencedByCount int `json:"referenced_by_count"`
	}](t, detail).ReferencedByCount; count != 2 {
		t.Errorf("issue %q referenced_by_count = %d; want 2, the edges its panel lists", target.Key, count)
	}
}

// A document's references read skips an edge of a node kind this build cannot address on both
// sides: its referenced-by list is the comment that cites it, and its outgoing list is what its
// text cites, the known issue and the missing document both unresolved as before.
func TestArtifactReferencesSkipANodeKindThisBuildCannotAddress(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n\nThe plan lives in dispatch://"+issue.Key+" and dispatch://CORE/artifact/missing.\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")
	insertUnknownKindEdges(t, database, [][4]string{
		{"artifact", design.ID, "future", "x#y"},
		{"future", "x#y", "artifact", design.RefKey},
	})

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+design.ID+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("get artifact references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.ArtifactReferences](t, response)
	if want := []model.OutgoingReference{
		{Kind: "artifact", ToID: "CORE/missing"},
		{Kind: "issue", ToID: issue.Key},
	}; !reflect.DeepEqual(value.Outgoing, want) {
		t.Errorf("outgoing references = %#v; want the unresolved issue and missing document, nothing of kind future", value.Outgoing)
	}
	if len(value.ReferencedBy) != 1 || value.ReferencedBy[0].Kind != "comment" || value.ReferencedBy[0].ID != comment.ID {
		t.Errorf("referenced by = %#v; want only comment %q", value.ReferencedBy, comment.ID)
	}
}

// A spec seeded at issue creation that cites an issue in bold, another project's issue in a code
// span and a third project's issue as an autolink, which the spec stores as a link whose text is
// its target, mentions all three. The source of each mention is the spec document, not the issue
// that owns it: the issue node writes no mention, and each target's backlink names the spec's
// block holding it.
func TestSeededSpecCitationsInMarkdownDelimitersAreDocumentMentions(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "LEGION")
	createReferenceAPIProject(t, handler, "ACME")
	createReferenceAPIProject(t, handler, "CORE")
	sameProject := createReferenceAPIIssue(t, handler, "LEGION")
	otherProject := createReferenceAPIIssue(t, handler, "ACME")
	autolinked := createReferenceAPIIssue(t, handler, "CORE")
	response := createIssueRequest(t, handler, map[string]any{
		"project": "LEGION", "title": "Due dates",
		"spec": "Tied to **dispatch://" + sameProject.Key + "** and `dispatch://" + otherProject.Key + "`, " +
			"see <dispatch://" + autolinked.Key + ">.\n",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue with spec: status=%d body=%s", response.Code, response.Body.String())
	}
	citing := decodeBody[model.Issue](t, response)
	specRef := "dispatch://" + citing.Key + "/spec"
	targets := []model.Issue{sameProject, otherProject, autolinked}

	mentioned := map[string]bool{}
	for _, edge := range graphEdges(t, handler, url.Values{"from": {specRef}, "kind": {"mentions"}}).Edges {
		mentioned[edge.Node.Ref] = true
	}
	want := map[string]bool{}
	for _, target := range targets {
		want["dispatch://"+target.Key] = true
	}
	if !reflect.DeepEqual(mentioned, want) {
		t.Fatalf("spec mentions = %v; want %v", mentioned, want)
	}
	if fromIssue := graphEdges(t, handler, url.Values{"from": {"dispatch://" + citing.Key}, "kind": {"mentions"}}); len(fromIssue.Edges) != 0 {
		t.Fatalf("issue node mentions = %#v; want none, the spec document writes them", fromIssue.Edges)
	}
	for _, target := range targets {
		backlinks := graphEdges(t, handler, url.Values{"to": {"dispatch://" + target.Key}, "kind": {"mentions"}}).Edges
		if len(backlinks) != 1 || backlinks[0].Node.Kind != "block" || backlinks[0].Node.Document != specRef {
			t.Fatalf("%s backlinks = %#v; want one mention from a block of %s", target.Key, backlinks, specRef)
		}
	}
}

// An item id that decodes to a NUL names nothing. The index binds ids as `text[]`, and Postgres
// refuses a NUL there, so a reader that decoded one failed the write holding it: the comment, the
// seeded spec and the author's edit answered 500, and a rebuild stopped at the first stored body
// that held one.
func TestCitationsOfAnIDDecodingToNULDoNotFailTheWrite(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	target := createReferenceAPIIssue(t, handler, "CORE")
	body := "See dispatch://" + target.Key + "/comment/%00, https://dispatch.example/issues/" + target.Key +
		"/comments/%00 and https://dispatch.example/issues/" + target.Key + "/spec?comment=%2500.\n"

	comment := createReferenceAPIComment(t, handler, target.Key, body)
	commentRef := "dispatch://" + target.Key + "/comment/" + comment.ID
	if edges := graphEdges(t, handler, url.Values{"from": {commentRef}, "kind": {"mentions"}}).Edges; len(edges) != 0 {
		t.Fatalf("comment mentions = %#v; want none", edges)
	}
	if seeded := createIssueRequest(t, handler, map[string]any{"project": "CORE", "title": "Seeded", "spec": body}); seeded.Code != http.StatusCreated {
		t.Fatalf("create issue whose spec cites a NUL id: status=%d body=%s", seeded.Code, seeded.Body.String())
	}
	plain := createReferenceAPIComment(t, handler, target.Key, "Plain comment.")
	if edit := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+plain.ID, map[string]string{
		"body": body,
	}, "alice"); edit.Code != http.StatusOK {
		t.Fatalf("edit a comment to cite a NUL id: status=%d body=%s", edit.Code, edit.Body.String())
	}
	if _, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example"); err != nil {
		t.Fatalf("rebuild refs over bodies citing a NUL id: %v", err)
	}
}

type refRow struct {
	FromKind, FromID, ToKind, ToID, Kind string
	SourceSeq                            *int64
	CreatedAt                            string
}

func readRefRows(t *testing.T, database *store.Store) []refRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select from_kind, from_id, to_kind, to_id, kind, source_seq, created_at::text
		from refs order by from_kind, from_id, to_kind, to_id
	`)
	if err != nil {
		t.Fatalf("read refs: %v", err)
	}
	defer rows.Close()
	var result []refRow
	for rows.Next() {
		var row refRow
		if err := rows.Scan(&row.FromKind, &row.FromID, &row.ToKind, &row.ToID, &row.Kind, &row.SourceSeq, &row.CreatedAt); err != nil {
			t.Fatalf("scan ref: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refs: %v", err)
	}
	return result
}

// The text is the truth and refs is its index: after the index is doctored (an edge deleted, a
// stale edge planted), a rebuild converges on exactly the edge set the live writes produced,
// keeping the provenance of every edge it did not have to recreate, and a second rebuild is a
// no-op.
func TestRebuildRefsConvergesOnTheLiveIndex(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	createProjectDocument(t, handler, "OPS", "Diagram", "# Diagram\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n\nSee dispatch://OPS/artifact/diagram and https://dispatch.example/issues/"+issue.Key+".\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes and dispatch://"+issue.Key+"/spec")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Merge dispatch://CORE/artifact/design-notes?", "options": []map[string]string{{"label": "Yes"}},
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Read dispatch://" + issue.Key + "/comment/" + comment.ID, "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", response.Code, response.Body.String())
	}
	live := readRefRows(t, database)
	if len(live) != 6 {
		t.Fatalf("live index = %#v; want six edges", live)
	}
	for _, row := range live {
		if row.SourceSeq == nil {
			t.Fatalf("live edge %#v has no source_seq", row)
		}
	}

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`delete from refs where from_kind = 'comment' and to_kind = 'artifact' and to_id = $1`, []any{issue.Key + "/spec"}},
		{`insert into refs (from_kind, from_id, to_kind, to_id, source_seq) values ('artifact', $1, 'issue', 'ZZZ-9', 1)`, []any{design.ID}},
		{`insert into refs (from_kind, from_id, to_kind, to_id) values ('message', '00000000-0000-0000-0000-000000000000', 'issue', $1)`, []any{issue.Key}},
	} {
		if _, err := database.Pool.Exec(context.Background(), statement.sql, statement.args...); err != nil {
			t.Fatalf("doctor refs: %v", err)
		}
	}
	if doctored := readRefRows(t, database); len(doctored) != 7 {
		t.Fatalf("doctored index = %#v; want seven rows", doctored)
	}

	report, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example")
	if err != nil {
		t.Fatalf("rebuild refs: %v", err)
	}
	// Every document has a version (three, counting the two specs); the planted row from a message
	// that does not exist is the one orphan. Each of the three is marked owed a block index.
	if report != (refs.Rebuild{Documents: 3, Asks: 1, Comments: 1, Messages: 1, Orphans: 1, Edges: 6, BlocksOwed: 3}) {
		t.Fatalf("rebuild report = %#v", report)
	}
	var documents, owed int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), count(*) filter (where b.artifact_id is not null and b.version is null)
		from artifacts a left join document_block_versions b on b.artifact_id = a.id
		where a.kind = 'doc'
	`).Scan(&documents, &owed); err != nil {
		t.Fatalf("read the block index of every document: %v", err)
	}
	if documents != 3 || owed != documents {
		t.Fatalf("after rebuild-refs %d of %d documents are owed a block index, want every one", owed, documents)
	}
	rebuilt := readRefRows(t, database)
	if len(rebuilt) != len(live) {
		t.Fatalf("rebuilt index = %#v; want the live index %#v", rebuilt, live)
	}
	for index, row := range live {
		got := rebuilt[index]
		if got.FromKind != row.FromKind || got.FromID != row.FromID || got.ToKind != row.ToKind || got.ToID != row.ToID || got.Kind != row.Kind {
			t.Fatalf("rebuilt edge %d = %#v; want %#v", index, got, row)
		}
		recreated := row.FromKind == "comment" && row.ToID == issue.Key+"/spec"
		if recreated {
			if got.SourceSeq != nil || got.CreatedAt == row.CreatedAt {
				t.Fatalf("recreated edge %#v kept provenance %#v", got, row)
			}
			continue
		}
		if got.SourceSeq == nil || *got.SourceSeq != *row.SourceSeq || got.CreatedAt != row.CreatedAt {
			t.Fatalf("surviving edge %#v lost provenance %#v", got, row)
		}
	}
	if _, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example"); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if again := readRefRows(t, database); !reflect.DeepEqual(again, rebuilt) {
		t.Fatalf("second rebuild changed the index: %#v -> %#v", rebuilt, again)
	}
}

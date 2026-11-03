package api

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

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

// referenceAnswerMinVersions is the fewest versions a referenceFixture's full answer must carry
// across its members: production's median answer carries 2,205 version rows.
const referenceAnswerMinVersions = 2205

// referenceAnswerMinBytes is the smallest a referenceFixture's full answer may be: production's
// median answer is about 1.02 MB.
const referenceAnswerMinBytes = 1_000_000

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

// TestIssueReferencesAtRealSize builds one production-sized referenceFixture and runs its checks
// in order on it. The latest-versions check writes a new version, so it runs last.
func TestIssueReferencesAtRealSize(t *testing.T) {
	fixture := newReferenceFixture(t)
	t.Run("MembersCarryTheirDocumentsOwnCreatorAndVersionAuthors", func(t *testing.T) {
		checkReferenceMembersCarryTheirDocumentsOwnCreatorAndVersionAuthors(t, fixture)
	})
	t.Run("LatestVersionsAnswerIsTheFullAnswerCutToEachLatestVersion", func(t *testing.T) {
		checkReferenceLatestVersionsAnswerIsTheFullAnswerCutToEachLatestVersion(t, fixture)
	})
}

// Every member of an issue's references carries what the per-document read (loadArtifact)
// answers for its document, field for field, but for the approval that read attaches and the
// references answer leaves out: the document's own creator and every version with its authors,
// JSON null and an empty list kept apart.
func checkReferenceMembersCarryTheirDocumentsOwnCreatorAndVersionAuthors(t *testing.T, fixture referenceFixture) {
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
	if totalVersions < referenceAnswerMinVersions {
		t.Fatalf("members carry %d versions in all; want at least %d", totalVersions, referenceAnswerMinVersions)
	}
	if len(body) < referenceAnswerMinBytes {
		t.Fatalf("references answer is %d bytes; want at least %d", len(body), referenceAnswerMinBytes)
	}
	// Stored JSON null authors and a stored empty list are told apart in the answer.
	if !bytes.Contains(body, []byte(`"authors":null`)) || !bytes.Contains(body, []byte(`"authors":[]`)) {
		t.Fatalf("references answer lacks %q or %q", `"authors":null`, `"authors":[]`)
	}
	t.Logf("references answer: %d members, %d versions, %d bytes, served in %s", len(value.Members), totalVersions, len(body), elapsed.Round(time.Millisecond))
}

// ?versions=latest answers the full answer with each member's versions cut to its latest one,
// under an ETag of its own that a conditional read of the trimmed answer matches, and that
// changes, like the full answer's, when a cited document gains a version. It writes that new
// version into the fixture.
func checkReferenceLatestVersionsAnswerIsTheFullAnswerCutToEachLatestVersion(t *testing.T, fixture referenceFixture) {
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

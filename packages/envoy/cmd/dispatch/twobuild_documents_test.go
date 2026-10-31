//go:build twobuild

// The stored-document half of the two-build suite: each build reading, editing and keeping the
// references of the other's documents, over the API and the document socket
// (twobuild_harness_test.go holds the harness both halves start their builds with).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ───── stored documents ─────────────────────────────────────────────────────

// referenceServerURL is DISPATCH_SERVER_URL for the document rows: the origin the reference
// fixtures' dashboard links name (text/testdata), so both builds read those links as references.
const referenceServerURL = "https://dispatch.test"

// documentSide is one build as a writer or a reader of documents: its process, the corpus and
// reference fixtures of its own tree, its block manifest, and what its page presents on a
// document socket.
type documentSide struct {
	name     string
	process  *dispatchProcess
	corpus   string
	fixtures string
	manifest blockManifest
	page     pageParameters
}

// documentSides is the previous build's side and the current build's, each from its own tree.
func (b *twoBuilds) documentSides(t *testing.T) []documentSide {
	t.Helper()
	tree := filepath.Join("..", "..", "internal", "dispatch")
	previousManifest := readManifest(t, b.build.path("pmdoc", "schema", "blocks.json"))
	currentManifest := readManifest(t, filepath.Join(tree, "pmdoc", "schema", "blocks.json"))
	return []documentSide{
		{
			name:     b.buildName(b.previous()),
			process:  b.previous(),
			corpus:   b.build.path("pmdoc", "testdata", "corpus"),
			fixtures: b.build.path("text", "testdata"),
			manifest: previousManifest,
			page:     previousManifest.page(pageSendsCreatable(t, b.build.path("web"))),
		},
		{
			name:     b.buildName(b.current()),
			process:  b.current(),
			corpus:   filepath.Join(tree, "pmdoc", "testdata", "corpus"),
			fixtures: filepath.Join(tree, "text", "testdata"),
			manifest: currentManifest,
			// The current page sends schema_creatable (packages/dispatch/web/src/features/doc/connection.ts).
			page: currentManifest.page(true),
		},
	}
}

// elementVersions are the two numbers the manifest may give an element: the schema version that
// first reads it and the one that first creates it, each 1 where it names none.
type elementVersions struct {
	Since          *int `json:"since"`
	CreatableSince *int `json:"creatable_since"`
}

type manifestAttribute struct {
	elementVersions
	Kind           string                     `json:"kind"`
	Choices        []string                   `json:"choices"`
	ChoiceVersions map[string]elementVersions `json:"choice_versions"`
	Default        json.RawMessage            `json:"default"`
	Server         bool                       `json:"server"`
}

type manifestType struct {
	elementVersions
	Name       string                       `json:"name"`
	Content    string                       `json:"content"`
	Attributes map[string]manifestAttribute `json:"attributes"`
}

// blockManifest is a build's pmdoc/schema/blocks.json.
type blockManifest struct {
	Version int               `json:"version"`
	Nodes   []elementVersions `json:"nodes"`
	Marks   []elementVersions `json:"marks"`
	Types   []manifestType    `json:"types"`
}

func readManifest(t *testing.T, path string) blockManifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the block manifest: %v", err)
	}
	var manifest blockManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode the block manifest %s: %v", path, err)
	}
	return manifest
}

// creatable is R for the manifest: the highest since among the elements an editor of its version
// may create, by the rule the page states (schemaCreatable in
// packages/dispatch/web/src/features/doc/schema.ts) and pmdoc.SchemaCreatable computes.
func (m blockManifest) creatable() int {
	creatable := 0
	count := func(element elementVersions) {
		since, creatableSince := 1, 1
		if element.Since != nil {
			since = *element.Since
		}
		if element.CreatableSince != nil {
			creatableSince = *element.CreatableSince
		}
		if creatableSince <= m.Version {
			creatable = max(creatable, since)
		}
	}
	for _, element := range slices.Concat(m.Nodes, m.Marks) {
		count(element)
	}
	for _, typed := range m.Types {
		count(typed.elementVersions)
		for _, attribute := range typed.Attributes {
			count(attribute.elementVersions)
			for _, choice := range attribute.Choices {
				count(attribute.ChoiceVersions[choice])
			}
		}
	}
	return creatable
}

// pageParameters is what a build's page presents when it opens a document socket
// (features/doc/connection.ts): the query its provider's websocket carries, and the token its
// provider authenticates with, which is the schema version alone.
type pageParameters struct {
	query url.Values
	token string
}

func (p pageParameters) String() string { return "?" + p.query.Encode() + " with token " + p.token }

// page is what a page built with this manifest presents: schema_version, and schema_creatable
// when that build's page sends it.
func (m blockManifest) page(sendsCreatable bool) pageParameters {
	query := url.Values{"schema_version": {strconv.Itoa(m.Version)}}
	if sendsCreatable {
		query.Set("schema_creatable", strconv.Itoa(m.creatable()))
	}
	return pageParameters{query: query, token: strconv.Itoa(m.Version)}
}

// pageSendsCreatable reports whether a built web app's scripts name schema_creatable, the
// parameter a page sends from the build that adds the fleet columns on. A page of a build that
// predates them sends schema_version alone.
func pageSendsCreatable(t *testing.T, web string) bool {
	t.Helper()
	scripts, err := filepath.Glob(filepath.Join(web, "assets", "*.js"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("the previous build's web app at %s holds no assets/*.js: %v", web, err)
	}
	for _, script := range scripts {
		raw, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		if bytes.Contains(raw, []byte("schema_creatable")) {
			return true
		}
	}
	return false
}

// createDocument creates an issue in project whose spec is markdown, through p, and returns its key
// and its spec's id. It is forced past the duplicate check: the rows create many issues of one
// shape on purpose.
func createDocument(t *testing.T, p *dispatchProcess, project, title, markdown string) (string, string) {
	t.Helper()
	created := p.call(t, http.MethodPost, "/api/v1/issues",
		jsonBody(t, map[string]any{"project": project, "title": title, "spec": markdown, "force": true}), http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the document %q: %v: %s", title, err, created)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// appendParagraph appends paragraph at the end of the document through p, an edit that versions
// the document in the request's own transaction.
func appendParagraph(t *testing.T, p *dispatchProcess, artifactID, paragraph string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		jsonBody(t, map[string]any{"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": paragraph + "\n"}}}), http.StatusOK)
}

// withParagraph is render with paragraph appended as the document's last block.
func withParagraph(render, paragraph string) string {
	switch {
	case render == "":
		return paragraph + "\n"
	case strings.HasSuffix(render, "\n"):
		return render + "\n" + paragraph + "\n"
	default:
		return render + "\n\n" + paragraph + "\n"
	}
}

// The Hocuspocus frames a document socket's admission is read from (ygo provider/websocket).
const (
	hocuspocusAuth          = 2
	hocuspocusAuthToken     = 0
	hocuspocusAuthDenied    = 1
	hocuspocusAuthenticated = 2
	// writableScope is the scope an Authenticated frame names for a socket that may write.
	writableScope = "read-write"
)

// documentSocket is a document socket as a page's provider holds it: authenticated with the
// page's token, reading every frame the room sends until the connection ends.
type documentSocket struct {
	artifactID string
	scope      string
	ended      chan struct{}
	endErr     error
}

// openDocumentSocket opens server's socket for the document with page's parameters, authenticates
// as the page's provider does, and returns once the server has answered with the socket's scope.
func openDocumentSocket(t *testing.T, server *dispatchProcess, artifactID string, page pageParameters) *documentSocket {
	t.Helper()
	address := "ws://127.0.0.1:" + strconv.Itoa(server.port) + "/ws/doc/" + artifactID + "?" + page.query.Encode()
	connection, response, err := gws.DefaultDialer.Dial(address, http.Header{"X-Dispatch-User": {dispatchTestLogin}})
	if err != nil {
		status := 0
		var body []byte
		if response != nil {
			status = response.StatusCode
			body, _ = io.ReadAll(response.Body)
		}
		t.Fatalf("open the document socket %s: status %d %s: %v", address, status, body, err)
	}
	socket := &documentSocket{artifactID: artifactID, ended: make(chan struct{})}
	t.Cleanup(func() {
		_ = connection.Close()
		<-socket.ended
	})
	auth := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(hocuspocusAuth)
		encoder.WriteVarUint(hocuspocusAuthToken)
		encoder.WriteVarString(page.token)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, auth); err != nil {
		t.Fatalf("authenticate the document socket: %v", err)
	}
	scopes := make(chan string, 1)
	go func() {
		defer close(socket.ended)
		for {
			_, message, err := connection.ReadMessage()
			if err != nil {
				socket.endErr = err
				return
			}
			decoder := encoding.NewDecoder(message)
			if _, err := decoder.ReadVarString(); err != nil {
				continue
			}
			if kind, err := decoder.ReadVarUint(); err != nil || kind != hocuspocusAuth {
				continue
			}
			answer, err := decoder.ReadVarUint()
			if err != nil {
				continue
			}
			text, _ := decoder.ReadVarString()
			switch answer {
			case hocuspocusAuthenticated:
				text = "scope " + text
			case hocuspocusAuthDenied:
				text = "denied: " + text
			default:
				continue
			}
			select {
			case scopes <- text:
			default:
			}
		}
	}()
	select {
	case answer := <-scopes:
		socket.scope = strings.TrimPrefix(answer, "scope ")
	case <-socket.ended:
		t.Fatalf("the document socket for %s closed before the server answered its authentication: %v", artifactID, socket.endErr)
	case <-time.After(10 * time.Second):
		t.Fatalf("the server never answered the document socket's authentication for %s", artifactID)
	}
	return socket
}

func (s *documentSocket) closed() bool {
	select {
	case <-s.ended:
		return true
	default:
		return false
	}
}

// corpusFiles is every markdown file of a corpus directory, by name.
func corpusFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the corpus %s: %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			files = append(files, entry.Name())
		}
	}
	if len(files) == 0 {
		t.Fatalf("the corpus %s holds no markdown file", dir)
	}
	return files
}

// A document either build wrote reads and edits on the other. For each writer X and each file of
// X's own corpus: a document created on X answers /text on Y and takes an append edit there, and X
// then renders it as its first render with the appended paragraph after it; on Y, a document socket
// opened with X's page parameters is admitted writable and stays open. A version X wrote holding
// every typed block of X's manifest is approved, the answer to its decision block makes Y write the
// next version, and the approval is carried to it. A body holding every reference form of X's
// text/testdata keeps its refs rows after Y rewrites it, but for the edges a Y from before the
// unknown-kind guard drops, which it logs (requireRefsKept).
func TestTwoBuildsReadAndEditEachOthersDocuments(t *testing.T) {
	builds := newTwoBuilds(t)
	env := map[string]string{"DISPATCH_SERVER_URL": referenceServerURL}
	builds.startPrevious(t, env)
	builds.startCurrent(t, env)
	sides := builds.documentSides(t)
	for index, writer := range sides {
		reader := sides[1-index]
		t.Run(writer.name+" writes, "+reader.name+" reads", func(t *testing.T) {
			project := uniqueProjectKey(t, "DOC")
			writer.process.createProject(t, project, "Two builds")
			t.Run("corpus", func(t *testing.T) {
				requireCorpusCrossesBuilds(t, writer, reader, project)
			})
			t.Run("approval carry", func(t *testing.T) {
				requireApprovalCarriedAcrossBuilds(t, writer, reader, project)
			})
			t.Run("references", func(t *testing.T) {
				requireReferencesKeptAcrossBuilds(t, builds, writer, reader, project)
			})
		})
	}
}

// socketOpenFor is how long every socket the corpus row opened must stay open after the last one
// was admitted: long enough for a server that closes a socket it admitted to have done so.
const socketOpenFor = 3 * time.Second

func requireCorpusCrossesBuilds(t *testing.T, writer, reader documentSide, project string) {
	var sockets []*documentSocket
	files := corpusFiles(t, writer.corpus)
	for _, file := range files {
		markdown, err := os.ReadFile(filepath.Join(writer.corpus, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		_, artifactID := createDocument(t, writer.process, project, "Corpus "+file, string(markdown))
		first := writer.process.documentText(t, artifactID)
		reader.process.documentText(t, artifactID)

		paragraph := "Appended on the " + reader.name + " to " + file + "."
		appendParagraph(t, reader.process, artifactID, paragraph)
		if got, want := writer.process.documentText(t, artifactID), withParagraph(first, paragraph); got != want {
			t.Errorf("%s: after the %s appended a paragraph, the %s renders\n%q\nwant its first render with the paragraph after it\n%q",
				file, reader.name, writer.name, got, want)
		}

		socket := openDocumentSocket(t, reader.process, artifactID, writer.page)
		if socket.scope != writableScope {
			t.Errorf("%s: the %s admits a socket with the %s page's parameters (%s) as %q, want %q",
				file, reader.name, writer.name, writer.page, socket.scope, writableScope)
		}
		sockets = append(sockets, socket)
	}
	time.Sleep(socketOpenFor)
	for _, socket := range sockets {
		if socket.closed() {
			t.Errorf("the %s closed the writable socket it admitted for document %s: %v", reader.name, socket.artifactID, socket.endErr)
		}
	}
	t.Logf("%d corpus documents of the %s read, took an edit and admitted a writable socket on the %s", len(files), writer.name, reader.name)
}

// typedBlockID is the block id the typed-block document gives each typed block: tb-<type name>.
func typedBlockID(name string) string { return "tb-" + name }

// typedBlocksMarkdown is a document holding one block of every typed block the manifest declares,
// each with its id and every attribute a writer sets at its default. A decision block (the ask
// type) is given two choices, First and Second.
func typedBlocksMarkdown(t *testing.T, manifest blockManifest) string {
	t.Helper()
	blocks := []string{"# Every typed block\n"}
	for _, typed := range manifest.Types {
		attributes := []string{"#" + typedBlockID(typed.Name)}
		for _, name := range slices.Sorted(maps.Keys(typed.Attributes)) {
			attribute := typed.Attributes[name]
			if attribute.Server || len(attribute.Default) == 0 {
				continue
			}
			var value any
			if err := json.Unmarshal(attribute.Default, &value); err != nil {
				t.Fatalf("decode the default of %s.%s: %v", typed.Name, name, err)
			}
			switch value := value.(type) {
			case string:
				if value == "" {
					attributes = append(attributes, name)
				} else {
					attributes = append(attributes, fmt.Sprintf("%s=%q", name, value))
				}
			case bool:
				attributes = append(attributes, fmt.Sprintf("%s=%q", name, strconv.FormatBool(value)))
			default:
				t.Fatalf("the harness writes no %T default, as %s.%s has; teach typedBlocksMarkdown to write it", value, typed.Name, name)
			}
		}
		var content []string
		for _, token := range strings.Fields(typed.Content) {
			switch strings.TrimRight(token, "+*?") {
			case "block", "paragraph":
				content = append(content, "The "+typed.Name+" block says one thing.")
			case "bullet_list":
				content = append(content, "- First: The first choice.\n- Second: The second choice.")
			default:
				t.Fatalf("the harness writes no %q content, as the %s block takes; teach typedBlocksMarkdown to write it", token, typed.Name)
			}
		}
		blocks = append(blocks, ":::"+typed.Name+"{"+strings.Join(attributes, " ")+"}\n"+strings.Join(content, "\n\n")+"\n:::\n")
	}
	return strings.Join(blocks, "\n")
}

// servedReview is a review as GET /api/v1/artifacts/{id}/reviews lists it, newest first.
type servedReview struct {
	Version int         `json:"version"`
	State   string      `json:"state"`
	Actor   model.Actor `json:"actor"`
}

func requireApprovalCarriedAcrossBuilds(t *testing.T, writer, reader documentSide, project string) {
	if !slices.ContainsFunc(writer.manifest.Types, func(typed manifestType) bool { return typed.Name == "ask" }) {
		t.Fatalf("the %s's manifest declares no ask block, the decision block an approval is carried by", writer.name)
	}
	issueKey, artifactID := createDocument(t, writer.process, project, "Every typed block", typedBlocksMarkdown(t, writer.manifest))
	askID := waitForBlockAsk(t, writer.process, issueKey, typedBlockID("ask"))

	var approval servedReview
	approved := writer.process.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", `{"state":"approved"}`, http.StatusCreated)
	if err := json.Unmarshal(approved, &approval); err != nil || approval.State != "approved" {
		t.Fatalf("approve the typed-block document on the %s: %v: %s", writer.name, err, approved)
	}
	reader.process.call(t, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		`{"selected":["First"],"expected_edited_at":null}`, http.StatusOK)

	deadline := time.Now().Add(30 * time.Second)
	for {
		var reviews []servedReview
		getJSON(t, writer.process, "/api/v1/artifacts/"+artifactID+"/reviews", &reviews)
		if len(reviews) > 1 && reviews[0].State == "approved" && reviews[0].Version > approval.Version &&
			reviews[0].Actor == (model.Actor{Kind: "user", ID: dispatchTestLogin}) {
			t.Logf("the %s carried the approval of version %d to version %d", reader.name, approval.Version, reviews[0].Version)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the answer on the %s to the decision block of a version the %s wrote and %s approved (version %d) carried no approval within 30s; reviews: %+v\n%s:\n%s",
				reader.name, writer.name, dispatchTestLogin, approval.Version, reviews, reader.name, reader.process.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForBlockAsk waits for the ask settlement indexed from the decision block blockID of the
// issue's documents, and returns its id.
func waitForBlockAsk(t *testing.T, p *dispatchProcess, issueKey, blockID string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var asks []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
		}
		getJSON(t, p, "/api/v1/issues/"+issueKey+"/asks?state=all", &asks)
		for _, ask := range asks {
			if ask.BlockID != nil && *ask.BlockID == blockID {
				return ask.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ask was indexed from the block %s of %s within 20s: %+v", blockID, issueKey, asks)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// referencesMarkdown is a body holding every reference form of a build's text/testdata that stands
// alone as a document body: each body of its text table, and each link of its href table as the
// dashboard writes it and as its dispatch:// form, a paragraph each. Two kinds of text row are left
// out, each logged:
//   - A row that names an origin of its own: its links are references only to a server at that
//     origin, and both builds serve referenceServerURL here.
//   - A row that opens a typed block (a line that starts with `:::`): the table holds the opening
//     line alone, so the text reader is tested on an attribute's value. A document must close
//     the block, and the writer's manifest must declare the attribute. The row carries neither,
//     so the writer refuses it as a document (`typed block "callout" is unclosed at document
//     level`).
//
// Every other row must stand alone. A writer that refuses one fails the row, so a refused row is
// never dropped without a trace.
func referencesMarkdown(t *testing.T, fixtures string) string {
	t.Helper()
	var texts []struct {
		Body   string `json:"body"`
		Origin string `json:"origin"`
	}
	var hrefs []struct {
		Href string `json:"href"`
		Ref  string `json:"ref"`
	}
	for name, into := range map[string]any{"dispatch-text-references.json": &texts, "dispatch-href-references.json": &hrefs} {
		raw, err := os.ReadFile(filepath.Join(fixtures, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
	}
	paragraphs := []string{"# Every reference form"}
	for _, row := range texts {
		switch {
		case row.Origin != "" && row.Origin != referenceServerURL:
			t.Logf("left out the text row %q: it names the origin %s", row.Body, row.Origin)
		case opensTypedBlock(row.Body):
			t.Logf("left out the text row %q: it opens a typed block it does not close", row.Body)
		default:
			paragraphs = append(paragraphs, row.Body)
		}
	}
	for _, row := range hrefs {
		paragraphs = append(paragraphs, referenceServerURL+row.Href, row.Ref)
	}
	return strings.Join(paragraphs, "\n\n") + "\n"
}

// opensTypedBlock reports whether a line of body starts with the `:::` of a typed block's fence.
func opensTypedBlock(body string) bool {
	for line := range strings.Lines(body) {
		if strings.HasPrefix(line, ":::") {
			return true
		}
	}
	return false
}

// documentRefs is every refs row the document is the source of, as `kind id`, sorted.
func documentRefs(t *testing.T, builds *twoBuilds, artifactID string) []string {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(), `
		select to_kind || ' ' || to_id from refs
		where from_kind = 'artifact' and from_id = $1
		order by 1
	`, artifactID)
	if err != nil {
		t.Fatalf("read the document's refs: %v", err)
	}
	defer rows.Close()
	refs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the document's refs: %v", err)
	}
	return refs
}

// referencesSettle is how long the references row waits after Y's edit before it reads the refs
// rows again: past the default two-second settle delay, so a settlement that rewrites the document
// after the edit has rewritten it too.
const referencesSettle = 4 * time.Second

// requireReferencesKeptAcrossBuilds creates the body holding every reference form of X's
// text/testdata on X, appends a paragraph on Y, and checks the refs rows after the rewrite and
// after it settled (requireRefsKept).
func requireReferencesKeptAcrossBuilds(t *testing.T, builds *twoBuilds, writer, reader documentSide, project string) {
	_, artifactID := createDocument(t, writer.process, project, "Every reference form", referencesMarkdown(t, writer.fixtures))
	written := documentRefs(t, builds, artifactID)
	if len(written) == 0 {
		t.Fatalf("the %s wrote no refs rows for the body holding every reference form of its text/testdata", writer.name)
	}
	appendParagraph(t, reader.process, artifactID, "Rewritten on the "+reader.name+".")
	requireRefsKept(t, documentRefs(t, builds, artifactID), written, "after the "+reader.name+" rewrote the body", writer.name, reader)
	time.Sleep(referencesSettle)
	dropped := requireRefsKept(t, documentRefs(t, builds, artifactID), written, "after the "+reader.name+"'s rewrite settled", writer.name, reader)
	t.Logf("%d of the %d refs rows the %s wrote survived the %s's rewrite", len(written)-len(dropped), len(written), writer.name, reader.name)
}

// requireRefsKept fails unless got holds every row of written that the reader owes it: every edge
// of a kind it extracts from the body, and, from a reader that keeps edges to the node kinds it
// does not know, those edges too. Whether it keeps them is read from its manifest: the guard that
// keeps one (refs.replaceRefs) ships with the node list this release added to the block manifest,
// so a build whose manifest names its nodes carries it. A reader from before that guard deletes
// every edge of the source it rewrites, so each edge of a kind it extracts none of is logged as
// dropped rather than failed: the protection binds from the release after the one that carries it
// (see "Two builds at once" in the README), and until a build that knows those kinds rewrites the
// body the edges are gone. The kinds the reader extracts are the kinds its rewrite left a row of.
// It may add rows, but only of a kind that written holds no row of: a kind it extracts and the
// writer does not, such as `block` from a `#b-` link when the writer is a build before the block
// graph. A row it adds of a kind the writer wrote means the two builds read one reference form as
// different targets. It returns the edges the reader dropped.
func requireRefsKept(t *testing.T, got, written []string, when, writer string, reader documentSide) []string {
	t.Helper()
	keepsUnknownKinds := len(reader.manifest.Nodes) > 0
	kinds := map[string]bool{}
	for _, row := range written {
		kind, _, _ := strings.Cut(row, " ")
		kinds[kind] = true
	}
	extracted := map[string]bool{}
	for _, row := range got {
		kind, _, _ := strings.Cut(row, " ")
		extracted[kind] = true
	}
	var lost, dropped, added []string
	for _, row := range written {
		if slices.Contains(got, row) {
			continue
		}
		if kind, _, _ := strings.Cut(row, " "); !keepsUnknownKinds && !extracted[kind] {
			dropped = append(dropped, row)
			continue
		}
		lost = append(lost, row)
	}
	for _, row := range got {
		if kind, _, _ := strings.Cut(row, " "); kinds[kind] && !slices.Contains(written, row) {
			added = append(added, row)
		}
	}
	if len(dropped) > 0 {
		t.Logf("%s, the %s dropped %d of the %d edges the %s wrote, each to a node kind it extracts none of and keeps no edge to:\n%s",
			when, reader.name, len(dropped), len(written), writer, refRowsText(dropped))
	}
	if len(lost) > 0 || len(added) > 0 {
		t.Fatalf("%s, the body's refs rows lack %d of the %d the %s wrote\n%s\nand hold %d rows of kinds the %s wrote that it did not write\n%s\nall rows now:\n%s",
			when, len(lost), len(written), writer, refRowsText(lost), len(added), writer, refRowsText(added), refRowsText(got))
	}
	return dropped
}

// refRowsText is rows one per line, each row longer than 120 bytes cut to its first 120 and its
// length: the text tables hold target ids of about 2 KB.
func refRowsText(rows []string) string {
	var text strings.Builder
	for _, row := range rows {
		if len(row) > 120 {
			row = fmt.Sprintf("%s… (%d bytes)", row[:120], len(row))
		}
		text.WriteString("\t" + row + "\n")
	}
	return text.String()
}

package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// testSigningKey is the one-key set the package's people stores seal refresh tokens under.
var testSigningKey = auth.SingleSigningKey("signing-key")

// headerIdentity is the test header identity that records each named person in the database.
func headerIdentity(database *store.Store) identity.HeaderIdentity {
	return identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(database.Pool, testSigningKey, nil)}
}

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(database),
		ServerURL: "https://dispatch.example",
		Settle:    20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, artifactID
}
func TestRepairingAnUnreadableAskBlockCountsTheAskItOpens(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ttl urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich cache TTL?\n\n- Short: 60 seconds\n- Long: one hour\n:::\n")
	// A browser leaves the first option without a label: settlement flags the block invalid.
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Children[1].Children[0].Children[0].Children = []*pmdoc.Node{{Type: "text", Text: ": 60 seconds"}}
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	outcome, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: ": 60 seconds", With: "Short: 60 seconds"},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	if err != nil {
		t.Fatalf("repair edit: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	var open int
	if err := service.store.Pool.QueryRow(context.Background(),
		`select count(*) from asks where block_artifact_id = $1 and state = 'open'`, artifactID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 1 || len(outcome.AddedAskBlocks) != open {
		t.Fatalf("repair opened %d ask(s), reported %d", open, len(outcome.AddedAskBlocks))
	}
	if !slices.Equal(outcome.AddedAskBlocks, []AddedAskBlock{{ID: "ttl", Question: "Which cache TTL?"}}) {
		t.Fatalf("repair named added blocks %+v, want ttl and its question", outcome.AddedAskBlocks)
	}
}

// An edit names the ask blocks it added in document order, each with its question, whatever
// their ids' order; a block already in the document is not among them.
func TestEditNamesTheAddedAskBlocksInDocumentOrder(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#held urgency=\"med\"}\nWhich region?\n:::\n")
	outcome, err := joinedApplyOps(service, artifactID, []model.EditOp{{
		Op: "insert", After: "end",
		Markdown: ":::ask{#zeta}\nWhich deployment window?\n:::\n\n> :::ask{#alpha}\n> Which cache TTL?\n> :::",
	}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	if err != nil {
		t.Fatalf("insert ask blocks: %v", err)
	}
	want := []AddedAskBlock{{ID: "zeta", Question: "Which deployment window?"}, {ID: "alpha", Question: "Which cache TTL?"}}
	if !slices.Equal(outcome.AddedAskBlocks, want) {
		t.Fatalf("edit named %+v, want %+v", outcome.AddedAskBlocks, want)
	}
}
func TestEditingBesideAnUnreadableAskBlockAddsNoAsk(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ttl urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich cache TTL?\n\n- Short: 60 seconds\n- Long: one hour\n:::\n")
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Children[1].Children[0].Children[0].Children = []*pmdoc.Node{{Type: "text", Text: ": 60 seconds"}}
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	outcome, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "insert", After: "end", Markdown: "A plain revision."},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	if err != nil {
		t.Fatalf("edit beside unreadable ask: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	var open int
	if err := service.store.Pool.QueryRow(context.Background(),
		`select count(*) from asks where block_artifact_id = $1 and state = 'open'`, artifactID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 || len(outcome.AddedAskBlocks) != open {
		t.Fatalf("edit beside unreadable ask opened %d ask(s), reported %d", open, len(outcome.AddedAskBlocks))
	}
}

func TestSettlementIndexesRetractsAndRestoresTypedAskBlocks(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const askMarkdown = ":::ask{#ask-block multiple=\"false\" state=\"open\" urgency=\"high\"}\nWhich transport should we expose?\n\n- REST: Matches the existing platform\n- gRPC: Adds streaming\n:::\n"
	seedServiceText(t, service, artifactID, askMarkdown)

	service.settleRoom(artifactID, 0)
	var askID, question, urgency, state, blockID string
	var multiple bool
	if err := service.store.Pool.QueryRow(context.Background(), `
		select id::text, question, urgency, state, block_id, multiple
		from asks where block_artifact_id = $1
	`, artifactID).Scan(&askID, &question, &urgency, &state, &blockID, &multiple); err != nil {
		t.Fatalf("read indexed ask: %v", err)
	}
	if blockID != "ask-block" || question != "Which transport should we expose?" ||
		urgency != "high" || multiple || state != "open" {
		t.Fatalf("indexed ask = block=%q question=%q urgency=%q multiple=%t state=%q",
			blockID, question, urgency, multiple, state)
	}

	editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node {
		return &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
			Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "replacement"},
			Children: []*pmdoc.Node{{Type: "text", Text: "No decision remains."}},
		}}}
	})
	settleCurrentGeneration(t, service, artifactID)
	var resolutionKind string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state, resolution->>'kind' from asks where id = $1
	`, askID).Scan(&state, &resolutionKind); err != nil {
		t.Fatalf("read retracted ask: %v", err)
	}
	if state != "resolved" || resolutionKind != "retracted" {
		t.Fatalf("retracted ask state=%q resolution=%q", state, resolutionKind)
	}

	editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node {
		restored, err := pmdoc.Parse(askMarkdown)
		if err != nil {
			t.Fatalf("parse restored ask: %v", err)
		}
		return restored
	})
	settleCurrentGeneration(t, service, artifactID)
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state from asks where id = $1
	`, askID).Scan(&state); err != nil {
		t.Fatalf("read restored ask: %v", err)
	}
	if state != "open" {
		t.Fatalf("restored ask state=%q, want open", state)
	}
}

// An ask's question and options are caller text, which the ask's edit writes into the block as
// plain text and reads back through the document's own markdown pipeline. Text holding a web
// address - after a space, after a letter and `;`, with a lowercase scheme - is written, settles,
// and its row reads back as written, whether or not a reader takes the address for a link
// (INFRA-531).
func TestAnAskWhoseTextHoldsWebAddressesSettlesAsWritten(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#hosts urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich host?\n\n- A: First\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	question := "Which host, see https://x.test\n\nx;https://x.test\n\nor see http://x.test"
	options := []model.AskOption{
		{Label: "Secure", Description: "see https://x.test"},
		{Label: "x;https://x.test"},
		{Label: "Plain", Description: "see http://x.test"},
	}
	var stored AskBlockText
	if err := joinedWrite(service, func(ctx context.Context) (err error) {
		stored, err = service.SetAskBlockText(ctx, artifactID, "hosts", AskBlockEdit{Question: &question, Options: &options},
			model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
		return err
	}); err != nil {
		t.Fatalf("write the ask's text: %v", err)
	}
	if stored.Question != question || !reflect.DeepEqual(stored.Options, options) {
		t.Fatalf("the block reads back question=%q options=%#v, want what was written", stored.Question, stored.Options)
	}
	settleCurrentGeneration(t, service, artifactID)
	var settled string
	var optionsJSON []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select question, options from asks where block_artifact_id = $1 and block_id = 'hosts' and state = 'open'
	`, artifactID).Scan(&settled, &optionsJSON); err != nil {
		t.Fatalf("read the settled ask: %v", err)
	}
	var settledOptions []model.AskOption
	if err := json.Unmarshal(optionsJSON, &settledOptions); err != nil {
		t.Fatalf("decode the settled options: %v", err)
	}
	if settled != question || !reflect.DeepEqual(settledOptions, options) {
		t.Fatalf("the settled ask holds question=%q options=%#v, want what was written", settled, settledOptions)
	}
}

// A spec's decisions are ask blocks, and the write that adds one already carries its words into
// a version: indexing it afterwards is bookkeeping over the same document. Settlement recording
// that bookkeeping as a version of its own wrote a byte-identical version credited to nobody,
// which stales the approval pinned to the version the agent wrote - and Legion's design gate,
// open exactly while the approved version is the latest, closes with nothing in the event log
// to explain it (LEGION-273).
func TestSettlementIndexingAnAskBlockTheEditVersionedWritesNoVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	seedServiceText(t, service, artifactID, "# Spec\n\nContext\n")
	snapshotAndCommitVersion(t, service, artifactID, agent)

	written := agentEditVersion(t, service, artifactID, agent, []model.EditOp{{
		Op: "insert", After: "end", Markdown: ":::ask{#decision urgency=\"high\" multiple=\"false\"}\n" +
			"Which transport?\n\n- REST: Matches the platform\n- gRPC: Adds streaming\n:::\n",
	}})
	settleCurrentGeneration(t, service, artifactID)

	// The settlement ran: it indexed the block the edit wrote.
	var state string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state from asks where block_artifact_id = $1 and block_id = 'decision'
	`, artifactID).Scan(&state); err != nil {
		t.Fatalf("read the ask settlement indexed: %v", err)
	}
	if state != "open" {
		t.Fatalf("indexed ask state = %q, want open", state)
	}
	var latest, count int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select coalesce(max(number), 0), count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&latest, &count); err != nil {
		t.Fatalf("read document versions: %v", err)
	}
	if latest != written.Number || count != written.Number {
		t.Fatalf("after settlement the document is at version %d of %d, want the edit's %d alone",
			latest, count, written.Number)
	}
}

// agentEditVersion applies ops as an agent and versions the result, the way
// POST /api/v1/artifacts/{id}/edits does: one transaction, the edit then the snapshot.
func agentEditVersion(t *testing.T, service *Service, artifactID string, actor model.Actor, ops []model.EditOp) model.Version {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin agent edit: %v", err)
	}
	defer tx.Rollback(ctx)
	editCtx, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	edit, err := service.ApplyOps(editCtx, artifactID, ops, actor, nil)
	if err != nil {
		t.Fatalf("agent edit: %v", err)
	}
	if !edit.Changed {
		t.Fatalf("agent edit changed nothing: %#v", edit)
	}
	written, err := service.SnapshotVersion(editCtx, artifactID, actor)
	if err != nil {
		t.Fatalf("version the agent edit: %v", err)
	}
	if !written.Wrote {
		t.Fatalf("agent edit wrote no version: %#v", written)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit agent edit: %v", err)
	}
	return written.Version
}

// The same defect from the other producer: settlement stamps a legacy document's missing block
// ids and records that as a content change, because the stored Proof state really did move. The
// rendered markdown did not - a paragraph's block id is not in it - so a version for the repair
// repeats the version before it on a document nobody edited (LEGION-229 requirement 2).
func TestSettlementStampingBlockIDsWritesNoVersion(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})

	settleCurrentGeneration(t, service, artifactID)

	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs != 0 {
		t.Fatalf("settlement left %d unstamped blocks, so it never ran its repair", repairs)
	}
	var versions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count document versions: %v", err)
	}
	if versions != 1 {
		t.Fatalf("document versions = %d, want the one the repair did not change", versions)
	}
}

// A settlement that writes no version moves no version cursor, so the row its closure appended
// stays past that cursor for good. The row therefore has to say what the closure actually
// rendered: one that claimed a content change it never made would leave `contentChanged` true on
// every later settlement of that document, and the first one after a renderer change - opening
// the document schedules one - would version a document nobody had touched, credited to nobody,
// staling its approval all over again.
func TestSettlementRepairThatRendersTheSameMarkdownRecordsNoContentChange(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "snake_case here")
	seedUnidentifiedProofDocument(t, database, artifactID, "snake_case here")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	alignLatestVersionWithUpdates(t, service, artifactID)

	settleCurrentGeneration(t, service, artifactID)

	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs != 0 {
		t.Fatalf("settlement left %d unstamped blocks, so it never ran its repair", repairs)
	}
	if rows := contentRowsPastLatestVersion(t, database, artifactID); rows != 0 {
		t.Fatalf("the repair left %d content-class rows past the latest version's cursor; a block id no rendering carries changed no text", rows)
	}

	// A deployed renderer change, as the stored version sees it: the document now renders
	// differently from the markdown the version holds, and nobody has touched it.
	if _, err := database.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1
	`, artifactID, "snake\\_case here\n"); err != nil {
		t.Fatalf("store the older rendering on the latest version: %v", err)
	}

	settleCurrentGeneration(t, service, artifactID)

	var versions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count document versions: %v", err)
	}
	if versions != 1 {
		t.Fatalf("settling an untouched document under a changed rendering wrote %d versions, want the one it found", versions)
	}
}

// contentRowsPastLatestVersion is how many content-class document updates lie past the cursor of
// the document's latest version - what `contentChangedSinceVersion` asks the next settlement.
func contentRowsPastLatestVersion(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from doc_updates
		where artifact_id = $1 and content_changed and version > (
			select doc_update_version from artifact_versions
			where artifact_id = $1 order by number desc limit 1
		)
	`, artifactID).Scan(&rows); err != nil {
		t.Fatalf("count content updates past the latest version: %v", err)
	}
	return rows
}

// A settlement that writes no version leaves the document at the version it found, and the
// events that name one - here `block.invalid`, whose `invalid` attribute no rendering carries -
// have to say that number rather than the one the settlement would have written.
func TestSettlementWritingNoVersionNamesTheOneItLeavesTheDocumentAt(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
	service.settleRoom(artifactID, 0)
	waitForDocumentVersion(t, service.store, artifactID, 2)

	// A browser leaves an option without a label, which settlement cannot parse, and a human
	// anchoring a comment versions that document before the settlement sees it.
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Children[1].Children[0].Children[0].Children = nil
		return tree
	})
	snapshotAndCommitVersion(t, service, artifactID, model.Actor{Kind: "user", ID: "alice"})
	settleCurrentGeneration(t, service, artifactID)

	var latest int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select max(number) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&latest); err != nil {
		t.Fatalf("read document versions: %v", err)
	}
	if latest != 3 {
		t.Fatalf("document is at version %d, want the human's 3: flagging a block invalid renders no markdown", latest)
	}
	var blockID, version string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select payload->>'block_id', payload->>'version'
		from events where type = 'block.invalid' order by id desc limit 1
	`).Scan(&blockID, &version); err != nil {
		t.Fatalf("load malformed-block event: %v", err)
	}
	if blockID != "ask-block" || version != "3" {
		t.Fatalf("malformed-block event = block=%q version=%q, want ask-block at version 3", blockID, version)
	}
}

func TestSettlementRepairsServerOwnedAskAttributesOncePerVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n")
	service.settleRoom(artifactID, 0)
	var askID string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select id::text from asks where block_artifact_id = $1
	`, artifactID).Scan(&askID); err != nil {
		t.Fatalf("read indexed ask: %v", err)
	}
	answer := model.AskAnswer{User: "alice", Selected: []string{}, At: time.Now().UTC()}
	answerJSON, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode answer: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		update asks set state = 'answered', answer = $2 where id = $1
	`, askID, answerJSON); err != nil {
		t.Fatalf("answer indexed ask: %v", err)
	}
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Attrs["state"] = "open"
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	waitForDocumentText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\""+answer.At.Format(time.RFC3339Nano)+"\" selected=\"[]\"}\nShip it?\n:::\n")
	var repaired int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = 'block.repaired' and payload->>'block_id' = 'ask-block'
	`).Scan(&repaired); err != nil {
		t.Fatalf("count repairs: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repair events = %d, want one", repaired)
	}
	settleCurrentGeneration(t, service, artifactID)
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = 'block.repaired' and payload->>'block_id' = 'ask-block'
	`).Scan(&repaired); err != nil {
		t.Fatalf("count idempotent repairs: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repair events after repeat = %d, want one", repaired)
	}
}

func TestSettlementRecordsMalformedAskAndRepairsIt(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 10 * time.Millisecond
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
	service.settleRoom(artifactID, 0)
	waitForDocumentVersion(t, service.store, artifactID, 2)

	var askID string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select id::text from asks where block_artifact_id = $1 and block_id = 'ask-block'
	`, artifactID).Scan(&askID); err != nil {
		t.Fatalf("load indexed ask: %v", err)
	}
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Children[1].Children[0].Children[0].Children = nil
		return tree
	})
	waitFor(t, time.Second, "malformed update persisted", func() bool {
		var updates int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from doc_updates where artifact_id = $1
		`, artifactID).Scan(&updates); err != nil {
			t.Fatalf("count malformed updates: %v", err)
		}
		return updates > 1
	})
	waitForDocumentVersion(t, service.store, artifactID, 3)
	state := service.room(artifactID)
	state.mu.Lock()
	failures := state.settleFailures
	state.mu.Unlock()
	if failures != 0 {
		t.Fatalf("malformed ask scheduled %d settlement retries, want none", failures)
	}
	const reason = `ask block "ask-block" has an option without a label`
	if got := liveTree(t, service, artifactID).Children[0].Attrs["invalid"]; got != reason {
		t.Fatalf("malformed ask invalid = %#v, want %q", got, reason)
	}
	var malformedMarkdown string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 and number = 3
	`, artifactID).Scan(&malformedMarkdown); err != nil {
		t.Fatalf("load malformed version: %v", err)
	}
	if strings.Contains(malformedMarkdown, ` invalid=`) {
		t.Fatalf("malformed version leaks server state: %q", malformedMarkdown)
	}
	var questionDuringInvalid string
	var optionsDuringInvalidJSON []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select question, options from asks where id = $1
	`, askID).Scan(&questionDuringInvalid, &optionsDuringInvalidJSON); err != nil {
		t.Fatalf("load ask during malformed block: %v", err)
	}
	var optionsDuringInvalid []model.AskOption
	if err := json.Unmarshal(optionsDuringInvalidJSON, &optionsDuringInvalid); err != nil {
		t.Fatalf("decode ask during malformed block: %v", err)
	}
	if questionDuringInvalid != "Should we ship?" || len(optionsDuringInvalid) != 2 ||
		optionsDuringInvalid[0] != (model.AskOption{Label: "Ship", Description: "Release it"}) ||
		optionsDuringInvalid[1] != (model.AskOption{Label: "Hold", Description: "Wait for review"}) {
		t.Fatalf("ask changed during malformed block: question=%q options=%#v", questionDuringInvalid, optionsDuringInvalid)
	}
	var eventBlockID, eventVersion, eventReason string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select payload->>'block_id', payload->>'version', payload->>'reason'
		from events where type = 'block.invalid' order by id desc limit 1
	`).Scan(&eventBlockID, &eventVersion, &eventReason); err != nil {
		t.Fatalf("load malformed-block event: %v", err)
	}
	if eventBlockID != "ask-block" || eventVersion != "3" || eventReason != reason {
		t.Fatalf("malformed-block event = block=%q version=%q reason=%q", eventBlockID, eventVersion, eventReason)
	}

	var updatesBeforeRepair int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from doc_updates where artifact_id = $1
	`, artifactID).Scan(&updatesBeforeRepair); err != nil {
		t.Fatalf("count updates before repair: %v", err)
	}
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Children[0].Children[0].Text = "Should we release after review?"
		tree.Children[0].Children[1].Children[0].Children[0].Children = []*pmdoc.Node{{
			Type: "text",
			Text: "Release: Ship after review",
		}}
		return tree
	})
	waitFor(t, time.Second, "repaired update persisted", func() bool {
		var updates int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from doc_updates where artifact_id = $1
		`, artifactID).Scan(&updates); err != nil {
			t.Fatalf("count repaired updates: %v", err)
		}
		return updates > updatesBeforeRepair
	})
	waitForDocumentVersion(t, service.store, artifactID, 4)
	if _, exists := liveTree(t, service, artifactID).Children[0].Attrs["invalid"]; exists {
		t.Fatal("repaired ask still has an invalid attribute")
	}
	var question string
	var optionsJSON []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select question, options from asks where id = $1
	`, askID).Scan(&question, &optionsJSON); err != nil {
		t.Fatalf("load repaired ask: %v", err)
	}
	var options []model.AskOption
	if err := json.Unmarshal(optionsJSON, &options); err != nil {
		t.Fatalf("decode repaired options: %v", err)
	}
	if question != "Should we release after review?" || len(options) != 2 ||
		options[0] != (model.AskOption{Label: "Release", Description: "Ship after review"}) ||
		options[1] != (model.AskOption{Label: "Hold", Description: "Wait for review"}) {
		t.Fatalf("repaired ask = question=%q options=%#v", question, options)
	}
	var edits int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = 'ask.edited' and payload->>'id' = $1
	`, askID).Scan(&edits); err != nil {
		t.Fatalf("count repaired ask edits: %v", err)
	}
	if edits != 1 {
		t.Fatalf("repaired ask edit events = %d, want one", edits)
	}
}

// A browser edit can put a block an ask's body does not allow into it; settlement keeps the block
// and flags it instead of indexing it.
func TestSettlementMarksUnsupportedAskBodyInvalid(t *testing.T) {
	service, artifactID := newTestService(t)
	// The edit lands before any settlement: one that ran first would index the seeded ask while its
	// body still allows it.
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-heading urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n:::\n")
	editLiveTree(t, service, artifactID, appendToAsk(&pmdoc.Node{Type: "heading", Attrs: pmdoc.Attrs{"level": float64(2)}, Children: []*pmdoc.Node{{Type: "text", Text: "Not an option"}}}))
	settleCurrentGeneration(t, service, artifactID)

	waitForDocumentVersion(t, service.store, artifactID, 2)
	const reason = `ask block "ask-heading" has unsupported body node "heading"`
	if got := liveTree(t, service, artifactID).Children[0].Attrs["invalid"]; got != reason {
		t.Fatalf("unsupported ask body invalid = %#v, want %q", got, reason)
	}
	var asks int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from asks where block_artifact_id = $1 and block_id = 'ask-heading'
	`, artifactID).Scan(&asks); err != nil {
		t.Fatalf("count unsupported ask rows: %v", err)
	}
	if asks != 0 {
		t.Fatalf("unsupported ask rows = %d, want none", asks)
	}
}

// Concurrent browser edits can leave an ask holding a second bullet list, which the browser keeps
// and draws its options from the last list alone. Settlement marks it invalid and leaves its row
// open and as it was, until an edit joins the lists.
func TestSettlementMarksAnAskWithASecondBulletListInvalid(t *testing.T) {
	const reason = `ask block "ask-lists", asking "Should we ship?", holds a second bullet list where its content rule, paragraph+ bullet_list?, allows one or more paragraphs and then at most one bullet list, last`
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	editLiveTree(t, service, artifactID, appendToAsk(parsedBlock(t, "- Defer: Wait a week\n")))
	settleCurrentGeneration(t, service, artifactID)
	assertAskMarkedInvalidAndKept(t, service, artifactID, "ask-lists", reason)

	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		ask := firstAsk(tree)
		ask.Children[1].Children = append(ask.Children[1].Children, ask.Children[2].Children...)
		ask.Children = ask.Children[:2]
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", []model.AskOption{
		{Label: "Ship", Description: "Release it"},
		{Label: "Hold", Description: "Wait for review"},
		{Label: "Defer", Description: "Wait a week"},
	})
}

// The ask-edit route rebuilds no decision that breaks its content rule, whatever shape breaks it:
// a second list, a second list repeating a label, an item holding more than its text, a block
// after the list. Every edit is refused - options, question, urgency - and writes nothing, and
// the refusal quotes every block of the decision, its id with it, so a caller deleting one with a
// document edit sees every word it removes.
func TestSetAskBlockTextRefusesEveryEditOfAnAskBreakingItsContentRule(t *testing.T) {
	ship := model.AskOption{Label: "Ship", Description: "Release it"}
	hold := model.AskOption{Label: "Hold", Description: "Wait for review"}
	defer_ := model.AskOption{Label: "Defer", Description: "Wait a week"}
	for name, test := range map[string]struct {
		block, quoted, kind string
	}{
		"second list":              {"- Defer: Wait a week\n", `"- Defer: Wait a week"`, "bullet_list"},
		"repeated label":           {"- Ship: Only on Fridays\n", `"- Ship: Only on Fridays"`, "bullet_list"},
		"nested sub-item":          {"- Defer: Wait a week\n  - only if finance agrees\n", `"- Defer: Wait a week\n  - only if finance agrees"`, "bullet_list"},
		"second item paragraph":    {"- Defer: Wait a week\n\n  Finance must sign off first.\n", `"- Defer: Wait a week\n\n  Finance must sign off first."`, "bullet_list"},
		"paragraph after the list": {"Stray words after the list\n", `"Stray words after the list"`, "paragraph"},
	} {
		t.Run(name, func(t *testing.T) {
			service, artifactID := seedIndexedShipAsk(t, "ask-lists")
			editLiveTree(t, service, artifactID, appendToAsk(parsedBlock(t, test.block)))
			settleCurrentGeneration(t, service, artifactID)
			before := liveTree(t, service, artifactID)
			for edit, sent := range map[string]AskBlockEdit{
				"the row's options":    {Options: &[]model.AskOption{ship, hold}},
				"every option":         {Options: &[]model.AskOption{ship, hold, defer_}},
				"options and question": {Question: new("Should we ship today?"), Options: &[]model.AskOption{ship, hold, defer_}},
				"question only":        {Question: new("Should we ship today?")},
				"urgency only":         {Urgency: new("high")},
			} {
				err := sendAskOptions(service, artifactID, sent)
				assertBrokenAskEditRefused(t, service, artifactID, before, err)
				if !strings.Contains(err.Error(), "("+test.kind+") reads "+test.quoted) {
					t.Fatalf("%s: err = %v, want it to quote the %s %s", edit, err, test.kind, test.quoted)
				}
			}
			if last := firstAsk(liveTree(t, service, artifactID)).Children[2]; last.Type != test.kind {
				t.Fatalf("the ask's last block = %s, want the %s it was given kept", last.Type, test.kind)
			}
		})
	}
}

// The refusal of a two-list decision, word for word: the content rule it breaks, the one path out,
// and every block quoted in full by its id.
func TestSetAskBlockTextRefusalOfATwoListAskQuotesEveryBlock(t *testing.T) {
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	editLiveTree(t, service, artifactID, appendToAsk(parsedBlock(t, "- Defer: Wait a week\n  - only if finance agrees\n")))
	settleCurrentGeneration(t, service, artifactID)
	ask := firstAsk(liveTree(t, service, artifactID))
	ids := make([]string, 0, len(ask.Children))
	for _, child := range ask.Children {
		id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
		ids = append(ids, strconv.Quote(id))
	}
	want := `block: ask block "ask-lists", asking "Should we ship?", holds a second bullet list where its content rule, paragraph+ bullet_list?, allows one or more paragraphs and then at most one bullet list, last; ` +
		`the ask-edit route does not rebuild a decision that breaks its content rule, so delete the blocks to drop, all in one document edit (dispatch doc-edit, deleting a block by its id; one edit holding one delete per block), which keeps the question's paragraphs and the first list after them, and keeps the same of every decision nested in the blocks it keeps, deleting whole a nested decision the rule leaves nothing of; ` +
		`the blocks to delete are ` + ids[2] + `; then edit the ask; its blocks are: ` +
		`block ` + ids[0] + ` (paragraph) reads "Should we ship?"; ` +
		`block ` + ids[1] + ` (bullet_list) reads "- Ship: Release it\n- Hold: Wait for review"; ` +
		`block ` + ids[2] + ` (bullet_list) reads "- Defer: Wait a week\n  - only if finance agrees"`
	err := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")})
	if err == nil || err.Error() != want {
		t.Fatalf("refusal =\n%v\nwant\n%s", err, want)
	}
}

// quotedAskBlock matches a block the refusal of a broken ask quotes: its id and its markdown, each
// Go-quoted.
var quotedAskBlock = regexp.MustCompile(`block ("(?:[^"\\]|\\.)*") \((\w+)\) reads ("(?:[^"\\]|\\.)*")`)

// textWords is every word of node's text, split at whatever is not a letter or a digit.
func textWords(node *pmdoc.Node) []string {
	words := []string{}
	pmdoc.Walk(node, func(node *pmdoc.Node) bool {
		words = append(words, strings.FieldsFunc(node.Text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })...)
		return true
	})
	return words
}

// blocksToDelete matches the refusal's one delete list: every block id the caller deletes, each
// Go-quoted, in one document edit. Where a block on it is or holds a decision, the list carries
// the clause naming what the client refuses until that decision's ask is answered or resolved
// (askRepairStep), which stands between the list and the ask edit after it.
var blocksToDelete = regexp.MustCompile(`the blocks to delete are ((?:"(?:[^"\\]|\\.)*"(?:, )?)+)` +
	`(?:; a decision among them whose ask is still open cannot be deleted by a document edit, so ` +
	`answer or resolve that ask, or repair the decision in the document editor, before this delete)?` +
	`; then edit the ask`)

// refusalDeletes reads the block ids a refusal has the caller delete, and every block it quotes.
func refusalDeletes(t *testing.T, err error) (deletes []string, quoted map[string]string) {
	t.Helper()
	var unrepresentable *ErrAskBlockUnrepresentable
	if !errors.As(err, &unrepresentable) {
		t.Fatalf("err = %v, want ErrAskBlockUnrepresentable", err)
	}
	quoted = map[string]string{}
	for _, match := range quotedAskBlock.FindAllStringSubmatch(unrepresentable.Reason, -1) {
		id, idErr := strconv.Unquote(match[1])
		text, textErr := strconv.Unquote(match[3])
		if idErr != nil || textErr != nil {
			t.Fatalf("the refusal quotes %s and %s, which do not unquote: %v %v", match[1], match[3], idErr, textErr)
		}
		quoted[id] = text
	}
	list := blocksToDelete.FindAllStringSubmatch(unrepresentable.Reason, -1)
	if len(list) != 1 {
		t.Fatalf("the refusal gives %d delete lists, want one: %v", len(list), err)
	}
	for _, id := range regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).FindAllString(list[0][1], -1) {
		unquoted, unquoteErr := strconv.Unquote(id)
		if unquoteErr != nil {
			t.Fatalf("the refusal names %s to delete, which does not unquote: %v", id, unquoteErr)
		}
		deletes = append(deletes, unquoted)
	}
	return deletes, quoted
}

// followRefusal sends edit, reads the refusal's delete list, checks it names exactly the ask's
// children at the indexes deleted, deletes every block on it in ONE document edit, and sends the
// question and every option back with the ask edit. The decision ends valid, and no word is lost:
// every word outside the deleted blocks is kept, every word of them the refusal quoted.
func followRefusal(t *testing.T, service *Service, artifactID string, edit AskBlockEdit, deleted []int, question string, every []model.AskOption) {
	t.Helper()
	ask := firstAsk(liveTree(t, service, artifactID))
	err := sendAskOptions(service, artifactID, edit)
	deletes, quoted := refusalDeletes(t, err)
	if len(quoted) != len(ask.Children) {
		t.Fatalf("the refusal quotes %d blocks, want the ask's %d: %v", len(quoted), len(ask.Children), err)
	}
	want := make([]string, 0, len(deleted))
	for _, index := range deleted {
		id, _ := ask.Children[index].Attrs[pmdoc.BlockIDAttr].(string)
		want = append(want, id)
	}
	if !slices.Equal(deletes, want) {
		t.Fatalf("the refusal names %q to delete, want %q: %v", deletes, want, err)
	}
	kept, deletedWords := []string{}, []string{}
	ops := make([]model.EditOp, 0, len(deletes))
	for _, child := range ask.Children {
		id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
		if !slices.Contains(deletes, id) {
			kept = append(kept, textWords(child)...)
			continue
		}
		ops = append(ops, model.EditOp{Op: "delete", Block: id})
		for _, word := range textWords(child) {
			if !strings.Contains(quoted[id], word) {
				t.Fatalf("the refusal does not quote %q of block %q it has the caller delete: %v", word, id, err)
			}
			deletedWords = append(deletedWords, word)
		}
	}
	if _, err := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
		t.Fatalf("one document edit deleting %q, as the refusal names: %v", deletes, err)
	}
	if err := sendAskOptions(service, artifactID, AskBlockEdit{Question: &question, Options: &every}); err != nil {
		t.Fatalf("ask edit after the document edit: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-lists", question, every)
	after := textWords(firstAsk(liveTree(t, service, artifactID)))
	for _, word := range append(kept, deletedWords...) {
		if !slices.Contains(after, word) {
			t.Fatalf("the decision lost %q: it reads %q", word, after)
		}
	}
}

// The refusal's one path applies when followed: the caller reads the ids to delete from the
// refusal, deletes them with one document edit, and edits the ask, sending back the deleted list's
// option with the words the refusal quoted.
func TestSetAskBlockTextRefusalOfABrokenAskNamesAPathThatApplies(t *testing.T) {
	ship := model.AskOption{Label: "Ship", Description: "Release it"}
	hold := model.AskOption{Label: "Hold", Description: "Wait for review"}
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	editLiveTree(t, service, artifactID, appendToAsk(parsedBlock(t, "- Defer: Wait a week\n  - only if finance agrees\n")))
	settleCurrentGeneration(t, service, artifactID)
	followRefusal(t, service, artifactID, AskBlockEdit{Options: &[]model.AskOption{ship, hold}}, []int{2}, "Should we ship?",
		[]model.AskOption{ship, hold, {Label: "Defer", Description: "Wait a week, only if finance agrees"}})
}

// With two extra lists, a document edit deleting one is refused (it leaves the decision broken),
// so the refusal names both, to delete in one document edit, which applies.
func TestSetAskBlockTextRefusalOfAThreeListAskNamesEveryDeleteInOneEdit(t *testing.T) {
	ship := model.AskOption{Label: "Ship", Description: "Release it"}
	hold := model.AskOption{Label: "Hold", Description: "Wait for review"}
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	editLiveTree(t, service, artifactID, appendToAsk(parsedBlock(t, "- Defer: Wait a week\n")))
	editLiveTree(t, service, artifactID, appendToAsk(parsedBlock(t, "- Drop: Abandon it\n")))
	settleCurrentGeneration(t, service, artifactID)
	followRefusal(t, service, artifactID, AskBlockEdit{Options: &[]model.AskOption{ship, hold}}, []int{2, 3}, "Should we ship?",
		[]model.AskOption{ship, hold, {Label: "Defer", Description: "Wait a week"}, {Label: "Drop", Description: "Abandon it"}})
}

// Whatever shape breaks the rule - a list before the question, a paragraph after the list, a
// paragraph holding no text before the one holding the question - the refusal names the blocks
// whose deletion leaves the question's paragraphs and the first list after them, and a document
// edit deleting exactly those applies.
func TestSetAskBlockTextRefusalNamesADeleteThatAppliesWhateverTheShape(t *testing.T) {
	ship := model.AskOption{Label: "Ship", Description: "Release it"}
	hold := model.AskOption{Label: "Hold", Description: "Wait for review"}
	emptyParagraph := func(id string) *pmdoc.Node {
		return &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: id}}
	}
	for name, test := range map[string]struct {
		// children is the ask's blocks, given its question paragraph and its Ship/Hold list.
		children func(t *testing.T, question, list *pmdoc.Node) []*pmdoc.Node
		deleted  []int
		question string
		every    []model.AskOption
	}{
		"list before the question": {
			children: func(t *testing.T, question, list *pmdoc.Node) []*pmdoc.Node {
				return []*pmdoc.Node{parsedBlock(t, "- Early: First words\n"), question, list}
			},
			deleted: []int{0}, question: "Should we ship?",
			every: []model.AskOption{{Label: "Early", Description: "First words"}, ship, hold},
		},
		"paragraph after the list": {
			children: func(t *testing.T, question, list *pmdoc.Node) []*pmdoc.Node {
				return []*pmdoc.Node{question, list, parsedBlock(t, "Decide by Friday.\n")}
			},
			deleted: []int{2}, question: "Should we ship?\n\nDecide by Friday.", every: []model.AskOption{ship, hold},
		},
		"empty first paragraph": {
			children: func(t *testing.T, question, list *pmdoc.Node) []*pmdoc.Node {
				return []*pmdoc.Node{emptyParagraph("empty-para"), list, question}
			},
			deleted: []int{0, 1}, question: "Should we ship?", every: []model.AskOption{ship, hold},
		},
		"empty first paragraph, two lists": {
			children: func(t *testing.T, question, list *pmdoc.Node) []*pmdoc.Node {
				return []*pmdoc.Node{emptyParagraph("empty-para"), list, parsedBlock(t, "- Defer: Wait a week\n"), question}
			},
			deleted: []int{0, 1, 2}, question: "Should we ship?",
			every: []model.AskOption{ship, hold, {Label: "Defer", Description: "Wait a week"}},
		},
		"list, empty paragraph, list": {
			children: func(t *testing.T, question, list *pmdoc.Node) []*pmdoc.Node {
				return []*pmdoc.Node{parsedBlock(t, "- Defer: Wait a week\n"), emptyParagraph("empty-para"), list, question}
			},
			deleted: []int{0, 1, 2}, question: "Should we ship?",
			every: []model.AskOption{ship, hold, {Label: "Defer", Description: "Wait a week"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, artifactID := seedIndexedShipAsk(t, "ask-lists")
			editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				ask := firstAsk(tree)
				ask.Children = test.children(t, ask.Children[0], ask.Children[1])
				return tree
			})
			settleCurrentGeneration(t, service, artifactID)
			followRefusal(t, service, artifactID, AskBlockEdit{Urgency: new("high")}, test.deleted, test.question, test.every)
		})
	}
}

// A person's document edit can leave an ask holding no question at all, which settlement flags
// `invalid` while it leaves the row as it was. An ask edit carrying a question writes one back
// into the block: the option list is the node the document already held, the row takes the new
// question, and the next settlement clears the flag. A decision holding a second list is written
// the same way, every list of it kept as it is, since the content rule it breaks is weighed
// after its question: no delete repairs a decision while it asks nothing.
func TestEditAskRepairsAnEmptyQuestion(t *testing.T) {
	t.Run("the decision keeps its content rule", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			firstAsk(tree).Children[0].Children = nil
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		options := firstAsk(liveTree(t, service, artifactID)).Children[1]
		var stored AskBlockText
		if err := joinedWrite(service, func(ctx context.Context) (err error) {
			stored, err = service.SetAskBlockText(ctx, artifactID, "ask-lists",
				AskBlockEdit{Question: new("Should we ship it today?")}, model.Actor{Kind: "user", ID: "alice"})
			return err
		}); err != nil {
			t.Fatalf("the ask edit sending a question to the emptied decision: %v", err)
		}
		if stored.SeveralOptionLists {
			t.Fatalf("the repair of a decision holding one list reports it as holding several, "+
				"so its row would keep options the block does not hold: %#v", stored)
		}
		ask := firstAsk(liveTree(t, service, artifactID))
		block, err := parseAskBlock(ask)
		if err != nil {
			t.Fatalf("the repaired block does not parse: %v", err)
		}
		if block.question != "Should we ship it today?" {
			t.Fatalf("the repaired block asks %q, want the question the ask edit sent", block.question)
		}
		if last := ask.Children[len(ask.Children)-1]; !last.EqualWithBlockIDs(options) {
			t.Fatalf("the question edit rewrote the option list, want the very nodes the document held")
		}
		settleCurrentGeneration(t, service, artifactID)
		assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship it today?", []model.AskOption{
			{Label: "Ship", Description: "Release it"},
			{Label: "Hold", Description: "Wait for review"},
		})
	})

	// The decision holds a second bullet list, which breaks its content rule, and the question is
	// written into it all the same: rebuilding the body would keep the last list and drop the
	// one before it, deleting a person's options unseen, so both lists are the very nodes the
	// document held, and the row the edit reports holds the options of both.
	t.Run("the decision holds a second bullet list", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			ask := firstAsk(tree)
			ask.Children[0].Children = nil
			ask.Children = append(ask.Children, parsedBlock(t, "- Defer: Wait a week\n"))
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		bulletLists := func(ask *pmdoc.Node) []*pmdoc.Node {
			found := []*pmdoc.Node{}
			for _, child := range ask.Children {
				if child.Type == "bullet_list" {
					found = append(found, child)
				}
			}
			return found
		}
		held := bulletLists(firstAsk(liveTree(t, service, artifactID)))
		if len(held) != 2 {
			t.Fatalf("the emptied decision holds %d bullet lists, want the two it was given", len(held))
		}
		var stored AskBlockText
		if err := joinedWrite(service, func(ctx context.Context) (err error) {
			stored, err = service.SetAskBlockText(ctx, artifactID, "ask-lists",
				AskBlockEdit{Question: new("Should we ship it today?")}, model.Actor{Kind: "user", ID: "alice"})
			return err
		}); err != nil {
			t.Fatalf("the ask edit sending a question to the emptied decision: %v", err)
		}
		ask := firstAsk(liveTree(t, service, artifactID))
		block, err := parseAskBlock(ask)
		if err != nil {
			t.Fatalf("the repaired block does not parse: %v", err)
		}
		every := []model.AskOption{
			{Label: "Ship", Description: "Release it"},
			{Label: "Hold", Description: "Wait for review"},
			{Label: "Defer", Description: "Wait a week"},
		}
		if block.question != "Should we ship it today?" || !slices.Equal(block.options, every) {
			t.Fatalf("the repaired block asks %q with options %#v, want the question sent and every option of both lists",
				block.question, block.options)
		}
		written := bulletLists(ask)
		if len(written) != len(held) {
			t.Fatalf("the question edit left %d bullet lists, want the %d the document held", len(written), len(held))
		}
		for index, list := range written {
			if !list.EqualWithBlockIDs(held[index]) {
				t.Fatalf("the question edit rewrote bullet list %d, want the very nodes the document held", index)
			}
		}
		if stored.Question != block.question || !slices.Equal(stored.Options, every) {
			t.Fatalf("the ask row takes question %q with options %#v, want what the block holds",
				stored.Question, stored.Options)
		}
		// The block holds both lists, so no parse reads it as a decision and the options above
		// are the two lists joined. The write says so, and the ask-edit route keeps the row's
		// own options rather than writing that reading into it.
		if !stored.SeveralOptionLists {
			t.Fatalf("the repair of a decision holding two lists does not report them: %#v", stored)
		}
	})
}

// An ask edit carrying no question cannot repair an emptied question, so it is refused with the
// step that does: sending the ask edit again with a question. Where that ask breaks its content
// rule too the refusal says the same thing and names no delete: a document edit may not leave a
// decision it changes unreadable, and one asking nothing is unreadable, so the deletes the
// content rule would name are a step no caller can take yet.
func TestEditAskOfAnEmptyQuestionNamesTheStep(t *testing.T) {
	t.Run("the decision keeps its content rule", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			firstAsk(tree).Children[0].Children = nil
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		before := liveTree(t, service, artifactID)
		err := editAskUrgency(service, artifactID, "ask-lists")
		want := `block: ask block "ask-lists" has an empty question; send this ask edit again with a question, ` +
			`which writes it into the block`
		if err == nil || err.Error() != want {
			t.Fatalf("refusal =\n%v\nwant\n%s", err, want)
		}
		assertAskEditWroteNothing(t, service, artifactID, before, err)
	})
	t.Run("the decision breaks its content rule too", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			ask := firstAsk(tree)
			ask.Children[0].Children = nil
			ask.Children = append(ask.Children, parsedBlock(t, "- Defer: Wait a week\n"))
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		before := liveTree(t, service, artifactID)
		err := editAskUrgency(service, artifactID, "ask-lists")
		want := `block: ask block "ask-lists" has an empty question; send this ask edit again with a question, ` +
			`which writes it into the block`
		if err == nil || err.Error() != want {
			t.Fatalf("refusal =\n%v\nwant\n%s", err, want)
		}
		// The blocks the content rule leaves no room for are deleted once the decision asks
		// something again, so the refusal of this edit names none of them.
		if blocksToDelete.MatchString(err.Error()) {
			t.Fatalf("the refusal = %v, want it to name no delete while the decision asks nothing", err)
		}
		assertAskEditWroteNothing(t, service, artifactID, before, err)
	})

	// The paragraph a question would be written over holds no text, but it holds an image, which
	// no question carries: writing the question would delete it unseen. The refusal quotes it and
	// names the two places it is kept, since a document edit cannot write a question beside it:
	// an upload of the document as a new version, which is the path an agent has, and the
	// document editor, where a person writes it.
	t.Run("a paragraph the question would replace holds an image", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			ask := firstAsk(tree)
			ask.Children[0] = parsedBlock(t, "![a photo](/photo.png)\n")
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		before := liveTree(t, service, artifactID)
		photo, _ := firstAsk(before).Children[0].Attrs[pmdoc.BlockIDAttr].(string)
		err := sendAskOptions(service, artifactID, AskBlockEdit{Question: new("Should we ship?")})
		want := `block: ask block "ask-lists" has an empty question, and writing a question into it would delete ` +
			`what one of its paragraphs holds, which is no text: block ` + strconv.Quote(photo) +
			` (paragraph) reads "![a photo](/photo.png)"; upload the document again as a new version with ` +
			`the question written beside that paragraph (dispatch artifact --name <the document> ` +
			`--content-file -), which is accepted for such a block, or write the question beside it in the ` +
			`document editor; either keeps what the paragraph holds`
		if err == nil || err.Error() != want {
			t.Fatalf("refusal =\n%v\nwant\n%s", err, want)
		}
		assertAskEditWroteNothing(t, service, artifactID, before, err)
	})
}

// The refusals of a decision whose question a person emptied and which holds a second bullet
// list name a path a caller can walk, step by step: the edit carrying no question is refused for
// the question, the ask edit carrying one writes it whatever the content rule says, the refusal
// after that names the blocks to delete, one document edit deletes them, and the ask edit after
// it applies. The decision ends reading as one, and every word no refusal named for deletion -
// the words under an option among them - is still in the document.
func TestEditAskRefusalOfAnEmptyQuestionNamesAPathThatApplies(t *testing.T) {
	service, artifactID := seedIndexedRichAsk(t)
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		ask := firstAsk(tree)
		ask.Children[0].Children = nil
		ask.Children = append(ask.Children, parsedBlock(t, "- Defer: Wait a week\n"))
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	before := liveTree(t, service, artifactID)

	// The urgency edit names the question, the one step that applies while the decision asks
	// nothing: no document edit may leave it unreadable, so none of them deletes its second list.
	urgency := editAskUrgency(service, artifactID, "ask-lists")
	want := `block: ask block "ask-lists" has an empty question; send this ask edit again with a question, ` +
		`which writes it into the block`
	if urgency == nil || urgency.Error() != want {
		t.Fatalf("refusal of the urgency edit =\n%v\nwant\n%s", urgency, want)
	}
	assertAskEditWroteNothing(t, service, artifactID, before, urgency)

	// The ask edit sent again with a question writes it, and the row it reports holds that
	// question and every option of both lists, which is what the block then parses back to.
	var stored AskBlockText
	if err := joinedWrite(service, func(ctx context.Context) (err error) {
		stored, err = service.SetAskBlockText(ctx, artifactID, "ask-lists",
			AskBlockEdit{Question: new("Should we ship?")}, model.Actor{Kind: "user", ID: "alice"})
		return err
	}); err != nil {
		t.Fatalf("the ask edit sent again with a question, as the refusal names: %v\nrefusal: %v", err, urgency)
	}
	every := []model.AskOption{
		{Label: "Ship", Description: "Release it"},
		{Label: "Hold", Description: "Wait for review"},
		{Label: "Defer", Description: "Wait a week"},
	}
	written, parseErr := parseAskBlock(firstAsk(liveTree(t, service, artifactID)))
	if parseErr != nil {
		t.Fatalf("the decision does not parse once the question is written: %v", parseErr)
	}
	if written.question != "Should we ship?" || !slices.Equal(written.options, every) {
		t.Fatalf("the decision asks %q with options %#v, want the question sent and every option of both lists",
			written.question, written.options)
	}
	if stored.Question != written.question || !slices.Equal(stored.Options, every) {
		t.Fatalf("the ask row takes question %q with options %#v, want what the block holds",
			stored.Question, stored.Options)
	}

	// The decision still breaks its content rule, so the next ask edit of it names the blocks to
	// delete, which is a step a caller can take now that it asks something.
	broken := editAskUrgency(service, artifactID, "ask-lists")
	deletes, _ := refusalDeletes(t, broken)
	ask := firstAsk(liveTree(t, service, artifactID))
	ops := make([]model.EditOp, 0, len(deletes))
	kept := []string{}
	for _, child := range ask.Children {
		if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); slices.Contains(deletes, id) {
			ops = append(ops, model.EditOp{Op: "delete", Block: id})
			continue
		}
		kept = append(kept, textWords(child)...)
	}
	if len(ops) != len(deletes) {
		t.Fatalf("the refusal names %q to delete, which are not all blocks of the decision: %v", deletes, broken)
	}

	// One document edit holding those deletes, and the ask edit after it, as the refusal says.
	if _, applyErr := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); applyErr != nil {
		t.Fatalf("one document edit deleting %q, as the refusal names: %v\nrefusal: %v", deletes, applyErr, broken)
	}
	if editErr := editAskUrgency(service, artifactID, "ask-lists"); editErr != nil {
		t.Fatalf("ask edit after deleting %q, as the refusal names: %v\nrefusal: %v", deletes, editErr, broken)
	}
	settleCurrentGeneration(t, service, artifactID)
	after := liveTree(t, service, artifactID)
	repaired := firstAsk(after)
	if got, exists := repaired.Attrs["invalid"]; exists {
		t.Fatalf("after following the refusals the decision is invalid: %#v", got)
	}
	if reason := readAskBlockStrictly(repaired); reason != nil || repaired.Attrs["urgency"] != "high" {
		t.Fatalf("after following the refusals the decision reads %v with urgency %v, want it valid and the ask edit applied",
			reason, repaired.Attrs["urgency"])
	}
	words := textWords(after)
	for _, word := range kept {
		if slices.Contains(words, word) {
			continue
		}
		t.Fatalf("the document lost %q, which no refusal named for deletion: it reads %q", word, words)
	}
}

// An options edit of a decision holding more than one bullet list cannot say which list an
// option is in, so it is refused on the options while the emptied question still has it read
// leniently: a merge across the lists would move a person's words from one into another. The
// question alone applies, and once the decision reads as one the same options edit applies too.
func TestEditAskOptionsOfADecisionHoldingTwoListsWaitForTheQuestion(t *testing.T) {
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		ask := firstAsk(tree)
		ask.Children[0].Children = nil
		ask.Children = append(ask.Children, parsedBlock(t, "- Defer: Wait a week\n"))
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	before := liveTree(t, service, artifactID)
	options := []model.AskOption{
		{Label: "Ship", Description: "Release it today"},
		{Label: "Hold", Description: "Wait for review"},
		{Label: "Defer", Description: "Wait a week"},
	}
	edit := AskBlockEdit{Question: new("Should we ship?"), Options: &options}
	err := sendAskOptions(service, artifactID, edit)
	want := `options: this decision holds more than one bullet list, so an options edit cannot say which list ` +
		`an option is in; send the question alone, and edit the options once the decision reads as one`
	if err == nil || err.Error() != want {
		t.Fatalf("refusal =\n%v\nwant\n%s", err, want)
	}
	assertAskEditWroteNothing(t, service, artifactID, before, err)
	if questionErr := sendAskOptions(service, artifactID, AskBlockEdit{Question: new("Should we ship?")}); questionErr != nil {
		t.Fatalf("the question alone, as the refusal names: %v\nrefusal: %v", questionErr, err)
	}
	// The decision reads as one once its second list is gone, which is the delete its own
	// refusal names, and the options edit refused above then applies.
	deletes, _ := refusalDeletes(t, sendAskOptions(service, artifactID, edit))
	ops := make([]model.EditOp, 0, len(deletes))
	for _, id := range deletes {
		ops = append(ops, model.EditOp{Op: "delete", Block: id})
	}
	if _, applyErr := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); applyErr != nil {
		t.Fatalf("one document edit deleting %q, as the refusal names: %v", deletes, applyErr)
	}
	if editErr := sendAskOptions(service, artifactID, edit); editErr != nil {
		t.Fatalf("the options edit once the decision reads as one: %v", editErr)
	}
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", options)
}

// askRowCount is how many ask rows are anchored to blockID. Settlement opens a row only for a
// block its parser reads (addedAskBlocks, readableAskBlockIDs), so a decision that never held a
// question has none, and a document edit may delete it.
func askRowCount(t *testing.T, service *Service, artifactID, blockID string) int {
	t.Helper()
	var count int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from asks where block_artifact_id = $1 and block_id = $2
	`, artifactID, blockID).Scan(&count); err != nil {
		t.Fatalf("count the ask rows of block %q: %v", blockID, err)
	}
	return count
}

// Every decision body of one to five blocks - each a paragraph holding text (P), a paragraph
// holding none (E), a bullet list (L), or a bullet list whose item holds a nested list (N) - that
// breaks the content rule is refused, and the refusal followed exactly as written - the same ask
// edit again with a question where no paragraph of the body holds text, then ONE document edit
// holding one delete per block it names, then the same ask edit - leaves the decision valid,
// every word it did not name for deletion still in the document, and every word it did quoted in
// it; or the refusal names no delete and says why. No body is skipped for resembling another, and
// each block's words are its own, so a lost word names its block.
func TestSetAskBlockTextRefusalOfEveryBrokenBodyLeadsToAValidDecision(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	bodies := askBodies([]string{"P", "E", "L", "N"}, 5)
	if len(bodies) != 4+16+64+256+1024 {
		t.Fatalf("enumerated %d bodies, want every one of 1364", len(bodies))
	}
	var (
		mu                        sync.Mutex
		broken, followed, unnamed int
		noDelete                  = map[string]int{}
		noDeleteShape             = map[string]string{}
	)
	t.Cleanup(func() {
		t.Logf("%d bodies enumerated, %d breaking the content rule: %d followed to a valid decision, %d naming no delete and saying why, %d failing",
			len(bodies), broken, followed, unnamed, broken-followed-unnamed)
		for reason, count := range noDelete {
			t.Logf("%d naming no delete (%s among them): %s", count, noDeleteShape[reason], reason)
		}
	})
	for index, body := range bodies {
		children := make([]*pmdoc.Node, len(body))
		for position, kind := range body {
			children[position] = askBodyBlock(t, kind, position+1)
		}
		if pmdoc.AskContentError(&pmdoc.Node{Type: "ask", Children: children}) == nil {
			continue
		}
		broken++
		shape := strings.Join(body, "-")
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			// A document of its own on an issue of its own, so no two bodies share a lock.
			artifactID := createIssueDocument(t, service.store, index+2, "# First")
			seedServiceText(t, service, artifactID, ":::ask{#ask-lists urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
			settleCurrentGeneration(t, service, artifactID)
			editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				firstAsk(tree).Children = children
				return tree
			})
			settleCurrentGeneration(t, service, artifactID)
			before := liveTree(t, service, artifactID)
			ask := firstAsk(before)
			edit := AskBlockEdit{Urgency: new("high")}
			err := sendAskOptions(service, artifactID, edit)
			unrepresentable := askEditRefusal(t, before, "ask-lists", err)
			breaksTheRule := true
			if unrepresentable.Reason == emptyQuestionStep("ask-lists") {
				before, ask, err, unrepresentable, breaksTheRule = followEmptyQuestionStep(
					t, service, artifactID, "ask-lists", &edit, err, unrepresentable)
			}
			kept := textWords(before)
			if breaksTheRule {
				if !blocksToDelete.MatchString(unrepresentable.Reason) {
					why := noDeleteExplanation(ask, unrepresentable.Reason)
					if why == "" || strings.Contains(unrepresentable.Reason, "delete the blocks") {
						t.Fatalf("the refusal names no delete list and does not say why: %v", err)
					}
					if !liveTree(t, service, artifactID).EqualWithBlockIDs(before) {
						t.Fatalf("the refused ask edit wrote to the document: %v", err)
					}
					mu.Lock()
					defer mu.Unlock()
					unnamed++
					if noDelete[why]++; noDelete[why] == 1 {
						noDeleteShape[why] = shape
					}
					return
				}
				deletes, quoted := refusalDeletes(t, err)
				if len(deletes) == 0 || len(quoted) != len(ask.Children) {
					t.Fatalf("the refusal names %q to delete and quotes %d blocks, want a delete and the ask's %d: %v", deletes, len(quoted), len(ask.Children), err)
				}
				ops := make([]model.EditOp, 0, len(deletes))
				for _, id := range deletes {
					if !slices.ContainsFunc(ask.Children, func(child *pmdoc.Node) bool { return child.Attrs[pmdoc.BlockIDAttr] == id }) {
						t.Fatalf("the refusal names %q to delete, which is no block of the ask: %v", id, err)
					}
					ops = append(ops, model.EditOp{Op: "delete", Block: id})
				}
				kept = []string{}
				for _, block := range before.Children {
					if block != ask {
						kept = append(kept, textWords(block)...)
						continue
					}
					for _, child := range ask.Children {
						id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
						if !slices.Contains(deletes, id) {
							kept = append(kept, textWords(child)...)
							continue
						}
						for _, word := range textWords(child) {
							if !strings.Contains(quoted[id], word) {
								t.Fatalf("the refusal does not quote %q of block %q it has the caller delete: %v", word, id, err)
							}
						}
					}
				}
				if _, applyErr := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); applyErr != nil {
					t.Fatalf("one document edit deleting %q, as the refusal names: %v\nrefusal: %v", deletes, applyErr, err)
				}
				if editErr := sendAskOptions(service, artifactID, edit); editErr != nil {
					t.Fatalf("ask edit after deleting %q, as the refusal names: %v\nrefusal: %v", deletes, editErr, err)
				}
			}
			settleCurrentGeneration(t, service, artifactID)
			after := liveTree(t, service, artifactID)
			repaired := firstAsk(after)
			if got, exists := repaired.Attrs["invalid"]; exists {
				t.Fatalf("after following the refusal the ask is invalid: %v\nrefusal: %v", got, err)
			}
			block, parseErr := parseAskBlock(repaired)
			if strictErr := readAskBlockStrictly(repaired); parseErr != nil || strictErr != nil || repaired.Attrs["urgency"] != "high" {
				t.Fatalf("after following the refusal the ask reads %v / %v with urgency %v, want it valid and the ask edit applied\nrefusal: %v",
					parseErr, strictErr, repaired.Attrs["urgency"], err)
			}
			if edit.Question != nil && block.question != *edit.Question {
				t.Fatalf("after following the refusal the ask asks %q, want the question the follow wrote, %q\nrefusal: %v",
					block.question, *edit.Question, err)
			}
			if state, question, options := blockAskRow(t, service, artifactID, "ask-lists"); state != "open" || question != block.question || !slices.Equal(options, block.options) {
				t.Fatalf("ask row = state=%q question=%q options=%#v, want it open as the block reads: %q %#v", state, question, options, block.question, block.options)
			}
			words := textWords(after)
			for _, word := range kept {
				if !slices.Contains(words, word) {
					t.Fatalf("the document lost %q, which the refusal did not name for deletion: it reads %q\nrefusal: %v", word, words, err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			followed++
		})
	}
}

// askBodyBlock is the block of kind - P a paragraph holding text, E a paragraph holding none, L a
// bullet list, N a bullet list whose item holds a nested list - at position in an ask's body, its
// words found nowhere else in the body.
func askBodyBlock(t *testing.T, kind string, position int) *pmdoc.Node {
	t.Helper()
	var block *pmdoc.Node
	switch kind {
	case "P":
		block = parsedBlock(t, fmt.Sprintf("Question%d asks%d?\n", position, position))
	case "E":
		block = &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}}
	case "L":
		block = parsedBlock(t, fmt.Sprintf("- Option%d: Means%d\n- Other%d\n", position, position, position))
	case "N":
		block = parsedBlock(t, fmt.Sprintf("- Choice%d: Holds%d\n  - Nested%d\n", position, position, position))
	default:
		t.Fatalf("no ask body block of kind %q", kind)
	}
	block.Attrs[pmdoc.BlockIDAttr] = fmt.Sprintf("body-%d", position)
	return block
}

// askBodies is every decision body of one to longest blocks over kinds, in order: each kind, then
// each pair, and so on, none skipped for resembling another.
func askBodies(kinds []string, longest int) [][]string {
	var bodies [][]string
	var extend func(body []string)
	extend = func(body []string) {
		if len(body) > 0 {
			bodies = append(bodies, body)
		}
		if len(body) == longest {
			return
		}
		for _, kind := range kinds {
			extend(append(slices.Clone(body), kind))
		}
	}
	extend(nil)
	return bodies
}

// noDeleteExplanation is why reason, a refusal of an edit of ask naming no delete, names none, as
// it says it - or "" when it does not say, or says what is not so. A refusal that names no delete
// because no edit of deletes repairs the decision says so too: the decision is repaired in the
// document editor, the one way out it offers, since no ask edit of another decision is one. A
// decision asking nothing is no such end: its refusal names the step that repairs it, this same
// ask edit again carrying a question (emptyQuestionStep), which a follow takes.
func noDeleteExplanation(ask *pmdoc.Node, reason string) string {
	question := slices.ContainsFunc(ask.Children, func(child *pmdoc.Node) bool {
		return child.Type == "paragraph" && strings.TrimSpace(pmdoc.TextContent(child)) != ""
	})
	if question {
		return ""
	}
	const explanation = "no document edit deleting this decision's blocks, or those of a decision nested in it, leaves it valid"
	if strings.Contains(reason, explanation) && strings.Contains(reason, repairedInTheEditor) {
		return explanation
	}
	return ""
}

// AskContentError reads the decisions nested in an ask too, so an ask whose own blocks keep the
// rule breaks it when an option holds a decision with two lists. Deleting the ask's own blocks
// repairs nothing there, and a document edit may not leave a decision it changes unreadable, so
// the refusal names the nested decision's own extra block, by its id, as the one edit that
// repairs both - which applies, and the ask edit after it.
func TestSetAskBlockTextRefusalOfAnAskHoldingABrokenDecisionNamesTheNestedDelete(t *testing.T) {
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	nested := parsedBlock(t, ":::ask{#ask-nested urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich day?\n\n- Monday\n:::\n")
	nested.Children = append(nested.Children, parsedBlock(t, "- Tuesday\n"))
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		item := firstAsk(tree).Children[1].Children[0]
		item.Children = append(item.Children, nested)
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	before := liveTree(t, service, artifactID)
	options, _ := firstAsk(before).Children[1].Attrs[pmdoc.BlockIDAttr].(string)
	tuesday, _ := firstAsk(before).Children[1].Children[0].Children[1].Children[2].Attrs[pmdoc.BlockIDAttr].(string)
	err := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")})
	assertBrokenAskEditRefused(t, service, artifactID, before, err)
	deletes, quoted := refusalDeletes(t, err)
	if !slices.Equal(deletes, []string{tuesday}) {
		t.Fatalf("the refusal names %q to delete, want the nested decision's second list %q alone: %v", deletes, tuesday, err)
	}
	if !strings.Contains(quoted[options], "Tuesday") {
		t.Fatalf("the refusal quotes the option list as %q, want the nested decision's words in it: %v", quoted[options], err)
	}
	ops := []model.EditOp{{Op: "delete", Block: tuesday}}
	if _, applyErr := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); applyErr != nil {
		t.Fatalf("one document edit deleting %q, as the refusal names: %v\nrefusal: %v", tuesday, applyErr, err)
	}
	if editErr := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")}); editErr != nil {
		t.Fatalf("ask edit after deleting %q, as the refusal names: %v\nrefusal: %v", tuesday, editErr, err)
	}
	settleCurrentGeneration(t, service, artifactID)
	repaired := firstAsk(liveTree(t, service, artifactID))
	if reason := readAskBlockStrictly(repaired); reason != nil || repaired.Attrs["urgency"] != "high" {
		t.Fatalf("after following the refusal the ask reads %v with urgency %v, want it valid and the ask edit applied", reason, repaired.Attrs["urgency"])
	}
	assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", []model.AskOption{
		{Label: "Ship", Description: "Release it"},
		{Label: "Hold", Description: "Wait for review"},
	})
	if state, question, options := blockAskRow(t, service, artifactID, "ask-nested"); state != "open" ||
		question != "Which day?" || !slices.Equal(options, []model.AskOption{{Label: "Monday"}}) {
		t.Fatalf("the nested ask row = state=%q question=%q options=%#v, want it open as its repaired block reads", state, question, options)
	}
}

// The exhaustive test above with a fifth kind of block, D: a bullet list whose option holds a
// decision of its own, valid or broken, its body cycled through every body of one to three blocks
// over the same four kinds. Every decision body of one to four blocks over the five kinds is
// enumerated, and each one that breaks the content rule - its own blocks', or a nested decision's,
// which the rule reads too - is refused for every decision an option holds that settlement cannot
// read and for the decision itself. Each refusal is followed exactly as written - the same ask
// edit again with a question where the decision asks nothing, then ONE document edit holding one
// delete per block it names, then the same ask edit - and leaves the decision it was sent to
// valid, every word it did not name for deletion still in the document and every word it did
// quoted in it; or it names no delete and says why, naming no decision that already reads as one,
// so a caller is never sent to one, nor from the decision to the one holding it and back.
func TestSetAskBlockTextRefusalOfEveryBrokenBodyHoldingADecisionLeadsToAValidDecision(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	nested := askBodies([]string{"P", "E", "L", "N"}, 3)
	bodies := askBodies([]string{"P", "E", "L", "N", "D"}, 4)
	if len(nested) != 4+16+64 || len(bodies) != 5+25+125+625 {
		t.Fatalf("enumerated %d nested bodies and %d bodies, want every one of 84 and 780", len(nested), len(bodies))
	}
	var (
		mu                               sync.Mutex
		broken, followed, unnamed, valid int
		noDelete                         = map[string]int{}
		noDeleteShape                    = map[string]string{}
	)
	t.Cleanup(func() {
		t.Logf("%d bodies enumerated, %d breaking the content rule: %d refusals followed to a valid decision, "+
			"%d naming no delete and saying why; %d of the %d bodies end with the decision reading as one",
			len(bodies), broken, followed, unnamed, valid, broken)
		for reason, count := range noDelete {
			t.Logf("%d naming no delete (%s among them): %s", count, noDeleteShape[reason], reason)
		}
	})
	for index, body := range bodies {
		// The blocks are built again for each use: the live room writes the nodes it is given,
		// and the tree read back holds its own.
		build := func(t *testing.T) []*pmdoc.Node {
			built := make([]*pmdoc.Node, len(body))
			for position, kind := range body {
				if kind != "D" {
					built[position] = askBodyBlock(t, kind, position+1)
					continue
				}
				built[position] = askNestedDecisionBlock(t, position+1, nested[(index+position)%len(nested)])
			}
			return built
		}
		shape := make([]string, len(body))
		for position, kind := range body {
			shape[position] = kind
			if kind == "D" {
				shape[position] += strings.Join(nested[(index+position)%len(nested)], "")
			}
		}
		if pmdoc.AskContentError(&pmdoc.Node{Type: "ask", Children: build(t)}) == nil {
			continue
		}
		broken++
		t.Run(strings.Join(shape, "-"), func(t *testing.T) {
			t.Parallel()
			// A document of its own on an issue of its own, so no two bodies share a lock.
			artifactID := createIssueDocument(t, service.store, index+2, "# First")
			seedServiceText(t, service, artifactID, ":::ask{#ask-lists urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
			settleCurrentGeneration(t, service, artifactID)
			editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				firstAsk(tree).Children = build(t)
				return tree
			})
			settleCurrentGeneration(t, service, artifactID)
			before := liveTree(t, service, artifactID)
			unreadable := unreadableAsks(before)
			whys := []string{}
			// The decision an option holds is refused on its own first: repairing it must not
			// wait on the decision holding it, and must not send the caller round in a circle.
			// A decision asking nothing is read here too, since the refusal of an ask edit of it
			// names the step that repairs it, a question, which no delete writes.
			for _, decision := range nestedAsks(firstAsk(before).Children) {
				if readAskBlockStrictly(decision) == nil {
					continue
				}
				id, _ := decision.Attrs[pmdoc.BlockIDAttr].(string)
				whys = append(whys, followAskRefusal(t, service, artifactID, id))
			}
			if readAskBlockStrictly(firstAsk(liveTree(t, service, artifactID))) != nil {
				whys = append(whys, followAskRefusal(t, service, artifactID, "ask-lists"))
			} else if err := editAskUrgency(service, artifactID, "ask-lists"); err != nil {
				t.Fatalf("ask edit of the decision the nested repair left reading as one: %v", err)
			}
			settleCurrentGeneration(t, service, artifactID)
			// No follow left a decision worse than it found it: every decision that still does
			// not read as one did not read as one before any of them either, each refusal having
			// named either a delete that repaired the decision it was sent to, or no delete at
			// all and why.
			after := liveTree(t, service, artifactID)
			for id, reason := range unreadableAsks(after) {
				if _, found := unreadable[id]; !found {
					t.Fatalf("after following every refusal, ask block %q no longer reads as a decision: %v", id, reason)
				}
				// Following the refusals has reached its end: a decision that still does not read
				// as one is refused the next edit with no delete at all, so a caller following
				// them is never sent round a circle.
				next := editAskUrgency(service, artifactID, id)
				var unrepresentable *ErrAskBlockUnrepresentable
				if !errors.As(next, &unrepresentable) || blocksToDelete.MatchString(unrepresentable.Reason) {
					t.Fatalf("the edit of ask block %q, which still does not read as a decision (%v), = %v, want a refusal naming no delete",
						id, reason, next)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if readAskBlockStrictly(firstAsk(after)) == nil {
				valid++
			}
			for _, why := range whys {
				if why == "" {
					followed++
					continue
				}
				unnamed++
				if noDelete[why]++; noDelete[why] == 1 {
					noDeleteShape[why] = strings.Join(shape, "-")
				}
			}
		})
	}
}

// The four shapes the review of sjawhar/legion#2433 found, word for word: a decision and the one
// an option holds both breaking the rule, where one edit must repair both; a decision holding one
// that reads as no decision for another reason (an empty question) before the one that breaks the
// rule, which the refusal must not name in its place; and the two where the decision holding the
// edited one would have to change - it has no question of its own, or it is decided and broken
// too - where the refusal names no delete and no decision to edit.
func TestSetAskBlockTextRefusalOfNestedDecisionsNamesOnlyWhatBreaksTheRule(t *testing.T) {
	t.Run("the decision and the one it holds both broken", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			ask := firstAsk(tree)
			decision := parsedBlock(t, ":::ask{#ask-nested urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich day?\n\n- Monday\n:::\n")
			decision.Children = append(decision.Children, parsedBlock(t, "- Tuesday\n"))
			item := ask.Children[1].Children[0]
			item.Children = append(item.Children, decision)
			ask.Children = append(ask.Children, parsedBlock(t, "- Defer: Wait a week\n"))
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		before := liveTree(t, service, artifactID)
		outer := firstAsk(before)
		tuesday, _ := outer.Children[1].Children[0].Children[1].Children[2].Attrs[pmdoc.BlockIDAttr].(string)
		defer_, _ := outer.Children[2].Attrs[pmdoc.BlockIDAttr].(string)
		// The decision an option holds cannot be repaired on its own: the delete changes the
		// decision holding it, which the edit may not leave unreadable, so its refusal names no
		// delete - and no decision to edit either, since an ask edit of the one holding it is not
		// the caller's to make.
		held := editAskUrgency(service, artifactID, "ask-nested")
		if held == nil || blocksToDelete.MatchString(held.Error()) || sendsTheCallerOn(held.Error()) ||
			noDeleteReason(held.Error()) != heldBackByItsHolder {
			t.Fatalf("the refusal of the held decision = %v, want it to name no delete and no decision to edit", held)
		}
		assertHeldBackByItsHolder(t, before, "ask-nested", held)
		if !liveTree(t, service, artifactID).EqualWithBlockIDs(before) {
			t.Fatalf("the refused ask edit wrote to the document: %v", held)
		}
		// The decision's own refusal names one edit repairing both, in document order.
		err := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")})
		deletes, _ := refusalDeletes(t, err)
		if !slices.Equal(deletes, []string{tuesday, defer_}) {
			t.Fatalf("the refusal names %q to delete, want the held decision's second list and the decision's own %q: %v",
				deletes, []string{tuesday, defer_}, err)
		}
		ops := []model.EditOp{{Op: "delete", Block: tuesday}, {Op: "delete", Block: defer_}}
		if _, applyErr := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); applyErr != nil {
			t.Fatalf("one document edit deleting %q, as the refusal names: %v\nrefusal: %v", deletes, applyErr, err)
		}
		if editErr := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")}); editErr != nil {
			t.Fatalf("ask edit after deleting %q, as the refusal names: %v\nrefusal: %v", deletes, editErr, err)
		}
		settleCurrentGeneration(t, service, artifactID)
		repaired := firstAsk(liveTree(t, service, artifactID))
		if reason := readAskBlockStrictly(repaired); reason != nil || repaired.Attrs["urgency"] != "high" {
			t.Fatalf("after following the refusal the decision reads %v with urgency %v, want it valid and the ask edit applied", reason, repaired.Attrs["urgency"])
		}
		if editErr := editAskUrgency(service, artifactID, "ask-nested"); editErr != nil {
			t.Fatalf("ask edit of the held decision after the one edit: %v", editErr)
		}
	})
	t.Run("an unreadable decision before the broken one", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			list := firstAsk(tree).Children[1]
			empty := parsedBlock(t, ":::ask{#ask-y urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich month?\n\n- May\n:::\n")
			broken := parsedBlock(t, ":::ask{#ask-nested urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich day?\n\n- Monday\n:::\n")
			empty.Children[0].Children = nil
			broken.Children = append(broken.Children, parsedBlock(t, "- Tuesday\n"))
			list.Children[0].Children = append(list.Children[0].Children, empty)
			list.Children[1].Children = append(list.Children[1].Children, broken)
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		before := liveTree(t, service, artifactID)
		tuesday, _ := firstAsk(before).Children[1].Children[1].Children[1].Children[2].Attrs[pmdoc.BlockIDAttr].(string)
		err := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")})
		if err == nil || slices.Contains(namedAsks(t, err), "ask-y") {
			t.Fatalf("the refusal = %v, want it to name only the decision breaking the content rule, never the one with no question", err)
		}
		deletes, _ := refusalDeletes(t, err)
		if !slices.Equal(deletes, []string{tuesday}) {
			t.Fatalf("the refusal names %q to delete, want the broken decision's second list %q alone: %v", deletes, tuesday, err)
		}
		ops := []model.EditOp{{Op: "delete", Block: tuesday}}
		if _, applyErr := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); applyErr != nil {
			t.Fatalf("one document edit deleting %q, as the refusal names: %v\nrefusal: %v", tuesday, applyErr, err)
		}
		if editErr := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")}); editErr != nil {
			t.Fatalf("ask edit after deleting %q, as the refusal names: %v\nrefusal: %v", tuesday, editErr, err)
		}
		settleCurrentGeneration(t, service, artifactID)
		after := liveTree(t, service, artifactID)
		repaired := firstAsk(after)
		if reason := readAskBlockStrictly(repaired); reason != nil || repaired.Attrs["urgency"] != "high" {
			t.Fatalf("after following the refusal the decision reads %v with urgency %v, want it valid and the ask edit applied", reason, repaired.Attrs["urgency"])
		}
		// The decision with no question is nobody's to repair by deleting: it keeps the content
		// rule, the refusal named none of its blocks, and settlement leaves it invalid.
		if reason := blockByID(after, "ask-y").Attrs["invalid"]; reason != `ask block "ask-y" has an empty question` {
			t.Fatalf("the decision with no question is invalid = %#v, want settlement's own reason", reason)
		}
	})

	// The decision nested three deep holds no paragraph with text, so deleting the blocks its own
	// content rule leaves no room for would keep none of them and leave a decision no parse reads.
	// The one edit that repairs the two decisions holding it deletes that decision itself, by its
	// own block id, and never the blocks inside it.
	t.Run("the decision nested three deep holds no question", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			middle := parsedBlock(t, ":::ask{#ask-middle urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich day?\n\n- Monday\n:::\n")
			inner := parsedBlock(t, ":::ask{#ask-inner urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich month?\n\n- May\n:::\n")
			inner.Children[0].Children = nil
			inner.Children = append(inner.Children, parsedBlock(t, "- June\n"))
			middle.Children[1].Children[0].Children = append(middle.Children[1].Children[0].Children, inner)
			item := firstAsk(tree).Children[1].Children[0]
			item.Children = append(item.Children, middle)
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		before := liveTree(t, service, artifactID)
		// A document edit may not delete a decision whose ask is still open. The innermost
		// decision never held a question, so no settlement ever read it or opened a row for it.
		if rows := askRowCount(t, service, artifactID, "ask-inner"); rows != 0 {
			t.Fatalf("the decision that never held a question has %d ask rows, want none", rows)
		}
		err := editAskUrgency(service, artifactID, "ask-lists")
		var unrepresentable *ErrAskBlockUnrepresentable
		if !errors.As(err, &unrepresentable) {
			t.Fatalf("the ask edit of the outermost decision = %v, want ErrAskBlockUnrepresentable", err)
		}
		if want := `the blocks to delete are "ask-inner";`; !strings.Contains(unrepresentable.Reason, want) {
			t.Fatalf("the refusal = %v, want its delete list to be the innermost decision's own block alone", err)
		}
		if !strings.Contains(unrepresentable.Reason, "a decision among them whose ask is still open cannot be deleted by a document edit") {
			t.Fatalf("the refusal = %v, want it to say a decision whose ask is open cannot be deleted", err)
		}
		assertAskEditWroteNothing(t, service, artifactID, before, err)
		ops := []model.EditOp{{Op: "delete", Block: "ask-inner"}}
		if _, applyErr := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); applyErr != nil {
			t.Fatalf("one document edit deleting %q, as the refusal names: %v\nrefusal: %v", "ask-inner", applyErr, err)
		}
		if editErr := editAskUrgency(service, artifactID, "ask-lists"); editErr != nil {
			t.Fatalf("ask edit after deleting %q, as the refusal names: %v\nrefusal: %v", "ask-inner", editErr, err)
		}
		settleCurrentGeneration(t, service, artifactID)
		after := liveTree(t, service, artifactID)
		for _, id := range []string{"ask-lists", "ask-middle"} {
			decision := blockByID(after, id)
			if reason := readAskBlockStrictly(decision); reason != nil {
				t.Fatalf("after the repair ask block %q does not read as a decision: %v", id, reason)
			}
		}
		if blockByID(after, "ask-inner") != nil {
			t.Fatalf("the decision the refusal named is still in the document")
		}
	})

	// The decision holding the broken one has no question of its own, so no delete inside the
	// broken one leaves it readable. Its refusal names no delete and no decision to edit: the
	// one holding it is repaired by its own ask edit, which writes the question it no longer
	// holds, and that edit is not this caller's to make. Once a person writes that question back
	// in the document editor, this same ask edit, sent again, names this decision's deletes, and
	// following them leaves both decisions reading as decisions.
	t.Run("the decision holding it has no question", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			ask := firstAsk(tree)
			decision := parsedBlock(t, ":::ask{#ask-nested urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich day?\n\n- Monday\n:::\n")
			decision.Children = append(decision.Children, parsedBlock(t, "- Tuesday\n"))
			ask.Children[0].Children = nil
			item := ask.Children[1].Children[0]
			item.Children = append(item.Children, decision)
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		before := liveTree(t, service, artifactID)
		tuesday, _ := blockByID(before, "ask-nested").Children[2].Attrs[pmdoc.BlockIDAttr].(string)
		err := editAskUrgency(service, artifactID, "ask-nested")
		if got, want := err.Error(), heldBackRefusal(t, before); got != want {
			t.Fatalf("refusal =\n%s\nwant\n%s", got, want)
		}
		assertHeldBackByItsHolder(t, before, "ask-nested", err)
		assertAskEditWroteNothing(t, service, artifactID, before, err)
		// The decision holding it is repaired by its own ask edit, which writes the question it
		// no longer holds: its refusal names that step, and no delete, since a document edit
		// writes no words into a decision.
		holder := editAskUrgency(service, artifactID, "ask-lists")
		want := "block: " + emptyQuestionStep("ask-lists")
		if holder == nil || holder.Error() != want {
			t.Fatalf("the ask edit of the decision holding it =\n%v\nwant\n%s", holder, want)
		}
		if blocksToDelete.MatchString(holder.Error()) {
			t.Fatalf("the refusal of the decision holding it = %v, want it to name no delete", holder)
		}
		// The holder holds its question again, written where a person writes it, since no
		// document edit of the route's own may touch it while the decision it holds breaks the
		// content rule. This same ask edit, sent again, now names this decision's deletes.
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			blockByID(tree, "ask-lists").Children[0].Children = []*pmdoc.Node{{Type: "text", Text: "Should we ship?"}}
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		if why := followAskRefusal(t, service, artifactID, "ask-nested"); why != "" {
			t.Fatalf("the ask edit of the held decision, the one holding it otherwise repaired, names no delete: %s", why)
		}
		assertHeldPairRepaired(t, service, artifactID, before, []string{tuesday})
	})

	// The decision holding the broken one is decided and broken too: deleting inside the held one
	// leaves it broken still, and its own ask edit is one the route refuses a caller outright
	// (`ASK_NOT_OPEN`). The refusal names no delete and no decision to edit. Its holder's own
	// problem is repaired in the document editor - its own second list deleted, which leaves the
	// decision it holds broken still - and this same ask edit, sent again, then names this
	// decision's deletes, which leaves both reading as decisions, for a holder no ask edit of its
	// own reaches.
	t.Run("the decision holding it is decided and broken too", func(t *testing.T) {
		service, artifactID := seedIndexedShipAsk(t, "ask-lists")
		answerIndexedAsk(t, service, artifactID, "ask-lists")
		settleCurrentGeneration(t, service, artifactID)
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			ask := firstAsk(tree)
			decision := parsedBlock(t, ":::ask{#ask-nested urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich day?\n\n- Monday\n:::\n")
			decision.Children = append(decision.Children, parsedBlock(t, "- Tuesday\n"))
			item := ask.Children[1].Children[0]
			item.Children = append(item.Children, decision)
			ask.Children = append(ask.Children, parsedBlock(t, "- Defer: Wait a week\n"))
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		before := liveTree(t, service, artifactID)
		if state := blockByID(before, "ask-lists").Attrs["state"]; state != "answered" {
			t.Fatalf("the decision holding it is %v, want it answered", state)
		}
		err := editAskUrgency(service, artifactID, "ask-nested")
		if got, want := err.Error(), heldBackRefusal(t, before); got != want {
			t.Fatalf("refusal =\n%s\nwant\n%s", got, want)
		}
		assertHeldBackByItsHolder(t, before, "ask-nested", err)
		assertAskEditWroteNothing(t, service, artifactID, before, err)
		// A document edit of the route's own cannot write the holder's repair: it would leave an
		// ask it changed breaking the content rule, since the decision it holds still does
		// (validateEditedAskBlocks), which is why the refusal sends a person to the editor first.
		holder := blockByID(before, "ask-lists")
		own, _ := holder.Children[len(holder.Children)-1].Attrs[pmdoc.BlockIDAttr].(string)
		tuesday, _ := blockByID(before, "ask-nested").Children[2].Attrs[pmdoc.BlockIDAttr].(string)
		ops := []model.EditOp{{Op: "delete", Block: own}}
		_, applyErr := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil)
		want := `ask block "ask-nested", asking "Which day?", holds a second bullet list where its content rule, ` +
			`paragraph+ bullet_list?, allows one or more paragraphs and then at most one bullet list, last`
		var invalid *ErrInvalidAskBlock
		if !errors.As(applyErr, &invalid) || invalid.Error() != want {
			t.Fatalf("the document edit deleting the holder's own second list %q =\n%v\nwant it refused with\n%s", own, applyErr, want)
		}
		// The holder repaired where a person writes it, which is where the refusal of the held
		// decision sends them. This same ask edit, sent again, now names this decision's deletes.
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			ask := blockByID(tree, "ask-lists")
			ask.Children = ask.Children[:len(ask.Children)-1]
			return tree
		})
		settleCurrentGeneration(t, service, artifactID)
		if why := followAskRefusal(t, service, artifactID, "ask-nested"); why != "" {
			t.Fatalf("the ask edit of the held decision, the one holding it otherwise repaired, names no delete: %s", why)
		}
		assertHeldPairRepaired(t, service, artifactID, before, []string{own, tuesday})
	})
}

// heldBackRefusal is the refusal of an ask edit of the decision ask-nested of tree, word for
// word: a decision whose repair would have to change the decision holding it, so that no delete
// of its own repairs it. It names no delete and no decision to edit, and quotes its three blocks
// - its question, its options and the second list breaking its rule - by their ids, as data.
func heldBackRefusal(t *testing.T, tree *pmdoc.Node) string {
	t.Helper()
	held := blockByID(tree, "ask-nested")
	ids := make([]string, 0, len(held.Children))
	for _, child := range held.Children {
		id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
		ids = append(ids, strconv.Quote(id))
	}
	if len(ids) != 3 {
		t.Fatalf("the held decision holds %d blocks, want its question, its options and a second list", len(ids))
	}
	return `block: ask block "ask-nested", asking "Which day?", holds a second bullet list where its content rule, ` +
		`paragraph+ bullet_list?, allows one or more paragraphs and then at most one bullet list, last; ` +
		`the ask-edit route does not rebuild a decision that breaks its content rule, and no document edit ` +
		`deleting blocks of this decision leaves every decision it changes readable: ` +
		heldBackByItsHolder + `; its blocks are: ` +
		`block ` + ids[0] + ` (paragraph) reads "Which day?"; ` +
		`block ` + ids[1] + ` (bullet_list) reads "- Monday"; ` +
		`block ` + ids[2] + ` (bullet_list) reads "- Tuesday"`
}

// assertHeldPairRepaired checks both decisions of the held pair read as decisions, settlement
// left neither of them `invalid`, and the document kept every word but those of the blocks the
// repair deletes, which deleted names by their ids and whose words are read from before.
func assertHeldPairRepaired(t *testing.T, service *Service, artifactID string, before *pmdoc.Node, deleted []string) {
	t.Helper()
	after := liveTree(t, service, artifactID)
	for _, id := range []string{"ask-lists", "ask-nested"} {
		decision := blockByID(after, id)
		if decision == nil {
			t.Fatalf("ask block %q is gone from the repaired document", id)
		}
		if reason := readAskBlockStrictly(decision); reason != nil {
			t.Fatalf("after the repair ask block %q does not read as a decision: %v", id, reason)
		}
		if invalid, exists := decision.Attrs["invalid"]; exists {
			t.Fatalf("after the repair settlement left ask block %q invalid: %v", id, invalid)
		}
	}
	dropped := map[string]bool{}
	for _, id := range deleted {
		block := blockByID(before, id)
		if block == nil {
			t.Fatalf("the repair deletes block %q, which the document did not hold", id)
		}
		for _, word := range textWords(block) {
			dropped[word] = true
		}
	}
	words := textWords(after)
	for _, word := range textWords(before) {
		if dropped[word] || slices.Contains(words, word) {
			continue
		}
		t.Fatalf("the repair lost %q, which no delete it named dropped: the document reads %q", word, words)
	}
}

// assertHeldBackByItsHolder checks a refusal saying the repair of blockID would have to change the
// decision holding it tells the truth, recomputed here from the content rule alone: the blocks
// that rule leaves no room for, deleted from a copy of tree, leave a decision holding blockID
// unreadable. It reads none of the plan the refusal came from, so a repair that sent every nested
// decision this way would be caught.
func assertHeldBackByItsHolder(t *testing.T, tree *pmdoc.Node, blockID string, err error) {
	t.Helper()
	drops := blocksTheAskRuleLeavesNoRoomFor(blockByID(tree, blockID))
	if len(drops) == 0 {
		t.Fatalf("the refusal of %q says its repair would change the decision holding it, and the content rule leaves none of its blocks to delete: %v",
			blockID, err)
	}
	pruned := tree
	for _, id := range drops {
		next, deleteErr := pmdoc.DeleteBlock(pruned, id)
		if deleteErr != nil {
			t.Fatalf("deleting block %q to weigh the refusal of %q: %v", id, blockID, deleteErr)
		}
		pruned = next
	}
	holders := decisionsHolding(tree, blockID)
	if len(holders) == 0 {
		t.Fatalf("the refusal of %q says a decision holds it, and none does: %v", blockID, err)
	}
	for _, id := range holders {
		if holder := blockByID(pruned, id); holder != nil && readAskBlockStrictly(holder) != nil {
			return
		}
	}
	t.Fatalf("the refusal of %q says deleting its blocks would leave the decision holding it unreadable, and with %q deleted every one of %q reads as a decision: %v",
		blockID, drops, holders, err)
}

// blocksTheAskRuleLeavesNoRoomFor is the block id of every block one repair of ask, a decision
// breaking the content rule, would delete, read off the rule itself. A decision loses blocks of
// its own only where its own blocks' types break the rule (askBreaksItsOwnRule), since the rule
// reads the decisions nested in it too and repairing one of those is what repairs this one: there
// it loses every child outside the run of paragraphs starting at its first paragraph holding text
// and the one bullet list right after them. Whether or not it loses any, every decision nested in
// a block it keeps is read the same way.
func blocksTheAskRuleLeavesNoRoomFor(ask *pmdoc.Node) []string {
	if ask == nil || pmdoc.AskContentError(ask) == nil {
		return nil
	}
	keptFrom, keptTo := 0, len(ask.Children)
	if askBreaksItsOwnRule(ask) {
		first := slices.IndexFunc(ask.Children, func(child *pmdoc.Node) bool {
			return child != nil && child.Type == "paragraph" && strings.TrimSpace(pmdoc.TextContent(child)) != ""
		})
		keptFrom, keptTo = len(ask.Children), len(ask.Children)
		if first >= 0 {
			keptFrom, keptTo = first, first
			for keptTo < len(ask.Children) && ask.Children[keptTo].Type == "paragraph" {
				keptTo++
			}
			if keptTo < len(ask.Children) && ask.Children[keptTo].Type == "bullet_list" {
				keptTo++
			}
		}
	}
	drops := []string{}
	for index, child := range ask.Children {
		if index < keptFrom || index >= keptTo {
			id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
			drops = append(drops, id)
			continue
		}
		for _, nested := range decisionsNestedIn(child) {
			drops = append(drops, blocksTheAskRuleLeavesNoRoomFor(nested)...)
		}
	}
	return drops
}

// decisionsNestedIn is each decision nested in node that no nearer decision holds.
func decisionsNestedIn(node *pmdoc.Node) []*pmdoc.Node {
	if node == nil {
		return nil
	}
	if node.Type == "ask" {
		return []*pmdoc.Node{node}
	}
	var found []*pmdoc.Node
	for _, child := range node.Children {
		found = append(found, decisionsNestedIn(child)...)
	}
	return found
}

// decisionsHolding is the block id of each decision of tree holding the block blockID, outermost
// first.
func decisionsHolding(tree *pmdoc.Node, blockID string) []string {
	var holders []string
	var find func(node *pmdoc.Node, holding []string) bool
	find = func(node *pmdoc.Node, holding []string) bool {
		if node == nil {
			return false
		}
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id == blockID {
			holders = slices.Clone(holding)
			return true
		}
		if node.Type == "ask" {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			holding = append(slices.Clone(holding), id)
		}
		for _, child := range node.Children {
			if find(child, holding) {
				return true
			}
		}
		return false
	}
	find(tree, nil)
	return holders
}

// namedAskBlock matches a decision a refusal names, its block id Go-quoted.
var namedAskBlock = regexp.MustCompile(`ask block ("(?:[^"\\]|\\.)*")`)

// namedAsks is the block id of each decision err, a refusal, names - never one it only quotes a
// block of.
func namedAsks(t *testing.T, err error) []string {
	t.Helper()
	named := []string{}
	for _, match := range namedAskBlock.FindAllStringSubmatch(err.Error(), -1) {
		id, unquoteErr := strconv.Unquote(match[1])
		if unquoteErr != nil {
			t.Fatalf("the refusal names ask block %s, which does not unquote: %v", match[1], unquoteErr)
		}
		named = append(named, id)
	}
	return named
}

// followAskRefusal sends an urgency edit to the ask block blockID of artifactID, which settlement
// cannot read, and follows the refusal exactly as a caller reading it would: where it names the
// question, this same ask edit again carrying one, which the route writes into the block whatever
// its content rule says; where it names blocks, ONE document edit holding one delete per block it
// names, then the same ask edit. The decision ends valid - it reads strictly, settlement leaves
// it no `invalid` attribute and the edit applied - with every word the refusal did not name for
// deletion still in the document and every word it did quoted in it, and, where the follow wrote
// a question, that question is the one the decision asks and the one its ask row holds. It
// returns "" once it has done that, and otherwise how the refusal says why it names no delete.
// Whatever it says, every decision it names must break the content rule, so a caller is never
// sent to one that reads as a decision.
func followAskRefusal(t *testing.T, service *Service, artifactID, blockID string) string {
	t.Helper()
	edit := AskBlockEdit{Urgency: new("high")}
	before := liveTree(t, service, artifactID)
	ask := blockByID(before, blockID)
	err := sendAskBlockEdit(service, artifactID, blockID, edit)
	unrepresentable := askEditRefusal(t, before, blockID, err)
	if _, parseErr := parseAskBlockLeniently(ask); parseErr != nil {
		// A decision settlement cannot read for a reason no ask edit writes away - an option the
		// parse cannot read, a block its body may not hold, attributes it does not carry - is
		// refused before its content rule is weighed (askBlockOf), with settlement's own reason
		// and no step of its own.
		if unrepresentable.Reason != parseErr.Error() {
			t.Fatalf("the refusal of the unreadable decision %q = %v, want settlement's own reason %v", blockID, err, parseErr)
		}
		assertAskEditWroteNothing(t, service, artifactID, before, err)
		return "the decision does not read as one at all"
	}
	breaksTheRule := true
	if unrepresentable.Reason == emptyQuestionStep(blockID) {
		before, ask, err, unrepresentable, breaksTheRule = followEmptyQuestionStep(
			t, service, artifactID, blockID, &edit, err, unrepresentable)
	}
	kept := textWords(before)
	if breaksTheRule {
		if !blocksToDelete.MatchString(unrepresentable.Reason) {
			why := noDeleteReason(unrepresentable.Reason)
			if why == "" {
				t.Fatalf("the refusal of the edit of %q names no delete and does not say why: %v", blockID, err)
			}
			if why == heldBackByItsHolder {
				assertHeldBackByItsHolder(t, before, blockID, err)
			}
			assertAskEditWroteNothing(t, service, artifactID, before, err)
			return why
		}
		deletes, quoted := refusalDeletes(t, err)
		if len(deletes) == 0 || len(quoted) != len(ask.Children) {
			t.Fatalf("the refusal names %q to delete and quotes %d blocks, want a delete and the decision's %d: %v",
				deletes, len(quoted), len(ask.Children), err)
		}
		ops := make([]model.EditOp, 0, len(deletes))
		dropped := map[string]bool{}
		for _, id := range deletes {
			block, holder := blockByID(ask, id), askChildHolding(ask, id)
			if block == nil || holder == nil {
				t.Fatalf("the refusal names %q to delete, which is no block of the decision: %v", id, err)
			}
			holderID, _ := holder.Attrs[pmdoc.BlockIDAttr].(string)
			for _, word := range textWords(block) {
				if !strings.Contains(quoted[holderID], word) {
					t.Fatalf("the refusal does not quote %q of block %q it has the caller delete: %v", word, id, err)
				}
				dropped[word] = true
			}
			ops = append(ops, model.EditOp{Op: "delete", Block: id})
		}
		remaining := kept[:0]
		for _, word := range kept {
			if !dropped[word] {
				remaining = append(remaining, word)
			}
		}
		kept = remaining
		if _, applyErr := joinedApplyOps(service, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); applyErr != nil {
			t.Fatalf("one document edit deleting %q, as the refusal names: %v\nrefusal: %v", deletes, applyErr, err)
		}
		if editErr := sendAskBlockEdit(service, artifactID, blockID, edit); editErr != nil {
			t.Fatalf("ask edit after deleting %q, as the refusal names: %v\nrefusal: %v", deletes, editErr, err)
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	after := liveTree(t, service, artifactID)
	repaired := blockByID(after, blockID)
	if got, exists := repaired.Attrs["invalid"]; exists {
		t.Fatalf("after following the refusal of %q the decision is invalid: %v\nrefusal: %v", blockID, got, err)
	}
	if reason := readAskBlockStrictly(repaired); reason != nil || repaired.Attrs["urgency"] != "high" {
		t.Fatalf("after following the refusal of %q the decision reads %v with urgency %v, want it valid and the ask edit applied\nrefusal: %v",
			blockID, reason, repaired.Attrs["urgency"], err)
	}
	if edit.Question != nil {
		block, parseErr := parseAskBlock(repaired)
		if parseErr != nil || block.question != *edit.Question {
			t.Fatalf("after following the refusal of %q the decision asks %q (%v), want the question the follow wrote, %q",
				blockID, block.question, parseErr, *edit.Question)
		}
		if _, question, _ := blockAskRow(t, service, artifactID, blockID); question != block.question {
			t.Fatalf("the ask row of %q holds %q, want the question the decision then holds, %q", blockID, question, block.question)
		}
	}
	words := textWords(after)
	for _, word := range kept {
		if !slices.Contains(words, word) {
			t.Fatalf("the document lost %q, which the refusal did not name for deletion: it reads %q\nrefusal: %v", word, words, err)
		}
	}
	return ""
}

// followEmptyQuestionStep follows the one step the refusal of a decision asking nothing names
// (emptyQuestionStep): this same ask edit again, carrying a question, which the route writes into
// the block whatever its content rule says and which no delete could write into it. It writes
// that question into edit, which its caller reads afterwards for the question the follow wrote,
// and sends the same edit once more: the decision may break its content rule still, and the
// refusal of that second edit names the blocks to delete, as it does for one that always asked
// something.
//
// It returns the tree and the decision read again after the question was written, the refusal
// the caller carries on with - the second edit's where that edit was refused, and otherwise the
// one this step answered, which the caller reports beside a later failure - and whether the
// decision breaks its content rule still, which is what the deletes repair. Both corpora of
// broken bodies follow this step, the bodies enumerated over one decision
// (TestSetAskBlockTextRefusalOfEveryBrokenBodyLeadsToAValidDecision) and the decisions nested in
// another (followAskRefusal), so neither can drift from the step the refusal names.
func followEmptyQuestionStep(
	t *testing.T,
	service *Service,
	artifactID, blockID string,
	edit *AskBlockEdit,
	refused error,
	given *ErrAskBlockUnrepresentable,
) (before, ask *pmdoc.Node, err error, refusal *ErrAskBlockUnrepresentable, breaksTheRule bool) {
	t.Helper()
	edit.Question = new(askRepairQuestion)
	if editErr := sendAskBlockEdit(service, artifactID, blockID, *edit); editErr != nil {
		t.Fatalf("the ask edit of %q sent again with a question, as the refusal names: %v\nrefusal: %v",
			blockID, editErr, refused)
	}
	settleCurrentGeneration(t, service, artifactID)
	before = liveTree(t, service, artifactID)
	ask = blockByID(before, blockID)
	next := sendAskBlockEdit(service, artifactID, blockID, *edit)
	if next == nil {
		return before, ask, refused, given, false
	}
	return before, ask, next, askEditRefusal(t, before, blockID, next), true
}

// askRepairQuestion is the question a follow writes into a decision asking nothing, the one step
// its refusal names (emptyQuestionStep). Its words are nowhere else in the documents these tests
// build, so the decision and its ask row are read for them afterwards.
const askRepairQuestion = "Which way now?"

// emptyQuestionStep is why an ask edit of the decision blockID is refused while no paragraph of
// it holds text: the one step that repairs it, this same ask edit again carrying a question,
// which the route writes into the block whatever its content rule says.
func emptyQuestionStep(blockID string) string {
	return fmt.Sprintf("ask block %q has an empty question; send this ask edit again with a question, "+
		"which writes it into the block", blockID)
}

// askEditRefusal is err, the refusal of an ask edit of the decision blockID, as the caller reads
// it, held to what no refusal may do: send the caller to edit something else before this decision
// (sendsTheCallerOn), or name a decision of before that already reads as one, which would send a
// caller nowhere. A decision it names that is not the one being edited must break the content
// rule, the one thing a delete of this decision's repairs.
func askEditRefusal(t *testing.T, before *pmdoc.Node, blockID string, err error) *ErrAskBlockUnrepresentable {
	t.Helper()
	var unrepresentable *ErrAskBlockUnrepresentable
	if !errors.As(err, &unrepresentable) {
		t.Fatalf("ask edit of the broken decision %q = %v, want ErrAskBlockUnrepresentable", blockID, err)
	}
	if sendsTheCallerOn(unrepresentable.Reason) {
		t.Fatalf("the refusal of the edit of %q sends the caller to edit another decision first: %v", blockID, err)
	}
	for _, named := range namedAsks(t, err) {
		found := blockByID(before, named)
		if found == nil || readAskBlockStrictly(found) == nil {
			t.Fatalf("the refusal names ask block %q, which reads as a decision: %v", named, err)
		}
		if named != blockID && pmdoc.AskContentError(found) == nil {
			t.Fatalf("the refusal names ask block %q, which breaks no content rule: %v", named, err)
		}
	}
	return unrepresentable
}

// repairedInTheEditor is the way out a refusal naming no delete offers: the decision is repaired
// where a person writes it, never by an ask edit of another decision, which the route can refuse
// outright - a decision that does not parse, a decided one, or another person's.
const repairedInTheEditor = "this decision is repaired in the document editor"

// heldBackByItsHolder is that way out for a decision whose repair would have to change the one
// holding it: the reason says the deletes would leave that decision unreadable, and sends the
// caller to the editor alone.
const heldBackByItsHolder = "a delete inside this one changes the decision holding it, which such an edit " +
	"may not leave unreadable, and these deletes would; " + repairedInTheEditor

// noDeleteReason is how a refusal that names no delete says why, or "" when it says none of the
// three things it can say.
func noDeleteReason(reason string) string {
	for _, why := range []string{
		heldBackByItsHolder,
		repairedInTheEditor,
		"the document has not settled its block ids yet",
	} {
		if strings.Contains(reason, why) {
			return why
		}
	}
	return ""
}

// sendsTheCallerOn reports whether a refusal tells the caller to edit something else before this
// decision. No refusal may: a decision it would send them to can be one no ask edit reaches - one
// that does not parse, one already decided, or another person's - so following the refusals would
// circle. The one "first" a refusal may hold names the first list it keeps.
func sendsTheCallerOn(reason string) bool {
	if strings.Contains(reason, "edit ask block") {
		return true
	}
	for _, match := range regexp.MustCompile(`\bfirst\b[a-z ]*`).FindAllString(reason, -1) {
		if !strings.HasPrefix(match, "first list") {
			return true
		}
	}
	return false
}

// assertAskEditWroteNothing checks a refused ask edit left the document as before.
func assertAskEditWroteNothing(t *testing.T, service *Service, artifactID string, before *pmdoc.Node, err error) {
	t.Helper()
	if !liveTree(t, service, artifactID).EqualWithBlockIDs(before) {
		t.Fatalf("the refused ask edit wrote to the document: %v", err)
	}
}

// editAskUrgency sends the urgency edit to the ask block blockID as alice, as sendAskOptions sends
// an edit to the document's first ask.
func editAskUrgency(service *Service, artifactID, blockID string) error {
	return sendAskBlockEdit(service, artifactID, blockID, AskBlockEdit{Urgency: new("high")})
}

// sendAskBlockEdit sends edit to the ask block blockID as alice.
func sendAskBlockEdit(service *Service, artifactID, blockID string, edit AskBlockEdit) error {
	return joinedWrite(service, func(ctx context.Context) error {
		_, err := service.SetAskBlockText(ctx, artifactID, blockID, edit, model.Actor{Kind: "user", ID: "alice"})
		return err
	})
}

// unreadableAsks is why each ask block of tree does not read as settlement reads it, by block id.
func unreadableAsks(tree *pmdoc.Node) map[string]error {
	found := map[string]error{}
	askReadability(tree, func(id string, reason error) bool {
		if reason != nil {
			found[id] = reason
		}
		return true
	})
	return found
}

// blockByID is the block carrying blockID in tree, or nil.
func blockByID(tree *pmdoc.Node, blockID string) *pmdoc.Node {
	var found *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id == blockID {
			found = node
		}
		return found == nil
	})
	return found
}

// askChildHolding is the block of ask's own body holding blockID - the block itself, or the one it
// is nested in - or nil when no block of its body does.
func askChildHolding(ask *pmdoc.Node, blockID string) *pmdoc.Node {
	for _, child := range ask.Children {
		if blockByID(child, blockID) != nil {
			return child
		}
	}
	return nil
}

// askNestedDecisionBlock is the D block of an ask's body at position: a bullet list whose option
// holds a decision of its own, its body the blocks of body, its words found nowhere else in the
// document.
func askNestedDecisionBlock(t *testing.T, position int, body []string) *pmdoc.Node {
	t.Helper()
	list := parsedBlock(t, fmt.Sprintf("- Choice%d: Holds%d\n", position, position))
	decision := parsedBlock(t, fmt.Sprintf(
		":::ask{#ask-held-%d urgency=\"med\" multiple=\"false\" state=\"open\"}\nPlaceholder%d?\n:::\n", position, position))
	children := make([]*pmdoc.Node, len(body))
	for index, kind := range body {
		children[index] = askHeldBodyBlock(t, kind, position, index+1)
	}
	decision.Children = children
	list.Children[0].Children = append(list.Children[0].Children, decision)
	list.Attrs[pmdoc.BlockIDAttr] = fmt.Sprintf("body-%d", position)
	return list
}

// askHeldBodyBlock is the block of kind - the four kinds askBodyBlock builds - at index in the body
// of the decision an option at position holds, its words found nowhere else in the document.
func askHeldBodyBlock(t *testing.T, kind string, position, index int) *pmdoc.Node {
	t.Helper()
	var block *pmdoc.Node
	switch kind {
	case "P":
		block = parsedBlock(t, fmt.Sprintf("Inner%dAsks%d?\n", position, index))
	case "E":
		block = &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}}
	case "L":
		block = parsedBlock(t, fmt.Sprintf("- Pick%dOne%d: Worth%dBy%d\n- Pick%dTwo%d\n", position, index, position, index, position, index))
	case "N":
		block = parsedBlock(t, fmt.Sprintf("- Take%dOne%d: Carries%dBy%d\n  - Under%dOne%d\n", position, index, position, index, position, index))
	default:
		t.Fatalf("no held ask body block of kind %q", kind)
	}
	block.Attrs[pmdoc.BlockIDAttr] = fmt.Sprintf("held-%d-%d", position, index)
	return block
}

// Before settlement stamps block ids, a block of the ask may have no id, or an id may repeat in the
// tree, which a document edit restamps first (keeping the id on the first block in document
// order). The refusal then names no block to delete and asks for a retry once the document
// settles, still quoting every block. Once it settles, the retried edit names the delete, which
// applies.
func TestSetAskBlockTextRefusalBeforeBlockIDsSettleNamesNoDelete(t *testing.T) {
	for name, edit := range map[string]func(t *testing.T) func(*pmdoc.Node) *pmdoc.Node{
		"list with no id": func(t *testing.T) func(*pmdoc.Node) *pmdoc.Node {
			list := parsedBlock(t, "- Defer: Wait a week\n")
			pmdoc.Walk(list, func(node *pmdoc.Node) bool { delete(node.Attrs, pmdoc.BlockIDAttr); return true })
			return appendToAsk(list)
		},
		"repeated id": func(t *testing.T) func(*pmdoc.Node) *pmdoc.Node {
			return func(tree *pmdoc.Node) *pmdoc.Node {
				outside := parsedBlock(t, "Unrelated words outside the decision.\n")
				outside.Attrs[pmdoc.BlockIDAttr] = "dup"
				list := parsedBlock(t, "- Defer: Wait a week\n")
				list.Attrs[pmdoc.BlockIDAttr] = "dup"
				tree.Children = append([]*pmdoc.Node{outside}, tree.Children...)
				return appendToAsk(list)(tree)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, artifactID := seedIndexedShipAsk(t, "ask-lists")
			editLiveTree(t, service, artifactID, edit(t))
			before := liveTree(t, service, artifactID)
			err := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")})
			assertBrokenAskEditRefused(t, service, artifactID, before, err)
			reason := err.Error()
			if !strings.Contains(reason, "the document has not settled its block ids yet") ||
				!strings.Contains(reason, "retry the edit once the document settles") {
				t.Fatalf("err = %v, want it to say the block ids have not settled and to retry", err)
			}
			if strings.Contains(reason, "the blocks to delete are") || strings.Contains(reason, "delete the blocks") {
				t.Fatalf("err = %v, want it to name no block to delete", err)
			}
			if !strings.Contains(reason, `reads "- Defer: Wait a week"`) || !strings.Contains(reason, `reads "Should we ship?"`) {
				t.Fatalf("err = %v, want every block quoted", err)
			}
			settleCurrentGeneration(t, service, artifactID)
			followRefusal(t, service, artifactID, AskBlockEdit{Urgency: new("high")}, []int{2}, "Should we ship?", []model.AskOption{
				{Label: "Ship", Description: "Release it"},
				{Label: "Hold", Description: "Wait for review"},
				{Label: "Defer", Description: "Wait a week"},
			})
		})
	}
}

// A block id is the document's data, which a collaborator can make hold a shell's quotes and
// command substitution. The refusal gives it only as Go-quoted data and names no command; %q does
// not escape $, backtick or ' for a shell, so the reason must hold no command text to paste.
func TestSetAskBlockTextRefusalGivesABlockIDOnlyAsQuotedData(t *testing.T) {
	const id = `x'; $(touch pwned); echo "`
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		list := parsedBlock(t, "- Defer: Wait a week\n  - only if finance agrees\n")
		list.Attrs[pmdoc.BlockIDAttr] = id
		return appendToAsk(list)(tree)
	})
	settleCurrentGeneration(t, service, artifactID)
	ask := firstAsk(liveTree(t, service, artifactID))
	if got := ask.Children[len(ask.Children)-1].Attrs[pmdoc.BlockIDAttr]; got != id {
		t.Fatalf("the list's block id after settlement = %#v, want %q kept", got, id)
	}
	before := liveTree(t, service, artifactID)

	err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &[]model.AskOption{{Label: "Ship", Description: "Release it"}}})
	assertBrokenAskEditRefused(t, service, artifactID, before, err)
	reason := err.Error()
	if !strings.Contains(reason, "block "+strconv.Quote(id)+" (bullet_list) reads ") {
		t.Fatalf("err = %v, want the list's id given as %s", err, strconv.Quote(id))
	}
	for _, banned := range []string{id, "--ops-json", "'[", "--", "$ dispatch", "dispatch doc-edit "} {
		if strings.Contains(reason, banned) {
			t.Fatalf("err = %v, want no %q in it", err, banned)
		}
	}
}

// richHoldAsk is seedIndexedShipAsk's ask with what a person wrote under its Hold option: a
// nested sub-list, a second paragraph, bold text and a link. No ask row holds any of it, so an
// options edit that rebuilt the list would delete words its caller never saw.
const richHoldAsk = ":::ask{#ask-lists urgency=\"med\" multiple=\"false\" state=\"open\"}\n" +
	"Should we ship?\n\n" +
	"- Ship: Release it\n" +
	"- Hold: Wait for review\n\n" +
	"  - only if **finance** agrees\n\n" +
	"  See [the memo](https://memo.test) first.\n" +
	":::\n"

// seedIndexedRichAsk is seedIndexedShipAsk over richHoldAsk: the same question, options and row,
// with the Hold option's item holding what the row cannot show.
func seedIndexedRichAsk(t *testing.T) (*Service, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, richHoldAsk)
	settleCurrentGeneration(t, service, artifactID)
	assertShipAskRow(t, service, artifactID, "ask-lists")
	return service, artifactID
}

// askOptionList is the document's first ask's bullet list, the one an options edit writes.
func askOptionList(t *testing.T, service *Service, artifactID string) *pmdoc.Node {
	t.Helper()
	ask := firstAsk(liveTree(t, service, artifactID))
	for _, child := range ask.Children {
		if child.Type == "bullet_list" {
			return child
		}
	}
	t.Fatalf("the ask holds no bullet list")
	return nil
}

// askOptionItemMarkdown is item read as a document holding one bullet list holding it alone,
// which is how a refusal quotes an option's item.
func askOptionItemMarkdown(t *testing.T, list, item *pmdoc.Node) string {
	t.Helper()
	markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
		{Type: "bullet_list", Attrs: list.Attrs, Children: []*pmdoc.Node{item}},
	}})
	if err != nil {
		t.Fatalf("render an option's item: %v", err)
	}
	return strings.TrimSpace(markdown)
}

// An options edit writes the lines it sends and nothing else: an option it sends unchanged keeps
// the very node the document holds, so the sub-list, the second paragraph, the bold run and the
// link's address under it survive with their own block ids and marks.
func TestAskOptionsEditKeepsWhatItDoesNotWrite(t *testing.T) {
	service, artifactID := seedIndexedRichAsk(t)
	list := askOptionList(t, service, artifactID)
	before := list.Children[1]

	options := []model.AskOption{
		{Label: "Ship", Description: "Release it today"},
		{Label: "Hold", Description: "Wait for review"},
	}
	if err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options}); err != nil {
		t.Fatalf("options edit: %v", err)
	}

	after := askOptionList(t, service, artifactID)
	if len(after.Children) != 2 {
		t.Fatalf("the list holds %d items after the edit, want 2", len(after.Children))
	}
	if !after.Children[1].EqualWithBlockIDs(before) {
		t.Fatalf("the unchanged option was rewritten:\nbefore %q\nafter  %q",
			askOptionItemMarkdown(t, list, before), askOptionItemMarkdown(t, after, after.Children[1]))
	}
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", options)
}

// An option whose description the edit changes is rewritten where it stands: its first paragraph
// carries the new line, and every other node the item holds is the one that was there.
func TestAskOptionsEditRewritesAnOptionAndKeepsTheRest(t *testing.T) {
	service, artifactID := seedIndexedRichAsk(t)
	list := askOptionList(t, service, artifactID)
	before := list.Children[1]

	options := []model.AskOption{
		{Label: "Ship", Description: "Release it"},
		{Label: "Hold", Description: "Wait for the sign-off"},
	}
	if err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options}); err != nil {
		t.Fatalf("options edit: %v", err)
	}

	after := askOptionList(t, service, artifactID).Children[1]
	if after.Attrs[pmdoc.BlockIDAttr] != before.Attrs[pmdoc.BlockIDAttr] {
		t.Fatalf("the rewritten item's block id = %v, want the original's %v",
			after.Attrs[pmdoc.BlockIDAttr], before.Attrs[pmdoc.BlockIDAttr])
	}
	if got := strings.TrimSpace(pmdoc.TextContent(after.Children[0])); got != "Hold: Wait for the sign-off" {
		t.Fatalf("the rewritten item's first paragraph reads %q, want the line the edit sent", got)
	}
	if len(after.Children) != len(before.Children) {
		t.Fatalf("the rewritten item holds %d nodes, want the original's %d", len(after.Children), len(before.Children))
	}
	for index := 1; index < len(before.Children); index++ {
		if !after.Children[index].EqualWithBlockIDs(before.Children[index]) {
			t.Fatalf("the rewritten item's node %d was rebuilt, want the one the document held", index)
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", options)
}

// An options edit pairs each option it sends with the item already holding its label, not with
// the item standing at its position: an edit that reorders the options carries what a person
// wrote under an option along with that option's own label. Pairing by position would leave the
// sub-list, the second paragraph and the link under another option's words, which is a wrong
// record rather than a deletion anyone can see.
func TestAskOptionsEditPairsAnOptionByItsLabelNotItsPosition(t *testing.T) {
	service, artifactID := seedIndexedRichAsk(t)
	list := askOptionList(t, service, artifactID)
	hold := list.Children[1]

	options := []model.AskOption{
		{Label: "Hold", Description: "Wait for review"},
		{Label: "Ship", Description: "Release it today"},
	}
	if err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options}); err != nil {
		t.Fatalf("options edit: %v", err)
	}

	after := askOptionList(t, service, artifactID)
	if len(after.Children) != 2 {
		t.Fatalf("the list holds %d items after the edit, want 2", len(after.Children))
	}
	if !after.Children[0].EqualWithBlockIDs(hold) {
		t.Fatalf("the Hold option reads %q after the edit, want the item the document held, %q",
			askOptionItemMarkdown(t, after, after.Children[0]), askOptionItemMarkdown(t, list, hold))
	}
	ship := after.Children[1]
	if got := strings.TrimSpace(pmdoc.TextContent(ship.Children[0])); got != "Ship: Release it today" {
		t.Fatalf("the second item reads %q, want the Ship line the edit sent", got)
	}
	if len(ship.Children) != 1 {
		t.Fatalf("the Ship item holds %d nodes, want its line alone: nothing of Hold's belongs under it",
			len(ship.Children))
	}
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", options)
}

// An edit that leaves an option out, renames it, or sends no option at all drops that option's
// item. An item holding nothing but its plain line is the edit's to drop; one holding what a
// person wrote is not, and the whole edit is refused, quoting the item so the caller can restore
// every word of it.
func TestAskOptionsEditRefusesDroppingAnItemHoldingContent(t *testing.T) {
	ship := model.AskOption{Label: "Ship", Description: "Release it"}
	hold := model.AskOption{Label: "Hold", Description: "Wait for review"}
	for _, test := range []struct {
		name    string
		options []model.AskOption
		refused bool
	}{
		{name: "a removal", options: []model.AskOption{ship}, refused: true},
		{name: "a rename", options: []model.AskOption{ship, {Label: "Wait", Description: "Wait for review"}}, refused: true},
		{name: "an empty options list", options: []model.AskOption{}, refused: true},
		{name: "an item holding nothing but its plain line", options: []model.AskOption{hold}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := seedIndexedRichAsk(t)
			before := liveTree(t, service, artifactID)
			list := askOptionList(t, service, artifactID)
			options := test.options
			err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options})
			if !test.refused {
				if err != nil {
					t.Fatalf("options edit dropping a plain item: %v", err)
				}
				after := askOptionList(t, service, artifactID)
				if len(after.Children) != 1 || !after.Children[0].EqualWithBlockIDs(list.Children[1]) {
					t.Fatalf("the list holds %d items, want the Hold item alone as it was", len(after.Children))
				}
				return
			}
			var unrepresentable *ErrAskBlockUnrepresentable
			if !errors.As(err, &unrepresentable) || unrepresentable.Field != "options" {
				t.Fatalf("err = %v, want ErrAskBlockUnrepresentable on the options", err)
			}
			want := `options: the edit drops the option "Hold", whose list item holds more than its plain line, ` +
				`and the ask-edit route does not delete what a person wrote under an option; its item reads ` +
				strconv.Quote(askOptionItemMarkdown(t, list, list.Children[1])) +
				`; send that option back with this edit, or remove what the item holds with a document edit ` +
				`first (dispatch doc-edit, deleting a block by its id)`
			if err.Error() != want {
				t.Fatalf("refusal =\n%v\nwant\n%s", err, want)
			}
			for _, word := range []string{"finance", "the memo", "https://memo.test", "dispatch doc-edit"} {
				if !strings.Contains(err.Error(), word) {
					t.Fatalf("err = %v, want it to quote %q", err, word)
				}
			}
			if after := liveTree(t, service, artifactID); !after.EqualWithBlockIDs(before) {
				t.Fatalf("the refused edit changed the document")
			}
			assertShipAskRow(t, service, artifactID, "ask-lists")
		})
	}
}

// typedMark is the mark the browser's editor puts on every character a person types. Canonical
// markdown writes none of it (pmdoc renders link, strong, emphasis, strike_through and
// inlineCode alone), so a line carrying it reads as the plain line it is.
func typedMark(id string) pmdoc.Mark {
	return pmdoc.Mark{Type: "proofAuthored", Attrs: pmdoc.Attrs{
		"id": id, "by": "user:alice", "at": int64(1_700_000_000_000),
	}}
}

// markAsTyped puts the editor's own mark on every text node under node, as the browser leaves
// what a person typed there.
func markAsTyped(node *pmdoc.Node, id string) {
	if node.Type == "text" {
		node.Marks = []pmdoc.Mark{typedMark(id)}
	}
	for _, child := range node.Children {
		markAsTyped(child, id)
	}
}

// An option a person typed in the browser carries the editor's own mark on every character of
// it, and no markdown writes that mark: the item holds nothing a rebuilt item would not write
// again, so the edit that drops it is applied rather than refused for a mark no reader can see.
func TestAskOptionsEditDropsAnItemTheEditorMarked(t *testing.T) {
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		markAsTyped(firstAsk(tree).Children[1], "typed")
		return tree
	})
	list := askOptionList(t, service, artifactID)
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofAuthored", "typed"); !found {
		t.Fatalf("the fixture's options carry no mark of the editor's")
	}
	ship := list.Children[0]

	options := []model.AskOption{{Label: "Ship", Description: "Release it"}}
	if err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options}); err != nil {
		t.Fatalf("options edit dropping an option the editor marked: %v", err)
	}

	after := askOptionList(t, service, artifactID)
	if len(after.Children) != 1 || !after.Children[0].EqualWithBlockIDs(ship) {
		t.Fatalf("the list holds %d items, want the Ship item alone as it was", len(after.Children))
	}
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", options)
}

// A person typing an option in the browser leaves the whitespace they typed in its line - a
// trailing space, or one before the colon the parse cuts the label at - under the editor's own
// mark. That whitespace is text of the item's own, which a rebuilt item writes again, and the
// ask row reads the same words either way, so an edit dropping such an option and one rewording
// it are both applied rather than refused for markup the line does not hold.
func TestAskOptionsEditOfALineHoldingTypedWhitespace(t *testing.T) {
	ship := model.AskOption{Label: "Ship", Description: "Release it"}
	for _, line := range []string{"Hold: Wait for review ", "Hold : Wait for review"} {
		t.Run("the drop of "+strconv.Quote(line), func(t *testing.T) {
			service, artifactID := seedTypedOptionLine(t, line)
			held := askOptionList(t, service, artifactID).Children[0]

			options := []model.AskOption{ship}
			if err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options}); err != nil {
				t.Fatalf("options edit dropping an option whose line holds the whitespace a person typed: %v", err)
			}

			after := askOptionList(t, service, artifactID)
			if len(after.Children) != 1 || !after.Children[0].EqualWithBlockIDs(held) {
				t.Fatalf("the list holds %d items, want the Ship item alone as it was", len(after.Children))
			}
			settleCurrentGeneration(t, service, artifactID)
			assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", options)
		})
		t.Run("the rewrite of "+strconv.Quote(line), func(t *testing.T) {
			service, artifactID := seedTypedOptionLine(t, line)

			options := []model.AskOption{ship, {Label: "Hold", Description: "Wait for the sign-off"}}
			if err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options}); err != nil {
				t.Fatalf("options edit rewording an option whose line holds the whitespace a person typed: %v", err)
			}

			after := askOptionList(t, service, artifactID)
			if len(after.Children) != 2 {
				t.Fatalf("the list holds %d items after the edit, want 2", len(after.Children))
			}
			rewritten := after.Children[1]
			if got := pmdoc.TextContent(rewritten.Children[0]); got != "Hold: Wait for the sign-off" {
				t.Fatalf("the rewritten item reads %q, want the line the edit sent", got)
			}
			settleCurrentGeneration(t, service, artifactID)
			assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", options)
		})
	}
}

// seedTypedOptionLine is seedIndexedShipAsk with the Hold option's line written as a person
// typed it in the browser: the text line holds, under the editor's mark on every character of
// it. The parse trims that whitespace away, so the ask row reads the option it was indexed with.
func seedTypedOptionLine(t *testing.T, line string) (*Service, string) {
	t.Helper()
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		hold := firstAsk(tree).Children[1].Children[1]
		hold.Children[0].Children = []*pmdoc.Node{{Type: "text", Text: line}}
		markAsTyped(hold, "typed")
		return tree
	})
	if got := pmdoc.TextContent(askOptionList(t, service, artifactID).Children[1]); got != line {
		t.Fatalf("the seeded option line reads %q, want %q", got, line)
	}
	assertShipAskRow(t, service, artifactID, "ask-lists")
	return service, artifactID
}

// A person who empties a decision's question in the browser can leave the whitespace they typed
// behind, under the editor's own mark. The markdown writes nothing of that mark, so a question
// written over the paragraph deletes no word, and the ask edit carrying one repairs the decision
// rather than being refused for a mark no reader can see.
func TestEditAskRepairsAnEmptyQuestionOfTypedWhitespace(t *testing.T) {
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		question := firstAsk(tree).Children[0]
		question.Children = []*pmdoc.Node{{Type: "text", Text: " ", Marks: []pmdoc.Mark{typedMark("typed")}}}
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)

	if err := sendAskOptions(service, artifactID, AskBlockEdit{Question: new("Should we ship it today?")}); err != nil {
		t.Fatalf("the ask edit sending a question to the decision a person emptied: %v", err)
	}

	block, err := parseAskBlock(firstAsk(liveTree(t, service, artifactID)))
	if err != nil {
		t.Fatalf("the repaired block does not parse: %v", err)
	}
	if block.question != "Should we ship it today?" {
		t.Fatalf("the repaired block asks %q, want the question the ask edit sent", block.question)
	}
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship it today?", []model.AskOption{
		{Label: "Ship", Description: "Release it"},
		{Label: "Hold", Description: "Wait for review"},
	})
}

// Rewriting an option replaces its line with the plain text the edit sends, so a link's address,
// bold or code written in that line would be deleted unseen - what the drop refusal exists to
// prevent. The whole edit is refused instead, quoting the line's markdown so every word of it
// can be restored and naming both ways out.
func TestAskOptionsEditRefusesRewritingALineHoldingMarkup(t *testing.T) {
	for _, test := range []struct {
		name, line, markdown, description string
	}{
		{
			name:        "a link's address",
			line:        "- Hold: see [the memo](https://memo.test)\n",
			markdown:    "Hold: see [the memo](https://memo.test)",
			description: "see the memo",
		},
		{
			name:        "bold",
			line:        "- Hold: wait for **finance**\n",
			markdown:    "Hold: wait for **finance**",
			description: "wait for finance",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, ":::ask{#ask-lists urgency=\"med\" multiple=\"false\" state=\"open\"}\n"+
				"Should we ship?\n\n- Ship: Release it\n"+test.line+":::\n")
			settleCurrentGeneration(t, service, artifactID)
			before := liveTree(t, service, artifactID)

			options := []model.AskOption{
				{Label: "Ship", Description: "Release it"},
				{Label: "Hold", Description: "wait for the sign-off"},
			}
			err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options})

			var unrepresentable *ErrAskBlockUnrepresentable
			if !errors.As(err, &unrepresentable) || unrepresentable.Field != "options" {
				t.Fatalf("err = %v, want ErrAskBlockUnrepresentable on the options", err)
			}
			want := `options: the edit rewrites the line of the option "Hold", which holds more than its plain ` +
				`text, and the ask-edit route does not delete the markup a person wrote in a line; its line reads ` +
				strconv.Quote(test.markdown) +
				`; send that option back unchanged with this edit, or rewrite the line with a document edit ` +
				`(dispatch doc-edit, replacing quoted text with replace)`
			if err.Error() != want {
				t.Fatalf("refusal =\n%v\nwant\n%s", err, want)
			}
			assertAskEditWroteNothing(t, service, artifactID, before, err)
			_, question, held := blockAskRow(t, service, artifactID, "ask-lists")
			if question != "Should we ship?" || !slices.Equal(held, []model.AskOption{
				{Label: "Ship", Description: "Release it"},
				{Label: "Hold", Description: test.description},
			}) {
				t.Fatalf("the ask row = question=%q options=%#v, want it as the refused edit found it", question, held)
			}
		})
	}
}

// An option that holds its markup in its own line and nothing under it is a drop the refusal
// must still catch: the item's words are the link's address and the bold run in that one line,
// which the plain line a rebuilt list writes would delete, and the row shows the sender none of
// it. Weighing the item by its text alone would read the item as plain and drop it.
func TestAskOptionsEditRefusesDroppingALineHoldingMarkup(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-lists urgency=\"med\" multiple=\"false\" state=\"open\"}\n"+
		"Should we ship?\n\n- Ship: Release it\n- Hold: wait for **finance**, see [the memo](https://memo.test)\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	before := liveTree(t, service, artifactID)
	list := askOptionList(t, service, artifactID)

	options := []model.AskOption{{Label: "Ship", Description: "Release it"}}
	err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options})

	var unrepresentable *ErrAskBlockUnrepresentable
	if !errors.As(err, &unrepresentable) || unrepresentable.Field != "options" {
		t.Fatalf("err = %v, want ErrAskBlockUnrepresentable on the options", err)
	}
	want := `options: the edit drops the option "Hold", whose list item holds more than its plain line, ` +
		`and the ask-edit route does not delete what a person wrote under an option; its item reads ` +
		strconv.Quote(askOptionItemMarkdown(t, list, list.Children[1])) +
		`; send that option back with this edit, or remove what the item holds with a document edit ` +
		`first (dispatch doc-edit, deleting a block by its id)`
	if err.Error() != want {
		t.Fatalf("refusal =\n%v\nwant\n%s", err, want)
	}
	for _, word := range []string{"**finance**", "https://memo.test"} {
		if !strings.Contains(err.Error(), word) {
			t.Fatalf("err = %v, want it to quote %q", err, word)
		}
	}
	assertAskEditWroteNothing(t, service, artifactID, before, err)
	_, question, held := blockAskRow(t, service, artifactID, "ask-lists")
	if question != "Should we ship?" || !slices.Equal(held, []model.AskOption{
		{Label: "Ship", Description: "Release it"},
		{Label: "Hold", Description: "wait for finance, see the memo"},
	}) {
		t.Fatalf("the ask row = question=%q options=%#v, want it as the refused edit found it", question, held)
	}
}

// Two items can carry one label, and what a person wrote belongs to the item they wrote it
// under. An ask's labels are unique, so one option of that label is what an edit can send, and
// it pairs with the item that says exactly what it says, wherever that item stands, keeping it
// whole. Pairing on the label alone would give it the first item of that label instead,
// rewriting that item's line and dropping the item holding the sub-list, which is no edit's to
// delete.
func TestAskOptionsEditPairsOnTheWholeOptionBeforeTheLabel(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-lists urgency=\"med\" multiple=\"false\" state=\"open\"}\n"+
		"Should we ship?\n\n- Hold: Wait for review\n\n- Hold: Wait for the memo\n\n  - only if **finance** agrees\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	list := askOptionList(t, service, artifactID)
	if len(list.Children) != 2 {
		t.Fatalf("the seeded list holds %d items, want the two of one label", len(list.Children))
	}
	held := list.Children[1]

	// The option says what the second item says, and the first, which says something else, is a
	// plain line the edit may drop. On the label alone it would take that first item, rewrite
	// its line, and drop the second - the one holding the sub-list a person wrote.
	options := []model.AskOption{{Label: "Hold", Description: "Wait for the memo"}}
	if err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options}); err != nil {
		t.Fatalf("options edit of two items of one label: %v", err)
	}

	after := askOptionList(t, service, artifactID)
	if len(after.Children) != 1 {
		t.Fatalf("the list holds %d items after the edit, want the one option the edit sent", len(after.Children))
	}
	if !after.Children[0].EqualWithBlockIDs(held) {
		t.Fatalf("the option the edit sent reads %q, want the item that already said it, %q",
			askOptionItemMarkdown(t, after, after.Children[0]), askOptionItemMarkdown(t, list, held))
	}
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-lists", "Should we ship?", options)
}

// A comment anchored inside what a person wrote under an option is anchored to the same words
// after an options edit: the nodes it points into are the document's own, not a rebuilt copy.
func TestAskOptionsEditKeepsACommentAnchoredUnderAnOption(t *testing.T) {
	service, artifactID := seedIndexedRichAsk(t)
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkComment, ID: "c1", By: model.Actor{Kind: "user", ID: "alice"},
	}, "finance", nil); err != nil {
		t.Fatalf("anchor a comment inside the sub-list: %v", err)
	}

	options := []model.AskOption{
		{Label: "Ship", Description: "Release it today"},
		{Label: "Hold", Description: "Wait for review"},
	}
	if err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options}); err != nil {
		t.Fatalf("options edit: %v", err)
	}

	_, quote, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "c1")
	if !found || quote != "finance" {
		t.Fatalf("the comment anchors to %q (found %t), want it still on \"finance\"", quote, found)
	}
}

// Settlement writes a decided ask's recorded wording back into a block a person reworded after
// the decision. That restore goes through the same options write, so the sub-list a person wrote
// under an option it does not reword stays where they wrote it.
func TestAskOptionsEditRestoringADecidedWordingKeepsASubList(t *testing.T) {
	service, artifactID := seedIndexedRichAsk(t)
	answerIndexedAsk(t, service, artifactID, "ask-lists")
	settleCurrentGeneration(t, service, artifactID)

	// A person rewords the Ship option in the document, after the ask was decided.
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		item := firstAsk(tree).Children[1].Children[0]
		item.Children[0].Children = []*pmdoc.Node{{Type: "text", Text: "Ship: Release it tomorrow"}}
		return tree
	})
	before := askOptionList(t, service, artifactID).Children[1]

	settleCurrentGeneration(t, service, artifactID)

	ask := firstAsk(liveTree(t, service, artifactID))
	block, err := parseAskBlock(ask)
	if err != nil {
		t.Fatalf("parse the restored ask: %v", err)
	}
	want := []model.AskOption{{Label: "Ship", Description: "Release it"}, {Label: "Hold", Description: "Wait for review"}}
	if !slices.Equal(block.options, want) {
		t.Fatalf("the restored block asks %#v, want the wording the ask was decided on", block.options)
	}
	if after := askOptionList(t, service, artifactID).Children[1]; !after.EqualWithBlockIDs(before) {
		t.Fatalf("the decided-wording restore flattened what a person wrote under the option it left alone")
	}
}

// Settlement writes a decided ask's recorded wording back into its block, and no caller can
// answer a refusal there: an option a person added under the decision, with words under it, is
// dropped as it was before an options edit kept an option's content, so the block and its row
// agree again rather than disagreeing for good.
func TestRestoringADecidedWordingDropsAnOptionTheDecisionNeverHeld(t *testing.T) {
	service, artifactID := seedIndexedRichAsk(t)
	answerIndexedAsk(t, service, artifactID, "ask-lists")
	settleCurrentGeneration(t, service, artifactID)

	// A person adds an option of their own, holding a sub-list, after the ask was decided.
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		list := firstAsk(tree).Children[1]
		added := parsedBlock(t, "- Defer: Wait a week\n  - only if finance agrees\n")
		list.Children = append(list.Children, added.Children[0])
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)

	block, err := parseAskBlock(firstAsk(liveTree(t, service, artifactID)))
	if err != nil {
		t.Fatalf("parse the restored ask: %v", err)
	}
	want := []model.AskOption{{Label: "Ship", Description: "Release it"}, {Label: "Hold", Description: "Wait for review"}}
	if !slices.Equal(block.options, want) {
		t.Fatalf("the restored block asks %#v, want the wording the ask was decided on", block.options)
	}
}

// sendAskOptions sends edit to the ask-lists block as alice (sendAskBlockEdit).
func sendAskOptions(service *Service, artifactID string, edit AskBlockEdit) error {
	return sendAskBlockEdit(service, artifactID, "ask-lists", edit)
}

// assertBrokenAskEditRefused checks err refuses an edit of the document's first ask, which breaks
// its content rule, naming the rule and the document edit, quoting every block of the ask by its
// id, and that nothing was written.
func assertBrokenAskEditRefused(t *testing.T, service *Service, artifactID string, before *pmdoc.Node, err error) {
	t.Helper()
	var unrepresentable *ErrAskBlockUnrepresentable
	if !errors.As(err, &unrepresentable) || unrepresentable.Field != "block" ||
		!strings.Contains(err.Error(), "paragraph+ bullet_list?") ||
		!strings.Contains(err.Error(), "dispatch doc-edit, deleting a block by its id") {
		t.Fatalf("err = %v, want ErrAskBlockUnrepresentable on the block naming the content rule and the document edit", err)
	}
	for _, child := range firstAsk(before).Children {
		id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
		name := "block " + strconv.Quote(id)
		if id == "" {
			name = "a block with no id"
		}
		if !strings.Contains(err.Error(), name+" ("+child.Type+") reads ") {
			t.Fatalf("err = %v, want it to quote the %s %q", err, child.Type, id)
		}
		for _, word := range textWords(child) {
			if !strings.Contains(err.Error(), word) {
				t.Fatalf("err = %v, want it to quote %q", err, word)
			}
		}
	}
	if strings.Contains(err.Error(), "--ops-json") || strings.Contains(err.Error(), "'[") {
		t.Fatalf("err = %v, want no shell-quoted ops", err)
	}
	if after := liveTree(t, service, artifactID); !after.EqualWithBlockIDs(before) {
		t.Fatalf("the refused edit changed the block")
	}
	assertShipAskRow(t, service, artifactID, "ask-lists")
}

// answerIndexedAsk marks the ask row of blockID answered by alice, as a person deciding it does;
// the next settlement projects that back into its block.
func answerIndexedAsk(t *testing.T, service *Service, artifactID, blockID string) {
	t.Helper()
	answerJSON, err := json.Marshal(model.AskAnswer{User: "alice", Selected: []string{"Ship"}, At: time.Now().UTC()})
	if err != nil {
		t.Fatalf("encode answer: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		update asks set state = 'answered', answer = $3 where block_artifact_id = $1 and block_id = $2
	`, artifactID, blockID, answerJSON); err != nil {
		t.Fatalf("answer indexed ask: %v", err)
	}
}

// A decided ask given a second list is marked invalid and keeps its decided row; once the second
// list is gone, settlement clears invalid and the block carries its decided wording again.
func TestSettlementMarksADecidedAskWithASecondBulletListInvalid(t *testing.T) {
	service, artifactID := seedIndexedShipAsk(t, "ask-lists")
	answerIndexedAsk(t, service, artifactID, "ask-lists")
	settleCurrentGeneration(t, service, artifactID)
	assertAnsweredShipAsk := func(stage string) {
		t.Helper()
		var state string
		var storedAnswer []byte
		if err := service.store.Pool.QueryRow(context.Background(), `
			select state, answer from asks where block_artifact_id = $1 and block_id = $2
		`, artifactID, "ask-lists").Scan(&state, &storedAnswer); err != nil {
			t.Fatalf("%s: load ask row: %v", stage, err)
		}
		var got model.AskAnswer
		if err := json.Unmarshal(storedAnswer, &got); err != nil {
			t.Fatalf("%s: decode answer: %v", stage, err)
		}
		if state != "answered" || got.User != "alice" || !slices.Equal(got.Selected, []string{"Ship"}) {
			t.Fatalf("%s: ask row = state=%q answer=%#v, want answered by alice with Ship", stage, state, got)
		}
	}
	assertAnsweredShipAsk("answered")

	editLiveTree(t, service, artifactID, appendToAsk(parsedBlock(t, "- Defer: Wait a week\n")))
	settleCurrentGeneration(t, service, artifactID)
	const reason = `ask block "ask-lists", asking "Should we ship?", holds a second bullet list where its content rule, paragraph+ bullet_list?, allows one or more paragraphs and then at most one bullet list, last`
	if got := firstAsk(liveTree(t, service, artifactID)).Attrs["invalid"]; got != reason {
		t.Fatalf("decided ask invalid = %#v, want %q", got, reason)
	}
	assertAnsweredShipAsk("second list")

	// Merge the second list into the first, so the block's wording differs from the decided one.
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		ask := firstAsk(tree)
		ask.Children[1].Children = append(ask.Children[1].Children, ask.Children[2].Children...)
		ask.Children = ask.Children[:2]
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	ask := firstAsk(liveTree(t, service, artifactID))
	if got, exists := ask.Attrs["invalid"]; exists {
		t.Fatalf("repaired decided ask invalid = %#v, want it cleared", got)
	}
	assertAnsweredShipAsk("repaired")
	block, err := parseAskBlock(ask)
	if err != nil {
		t.Fatalf("parse repaired ask: %v", err)
	}
	want := []model.AskOption{{Label: "Ship", Description: "Release it"}, {Label: "Hold", Description: "Wait for review"}}
	if block.question != "Should we ship?" || !slices.Equal(block.options, want) || ask.Attrs["state"] != "answered" {
		t.Fatalf("repaired decided ask = state=%v question=%q options=%#v, want the decided wording", ask.Attrs["state"], block.question, block.options)
	}
}

// Concurrent browser edits can leave an ask holding a paragraph after its options, which the
// browser keeps and takes no part of its question from. Settlement marks it invalid and leaves its
// row open and as it was, until an edit moves the paragraph into the question.
func TestSettlementMarksAnAskWithAParagraphAfterItsOptionsInvalid(t *testing.T) {
	const reason = `ask block "ask-paragraph", asking "Should we ship?", holds a paragraph after its options where its content rule, paragraph+ bullet_list?, allows one or more paragraphs and then at most one bullet list, last`
	service, artifactID := seedIndexedShipAsk(t, "ask-paragraph")
	editLiveTree(t, service, artifactID, appendToAsk(parsedBlock(t, "Decide by Friday.\n")))
	settleCurrentGeneration(t, service, artifactID)
	assertAskMarkedInvalidAndKept(t, service, artifactID, "ask-paragraph", reason)

	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		ask := firstAsk(tree)
		ask.Children = []*pmdoc.Node{ask.Children[0], ask.Children[2], ask.Children[1]}
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	assertAskRepaired(t, service, artifactID, "ask-paragraph", "Should we ship?\n\nDecide by Friday.", []model.AskOption{
		{Label: "Ship", Description: "Release it"},
		{Label: "Hold", Description: "Wait for review"},
	})
}

// seedIndexedShipAsk is a document holding one ask, "Should we ship?" with the options Ship and
// Hold, which settlement has indexed as an open ask row. Its settlements run only when the test
// runs them.
func seedIndexedShipAsk(t *testing.T, blockID string) (*Service, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#"+blockID+" urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	assertShipAskRow(t, service, artifactID, blockID)
	return service, artifactID
}

// parsedBlock is the first block markdown parses to, stamped with block ids as the browser editor
// would.
func parsedBlock(t *testing.T, markdown string) *pmdoc.Node {
	t.Helper()
	parsed, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse block: %v", err)
	}
	return parsed.Children[0]
}

// assertAskMarkedInvalidAndKept checks settlement flagged the document's first ask invalid for
// reason, said so in a block.invalid event, and left its row open as seedIndexedShipAsk indexed it.
func assertAskMarkedInvalidAndKept(t *testing.T, service *Service, artifactID, blockID, reason string) {
	t.Helper()
	if got := firstAsk(liveTree(t, service, artifactID)).Attrs["invalid"]; got != reason {
		t.Fatalf("ask invalid = %#v, want %q", got, reason)
	}
	var eventBlockID, eventReason string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select payload->>'block_id', payload->>'reason'
		from events where type = 'block.invalid' order by id desc limit 1
	`).Scan(&eventBlockID, &eventReason); err != nil {
		t.Fatalf("load block.invalid event: %v", err)
	}
	if eventBlockID != blockID || eventReason != reason {
		t.Fatalf("block.invalid event = block=%q reason=%q, want block=%q reason=%q", eventBlockID, eventReason, blockID, reason)
	}
	assertShipAskRow(t, service, artifactID, blockID)
}

// assertShipAskRow checks the ask row of blockID is open with seedIndexedShipAsk's question and
// options.
func assertShipAskRow(t *testing.T, service *Service, artifactID, blockID string) {
	t.Helper()
	state, question, options := blockAskRow(t, service, artifactID, blockID)
	want := []model.AskOption{{Label: "Ship", Description: "Release it"}, {Label: "Hold", Description: "Wait for review"}}
	if state != "open" || question != "Should we ship?" || !slices.Equal(options, want) {
		t.Fatalf("ask row = state=%q question=%q options=%#v, want it open as indexed", state, question, options)
	}
}

// assertAskRepaired checks settlement cleared the document's first ask's invalid attribute and
// wrote question and options to its open row.
func assertAskRepaired(t *testing.T, service *Service, artifactID, blockID, question string, options []model.AskOption) {
	t.Helper()
	if got, exists := firstAsk(liveTree(t, service, artifactID)).Attrs["invalid"]; exists {
		t.Fatalf("repaired ask invalid = %#v, want it cleared", got)
	}
	state, gotQuestion, gotOptions := blockAskRow(t, service, artifactID, blockID)
	if state != "open" || gotQuestion != question || !slices.Equal(gotOptions, options) {
		t.Fatalf("repaired ask row = state=%q question=%q options=%#v, want open with question=%q options=%#v", state, gotQuestion, gotOptions, question, options)
	}
}

// blockAskRow is the state, question and options of the ask row anchored to blockID.
func blockAskRow(t *testing.T, service *Service, artifactID, blockID string) (string, string, []model.AskOption) {
	t.Helper()
	var state, question string
	var optionsJSON []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state, question, options from asks where block_artifact_id = $1 and block_id = $2
	`, artifactID, blockID).Scan(&state, &question, &optionsJSON); err != nil {
		t.Fatalf("load ask row of block %q: %v", blockID, err)
	}
	var options []model.AskOption
	if err := json.Unmarshal(optionsJSON, &options); err != nil {
		t.Fatalf("decode ask row options: %v", err)
	}
	return state, question, options
}

// firstAsk is the document's first top-level ask.
func firstAsk(tree *pmdoc.Node) *pmdoc.Node {
	for _, child := range tree.Children {
		if child.Type == "ask" {
			return child
		}
	}
	panic("the document holds no ask")
}

// setAskAttrs sets attributes on the document's first ask, as settlement or an answer does.
func setAskAttrs(attrs pmdoc.Attrs) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		for name, value := range attrs {
			firstAsk(tree).Attrs[name] = value
		}
		return tree
	}
}

// appendToAsk adds a block to the end of the document's first ask, as a browser edit can.
func appendToAsk(block *pmdoc.Node) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		ask := firstAsk(tree)
		ask.Children = append(ask.Children, block)
		return tree
	}
}

func codeBlockNode(text string) *pmdoc.Node {
	return &pmdoc.Node{Type: "code_block", Attrs: pmdoc.Attrs{"language": nil}, Children: []*pmdoc.Node{{Type: "text", Text: text}}}
}

// A version's markdown carries what a rendering writes, and an upload's server-owned ask
// attributes are discarded for the ask row's, so an unreadable ask it carries through unchanged is
// taken whatever the live ask holds besides: a comment's anchor mark in its text, the id a
// reader's browser derives for a heading in it, or the answer or resolution the ask was given.
func TestReplaceCarriesAnUnreadableAskWithWhatNoRenderingWrites(t *testing.T) {
	const ask = "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship this week?\n:::\n"
	actor := model.Actor{Kind: "user", ID: "alice"}
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, service *Service, artifactID string)
	}{
		{"a comment anchored in its question", func(t *testing.T, service *Service, artifactID string) {
			if _, err := joinedMarkQuote(service, artifactID, MarkSpec{Kind: MarkComment, ID: "c1", By: actor}, "this week", nil); err != nil {
				t.Fatal(err)
			}
			editLiveTree(t, service, artifactID, appendToAsk(codeBlockNode("code")))
		}},
		{"a heading with the id a browser derived", func(t *testing.T, service *Service, artifactID string) {
			editLiveTree(t, service, artifactID, appendToAsk(&pmdoc.Node{Type: "heading", Attrs: pmdoc.Attrs{"level": float64(2), "id": "decision"}, Children: []*pmdoc.Node{{Type: "text", Text: "Decision"}}}))
		}},
		{"an answered ask", func(t *testing.T, service *Service, artifactID string) {
			editLiveTree(t, service, artifactID, setAskAttrs(pmdoc.Attrs{"state": "answered", "answered_by": "alice", "answered_at": "2026-09-12T13:20:00Z", "selected": []string{"Ship"}, "answer": "Ship it."}))
			editLiveTree(t, service, artifactID, appendToAsk(codeBlockNode("code")))
		}},
		{"a resolved ask", func(t *testing.T, service *Service, artifactID string) {
			editLiveTree(t, service, artifactID, setAskAttrs(pmdoc.Attrs{"state": "resolved"}))
			editLiveTree(t, service, artifactID, appendToAsk(codeBlockNode("code")))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, ask)
			test.setup(t, service, artifactID)
			current, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := joinedReplaceText(service, artifactID, strings.Replace(current, "Intro.", "Introduction.", 1), actor); err != nil {
				t.Fatalf("ReplaceText carrying the unreadable ask = %v, want it taken", err)
			}
		})
	}
}

// A refused seed leaves no room behind. A room leaves the service only when it is evicted, and every
// room counts toward the live-room limit, so a room made for a document that was never written
// would hold a slot for good.
func TestARefusedSeedLeavesNoRoom(t *testing.T) {
	for _, test := range []struct{ name, markdown string }{
		{"an ask breaking its content rule", "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich?\n\n```\ncode\n```\n:::\n"},
		{"markdown outside the schema", "<div>\nblock html\n</div>\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			tx, err := service.store.Pool.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			seedCtx, ledger := service.Join(context.Background(), tx)
			defer ledger.Discard()
			if _, err := service.SeedText(seedCtx, artifactID, test.markdown, model.Actor{Kind: "user", ID: "alice"}); err == nil {
				t.Fatal("SeedText took the markdown, want it refused")
			}
			if _, ok := service.rooms.Load(artifactID); ok {
				t.Fatal("the refused seed left a room for the document")
			}
		})
	}
}

// An ask body the schema does not allow - `paragraph+ bullet_list?` - is refused where markdown
// enters a document, as the browser editor's parser refuses to build the block; a replace of the
// document refuses it the same way and leaves the document as it was.
func TestSeedAndReplaceRefuseAnAskBodyTheSchemaDoesNotAllow(t *testing.T) {
	const withCode = "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich?\n\n```\ncode\n```\n:::\n"
	service, artifactID := newTestService(t)
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seedCtx, ledger := service.Join(context.Background(), tx)
	_, err = service.SeedText(seedCtx, artifactID, withCode, model.Actor{Kind: "user", ID: "seed"})
	ledger.Discard()
	_ = tx.Rollback(context.Background())
	var invalid *ErrInvalidAskBlock
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "a code block") || !strings.Contains(err.Error(), "paragraph+ bullet_list?") {
		t.Fatalf("SeedText(ask holding code) = %v, want ErrInvalidAskBlock naming the code block and the rule", err)
	}

	seedServiceText(t, service, artifactID, "Intro.\n")
	if _, err := joinedReplaceText(service, artifactID, withCode, model.Actor{Kind: "user", ID: "alice"}); !errors.As(err, &invalid) {
		t.Fatalf("ReplaceText(ask holding code) = %v, want ErrInvalidAskBlock", err)
	}
	if markdown, err := service.Text(context.Background(), artifactID); err != nil || markdown != "Intro.\n" {
		t.Fatalf("after a refused replace = %q (%v), want it unchanged", markdown, err)
	}
}

// A new version that carries an ask a browser edit left unreadable through unchanged is taken, as
// an edit beside it is; one that writes into it, or writes another ask breaking the rule, is not.
func TestReplaceCarriesAnAskTheBrowserLeftUnreadable(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n:::\n")
	editLiveTree(t, service, artifactID, appendToAsk(codeBlockNode("code")))
	current, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatal(err)
	}
	actor := model.Actor{Kind: "user", ID: "alice"}
	if _, err := joinedReplaceText(service, artifactID, current+"\nMore.\n", actor); err != nil {
		t.Fatalf("ReplaceText carrying the unreadable ask = %v, want it taken", err)
	}
	var invalid *ErrInvalidAskBlock
	changed := strings.Replace(current, "Should we ship?", "Ship now?", 1)
	if _, err := joinedReplaceText(service, artifactID, changed, actor); !errors.As(err, &invalid) {
		t.Fatalf("ReplaceText changing the unreadable ask = %v, want ErrInvalidAskBlock", err)
	}
}

// An upload is checked against the asks the document holds by each ask's markdown
// (newAskMarkdown), and an ask a browser edit left holding a table the upload carries unchanged is
// taken. The document's render wrote its tables' spans out under one budget, so the asks of one
// check share one: the upload of the document's own markdown, holding twenty asks each over a table
// whose body rows each span its twenty-column header, is taken. The document stays under the
// elements one write may make (pmdoc.MaxDocumentElements), as an upload must: twenty asks over tables
// whose spans took the render's whole budget would make some 400,000, and their upload is refused
// when it is parsed, before any ask is checked.
func TestAnUploadOfTheDocumentsOwnMarkdownKeepsAsksOverSpannedTables(t *testing.T) {
	var source strings.Builder
	for index := range 20 {
		fmt.Fprintf(&source, ":::ask{#ask-%d urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich %d?\n\n- A\n- B\n:::\n\n", index, index)
	}
	current, err := pmdoc.Parse(source.String())
	if err != nil {
		t.Fatal(err)
	}
	pmdoc.Walk(current, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			node.Children = append(node.Children, spannedTable(20, 20))
		}
		return true
	})
	pmdoc.EnsureBlockIDs(current)
	markdown, err := pmdoc.Render(current)
	if err != nil {
		t.Fatal(err)
	}
	target, err := parseReplacing(current, markdown)
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err = refuseChangedAsks(current, target, pmdoc.AskContentError, newAskMarkdown())
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Errorf("the upload of the document's own markdown was refused: %v", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 512<<20 {
		t.Errorf("checking the asks allocated %d MiB, want at most 512", allocated>>20)
	}
}

// An ask a browser edit left unreadable is the browser's to repair; an edit elsewhere in the
// document is accepted beside it, while an edit that writes into it, or writes another unreadable
// ask, is refused.
func TestEditsBesideAnAskTheBrowserLeftUnreadableAreAccepted(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n:::\n\nAfter.\n")
	editLiveTree(t, service, artifactID, appendToAsk(codeBlockNode("code")))
	actor := model.Actor{Kind: "user", ID: "alice"}

	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "After.", With: "Later."}}, actor, nil); err != nil {
		t.Fatalf("replace beside the unreadable ask = %v, want it accepted", err)
	}
	var invalid *ErrInvalidAskBlock
	for name, op := range map[string]model.EditOp{
		"into the unreadable ask":                   {Op: "replace", Find: "Should we ship?", With: "Ship now?"},
		"another unreadable ask":                    {Op: "insert", After: "end", Markdown: ":::ask{#a2 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich?\n\n> quoted\n:::\n"},
		"an ask with a paragraph after its options": {Op: "insert", After: "end", Markdown: ":::ask{#a3 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich?\n\n- A\n- B\n\nAn afterthought.\n:::\n"},
	} {
		if _, err := joinedApplyOps(service, artifactID, []model.EditOp{op}, actor, nil); !errors.As(err, &invalid) {
			t.Fatalf("%s = %v, want ErrInvalidAskBlock", name, err)
		}
	}
	markdown, err := service.Text(context.Background(), artifactID)
	if err != nil || !strings.Contains(markdown, "Should we ship?") || !strings.Contains(markdown, "Later.") || strings.Contains(markdown, "a2") {
		t.Fatalf("document = %q (%v), want only the accepted replace applied", markdown, err)
	}
}

func TestSettleRendersTreeAndWritesVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	alignLatestVersionWithUpdates(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	var markdown string
	if err := service.store.Pool.QueryRow(context.Background(), `select markdown from artifact_versions where artifact_id = $1 and number = 2`, artifactID).Scan(&markdown); err != nil {
		t.Fatal(err)
	}
	if markdown != "after\n" || version.Named {
		t.Fatalf("version 2 = %q named=%v", markdown, version.Named)
	}
}

// The browser editor derives attributes that no rendering carries: every heading's id, slugged
// from its text as soon as it opens the document (Milkdown's syncHeadingIdPlugin), and every
// ordered list item's label and list type, rewritten on the first keyboard caret move or edit
// (syncListOrderPlugin). The server parses headings with an empty id and every list item as a
// bullet. A version records the rendered markdown alone, so such an update changes nothing a
// version records: it credits no connected reader and settles without a version, which would
// otherwise stale an approval.
func TestAttributesTheEditorDerivesSettleWithoutAVersion(t *testing.T) {
	for _, test := range []struct {
		name     string
		markdown string
		edit     func(*pmdoc.Node) *pmdoc.Node
	}{
		{name: "heading ids", markdown: "## Database\n\nUse SQLite", edit: setHeadingID("database")},
		{name: "ordered list labels", markdown: "## Plan\n\n1. First step.\n2. Second step.", edit: labelOrderedListItems},
		{name: "ordered list starting at 3", markdown: "3. Third step.\n4. Fourth step.", edit: labelOrderedListItems},
		{name: "ordered list inside bullets", markdown: "- Outer\n  1. Inner one.\n  2. Inner two.", edit: labelOrderedListItems},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, test.markdown)
			alignLatestVersionWithUpdates(t, service, artifactID)
			reader := model.Actor{Kind: "user", ID: "bob"}
			service.addConnection(artifactID, service.nextConnection.Add(1), reader)

			editLiveTree(t, service, artifactID, test.edit)
			settleCurrentGeneration(t, service, artifactID)

			assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 1, 0)
			if pending := pendingAuthors(t, service, artifactID); len(pending) != 0 {
				t.Fatalf("pending authors = %v, want none (reader %v changed nothing)", pending, reader)
			}
		})
	}
}

// A comment or suggestion marked on part of an identifier changes no version's markdown, whether
// the mark reaches the room directly, where the room's update observer classifies it, or through
// a transaction that then snapshots the document, as an anchored comment's handler does. Either
// way the document keeps its one version, and the marker is credited with nothing.
func TestAMarkInsideAWordWritesNoVersion(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	// snapshot marks the quote when mark is set, then versions the document, in one transaction,
	// as an anchored comment's handler does.
	snapshot := func(t *testing.T, service *Service, artifactID string, mark *MarkSpec) VersionResult {
		t.Helper()
		tx, err := service.store.Pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		ctx, ledger := service.Join(context.Background(), tx)
		defer ledger.Discard()
		if mark != nil {
			if _, err := service.MarkQuote(ctx, artifactID, *mark, "case", nil); err != nil {
				t.Fatalf("mark: %v", err)
			}
		}
		result, err := service.SnapshotVersion(ctx, artifactID, alice)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if err := ledger.Commit(context.Background()); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return result
	}
	for _, kind := range []MarkKind{MarkComment, MarkSuggestion} {
		for _, joined := range []bool{false, true} {
			name := string(kind) + ", direct"
			if joined {
				name = string(kind) + ", in a transaction that snapshots"
			}
			t.Run(name, func(t *testing.T) {
				service, artifactID := newTestService(t)
				service.settle = time.Hour
				seedServiceText(t, service, artifactID, "Use snake_case here.")
				// Version 2 is the seeded text, so the latest version and the document agree.
				if baseline := snapshot(t, service, artifactID, nil); baseline.Version.Number != 2 {
					t.Fatalf("baseline version = %d, want 2", baseline.Version.Number)
				}
				alignLatestVersionWithUpdates(t, service, artifactID)
				spec := MarkSpec{Kind: kind, ID: "m1", By: alice}

				if joined {
					if result := snapshot(t, service, artifactID, &spec); result.Wrote {
						t.Errorf("snapshot after the mark wrote version %d", result.Version.Number)
					}
				} else if _, err := joinedMarkQuote(service, artifactID, spec, "case", nil); err != nil {
					t.Fatalf("mark: %v", err)
				}
				settleCurrentGeneration(t, service, artifactID)

				assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 2, 0)
				if pending := pendingAuthors(t, service, artifactID); len(pending) != 0 {
					t.Fatalf("pending authors = %v, want none (a mark is not content)", pending)
				}
			})
		}
	}
}

// Renaming a heading changes its text, and the editor re-derives its id with it: that is a real
// edit, versioned and credited to the reader connected when it happened.
func TestRenamingAHeadingIsVersionedAndCredited(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "## Database\n\nUse SQLite")
	alignLatestVersionWithUpdates(t, service, artifactID)
	reader := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, service.nextConnection.Add(1), reader)

	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		return setHeadingID("storage")(replaceRun("Database", "Storage")(tree))
	})
	settleCurrentGeneration(t, service, artifactID)

	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	var markdown string
	if err := service.store.Pool.QueryRow(context.Background(), `select markdown from artifact_versions where artifact_id = $1 and number = 2`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read version 2: %v", err)
	}
	if markdown != "## Storage\n\nUse SQLite\n" || !reflect.DeepEqual(version.Authors, []model.Actor{reader}) {
		t.Fatalf("version 2 = %q by %v, want the renamed heading by %v", markdown, version.Authors, reader)
	}
}

// A browser that has a document open and changes nothing is no author of an agent's edit: the
// version written for it names only the agent, whether the edit reaches the room directly or
// through a committed transaction, and whether the reader left before it or is still connected.
func TestAReaderIsNoAuthorOfAnAgentsVersion(t *testing.T) {
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	edit := []model.EditOp{{Op: "replace", Find: "before", With: "after"}}
	for _, test := range []struct {
		name         string
		readerLeaves bool
		joined       bool
	}{
		{name: "reader left, direct edit", readerLeaves: true},
		{name: "reader connected, direct edit"},
		{name: "reader connected, transactional edit", joined: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "before")
			alignLatestVersionWithUpdates(t, service, artifactID)
			reader := model.Actor{Kind: "user", ID: "bob"}
			connectionID := service.nextConnection.Add(1)
			service.addConnection(artifactID, connectionID, reader)
			if test.readerLeaves {
				service.removeConnection(artifactID, connectionID)
			}

			if test.joined {
				tx, err := service.store.Pool.Begin(context.Background())
				if err != nil {
					t.Fatalf("begin agent edit: %v", err)
				}
				defer tx.Rollback(context.Background())
				joinedCtx, ledger := service.Join(context.Background(), tx)
				defer ledger.Discard()
				if _, err := service.ApplyOps(joinedCtx, artifactID, edit, agent, nil); err != nil {
					t.Fatalf("agent edit: %v", err)
				}
				if err := ledger.Commit(context.Background()); err != nil {
					t.Fatalf("commit agent edit: %v", err)
				}
			} else if _, err := joinedApplyOps(service, artifactID, edit, agent, nil); err != nil {
				t.Fatalf("agent edit: %v", err)
			}
			settleCurrentGeneration(t, service, artifactID)

			version := waitForDocumentVersion(t, service.store, artifactID, 2)
			if !reflect.DeepEqual(version.Authors, []model.Actor{agent}) {
				t.Fatalf("version 2 authors = %v, want only the agent %v (reader %v changed nothing)", version.Authors, agent, reader)
			}
		})
	}
}

// pendingAuthors is every author the document owes a version: the room's in-flight credits no
// committed version has listed and its durable pending authors. F is read first: a credit that
// lands in between commits to R before it leaves F, so the R read that follows holds it.
func pendingAuthors(t *testing.T, service *Service, artifactID string) []model.Actor {
	t.Helper()
	owed := make(map[string]model.Actor)
	state := service.room(artifactID)
	state.mu.Lock()
	for _, record := range state.unconsumedInflightLocked(state.creditSeq.Load()) {
		maps.Copy(owed, record.authors)
	}
	state.mu.Unlock()
	durable, err := readPendingAuthors(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending authors: %v", err)
	}
	maps.Copy(owed, durable)
	return actorSlice(owed)
}

// setHeadingID returns a live edit that gives every heading id, as the editor's heading plugin
// does for a heading whose text slugs to id.
func setHeadingID(id string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		for _, block := range tree.Children {
			if block.Type == "heading" {
				block.Attrs["id"] = id
			}
		}
		return tree
	}
}

// labelOrderedListItems is the live edit the editor's list plugin makes on the first keyboard caret
// move or edit: every item of an ordered list is labelled with its number and typed as ordered.
func labelOrderedListItems(node *pmdoc.Node) *pmdoc.Node {
	if node.Type == "ordered_list" {
		start := 1
		switch order := node.Attrs["order"].(type) {
		case int:
			start = order
		case int64:
			start = int(order)
		case float64:
			start = int(order)
		}
		for index, item := range node.Children {
			item.Attrs["label"] = fmt.Sprintf("%d.", start+index)
			item.Attrs["listType"] = "ordered"
		}
	}
	for _, child := range node.Children {
		labelOrderedListItems(child)
	}
	return node
}

func TestSettleStampsPersistedLegacyProofDocument(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})

	service.settleRoom(artifactID, 0)

	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load stamped document: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode stamped document: %v", err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read stamped document: %v", err)
	}
	if repairs := pmdoc.BlockIDRepairCount(tree); repairs != 0 {
		t.Fatalf("persisted document has %d unstamped blocks", repairs)
	}
}
func TestSettleCapturesOnlyItsOwnIdentityUpdate(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load legacy document: %v", err)
	}
	captured := make(chan []byte, 1)
	service := New(Deps{
		Store:       database,
		Persistence: captureAppendUpdateTxStore{VersionedStore: NewPgVersioned(database), captured: captured},
		Events:      events.NewBroker(),
		Settle:      time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})

	var injected atomic.Bool
	foreignApplied := make(chan struct{})
	var foreignErr error
	var unsubscribe func()
	err = service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		unsubscribe = doc.OnUpdate(func(_ []byte, _ any) {
			if !injected.CompareAndSwap(false, true) {
				return
			}
			tree, err := treeOf(doc)
			if err == nil {
				fragment := doc.GetXmlFragment(fragmentName)
				foreignErr = doc.TransactE(func(transaction *crdt.Transaction) error {
					return pmdoc.Update(transaction, fragment, replaceRun("before", "foreign")(tree))
				}, "foreign update")
			}
			if foreignErr == nil {
				foreignErr = err
			}
			close(foreignApplied)
		})
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("warm document for settlement: %v", err)
	}
	t.Cleanup(unsubscribe)

	// The load armed the settlement the seeded update owes, which moved the generation.
	state := service.room(artifactID)
	state.mu.Lock()
	armed := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(artifactID, armed)
	<-foreignApplied
	if foreignErr != nil {
		t.Fatalf("apply foreign update during identity settlement: %v", foreignErr)
	}
	identityUpdate := <-captured
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode legacy document: %v", err)
	}
	if err := crdt.ApplyUpdateV1(doc, identityUpdate, nil); err != nil {
		t.Fatalf("apply captured identity update: %v", err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read captured identity document: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatalf("render captured identity document: %v", err)
	}
	if markdown != "before\n" {
		t.Fatalf("identity update changed document = %q, want only identity repairs", markdown)
	}
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	// The foreign update arms a settlement of its own, which moves the room on from the
	// generation this one was armed with. Which value it moves to is the service's to choose
	// (nextRoomGeneration), so what this asserts is that it moved.
	if generation == armed {
		t.Fatalf("generation after foreign update = %d, the one the settlement was armed with: the update armed none", generation)
	}
	waitForPersistedProofText(t, database, artifactID, "foreign\n")
	service.settleRoom(artifactID, generation)
	waitForDocumentVersion(t, database, artifactID, 2)
}

func TestSettleStampsLegacyChangeInExactlyOneVersion(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForPersistedProofText(t, database, artifactID, "after\n")
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)

	waitForDocumentVersion(t, database, artifactID, 2)
	var versions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count settled versions: %v", err)
	}
	if versions != 2 {
		t.Fatalf("versions after identity settlement = %d, want 2", versions)
	}
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs != 0 {
		t.Fatalf("persisted changed document has %d unstamped blocks", repairs)
	}
}

func TestSettleDiscardsIdentityUpdateWhenVersionTransactionFails(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, err := database.Pool.Exec(context.Background(), `
		create function dispatch_test_reject_identity_settlement() returns trigger language plpgsql as $$
		begin
			if new.number = 2 then
				raise exception 'reject identity settlement version';
			end if;
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create identity settlement failure function: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		create trigger dispatch_test_reject_identity_settlement
		before insert on artifact_versions for each row
		execute function dispatch_test_reject_identity_settlement()
	`); err != nil {
		t.Fatalf("create identity settlement failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_reject_identity_settlement on artifact_versions`)
		_, _ = database.Pool.Exec(context.Background(), `drop function if exists dispatch_test_reject_identity_settlement()`)
	})

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForPersistedProofText(t, database, artifactID, "after\n")
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)

	waitForRoomFailure(t, service, artifactID)
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs == 0 {
		t.Fatal("failed settlement persisted identity updates")
	}
	var versions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after failed settlement: %v", err)
	}
	if versions != 1 {
		t.Fatalf("versions after failed settlement = %d, want 1", versions)
	}
	if _, err := database.Pool.Exec(context.Background(), `drop trigger dispatch_test_reject_identity_settlement on artifact_versions`); err != nil {
		t.Fatalf("drop identity settlement failure trigger: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `drop function dispatch_test_reject_identity_settlement()`); err != nil {
		t.Fatalf("drop identity settlement failure function: %v", err)
	}
	if err := service.awaitRoomRecovery(context.Background(), artifactID); err != nil {
		t.Fatalf("await room recovery: %v", err)
	}
	service.settleRoom(artifactID, 0)

	waitForDocumentVersion(t, database, artifactID, 2)
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs != 0 {
		t.Fatalf("retry persisted document with %d unstamped blocks", repairs)
	}
}

func TestFailedSettlementDoesNotDiscardSuccessorRoomUpdate(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	entered := make(chan struct{})
	release := make(chan struct{})
	persistence := &blockingFirstAppendStore{
		VersionedStore: NewPgVersioned(database),
		entered:        entered,
		release:        release,
	}
	service := New(Deps{
		Store:       database,
		Persistence: persistence,
		Events:      events.NewBroker(),
		Settle:      time.Hour,
	})
	t.Cleanup(func() {
		if persistence.released.CompareAndSwap(false, true) {
			close(release)
		}
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, err := database.Pool.Exec(context.Background(), `
		create function dispatch_test_reject_successor_settlement() returns trigger language plpgsql as $$
		begin
			if new.number = 2 then
				raise exception 'reject successor settlement version';
			end if;
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create successor settlement failure function: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		create trigger dispatch_test_reject_successor_settlement
		before insert on artifact_versions for each row
		execute function dispatch_test_reject_successor_settlement()
	`); err != nil {
		t.Fatalf("create successor settlement failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_reject_successor_settlement on artifact_versions`)
		_, _ = database.Pool.Exec(context.Background(), `drop function if exists dispatch_test_reject_successor_settlement()`)
	})

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	<-entered
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)
	if persistence.released.CompareAndSwap(false, true) {
		close(release)
	}
	waitForRoomFailure(t, service, artifactID)
	if err := service.awaitRoomRecovery(context.Background(), artifactID); err != nil {
		t.Fatalf("evict failed room: %v", err)
	}

	editLiveTree(t, service, artifactID, replaceRun("after", "successor"))
	waitForPersistedProofText(t, database, artifactID, "successor\n")
}

func TestBackfillStampsClosedIssueDocument(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, err := database.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed(context.Background(), "DOC-1", true)

	reports, err := service.BackfillBlockIDs(context.Background())
	if err != nil {
		t.Fatalf("backfill closed document: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("backfill reports = %#v, want one document", reports)
	}
	if reports[0].Stamped != 1 {
		t.Fatalf("closed document stamped = %d, want 1", reports[0].Stamped)
	}
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs != 0 {
		t.Fatalf("backfill persisted document with %d unstamped blocks", repairs)
	}
}

// A closed issue's document stays readable: GET /blocks reads it as GET /text does
// (readDocument), rather than asking the room, whose inject gate refuses a closed issue, and
// failing the room over that refusal.
func TestAClosedIssuesDocumentReadsItsBlocksWithoutFailingItsRoom(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "before")
	liveTree(t, service, artifactID)
	if _, err := database.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed(context.Background(), "DOC-1", true)

	markdown, blocks, err := service.TextWithBlocks(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read a closed issue's blocks: %v", err)
	}
	if markdown != "before\n" || len(blocks) != 1 {
		t.Fatalf("closed document = %q with %d blocks, want %q with one", markdown, len(blocks), "before\n")
	}
	if err := service.roomFailure(artifactID); err != nil {
		t.Fatalf("reading a closed issue's blocks failed its room: %v", err)
	}
}

func TestBackfillDoesNotBypassClosedIssueForConcurrentApplyOps(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, err := database.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed(context.Background(), "DOC-1", true)

	entered := make(chan struct{})
	release := make(chan struct{})
	var blocked atomic.Bool
	var released atomic.Bool
	t.Cleanup(func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	})
	service.srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return service.allowInject(ctx, info)
	}
	type result struct {
		err error
	}
	backfill := make(chan result, 1)
	go func() {
		_, err := service.BackfillBlockIDs(context.Background())
		backfill <- result{err: err}
	}()
	<-entered
	_, applyErr := joinedApplyOps(service, artifactID, []model.EditOp{{
		Op:   "replace",
		Find: "before",
		With: "foreign",
	}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)

	if released.CompareAndSwap(false, true) {
		close(release)
	}
	backfillResult := <-backfill
	if backfillResult.err != nil {
		t.Fatalf("backfill document: %v", backfillResult.err)
	}
	if !errors.Is(applyErr, ErrIssueClosed) {
		t.Fatalf("concurrent edit during closed-document backfill = %v, want ErrIssueClosed", applyErr)
	}
}

func TestBackfillReportsStoppingDocumentAsSkipped(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	service.stopping.Store(true)

	reports, err := service.BackfillBlockIDs(context.Background())
	if err != nil {
		t.Fatalf("backfill stopping service: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("backfill reports = %#v, want one document", reports)
	}
	if reports[0].ArtifactID != artifactID {
		t.Fatalf("backfill artifact = %q, want %q", reports[0].ArtifactID, artifactID)
	}
	if reports[0].Skipped != "service stopping" {
		t.Fatalf("backfill skip = %q, want service stopping", reports[0].Skipped)
	}
}

func TestBackfillReportsDocumentPersistenceFailure(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{
		Store: database,
		Persistence: failingBackfillVersionedStore{
			VersionedStore: NewPgVersioned(database),
			err:            errors.New("persist identity update"),
		},
		Events: events.NewBroker(),
		Settle: time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})

	reports, err := service.BackfillBlockIDs(context.Background())
	if err != nil {
		t.Fatalf("backfill persistence failure: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("backfill reports = %#v, want one document", reports)
	}
	if !errors.Is(reports[0].Err, service.persistence.(failingBackfillVersionedStore).err) {
		t.Fatalf("backfill error = %v, want persistence error", reports[0].Err)
	}
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs == 0 {
		t.Fatal("failed backfill persisted identity updates")
	}
}

func TestSettleWritesArtifactOwnedEventForUnlinkedDocument(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createProjectDocument(t, database, "before")
	broker := events.NewBroker()
	service := New(Deps{Store: database, Events: broker, Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "before")
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForDocumentVersion(t, database, artifactID, 2)

	var eventArtifactID string
	var eventIssueKey *string
	var lastSeq int
	if err := database.Pool.QueryRow(context.Background(), `
		select e.artifact_id::text, e.issue_key, a.last_seq
		from events e join artifacts a on a.id = e.artifact_id
		where e.artifact_id = $1 and e.type = 'artifact.version'
	`, artifactID).Scan(&eventArtifactID, &eventIssueKey, &lastSeq); err != nil {
		t.Fatalf("read project document event: %v", err)
	}
	if eventArtifactID != artifactID || eventIssueKey != nil || lastSeq != 1 {
		t.Fatalf("project document event artifact=%q issue=%#v last_seq=%d", eventArtifactID, eventIssueKey, lastSeq)
	}
}

func TestUnlinkedDocumentIsAlwaysOpen(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createProjectDocument(t, database, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "before")
	open, err := service.issueOpen(context.Background(), service.queryFrom(context.Background()), artifactID)
	if err != nil {
		t.Fatalf("check unlinked document open: %v", err)
	}
	if !open {
		t.Fatal("unlinked document is closed")
	}
	namedResult, err := namedVersion(t, service, artifactID, "checkpoint", model.Actor{Kind: "user", ID: "alice"})
	version := namedResult.Version
	if err != nil {
		t.Fatalf("name unlinked document version: %v", err)
	}
	if !version.Named || version.Summary == nil || *version.Summary != "checkpoint" {
		t.Fatalf("unlinked document version = %#v", version)
	}
}

func TestSettleSkipsVersionWhenTreeLeavesTheSchema(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 20 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		transact(func(txn *crdt.Transaction) {
			fragment.InsertElement(txn, 0, crdt.NewYXmlElement("callout"))
		})
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("settle wrote %d versions for a document outside the schema", versions)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocSchema) {
		t.Fatalf("Text err = %v, want ErrDocSchema", err)
	}
}

// A joined write reaches the room only when its transaction commits: the room never holds a
// write that rolls back, and no settlement can version one.
func TestRolledBackWriteNeverReachesTheRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	const settleInterval = 50 * time.Millisecond
	service.settle = settleInterval
	seedServiceText(t, service, artifactID, "before")
	// The seed owes a settlement, which versions it (the fixture's first version is another
	// text); settled here, it is not what a load during the transaction arms below.
	settleCurrentGeneration(t, service, artifactID)
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	if _, err := service.ReplaceText(joined, artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	if got, err := service.Text(joined, artifactID); err != nil || got != "after\n" {
		t.Fatalf("document inside the transaction = %q (%v), want its own write", got, err)
	}
	if got, err := service.Text(ctx, artifactID); err != nil || got != "before\n" {
		t.Fatalf("live document before commit = %q (%v), want before", got, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back document mutation: %v", err)
	}
	ledger.Discard()
	time.Sleep(3 * settleInterval)
	if got, err := service.Text(ctx, artifactID); err != nil || got != "before\n" {
		t.Fatalf("live document after rollback = %q (%v), want before", got, err)
	}
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after rollback: %v", err)
	}
	if versions != 2 {
		t.Fatalf("versions after rollback = %d, want the fixture's and the seed's 2", versions)
	}
}

func TestSettlePersistsPendingAuthorAfterDisconnect(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	alignLatestVersionWithUpdates(t, service, artifactID)
	actor := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, actor)

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	service.removeConnection(artifactID, connectionID)

	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 1 || version.Authors[0] != actor {
		t.Fatalf("settled version authors = %#v, want %v", version.Authors, actor)
	}
}

func TestSettleWritesDirtyVersionWithoutPendingAuthors(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))

	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 0 {
		t.Fatalf("settled version authors = %#v, want none", version.Authors)
	}
}

func TestConnectedActorIsPendingAfterEachDocumentUpdate(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	alignLatestVersionWithUpdates(t, service, artifactID)
	actor := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, actor)

	current := "before"
	for number, markdown := range []string{"after first update", "after second update"} {
		editLiveTree(t, service, artifactID, replaceRun(current, markdown))
		current = markdown
		version := waitForDocumentVersion(t, service.store, artifactID, number+2)
		if len(version.Authors) != 1 || version.Authors[0] != actor {
			t.Fatalf("version %d authors = %#v, want %v", number+2, version.Authors, actor)
		}
	}
}

// A browser edit's author is a connected peer, never the API caller who edited earlier. When a
// version consumes the room's pending authors before settlement (an anchored comment snapshots
// the document), the settlement's fallback must be that peer, or a human's block ask is
// attributed to whichever agent last wrote through the API.
func TestSettleAttributesBrowserWrittenAskBlockToTheConnectedPeer(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, agent, nil); err != nil {
		t.Fatalf("agent edit: %v", err)
	}
	snapshotAndCommitVersion(t, service, artifactID, agent)

	human := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, human)
	editLiveTree(t, service, artifactID, appendBlocks(t, ":::ask{#browser-ask urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n"))
	snapshotAndCommitVersion(t, service, artifactID, human)

	settleCurrentGeneration(t, service, artifactID)
	if author := blockAskAuthor(t, service, artifactID, "browser-ask"); author != human {
		t.Fatalf("browser-written ask author = %#v, want %v", author, human)
	}
}

// The converse: a service mutation's block ask belongs to its API actor even while a human has
// the document open in a browser, and even after an anchored comment consumed the room's
// pending authors before settlement.
func TestSettleAttributesServiceWrittenAskBlockToTheAPIActorWhilePeerConnected(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	human := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, human)
	editLiveTree(t, service, artifactID, replaceRun("before", "human edit"))
	snapshotAndCommitVersion(t, service, artifactID, human)

	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{
		Op: "insert", After: "end", Markdown: ":::ask{#agent-ask urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n",
	}}, agent, nil); err != nil {
		t.Fatalf("agent edit: %v", err)
	}
	snapshotAndCommitVersion(t, service, artifactID, human)

	settleCurrentGeneration(t, service, artifactID)
	if author := blockAskAuthor(t, service, artifactID, "agent-ask"); author != agent {
		t.Fatalf("service-written ask author = %#v, want %v", author, agent)
	}
}

// A service edit's ask block belongs to that service actor even when a browser edits elsewhere
// before settlement. The service knows which write introduced the block; the browser's later
// content is not its author (LEGION-503).
func TestSettleAttributesAServiceWrittenAskBlockToItsWriterAfterBrowserEdits(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 1, bob)
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin agent edit: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{
		Op: "insert", After: "end", Markdown: ":::ask{#agent-after-browser urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n",
	}}, agent, nil); err != nil {
		t.Fatalf("agent writes an ask block: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit agent edit: %v", err)
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, "Bob typed later.\n"))
	settleCurrentGeneration(t, service, artifactID)

	if author := blockAskAuthor(t, service, artifactID, "agent-after-browser"); author != agent {
		t.Fatalf("service-written ask author = %#v, want %v", author, agent)
	}
	if actor := blockAskOpenedActor(t, service, artifactID, "agent-after-browser"); actor != agent {
		t.Fatalf("ask.opened actor = %#v, want %v", actor, agent)
	}
	if !blockAskFollows(t, service, artifactID, "agent-after-browser", agent.ID) {
		t.Fatalf("service-written ask does not follow its author %q", agent.ID)
	}
}

// An upload's ask block belongs to its uploader even when a browser edits elsewhere before
// settlement. The upload is a service write that introduced the block (LEGION-503).
func TestSettleAttributesAnUploadedAskBlockToItsUploaderAfterBrowserEdits(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 1, bob)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := joinedReplaceText(service, artifactID, "before\n\n:::ask{#upload-after-browser urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n", alice); err != nil {
		t.Fatalf("upload the ask block: %v", err)
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, "Bob typed later.\n"))
	settleCurrentGeneration(t, service, artifactID)

	if author := blockAskAuthor(t, service, artifactID, "upload-after-browser"); author != alice {
		t.Fatalf("uploaded ask author = %#v, want %v", author, alice)
	}
	if actor := blockAskOpenedActor(t, service, artifactID, "upload-after-browser"); actor != alice {
		t.Fatalf("ask.opened actor = %#v, want %v", actor, alice)
	}
}

// A settlement attributes the block ask it indexes to the room's latest editor, whose change its
// version credits, not to an author still pending from an earlier change: an author whose edits
// came to nothing - typed and undone - stays pending past the settlement that found nothing to
// version (LEGION-503), and sorts ahead of the ask's writer here.
func TestSettleAttributesAnAskBlockToItsLatestEditorOverAnEarlierPendingAuthor(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	typed := model.Actor{Kind: "user", ID: "bob"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, typed)
	editLiveTree(t, service, artifactID, replaceRun("before", "before, typed"))
	editLiveTree(t, service, artifactID, replaceRun("before, typed", "before"))
	settleCurrentGeneration(t, service, artifactID)
	service.removeConnection(artifactID, connectionID)

	writer := model.Actor{Kind: "user", ID: "carol"}
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{
		Op: "insert", After: "end", Markdown: ":::ask{#carol-ask urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n",
	}}, writer, nil); err != nil {
		t.Fatalf("carol's edit: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	if author := blockAskAuthor(t, service, artifactID, "carol-ask"); author != writer {
		t.Fatalf("ask author = %#v, want %v, who wrote it", author, writer)
	}
}

// snapshotAndCommitVersion versions the live text the way an anchored comment does, which
// consumes the room's pending authors ahead of the next settlement.
func snapshotAndCommitVersion(t *testing.T, service *Service, artifactID string, actor model.Actor) {
	t.Helper()
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin snapshot transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	snapshotCtx, ledger := service.Join(context.Background(), tx)
	defer ledger.Discard()
	if _, err := service.SnapshotVersion(snapshotCtx, artifactID, actor); err != nil {
		t.Fatalf("snapshot version: %v", err)
	}
	if err := ledger.Commit(context.Background()); err != nil {
		t.Fatalf("commit snapshot transaction: %v", err)
	}
}

// settleCurrentGeneration runs the room's settlement at its current generation once every live
// update is durable. ygo persists live updates on its own worker, and a settlement that still
// finds one queued defers itself to a retry timer instead of settling, so a caller that asserts on
// the settlement's writes must not start it until that queue is empty.
func settleCurrentGeneration(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for live updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for live updates to become durable: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)
}

// appendBlocks returns a live edit that appends the blocks parsed from markdown, stamped with
// block ids as the browser editor would.
func appendBlocks(t *testing.T, markdown string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	parsed, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse appended blocks: %v", err)
	}
	pmdoc.EnsureBlockIDs(parsed)
	return func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = append(tree.Children, parsed.Children...)
		return tree
	}
}

func blockAskAuthor(t *testing.T, service *Service, artifactID, blockID string) model.Actor {
	t.Helper()
	var raw []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select author from asks where block_artifact_id = $1 and block_id = $2
	`, artifactID, blockID).Scan(&raw); err != nil {
		t.Fatalf("read block ask %q author: %v", blockID, err)
	}
	var author model.Actor
	if err := json.Unmarshal(raw, &author); err != nil {
		t.Fatalf("decode block ask author: %v", err)
	}
	return author
}

func blockAskOpenedActor(t *testing.T, service *Service, artifactID, blockID string) model.Actor {
	t.Helper()
	var raw []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select e.actor
		from events e
		join asks a on e.payload->>'id' = a.id::text
		where e.type = 'ask.opened' and a.block_artifact_id = $1 and a.block_id = $2
	`, artifactID, blockID).Scan(&raw); err != nil {
		t.Fatalf("read block ask %q opened actor: %v", blockID, err)
	}
	var actor model.Actor
	if err := json.Unmarshal(raw, &actor); err != nil {
		t.Fatalf("decode block ask %q opened actor: %v", blockID, err)
	}
	return actor
}

func blockAskFollows(t *testing.T, service *Service, artifactID, blockID, sessionID string) bool {
	t.Helper()
	var follows bool
	if err := service.store.Pool.QueryRow(context.Background(), `
		select exists(
			select 1
			from ask_followers f
			join asks a on a.id = f.ask_id
			where a.block_artifact_id = $1 and a.block_id = $2 and f.session_id = $3
		)
	`, artifactID, blockID, sessionID).Scan(&follows); err != nil {
		t.Fatalf("read block ask %q followers: %v", blockID, err)
	}
	return follows
}

func TestSupersededSettleGenerationDoesNotWrite(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	state := service.room(artifactID)
	state.mu.Lock()
	stale := state.roomGeneration
	state.mu.Unlock()
	service.scheduleSettle(artifactID)
	state.mu.Lock()
	current := state.roomGeneration
	state.mu.Unlock()

	service.settleRoom(artifactID, stale)
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after stale settle: %v", err)
	}
	if versions != 1 {
		t.Fatalf("versions after stale settle = %d, want 1", versions)
	}
	service.settleRoom(artifactID, current)
	waitForDocumentVersion(t, service.store, artifactID, 2)
}

func TestSettleRetriesTransientVersionWriteFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 10 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.store.Pool.Exec(context.Background(), `create sequence dispatch_test_settle_failure`); err != nil {
		t.Fatalf("create settlement failure sequence: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create function dispatch_test_fail_first_settlement() returns trigger language plpgsql as $$
		begin
			if new.number = 2 and nextval('dispatch_test_settle_failure') = 1 then
				raise exception 'transient settlement write failure';
			end if;
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create settlement failure function: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create trigger dispatch_test_fail_first_settlement
		before insert on artifact_versions for each row
		execute function dispatch_test_fail_first_settlement()
	`); err != nil {
		t.Fatalf("create settlement failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = service.store.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_fail_first_settlement on artifact_versions`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop function if exists dispatch_test_fail_first_settlement()`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop sequence if exists dispatch_test_settle_failure`)
	})

	if _, err := joinedReplaceText(service, artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("change document before transient settle failure: %v", err)
	}
	waitForDocumentVersion(t, service.store, artifactID, 2)
}

func TestSettleFailsRoomAfterPersistentVersionWriteFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 10 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.store.Pool.Exec(context.Background(), `
		create function dispatch_test_fail_every_settlement() returns trigger language plpgsql as $$
		begin
			if new.number = 2 then
				raise exception 'persistent settlement write failure';
			end if;
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create persistent settlement failure function: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create trigger dispatch_test_fail_every_settlement
		before insert on artifact_versions for each row
		execute function dispatch_test_fail_every_settlement()
	`); err != nil {
		t.Fatalf("create persistent settlement failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = service.store.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_fail_every_settlement on artifact_versions`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop function if exists dispatch_test_fail_every_settlement()`)
	})
	if _, err := joinedReplaceText(service, artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("change document before persistent settlement failure: %v", err)
	}
	waitForRoomFailure(t, service, artifactID)
}

func TestShutdownDrainsPendingUpdateBeforeSettling(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	if _, err := namedVersion(t, service, artifactID, "checkpoint", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write named version before shutdown: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with pending document update: %v", err)
	}
	reloaded := New(Deps{Store: service.store, Events: events.NewBroker(), Settle: time.Hour})
	defer reloaded.Shutdown(context.Background())
	if got, err := reloaded.Text(context.Background(), artifactID); err != nil || got != "after\n" {
		t.Fatalf("text after shutdown = %q (%v), want after", got, err)
	}
}

func TestShutdownSkipsColdDocumentSettlement(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown cold document: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.settle != nil {
		t.Fatal("shutdown cold document armed settlement")
	}
}

func TestShutdownStopsTimerForEvictedRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	service.scheduleSettle(artifactID)
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict scheduled room: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown after eviction: %v", err)
	}
}

func TestImmediateSettlementTimerUnregistersAfterFiring(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 0
	seedServiceText(t, service, artifactID, "before")

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForDocumentVersion(t, service.store, artifactID, 2)

	state := service.room(artifactID)
	state.mu.Lock()
	timer := state.settle
	state.mu.Unlock()
	if timer == nil {
		t.Fatal("immediate settlement did not schedule a timer")
	}
	waitFor(t, time.Second, "expired immediate settlement timer to unregister", func() bool {
		return !service.isSettleTimerArmed(timer)
	})
}

type shutdownCompactStore struct {
	VersionedStore
	closed               atomic.Bool
	compactions          atomic.Int32
	compactionAfterClose chan struct{}
}

func (s *shutdownCompactStore) Compact(ctx context.Context, room string, keep int) (int, error) {
	if s.closed.Load() {
		select {
		case s.compactionAfterClose <- struct{}{}:
		default:
		}
	}
	s.compactions.Add(1)
	return s.VersionedStore.Compact(ctx, room, keep)
}

func TestShutdownCompletesCompactionBeforeTestStoreCloses(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	persistence := &shutdownCompactStore{
		VersionedStore:       NewPgVersioned(database),
		compactionAfterClose: make(chan struct{}, 1),
	}
	service := New(Deps{Store: database, Persistence: persistence, Events: events.NewBroker(), Settle: time.Hour})
	service.settle = 0
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	seedServiceText(t, service, artifactID, "before")
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForDocumentVersion(t, database, artifactID, 2)

	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown document service: %v", err)
	}
	if persistence.compactions.Load() == 0 {
		t.Fatal("shutdown did not run document compaction")
	}

	persistence.closed.Store(true)
	database.Pool.Close()
	select {
	case <-persistence.compactionAfterClose:
		t.Fatal("document compaction ran after the test store closed")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestShutdownBoundsAdvisoryLockedAppendAndPreservesUpdate(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	locker, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin append lock transaction: %v", err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(context.Background(), `select pg_advisory_xact_lock(hashtext($1))`, artifactID); err != nil {
		t.Fatalf("lock document append: %v", err)
	}
	// A browser's edit returns once the room has applied it, and the room's update observer
	// counts the durable append before that; the append itself cannot finish while the lock is
	// held, so the count is still up here.
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	if !service.hasDurableAppend(artifactID) {
		t.Fatal("durable append finished while its advisory lock was held")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := service.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown while append lock held = %v, want deadline exceeded", err)
	}
	if err := locker.Commit(context.Background()); err != nil {
		t.Fatalf("release append lock: %v", err)
	}
	waitForPersistedUpdates(t, service, artifactID, 2)
	reloaded := New(Deps{Store: service.store, Events: events.NewBroker(), Settle: time.Hour})
	defer reloaded.Shutdown(context.Background())
	if got, err := reloaded.Text(context.Background(), artifactID); err != nil || got != "after\n" {
		t.Fatalf("text after delayed shutdown = %q (%v), want after", got, err)
	}
}
func TestAdversarialSettlementDoesNotMissAppendAfterClassConsume(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	entered := make(chan struct{})
	release := make(chan struct{})
	persistence := &blockingFirstAppendStore{
		VersionedStore: NewPgVersioned(database),
		entered:        entered,
		release:        release,
	}
	service := New(Deps{Store: database, Persistence: persistence, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if persistence.released.CompareAndSwap(false, true) {
			close(release)
		}
		_ = service.Shutdown(context.Background())
	})
	seedServiceText(t, service, artifactID, "before")
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	<-entered
	state := service.room(artifactID)
	state.mu.Lock()
	if state.settle == nil || !service.stopSettleTimer(state.settle) {
		state.mu.Unlock()
		t.Fatal("stop armed settlement")
	}
	generation := state.roomGeneration
	service.settle = 10 * time.Millisecond
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)
	if persistence.released.CompareAndSwap(false, true) {
		close(release)
	}
	waitForDocumentVersion(t, database, artifactID, 2)
	assertTableCellPipeVersionAndEventCounts(t, database, artifactID, 2, 1)
}

func TestShutdownContextDoesNotWaitForBlockedSettlement(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 5 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")

	blocker, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker transaction: %v", err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(context.Background(), `
		select 1 from issues where key = (select issue_key from artifacts where id = $1) for update
	`, artifactID); err != nil {
		t.Fatalf("lock document issue: %v", err)
	}
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	locked := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			locked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !locked {
		t.Fatal("settlement did not wait on the document lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- service.Shutdown(ctx) }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown waited on the blocked settlement")
	}
}

// A deploy stops the server on a fixed budget, and a large document's settlement can take longer
// than it: a 1 MiB `a_b*` document's took 4.5 s at load 90, and a shutdown running it took 8.4 s
// against its 5 s budget. Shutdown abandoned the settlement its budget cut short, and the
// settlement lived only in memory, so the restarted server armed none: the document's ask blocks
// stayed out of the open asks, and an edit's version unwritten, until someone edited it again. The
// settlement a shutdown leaves runs on the document's next load instead, and Shutdown names it.
func TestShutdownThatCutsASettlementShortLeavesItToTheNextLoad(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID,
		":::ask{#ask-block urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load live document: %v", err)
	}
	// The settlement Shutdown runs stops once it holds the document's locks, and stays stopped
	// past the shutdown budget, as a settlement that renders 1 MiB under load does.
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	service.afterSettleLock = func(string) {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := service.Shutdown(ctx)
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown past its budget = %v, want deadline exceeded", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("shutdown never ran the document's settlement")
	}
	if asks := indexedAsks(t, service, artifactID); asks != 0 {
		t.Fatalf("the settlement shutdown cut short indexed %d asks", asks)
	}

	restarted := New(Deps{Store: service.store, Events: events.NewBroker(), Settle: 10 * time.Millisecond})
	t.Cleanup(func() {
		if err := restarted.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown restarted document service: %v", err)
		}
	})
	if err := restarted.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document on the restarted server: %v", err)
	}
	deadline := time.Now().Add(recoveryBound)
	for indexedAsks(t, restarted, artifactID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the restarted server never ran the settlement the shutdown left: its ask block is unindexed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(err.Error(), artifactID) {
		t.Fatalf("shutdown error %q does not name the document whose settlement it left", err)
	}
}

// A document left owing a settlement settles without anyone opening it. On a rolling deploy the
// old server stops after the new one has started, so the new server's resumption, not a room
// load, is what reaches the settlement the old one's shutdown cut short. A row younger than the
// resumption's age belongs to a settlement its writer is about to run, and is left to it.
func TestResumptionSettlesAnOwedDocumentNobodyOpens(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID,
		":::ask{#ask-block urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n")
	if err := service.resumeOwedSettlements(context.Background(), time.Hour); err != nil {
		t.Fatalf("resume settlements owed for an hour: %v", err)
	}
	// Ten times the test service's settle delay.
	time.Sleep(200 * time.Millisecond)
	if asks := indexedAsks(t, service, artifactID); asks != 0 {
		t.Fatalf("the resumption settled a document owing one for less than its age: %d asks indexed", asks)
	}
	if err := service.resumeOwedSettlements(context.Background(), 0); err != nil {
		t.Fatalf("resume every owed settlement: %v", err)
	}
	deadline := time.Now().Add(recoveryBound)
	for indexedAsks(t, service, artifactID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the resumption never settled the owed document: its ask block is unindexed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIssueReopenRestoresLiveWrites(t *testing.T) {
	service, artifactID := newTestService(t)
	if got := service.events.SubscriberCount(); got != 0 {
		t.Fatalf("document service registered %d event subscriptions, want none", got)
	}
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed(context.Background(), "DOC-1", true)
	if _, err := joinedReplaceText(service, artifactID, "closed", model.Actor{Kind: "user", ID: "alice"}); !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("write to closed issue = %v, want ErrIssueClosed", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = null where key = 'DOC-1'`); err != nil {
		t.Fatalf("reopen document issue: %v", err)
	}
	service.SetIssueClosed(context.Background(), "DOC-1", false)
	service.events.Publish(model.Event{IssueKey: new("DOC-1"), Type: "issue.closed"})
	if got := service.events.SubscriberCount(); got != 0 {
		t.Fatalf("stale issue.closed event gained %d subscriptions, want none", got)
	}
	if _, err := joinedReplaceText(service, artifactID, "reopened", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write to reopened issue: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "reopened\n")
}

func TestCancelledTextLoadDoesNotQuarantineRoom(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), respectContext: true},
		Events:      events.NewBroker(),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Text(ctx, artifactID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled text load = %v, want context.Canceled", err)
	}
	if _, err := service.Text(context.Background(), artifactID); err != nil {
		t.Fatalf("text after cancelled load = %v, want room to remain available", err)
	}
}

func TestSettleCapturesAuthorsAtSnapshotTime(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	first := model.Actor{Kind: "user", ID: "alice"}
	second := model.Actor{Kind: "user", ID: "bob"}
	if _, err := joinedReplaceText(service, artifactID, "after", first); err != nil {
		t.Fatalf("replace document text: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	blocker, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker transaction: %v", err)
	}
	if _, err := blocker.Exec(context.Background(), `
		select 1 from issues where key = (select issue_key from artifacts where id = $1) for update
	`, artifactID); err != nil {
		t.Fatalf("lock document issue: %v", err)
	}
	settled := make(chan struct{})
	go func() {
		service.settleRoom(artifactID, generation)
		close(settled)
	}()
	waitForDatabaseLock(t, service.store)
	service.recordActor(t, artifactID, second)
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatalf("release document lock: %v", err)
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("settlement did not complete")
	}
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 2 || version.Authors[0] != first || version.Authors[1] != second {
		t.Fatalf("settled version authors = %#v, want %v and %v", version.Authors, first, second)
	}
}

func waitForDatabaseLock(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("settlement did not wait on the document lock")
}

// failingOnceVersionedStore fails the first load of the stored document, of either kind: a room's
// (Load) or a cold read's (LoadDocument).
type failingOnceVersionedStore struct {
	VersionedStore
	loads atomic.Int32
}

func (s *failingOnceVersionedStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if s.loads.Add(1) == 1 {
		return persistence.LoadResult{}, errors.New("transient load failure")
	}
	return s.VersionedStore.Load(ctx, room)
}

func (s *failingOnceVersionedStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	if s.loads.Add(1) == 1 {
		return LoadedDocument{}, errors.New("transient load failure")
	}
	return s.VersionedStore.LoadDocument(ctx, room)
}

// failingVersionedStore fails the store with loadErr, every read of it included, or loads every
// document as loadUpdate, one stored state at version 1, or fails appends with appendErr.
type failingVersionedStore struct {
	VersionedStore
	loadErr        error
	loadUpdate     []byte
	appendErr      error
	respectContext bool
}

func (s failingVersionedStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if s.respectContext && ctx.Err() != nil {
		return persistence.LoadResult{}, ctx.Err()
	}
	if s.loadErr != nil {
		return persistence.LoadResult{}, s.loadErr
	}
	if s.loadUpdate != nil {
		return persistence.LoadResult{Update: s.loadUpdate, Version: 1}, nil
	}
	return s.VersionedStore.Load(ctx, room)
}

func (s failingVersionedStore) DocumentStamp(ctx context.Context, room string) (DocumentStamp, error) {
	if s.respectContext && ctx.Err() != nil {
		return DocumentStamp{}, ctx.Err()
	}
	if s.loadErr != nil {
		return DocumentStamp{}, s.loadErr
	}
	if s.loadUpdate != nil {
		return DocumentStamp{Version: 1}, nil
	}
	return s.VersionedStore.DocumentStamp(ctx, room)
}

func (s failingVersionedStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	if s.respectContext && ctx.Err() != nil {
		return LoadedDocument{}, ctx.Err()
	}
	if s.loadErr != nil {
		return LoadedDocument{}, s.loadErr
	}
	if s.loadUpdate != nil {
		doc := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(doc, s.loadUpdate, nil); err != nil {
			return LoadedDocument{}, fmt.Errorf("%w: decode live document: %w", ErrDocumentUnloadable, err)
		}
		return LoadedDocument{Doc: doc, Stamp: DocumentStamp{Version: 1}}, nil
	}
	return s.VersionedStore.LoadDocument(ctx, room)
}

func (s failingVersionedStore) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	if s.appendErr != nil {
		return 0, s.appendErr
	}
	return s.VersionedStore.AppendUpdate(ctx, room, update)
}

func (s failingVersionedStore) AppendUpdateWithCredit(ctx context.Context, room string, update []byte, contentChanged bool, credit *UpdateCredit) (persistence.Version, error) {
	if s.appendErr != nil {
		return 0, s.appendErr
	}
	return s.VersionedStore.AppendUpdateWithCredit(ctx, room, update, contentChanged, credit)
}

type failingBackfillVersionedStore struct {
	VersionedStore
	err error
}

func (s failingBackfillVersionedStore) AppendUpdateTx(_ context.Context, _ pgx.Tx, _ string, _ []byte, _ bool) (persistence.Version, error) {
	return 0, s.err
}

type captureAppendUpdateTxStore struct {
	VersionedStore
	captured chan<- []byte
}

func (s captureAppendUpdateTxStore) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	s.captured <- append([]byte(nil), update...)
	return s.VersionedStore.AppendUpdateTx(ctx, tx, room, update, contentChanged)
}

type blockingFirstAppendStore struct {
	VersionedStore
	entered  chan<- struct{}
	release  <-chan struct{}
	blocked  atomic.Bool
	released atomic.Bool
}

func (s *blockingFirstAppendStore) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	if s.blocked.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.VersionedStore.AppendUpdate(ctx, room, update)
}

func (s *blockingFirstAppendStore) AppendUpdateWithCredit(ctx context.Context, room string, update []byte, contentChanged bool, credit *UpdateCredit) (persistence.Version, error) {
	if s.blocked.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.VersionedStore.AppendUpdateWithCredit(ctx, room, update, contentChanged, credit)
}

// room is the state the service holds for room now, created when it holds none, for a test to read
// or arrange under its lock.
func (s *Service) room(name string) *roomState {
	state := s.lockState(name)
	state.mu.Unlock()
	return state
}

// recordActor makes actor a pending author of room's next version, and its latest editor, as a
// landed browser edit does: a durable pending author, written under the document's advisory lock
// (AppendUpdateWithCredit), and the room's latest edit source.
func (s *Service) recordActor(t *testing.T, room string, actor model.Actor) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin recording %s: %v", actor.ID, err)
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		t.Fatalf("lock the document to record %s: %v", actor.ID, err)
	}
	if err := upsertPendingAuthors(ctx, tx, room, pendingAuthorsAt(map[string]model.Actor{actorKey(actor): actor}, 0)); err != nil {
		t.Fatalf("record %s as a pending author: %v", actor.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s as a pending author: %v", actor.ID, err)
	}
	state := s.lockState(room)
	// Every recorder of an edit source moves creditSeq (creditContentChange, Ledger.creditRooms),
	// which a room's load reads to tell a source recorded after its row read (editSourceMark).
	state.creditSeq.Add(1)
	state.lastActor = new(actor)
	state.unsettled = true
	s.unlockState(room, state)
}

func seedServiceText(t *testing.T, service *Service, artifactID, markdown string) {
	t.Helper()
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin seed text: %v", err)
	}
	defer tx.Rollback(context.Background())
	seedCtx, ledger := service.Join(context.Background(), tx)
	defer ledger.Discard()
	if _, err := service.SeedText(seedCtx, artifactID, markdown, model.Actor{Kind: "user", ID: "seed"}); err != nil {
		t.Fatalf("seed service text: %v", err)
	}
	if err := ledger.Commit(context.Background()); err != nil {
		t.Fatalf("commit seed text: %v", err)
	}
}

// joinedWrite runs write inside a transaction joined with Service.Join, as every caller of a
// document write does, and commits it when write succeeds. It returns write's own error, which
// tests assert on, or the transaction's, and never fails the test itself, so a goroutine may call
// it.
func joinedWrite(service *Service, write func(ctx context.Context) error) error {
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the write's transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if err := write(joined); err != nil {
		return err
	}
	return ledger.Commit(ctx)
}

func joinedReplaceText(service *Service, artifactID, markdown string, actor model.Actor) (string, error) {
	var canonical string
	err := joinedWrite(service, func(ctx context.Context) (err error) {
		canonical, err = service.ReplaceText(ctx, artifactID, markdown, actor)
		return err
	})
	return canonical, err
}

func joinedApplyOps(service *Service, artifactID string, ops []model.EditOp, actor model.Actor, precondition *model.EditPrecondition) (EditOutcome, error) {
	var outcome EditOutcome
	err := joinedWrite(service, func(ctx context.Context) (err error) {
		outcome, err = service.ApplyOps(ctx, artifactID, ops, actor, precondition)
		return err
	})
	return outcome, err
}

func joinedMarkQuote(service *Service, artifactID string, mark MarkSpec, quote string, occurrence *int) (Anchored, error) {
	var anchored Anchored
	err := joinedWrite(service, func(ctx context.Context) (err error) {
		anchored, err = service.MarkQuote(ctx, artifactID, mark, quote, occurrence)
		return err
	})
	return anchored, err
}

func joinedProjectMark(service *Service, artifactID, markID string, record MarkRecord, actor model.Actor) error {
	return joinedWrite(service, func(ctx context.Context) error {
		return service.ProjectMark(ctx, artifactID, markID, record, actor)
	})
}

func joinedAcceptSuggestion(service *Service, artifactID, id, replaceWith string, actor model.Actor) error {
	return joinedWrite(service, func(ctx context.Context) error {
		return service.AcceptSuggestion(ctx, artifactID, id, replaceWith, actor)
	})
}

func joinedRejectSuggestion(service *Service, artifactID, id string, actor model.Actor) error {
	return joinedWrite(service, func(ctx context.Context) error {
		return service.RejectSuggestion(ctx, artifactID, id, actor)
	})
}

func joinedSetBlockAttributes(service *Service, artifactID, blockID string, attributes map[string]any, actor model.Actor) error {
	return joinedWrite(service, func(ctx context.Context) error {
		return service.SetBlockAttributes(ctx, artifactID, blockID, attributes, actor)
	})
}

// namedVersion names a version the way every caller does: inside a transaction joined with
// Service.Join, whose commit credits the version's authors and publishes its events. It
// returns NamedVersion's own error, which several tests assert on, and commits only without
// one.
func namedVersion(t *testing.T, service *Service, artifactID, summary string, actor model.Actor) (VersionResult, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin named version: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	result, err := service.NamedVersion(joined, artifactID, summary, actor)
	if err != nil {
		return VersionResult{}, err
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit named version: %v", err)
	}
	return result, nil
}

// alignLatestVersionWithUpdates makes the latest version hold every update the document has, as a
// seeded document's first version does outside these tests (SeedText's caller writes it). Without
// it the text seedServiceText wrote is a change no version holds, and the settlement the room's
// first load arms for the seed versions it on its own whenever it runs before the test's first
// browser edit is credited: a version none of that edit's authors are listed on.
func alignLatestVersionWithUpdates(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions
		set doc_update_version = coalesce((select max(version) from doc_updates where artifact_id = $1), 0)
		where artifact_id = $1 and number = (select max(number) from artifact_versions where artifact_id = $1)
	`, artifactID); err != nil {
		t.Fatalf("align seeded document update with version: %v", err)
	}
}

func seedUnidentifiedProofDocument(t *testing.T, database *store.Store, artifactID, markdown string) {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse unidentified document: %v", err)
	}
	removeBlockIDs(tree)
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.Transact(func(txn *crdt.Transaction) {
		if err := pmdoc.Update(txn, fragment, tree); err != nil {
			t.Errorf("write unidentified document: %v", err)
		}
	})
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin unidentified document: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := NewPgVersioned(database).AppendUpdateTx(context.Background(), tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil), true); err != nil {
		t.Fatalf("append unidentified document: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit unidentified document: %v", err)
	}
}

func removeBlockIDs(node *pmdoc.Node) {
	delete(node.Attrs, pmdoc.BlockIDAttr)
	for _, child := range node.Children {
		removeBlockIDs(child)
	}
}

func persistedProofTree(t *testing.T, database *store.Store, artifactID string) *pmdoc.Node {
	t.Helper()
	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load persisted document: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode persisted document: %v", err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read persisted document: %v", err)
	}
	return tree
}

func waitForPersistedProofText(t *testing.T, database *store.Store, artifactID, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		markdown, err := renderTree(persistedProofTree(t, database, artifactID))
		if err == nil && markdown == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	markdown, err := renderTree(persistedProofTree(t, database, artifactID))
	t.Fatalf("persisted document = %q (%v), want %q", markdown, err, want)
}

// editLiveTree writes a browser-style tree change through the live Yjs room.
func editLiveTree(t *testing.T, service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := pmdoc.Read(fragment)
		if err != nil {
			t.Fatalf("read live tree: %v", err)
		}
		want := edit(tree)
		transact(func(txn *crdt.Transaction) {
			if err := pmdoc.Update(txn, fragment, want); err != nil {
				t.Errorf("update live tree: %v", err)
			}
		})
	})
	if err != nil {
		t.Fatalf("apply browser edit: %v", err)
	}
}

// browserReplaceText writes markdown as the whole live document the way a browser peer's edit
// reaches the room - straight into it, outside any transaction, persisted by the room's own
// observer - from a peer connected as actor, whom the room credits with it (creditContentChange).
// It is the only write that can land while another transaction holds the document: every server
// write joins a transaction, and waits for that one's owner row.
func browserReplaceText(t *testing.T, service *Service, artifactID, markdown string, actor model.Actor) {
	t.Helper()
	written, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the browser's document: %v", err)
	}
	connection := service.nextConnection.Add(1)
	service.addConnection(artifactID, connection, actor)
	defer service.removeConnection(artifactID, connection)
	editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node { return written })
}

// liveTree reads the resident room as of one moment under its document lock (Service.liveTree),
// inside the Apply that loads and holds the room.
func liveTree(t *testing.T, service *Service, artifactID string) *pmdoc.Node {
	t.Helper()
	var tree *pmdoc.Node
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		var readErr error
		tree, readErr = service.liveTree(artifactID, doc)
		if readErr != nil {
			t.Errorf("read live tree: %v", readErr)
		}
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("read live document: %v", err)
	}
	return tree
}

// replaceRun rewrites the text of the first run containing find, keeping its marks.
func replaceRun(find, with string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		var visit func(*pmdoc.Node) bool
		visit = func(node *pmdoc.Node) bool {
			if node.Type == "text" && strings.Contains(node.Text, find) {
				node.Text = strings.Replace(node.Text, find, with, 1)
				return true
			}
			for _, child := range node.Children {
				if visit(child) {
					return true
				}
			}
			return false
		}
		visit(tree)
		return tree
	}
}

// deleteRun removes the first text run with exactly text, preserving ProseMirror's no-empty-text invariant.
func deleteRun(text string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		var visit func(*pmdoc.Node) bool
		visit = func(node *pmdoc.Node) bool {
			for index, child := range node.Children {
				if child.Type == "text" && child.Text == text {
					node.Children = append(node.Children[:index], node.Children[index+1:]...)
					return true
				}
				if visit(child) {
					return true
				}
			}
			return false
		}
		visit(tree)
		return tree
	}
}

// waitForRoomFailure returns once the room has failed. failRoom evicts the failed room from a
// goroutine that also removes its state, so a poll that first runs after that eviction sees no
// failure; the completed eviction of a room that was live is the same observation.
func waitForRoomFailure(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if service.roomFailure(artifactID) != nil {
			return
		}
		if _, resident := service.rooms.Load(artifactID); !resident && service.srv.GetDoc(artifactID) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document room did not become unavailable after append failure")
}

func waitForNoLiveDocument(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if service.srv.GetDoc(artifactID) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document room remained resident after closure")
}
func waitForDocumentText(t *testing.T, service *Service, artifactID, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, err := service.Text(context.Background(), artifactID)
		if err == nil && got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, err := service.Text(context.Background(), artifactID)
	t.Fatalf("document text = %q (%v), want %q", got, err, want)
}

func waitForDocumentVersion(t *testing.T, database *store.Store, artifactID string, number int) model.Version {
	t.Helper()
	// A ceiling, not a duration: a settlement that needs a room reload first (a test that evicted
	// the room) takes longer than one that only writes, and a loaded CI runner stretches both.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		version, ok, err := loadDocumentVersion(context.Background(), database, artifactID, number)
		if err != nil {
			t.Fatalf("load document version: %v", err)
		}
		if ok {
			return version
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document version did not settle")
	return model.Version{}
}

func loadDocumentVersion(ctx context.Context, database *store.Store, artifactID string, number int) (model.Version, bool, error) {
	var version model.Version
	var authors []byte
	err := database.Pool.QueryRow(ctx, `
		select number, named, summary, authors, created_at
		from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Version{}, false, nil
	}
	if err != nil {
		return model.Version{}, false, err
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, false, err
	}
	return version, true, nil
}

func TestTreeOfRefusesAnUnloadedDocument(t *testing.T) {
	if _, err := treeOf(nil); !errors.Is(err, errDocUnloaded) {
		t.Fatalf("treeOf(nil) error = %v, want errDocUnloaded", err)
	}
}

// A room evicted after settleRoom's generation check but before it reads the live document
// (an Evict whose timer Stop misses the timer that already fired) must not make the settle
// dereference a nil document and crash the server. The settle ends quietly and the next
// write to the room settles on its own.
func TestSettleSurvivesEvictionBetweenWarmAndTreeRead(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	var evicted atomic.Int32
	service.afterSettleWarm = func(room string) {
		if room != artifactID || !evicted.CompareAndSwap(0, 1) {
			return
		}
		if err := service.Evict(ctx, room); err != nil {
			t.Errorf("evict mid-settle: %v", err)
		}
	}
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	deadline := time.Now().Add(3 * time.Second)
	for evicted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if evicted.Load() == 0 {
		t.Fatal("settlement never reached the hook")
	}
	// The hook is one-shot through the CAS above; leaving it in place avoids racing the settle
	// goroutine that reads it.
	// The live edit was already persisted incrementally, so the reloaded text carries it; the
	// room is not failed, so a new write still settles normally.
	waitForNoLiveDocument(t, service, artifactID)
	if got, err := service.Text(ctx, artifactID); err != nil || got != "after\n" {
		t.Fatalf("text after mid-settle eviction = %q (%v), want the persisted live text", got, err)
	}
	editLiveTree(t, service, artifactID, replaceRun("after", "later"))
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	var markdown string
	if err := service.store.Pool.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = $2`, artifactID, version.Number).Scan(&markdown); err != nil {
		t.Fatal(err)
	}
	if markdown != "later\n" {
		t.Fatalf("version %d markdown = %q, want %q", version.Number, markdown, "later\n")
	}
}

// A read that loads its room and then reads it takes what it reads from the room it holds: a
// version's capture (POST /versions, captureLiveTextAndAuthors) and a mark's verification
// (VerifyMark) read inside the Apply that holds the room, so an eviction once the room is warm
// - the last peer leaving, a settlement's forced eviction - does not leave them a room that is no
// longer there, which they answered `warm live document did not retain room`, a 500.
func TestAWarmedReadSurvivesItsRoomsEviction(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	for _, test := range []struct {
		name string
		read func(t *testing.T, service *Service, artifactID string)
	}{
		{"a version's capture", func(t *testing.T, service *Service, artifactID string) {
			ctx := context.Background()
			tx, err := service.store.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			result, err := service.NamedVersion(joined, artifactID, "named", alice)
			if err != nil {
				t.Fatalf("name a version of a room evicted once warm: %v", err)
			}
			var markdown string
			if err := tx.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = $2`, artifactID, result.Version.Number).Scan(&markdown); err != nil {
				t.Fatal(err)
			}
			if markdown != "The quick brown fox\n" {
				t.Fatalf("named version markdown = %q, want the document's text", markdown)
			}
		}},
		{"a mark's verification", func(t *testing.T, service *Service, artifactID string) {
			anchored, err := service.VerifyMark(context.Background(), artifactID, MarkComment, "b1")
			if err != nil || anchored.Quote != "brown" {
				t.Fatalf("verify a mark in a room evicted once warm = %q, %v, want brown", anchored.Quote, err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "The quick brown fox")
			browserMark(t, service, artifactID, "proofComment", "b1", "brown")
			var evicted atomic.Int32
			service.afterReadWarm = func(room string) {
				if room != artifactID || !evicted.CompareAndSwap(0, 1) {
					return
				}
				if err := service.Evict(context.Background(), room); err != nil {
					t.Errorf("evict the warm room: %v", err)
				}
			}
			test.read(t, service, artifactID)
			if evicted.Load() == 0 {
				t.Fatal("the read never reached the hook")
			}
		})
	}
}

func TestEditedLegacyTableCellPipeDocumentSettlesOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "| header |\n| :--- |\n| `one\\|two` |\n")
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1
	`, artifactID, "| header |\n| :--- |\n| `one|two` |\n"); err != nil {
		t.Fatalf("seed legacy canonical markdown: %v", err)
	}
	const edited = "| header |\n| :--- |\n| `one\\|three` |\n"
	if got, err := joinedReplaceText(service, artifactID, edited, model.Actor{Kind: "user", ID: "alice"}); err != nil || got != edited {
		t.Fatalf("edit legacy document = %q (%v), want %q", got, err, edited)
	}
	waitForPersistedProofText(t, service.store, artifactID, edited)

	settleCurrentGeneration(t, service, artifactID)
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 2, 1)
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != edited {
		t.Fatalf("settled edited text = %q (%v), want %q", got, err, edited)
	}

	settleCurrentGeneration(t, service, artifactID)
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 2, 1)
}

func TestDurableContentEditSurvivesEvictionThenSettles(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if got, err := joinedReplaceText(service, artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil || got != "after\n" {
		t.Fatalf("replace text = %q (%v), want after", got, err)
	}
	waitForPersistedProofText(t, service.store, artifactID, "after\n")
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict edited document: %v", err)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "after\n" {
		t.Fatalf("reloaded text = %q (%v), want after", got, err)
	}
	settleCurrentGeneration(t, service, artifactID)
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 2, 1)
}

func TestOpeningLegacyTableCellPipeDocumentDoesNotSettle(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "| header |\n| :--- |\n| `one\\|two` |\n")
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1
	`, artifactID, "| header |\n| :--- |\n| `one|two` |\n"); err != nil {
		t.Fatalf("seed legacy canonical markdown: %v", err)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "| header |\n| :--- |\n| `one\\|two` |\n" {
		t.Fatalf("open legacy document = %q (%v)", got, err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.settle != nil {
		t.Fatal("opening legacy document scheduled settlement")
	}
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 1, 0)
}

func TestProjectMarkSettlesLegacyTableWithoutCanonicalizing(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "| header |\n| :--- |\n| `one\\|two` |\n")
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1
	`, artifactID, "| header |\n| :--- |\n| `one|two` |\n"); err != nil {
		t.Fatalf("seed legacy canonical markdown: %v", err)
	}
	alignLatestVersionWithUpdates(t, service, artifactID)
	if err := joinedProjectMark(service, artifactID, "mark-1", MarkRecord{
		Kind: "comment", By: "alice", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Text: "note",
	}, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("project mark: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 1, 0)
}

func TestQuoteMarkSettlesLegacyTableWithoutCanonicalizing(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.unrecordedMarkTTL = 10 * time.Millisecond
	seedServiceText(t, service, artifactID, "| header |\n| :--- |\n| `one\\|two` |\n")
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1
	`, artifactID, "| header |\n| :--- |\n| `one|two` |\n"); err != nil {
		t.Fatalf("seed legacy canonical markdown: %v", err)
	}
	alignLatestVersionWithUpdates(t, service, artifactID)
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkComment, ID: "mark-1", By: model.Actor{Kind: "user", ID: "alice"},
	}, "one|two", nil); err != nil {
		t.Fatalf("mark quote: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	waitFor(t, time.Second, "quote mark removed", func() bool {
		_, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "mark-1")
		return !found
	})
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 1, 0)
}
func assertTableCellPipeVersionAndEventCounts(t *testing.T, database *store.Store, artifactID string, wantVersions, wantEvents int) {
	t.Helper()
	var versions, events int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count document versions: %v", err)
	}
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = 'artifact.version' and payload->>'artifact_id' = $1
	`, artifactID).Scan(&events); err != nil {
		t.Fatalf("count document version events: %v", err)
	}
	if versions != wantVersions || events != wantEvents {
		t.Fatalf("document counts = versions:%d events:%d, want versions:%d events:%d", versions, events, wantVersions, wantEvents)
	}
}

// Quiesce is what lets the browser-test harness truncate its database between scenarios: it
// leaves the server with no live document and no settlement of its own to run. It has to flush
// each room before it closes it, or an edit that had only reached the room would be lost, and
// it has to leave the service able to load the document again afterwards. The record of a room it
// closes goes with the settlement it cancels, unless a browser that joined while it ran still
// holds it (evictRoom).
func TestQuiesceFlushesEveryLiveDocumentAndLeavesTheServiceUsable(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))

	if err := service.Quiesce(context.Background()); err != nil {
		t.Fatalf("quiesce document service: %v", err)
	}
	if _, live := service.rooms.Load(artifactID); live {
		t.Fatal("a document room outlived the quiesce")
	}

	// The room is gone, so this text is what the store holds: the edit reached it.
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read document after quiesce: %v", err)
	}
	if !strings.Contains(text, "after") {
		t.Fatalf("document text after quiesce = %q, want the edit", text)
	}

	// And the service schedules its own settlements again: an edit after the quiesce arms the
	// room's timer. A quiesce that never lifted its hold would leave that timer unarmed and
	// every later edit unsettled, which settling this room by hand below cannot show.
	editLiveTree(t, service, artifactID, replaceRun("after", "later"))
	state := service.room(artifactID)
	state.mu.Lock()
	armed := service.isSettleTimerArmed(state.settle)
	state.mu.Unlock()
	if !armed {
		t.Fatal("no settlement was scheduled for the edit after the quiesce")
	}
	settleCurrentGeneration(t, service, artifactID)
	waitForDocumentVersion(t, service.store, artifactID, 2)
}

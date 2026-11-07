package texttrees_test

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// A text's tree is stored once and read back as the bytes pmdoc.DisplayJSON answers, which is what
// a response carries: a read serves the stored bytes rather than reading the markdown again.
func TestAStoredTreeIsReadBackAsTheTreeTheTextIsShownAs(t *testing.T) {
	ctx := context.Background()
	table := texttrees.New(storetest.Open(t).Pool)
	const body = "Check **this** before the deploy, in [staging](https://example.test)."
	const question = "Ship the ~~draft~~ release today?"
	if err := table.Store(ctx, pmdoc.DisplayLatest, body, question); err != nil {
		t.Fatalf("store two texts: %v", err)
	}
	stored, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{texttrees.Sum(body), texttrees.Sum(question)})
	if err != nil {
		t.Fatalf("read the two trees: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("the read answered %d rows, want the two stored", len(stored))
	}
	for _, text := range []string{body, question} {
		want := pmdoc.DisplayJSON(text, pmdoc.DisplayLatest)
		if got := stored[texttrees.Sum(text)]; !bytes.Equal(got, want) {
			t.Errorf("the stored tree of %q is\n\t%s\nwant\n\t%s", text, got, want)
		}
	}
	// A sum nothing stored is simply absent, which is how its caller knows to read the text.
	unstored, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{texttrees.Sum("Nothing stored this.")})
	if err != nil {
		t.Fatalf("read an unstored sum: %v", err)
	}
	if len(unstored) != 0 {
		t.Errorf("the read answered %d rows for a text nothing stored, want none", len(unstored))
	}
}

// A text stored again keeps the row it has: the key is the reading, so a second store of the same
// text writes nothing and refuses nothing.
func TestStoringATextAgainKeepsTheRowItHas(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	table := texttrees.New(database.Pool)
	const body = "The *staging* run is green."
	if err := table.Store(ctx, pmdoc.DisplayLatest, body); err != nil {
		t.Fatalf("store the text: %v", err)
	}
	// The sum is handed to a query of the test's own as the bytes the column holds: pgx encodes a
	// byte slice as bytea and knows no plan for the array Sum answers.
	sum := texttrees.Sum(body)
	var created string
	if err := database.Pool.QueryRow(ctx,
		`select created_at::text from text_trees where sum = $1`, sum[:]).Scan(&created); err != nil {
		t.Fatalf("read the row's time: %v", err)
	}
	// The same text twice in one batch, and then a batch of its own: neither writes a second row.
	if err := table.Store(ctx, pmdoc.DisplayLatest, body, body); err != nil {
		t.Fatalf("store the text twice over: %v", err)
	}
	if err := table.Store(ctx, pmdoc.DisplayLatest, body); err != nil {
		t.Fatalf("store the text again: %v", err)
	}
	var rows int
	var stillCreated string
	if err := database.Pool.QueryRow(ctx,
		`select count(*), min(created_at)::text from text_trees where sum = $1`, sum[:]).Scan(&rows, &stillCreated); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("the table holds %d rows for one text, want 1", rows)
	}
	if stillCreated != created {
		t.Errorf("the row was written again at %s, want the %s it had", stillCreated, created)
	}
}

// The two display modes of one text are two trees (pmdoc.ReadForDisplay), so they are two rows,
// and a read in one mode never answers the other's.
func TestTheTwoModesOfOneTextAreTwoRows(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	table := texttrees.New(database.Pool)
	const text = ":::ask{state=\"answered\"}\nIs it ready?\n:::\n"
	if err := table.Store(ctx, pmdoc.DisplayLatest, text); err != nil {
		t.Fatalf("store the latest reading: %v", err)
	}
	if err := table.Store(ctx, pmdoc.DisplayVersion, text); err != nil {
		t.Fatalf("store the version reading: %v", err)
	}
	sum := texttrees.Sum(text)
	var rows int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from text_trees where sum = $1`, sum[:]).Scan(&rows); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("the table holds %d rows for one text in two modes, want 2", rows)
	}
	for _, mode := range []pmdoc.DisplayMode{pmdoc.DisplayLatest, pmdoc.DisplayVersion} {
		stored, err := table.Read(ctx, mode, [][32]byte{texttrees.Sum(text)})
		if err != nil {
			t.Fatalf("read mode %d: %v", mode, err)
		}
		want := pmdoc.DisplayJSON(text, mode)
		if got := stored[texttrees.Sum(text)]; !bytes.Equal(got, want) {
			t.Errorf("mode %d answered\n\t%s\nwant\n\t%s", mode, got, want)
		}
	}
}

// A tree stored at one display parser version is not read at another: a build whose reading of a
// text has changed reads every text again rather than serving a tree the build before it made.
func TestATreeStoredAtOneVersionIsNotReadAtAnother(t *testing.T) {
	ctx := context.Background()
	table := texttrees.New(storetest.Open(t).Pool)
	const text = "A tree the version before this one stored."
	if err := table.Store(ctx, pmdoc.DisplayLatest, text); err != nil {
		t.Fatalf("store at this version: %v", err)
	}
	restore := pmdoc.SetDisplayTreeVersionForTest(pmdoc.DisplayTreeVersion + 1)
	defer restore()
	stored, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{texttrees.Sum(text)})
	if err != nil {
		t.Fatalf("read at the next version: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("the read at version %d answered the tree stored at %d",
			pmdoc.DisplayTreeVersionNow(), pmdoc.DisplayTreeVersion)
	}
	// The next version stores its own reading beside the one before it, which stays where it is.
	if err := table.Store(ctx, pmdoc.DisplayLatest, text); err != nil {
		t.Fatalf("store at the next version: %v", err)
	}
	if stored, err = table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{texttrees.Sum(text)}); err != nil {
		t.Fatalf("read the next version's row: %v", err)
	}
	if len(stored) != 1 {
		t.Errorf("the read at version %d answered %d rows, want its own", pmdoc.DisplayTreeVersionNow(), len(stored))
	}
	restore()
	if stored, err = table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{texttrees.Sum(text)}); err != nil {
		t.Fatalf("read this version's row again: %v", err)
	}
	if len(stored) != 1 {
		t.Errorf("the row stored at version %d is gone after the next version stored its own", pmdoc.DisplayTreeVersion)
	}
}

// An empty text has no tree and no row: a write that stored nothing leaves the table as it was.
func TestAnEmptyTextStoresNothing(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	table := texttrees.New(database.Pool)
	if err := table.Store(ctx, pmdoc.DisplayLatest, ""); err != nil {
		t.Fatalf("store an empty text: %v", err)
	}
	if err := table.Store(ctx, pmdoc.DisplayLatest); err != nil {
		t.Fatalf("store no text at all: %v", err)
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from text_trees`).Scan(&rows); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("the table holds %d rows after storing an empty text, want none", rows)
	}
}

// Every method of a table with no pool behind it answers an error and none of them panics: storing
// a tree is never allowed to fail the write it belongs to, and a server with no database refuses
// that write itself.
func TestATableWithNoPoolAnswersAnError(t *testing.T) {
	ctx := context.Background()
	table := texttrees.New(nil)
	if err := table.Store(ctx, pmdoc.DisplayLatest, "A text."); err == nil {
		t.Error("the store on a table with no pool answered no error")
	}
	if _, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{texttrees.Sum("A text.")}); err == nil {
		t.Error("the read on a table with no pool answered no error")
	}
}

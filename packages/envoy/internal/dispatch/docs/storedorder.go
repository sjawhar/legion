package docs

import (
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
)

// The stored-order check: whether a document's stored history replays. Folding its stored updates
// in version order must give, at every update, the class that update stores, and at every
// version, the markdown that version stores. Its rules:
//
//  1. Classes. An update's content_changed is measured against the state its stored prefix folds
//     to: true when either side does not render, otherwise whether the rendered markdown differs
//     (the measure updateChangesMarkdown applies to a live update).
//  2. Versions. A version's markdown is the render of the fold through its doc_update_version.
//  3. Pending. No update is stored ahead of one it depends on: a row that adds parked structs or
//     deletions is a mismatch unless a later row carries what it parked again, which the commit
//     that integrates a peer's stored parked edit does (storeParked, judgeParkedRows). A row
//     still parked when the history ends is counted as awaiting its predecessor, not reported:
//     the store cannot tell an edit whose predecessor has not arrived from one whose dependency
//     was never stored.
//
// The fold starts from the document's oldest retained update, its base. Compaction folds older
// history into that row (Compact) and a rebuild replaces the whole history with one row there
// (RebuildTx), so the base is a state and its own class is not checked. Compaction runs on its
// own, under no setting a check can hold off: the per-process queue folds a room's rows once
// compactMinRows of them are older than compactTail or one is older than compactMaxTailAge, and
// the sweep folds every document due that rule once an hour. So a version created inside the
// checked window can record an update already folded into the base, and the check compares
// nothing of it: it counts those versions (VersionsCompacted) rather than passing over them, and
// a document they leave with no version compared is unchecked. A history rewritten after the
// base - a prune's checkpoint standing, or a prune that finished and was appended past
// (PruneAfter, recoverPruneTx) - is refused: the versions recorded against it name updates the
// store no longer holds.

// StoredOrderDB is what the check reads a document through: a pool it opens one read-only
// transaction on (*store.Pool, *pgxpool.Pool).
type StoredOrderDB interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// StoredOrderMismatchKind names which rule a stored row breaks.
type StoredOrderMismatchKind string

const (
	// StoredOrderClass is an update whose stored content_changed is not the class its stored
	// prefix measures.
	StoredOrderClass StoredOrderMismatchKind = "class"
	// StoredOrderVersion is a version whose stored markdown is not the render of the fold through
	// its doc_update_version.
	StoredOrderVersion StoredOrderMismatchKind = "version"
	// StoredOrderPending is an update that adds parked content, stored ahead of an update it
	// depends on that no later row carries again: a misordered store (judgeParkedRows).
	StoredOrderPending StoredOrderMismatchKind = "pending"
)

// StoredOrderMismatch is one stored row the fold disagrees with. It carries no document text:
// a version's markdown is named by its md5 and byte length on each side.
type StoredOrderMismatch struct {
	Kind StoredOrderMismatchKind
	// Update is the doc_updates version of a class or pending mismatch.
	Update int64
	// StoredClass and ExpectedClass are a class mismatch's stored content_changed and the class
	// the fold measures.
	StoredClass, ExpectedClass bool
	// ParkedItems and ParkedDeleteRanges are what the fold holds parked after a pending mismatch's
	// update.
	ParkedItems, ParkedDeleteRanges int
	// Version and DocUpdateVersion name a version mismatch: the artifact_versions number and the
	// update it records.
	Version          int
	DocUpdateVersion int64
	// StoredMD5 and StoredLength are the version's markdown; FoldMD5 and FoldLength the fold's
	// render through DocUpdateVersion, empty and zero when FoldRenders is false.
	StoredMD5    string
	StoredLength int
	FoldMD5      string
	FoldLength   int
	FoldRenders  bool
}

// String is the mismatch on one line, as `envoy-dispatch stored-order` prints it: numbers, classes,
// hashes and lengths, never document text.
func (m StoredOrderMismatch) String() string {
	switch m.Kind {
	case StoredOrderClass:
		return fmt.Sprintf("class update=%d stored=%t expected=%t", m.Update, m.StoredClass, m.ExpectedClass)
	case StoredOrderPending:
		return fmt.Sprintf("pending update=%d parked_items=%d parked_delete_ranges=%d", m.Update, m.ParkedItems, m.ParkedDeleteRanges)
	default:
		fold := fmt.Sprintf("fold_md5=%s fold_length=%d", m.FoldMD5, m.FoldLength)
		if !m.FoldRenders {
			fold = "fold_renders=false"
		}
		return fmt.Sprintf("version number=%d doc_update_version=%d stored_md5=%s stored_length=%d %s",
			m.Version, m.DocUpdateVersion, m.StoredMD5, m.StoredLength, fold)
	}
}

// StoredOrderReport is the check of one document.
type StoredOrderReport struct {
	ArtifactID string
	// Base is the version of the oldest retained update, the state the fold starts from; zero for
	// a document with no stored update.
	Base int64
	// UpdatesChecked counts the updates after the base whose class was checked; UpdatesSkipped
	// those folded unchecked because they were stored before the check's since time.
	UpdatesChecked, UpdatesSkipped int
	// VersionsChecked counts the versions compared with the fold; VersionsSkipped those created
	// before the since time and those recording no stored update at all - a document's first,
	// written before any row of it was stored, which no fold ever reaches - and
	// VersionsCompacted those created inside the window recording an update below the base,
	// which a compaction folded into it: nothing of those was compared either.
	VersionsChecked, VersionsSkipped, VersionsCompacted int
	// UpdatesAwaitingPredecessor counts the updates after which the fold parks structs no later
	// row resolves: a peer's edit the room stored ahead of an edit it builds on that has not
	// arrived (storeParked), or a row whose dependency was never stored, which the store alone
	// cannot tell apart. A dependency the room held and the store lost also leaves every version
	// recorded after it rendering text the fold lacks, which the version rule reports.
	UpdatesAwaitingPredecessor int
	// Refused says why the document's history cannot be checked; a refused document never passes.
	Refused    string
	Mismatches []StoredOrderMismatch
}

// Clean reports a document whose stored history replays: something checked, and no row
// disagreeing. A check that compared no update and no version proves nothing about the document.
func (r StoredOrderReport) Clean() bool {
	return r.Checked() && r.Refused == "" && len(r.Mismatches) == 0
}

// Checked reports whether the check compared at least one update or version with the fold. A
// document whose versions inside the window all record updates below the base is not checked,
// whatever classes the fold measured: the check read no version against the history it folded.
func (r StoredOrderReport) Checked() bool {
	if r.VersionsCompacted > 0 && r.VersionsChecked == 0 {
		return false
	}
	return r.UpdatesChecked > 0 || r.VersionsChecked > 0
}

// storedVersion is one artifact_versions row as the check reads it: its markdown by md5 and length.
type storedVersion struct {
	number           int
	docUpdateVersion int64
	createdAt        time.Time
	md5              string
	length           int
}

// CheckStoredOrder checks artifactID's stored history in one repeatable-read, read-only
// transaction (see the rules above). Updates and versions created before since are folded or
// passed over unchecked, so a history an older build wrote is not read as this build's; a zero
// since checks everything after the base. The error is a failure to read the history; what the
// history itself breaks is in the report.
func CheckStoredOrder(ctx context.Context, db StoredOrderDB, artifactID string, since time.Time) (StoredOrderReport, error) {
	report := StoredOrderReport{ArtifactID: artifactID}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, fmt.Errorf("begin stored-order check: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := checkStoredOrderTx(ctx, tx, artifactID, since, &report); err != nil {
		return report, err
	}
	if err := tx.Commit(ctx); err != nil {
		return report, fmt.Errorf("commit stored-order check: %w", err)
	}
	return report, nil
}

func checkStoredOrderTx(ctx context.Context, tx pgx.Tx, artifactID string, since time.Time, report *StoredOrderReport) error {
	var ceiling int64
	switch err := tx.QueryRow(ctx, `select ceiling from doc_checkpoints where artifact_id = $1`, artifactID).Scan(&ceiling); {
	case err == nil:
		report.Refused = fmt.Sprintf("a prune checkpoint stands at update %d: the history above it is being rewritten", ceiling)
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("read the document's prune checkpoint: %w", err)
	}
	var first, last, count int64
	if err := tx.QueryRow(ctx, `
		select coalesce(min(version), 0), coalesce(max(version), 0), count(*) from doc_updates where artifact_id = $1
	`, artifactID).Scan(&first, &last, &count); err != nil {
		return fmt.Errorf("read the document's stored updates: %w", err)
	}
	report.Base = first
	versions, err := readStoredVersions(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	if refusal := rewrittenHistory(versions, first, last, count); refusal != "" {
		report.Refused = refusal
		return nil
	}
	// The versions each update is compared against, by the update they record. A version created
	// before since is skipped, and so is one that records no update: the document's first, held
	// before any row of it was stored, names a state no fold starts from. A version created
	// inside the window that records an update below the base is counted as compacted, since a
	// compaction folded that row into the base and the fold no longer reaches it. A document
	// with no stored update holds no fold at all, so every version of it is skipped.
	at := make(map[int64][]storedVersion)
	for _, version := range versions {
		switch {
		case count == 0 || version.createdAt.Before(since) || version.docUpdateVersion == 0:
			report.VersionsSkipped++
		case version.docUpdateVersion < first:
			report.VersionsCompacted++
		default:
			at[version.docUpdateVersion] = append(at[version.docUpdateVersion], version)
		}
	}
	if count == 0 {
		return nil
	}
	return foldStoredOrder(ctx, tx, artifactID, since, at, report)
}

// readStoredVersions reads the document's versions in number order, each markdown as Postgres
// hashes and measures it, so no document text leaves the database.
func readStoredVersions(ctx context.Context, tx pgx.Tx, artifactID string) ([]storedVersion, error) {
	rows, err := tx.Query(ctx, `
		select number, doc_update_version, created_at, md5(coalesce(markdown, '')), octet_length(coalesce(markdown, ''))
		from artifact_versions where artifact_id = $1 order by number
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the document's versions: %w", err)
	}
	defer rows.Close()
	var versions []storedVersion
	for rows.Next() {
		var version storedVersion
		if err := rows.Scan(&version.number, &version.docUpdateVersion, &version.createdAt, &version.md5, &version.length); err != nil {
			return nil, fmt.Errorf("scan a document version: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the document's versions: %w", err)
	}
	return versions, nil
}

// rewrittenHistory names how the stored history was rewritten after its base, or is empty. Every
// writer appends at the head plus one and only compaction (below the base) and a rebuild (all of
// it) delete, so the retained updates run without a gap, and a version's doc_update_version never
// falls below an earlier version's. A prune breaks both: it deletes the updates above its target,
// so a version recorded before it names an update past the head, and the appends after it reuse
// those numbers, so a later version records a lower update than an earlier one.
func rewrittenHistory(versions []storedVersion, first, last, count int64) string {
	if count > 0 && last-first+1 != count {
		return fmt.Sprintf("updates are missing between %d and %d: the history was pruned after its base", first, last)
	}
	var highest storedVersion
	for _, version := range versions {
		if version.docUpdateVersion > last {
			return fmt.Sprintf("version %d records update %d, past the stored head %d: the history was pruned after it", version.number, version.docUpdateVersion, last)
		}
		if count > 0 && highest.docUpdateVersion >= first && version.docUpdateVersion < highest.docUpdateVersion {
			return fmt.Sprintf("version %d records update %d, before version %d's update %d: the history was pruned and rewritten after it",
				version.number, version.docUpdateVersion, highest.number, highest.docUpdateVersion)
		}
		if version.docUpdateVersion >= highest.docUpdateVersion {
			highest = version
		}
	}
	return ""
}

// storedRender is what the fold renders at one update: its markdown, or that it does not render.
type storedRender struct {
	markdown string
	renders  bool
}

func renderStoredFold(doc *crdt.Doc) storedRender {
	markdown, err := renderFragmentMarkdown(doc.GetXmlFragment(fragmentName))
	return storedRender{markdown: markdown, renders: err == nil}
}

// foldStoredOrder folds the document's updates in version order into one document, reading one row
// at a time, and checks each update after the base and each version against the fold. Each checked
// row that adds parked content is tracked until its own inserted and deleted clocks integrate,
// independently of unrelated parked content. Resolved rows are judged individually
// (judgeParkedRows); unresolved rows are counted as awaiting their predecessor.
func foldStoredOrder(ctx context.Context, tx pgx.Tx, artifactID string, since time.Time, at map[int64][]storedVersion, report *StoredOrderReport) error {
	rows, err := tx.Query(ctx, `
		select version, update, content_changed, created_at from doc_updates where artifact_id = $1 order by version
	`, artifactID)
	if err != nil {
		return fmt.Errorf("read the document's stored updates: %w", err)
	}
	defer rows.Close()
	doc := newDocumentCopy()
	// previous is the fold's render before the row being read, valid while rendered says so: a row
	// folded unchecked is rendered only when a version records it, so a check over the rows after
	// a since time does not render every row before it.
	var previous storedRender
	rendered := false
	var parked crdt.ContentIDs
	base := true
	var waiting, resolved []*parkedRow
	for rows.Next() {
		var version int64
		var update []byte
		var stored bool
		var createdAt time.Time
		if err := rows.Scan(&version, &update, &stored, &createdAt); err != nil {
			return fmt.Errorf("scan a stored document update: %w", err)
		}
		checked := !base && !createdAt.Before(since)
		if checked && !rendered {
			previous = renderStoredFold(doc)
		}
		if err := decodeStoredState(doc, update); err != nil {
			report.Refused = fmt.Sprintf("update %d does not apply to the fold before it: %v", version, err)
			return nil
		}
		now := doc.PendingStats()
		var pending crdt.ContentIDs
		var clocks crdt.StateVector
		if now.Items > 0 || now.DeleteRanges > 0 {
			clocks = doc.StateVector()
			pending, err = crdt.ContentIDsFromUpdateV1(crdt.EncodeStateAsUpdateV1(doc, clocks))
			if err != nil {
				return fmt.Errorf("read parked content at update %d: %w", version, err)
			}
			pending.Deletes = crdt.ExcludeIDSet(pending.Deletes, crdt.DeleteSetFromDoc(doc))
		}
		recorded := at[version]
		rendered = checked || len(recorded) > 0
		var current storedRender
		if rendered {
			current = renderStoredFold(doc)
		}
		switch {
		case base:
			base = false
		case !checked:
			report.UpdatesSkipped++
		default:
			report.UpdatesChecked++
			expected := !previous.renders || !current.renders || previous.markdown != current.markdown
			if expected != stored {
				report.Mismatches = append(report.Mismatches, StoredOrderMismatch{
					Kind: StoredOrderClass, Update: version, StoredClass: stored, ExpectedClass: expected,
				})
			}
			if needed := addedParkedClocks(parked, pending); len(needed) > 0 {
				waiting = append(waiting, &parkedRow{
					mismatch: StoredOrderMismatch{
						Kind: StoredOrderPending, Update: version, ParkedItems: now.Items, ParkedDeleteRanges: now.DeleteRanges,
					},
					needed: needed,
				})
			}
		}
		previous, parked = current, pending
		if len(waiting) > 0 {
			if clocks == nil {
				clocks = doc.StateVector()
			}
			unresolved := waiting[:0]
			for _, row := range waiting {
				if storedClocksReached(clocks, row.needed) {
					row.through = version
					resolved = append(resolved, row)
				} else {
					unresolved = append(unresolved, row)
				}
			}
			clear(waiting[len(unresolved):])
			waiting = unresolved
		}
		for _, written := range recorded {
			report.VersionsChecked++
			if mismatch, differs := compareStoredVersion(written, current); differs {
				report.Mismatches = append(report.Mismatches, mismatch)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the document's stored updates: %w", err)
	}
	rows.Close()
	report.UpdatesAwaitingPredecessor += len(waiting)
	if len(resolved) == 0 {
		return nil
	}
	return judgeParkedRows(ctx, tx, artifactID, resolved, report)
}

// addedParkedClocks records the ends of the newly parked runs. Extending a pending delete range
// adds content even when the pending item and range counts do not increase.
func addedParkedClocks(was, now crdt.ContentIDs) crdt.StateVector {
	if now.Inserts == nil && now.Deletes == nil {
		return nil
	}
	var clocks crdt.StateVector
	for _, ids := range []*crdt.IDSet{crdt.ExcludeIDSet(now.Inserts, was.Inserts), crdt.ExcludeIDSet(now.Deletes, was.Deletes)} {
		for _, client := range ids.Clients() {
			ranges := ids.Ranges(client)
			last := ranges[len(ranges)-1]
			if clocks == nil {
				clocks = make(crdt.StateVector)
			}
			clocks[client] = max(clocks[client], last.Clock+last.Len)
		}
	}
	return clocks
}

func storedClocksReached(have, need crdt.StateVector) bool {
	for client, clock := range need {
		if have[client] < clock {
			return false
		}
	}
	return true
}

// parkedRow is one checked row that added parked content. needed identifies its newly parked
// runs; through is the first later row whose fold has integrated them all. A deletion resolves
// once its target clocks integrate, even if another client's structs or deletions still wait.
type parkedRow struct {
	mismatch StoredOrderMismatch
	needed   crdt.StateVector
	through  int64
}

// judgeParkedRows advances one shared prefix in omitted-version order. Each comparison copies
// that prefix and replays only the omitted row's unresolved span, not the older history. Three
// documents suffice regardless of how many rows wait; suffix work follows the unresolved spans.
func judgeParkedRows(ctx context.Context, tx pgx.Tx, artifactID string, resolved []*parkedRow, report *StoredOrderReport) error {
	ordered := slices.Clone(resolved)
	slices.SortFunc(ordered, func(a, b *parkedRow) int {
		return cmp.Compare(a.mismatch.Update, b.mismatch.Update)
	})
	prefix := newDocumentCopy()
	defer prefix.Destroy()
	var through int64
	differs := make(map[*parkedRow]bool, len(resolved))
	for _, row := range ordered {
		before := row.mismatch.Update - 1
		if err := replayStoredRange(ctx, tx, artifactID, through, before, func(_ int64, update []byte) error {
			return decodeStoredState(prefix, update)
		}); err != nil {
			return err
		}
		through = before
		var err error
		differs[row], err = compareParkedPrefix(ctx, tx, artifactID, prefix, row)
		if err != nil {
			return err
		}
	}
	// Keep the report in resolution order, even when rows resolve in reverse order.
	for _, row := range resolved {
		if differs[row] {
			report.Mismatches = append(report.Mismatches, row.mismatch)
		}
	}
	return nil
}

func compareParkedPrefix(ctx context.Context, tx pgx.Tx, artifactID string, prefix *crdt.Doc, omitted *parkedRow) (bool, error) {
	full, without := newDocumentCopy(), newDocumentCopy()
	defer full.Destroy()
	defer without.Destroy()
	// Decode a snapshot rather than copying ygo's pending-queue internals: a queue with
	// emptied slots must rebuild its coverage from content, not retain those slots in a clone.
	snapshot := crdt.EncodeStateAsUpdateV1(prefix, nil)
	for _, doc := range []*crdt.Doc{full, without} {
		if err := decodeStoredState(doc, snapshot); err != nil {
			return false, fmt.Errorf("copy the stored prefix: %w", err)
		}
	}
	applies := true
	err := replayStoredRange(ctx, tx, artifactID, omitted.mismatch.Update-1, omitted.through, func(version int64, update []byte) error {
		var unsubscribe func()
		if applies && version == omitted.mismatch.Update {
			// OnUpdate carries only what integrated, not what parked. Earlier rows' parked
			// content is already in without; retain this row's immediate changes too, so
			// only its newly parked content must be supplied again by a later row.
			unsubscribe = full.OnUpdate(func(integrated []byte, _ any) {
				applies = decodeStoredState(without, integrated) == nil
			})
		}
		err := decodeStoredState(full, update)
		if unsubscribe != nil {
			unsubscribe()
		}
		if err != nil {
			return err
		}
		if applies && version != omitted.mismatch.Update {
			applies = decodeStoredState(without, update) == nil
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return !applies || !sameStoredState(without, full), nil
}

func replayStoredRange(ctx context.Context, tx pgx.Tx, artifactID string, after, through int64, apply func(int64, []byte) error) error {
	if after >= through {
		return nil
	}
	rows, err := tx.Query(ctx, `
		select version, update from doc_updates where artifact_id = $1 and version <= $2 and version > $3 order by version
	`, artifactID, through, after)
	if err != nil {
		return fmt.Errorf("read the document's stored updates again: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int64
		var update []byte
		if err := rows.Scan(&version, &update); err != nil {
			return fmt.Errorf("scan a stored document update: %w", err)
		}
		if err := apply(version, update); err != nil {
			return fmt.Errorf("replay the fold through update %d: %w", version, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the document's stored updates again: %w", err)
	}
	return nil
}

// sameStoredState compares integrated clocks and deletions, then the encoded parked content.
// Encoding above the integrated clocks retains parked structs and both integrated and pending
// deletions, without copying the integrated structs. Equal pending counts alone prove nothing
// about what a missing predecessor will eventually reveal.
func sameStoredState(a, b *crdt.Doc) bool {
	aParked, bParked := a.PendingStats(), b.PendingStats()
	aClocks, bClocks := a.StateVector(), b.StateVector()
	if aParked.Items != bParked.Items || aParked.DeleteRanges != bParked.DeleteRanges || !maps.Equal(aClocks, bClocks) {
		return false
	}
	if aParked.Items > 0 || aParked.DeleteRanges > 0 {
		// Limitation: ygo exposes no decoded pending structs; ContentIDs cannot compare payloads or normalize split runs.
		return bytes.Equal(crdt.EncodeStateAsUpdateV1(a, aClocks), crdt.EncodeStateAsUpdateV1(b, bClocks))
	}
	return sameDeletes(crdt.DeleteSetFromDoc(a), crdt.DeleteSetFromDoc(b))
}

// sameDeletes reports whether two delete sets cover the same runs. Ranges are normalized, so
// equal sets compare equal run for run.
func sameDeletes(was, now *crdt.IDSet) bool {
	wasClients := was.Clients()
	if len(wasClients) != len(now.Clients()) {
		return false
	}
	for _, client := range wasClients {
		if !slices.Equal(was.Ranges(client), now.Ranges(client)) {
			return false
		}
	}
	return true
}

// compareStoredVersion compares a version's stored markdown with the fold's render at the update
// it records, by md5 and length.
func compareStoredVersion(version storedVersion, fold storedRender) (StoredOrderMismatch, bool) {
	mismatch := StoredOrderMismatch{
		Kind:             StoredOrderVersion,
		Version:          version.number,
		DocUpdateVersion: version.docUpdateVersion,
		StoredMD5:        version.md5,
		StoredLength:     version.length,
		FoldRenders:      fold.renders,
	}
	if !fold.renders {
		return mismatch, true
	}
	sum := md5.Sum([]byte(fold.markdown))
	mismatch.FoldMD5, mismatch.FoldLength = hex.EncodeToString(sum[:]), len(fold.markdown)
	return mismatch, mismatch.FoldMD5 != version.md5 || mismatch.FoldLength != version.length
}

// StoredOrderDocuments lists every document the check can read, oldest first.
func StoredOrderDocuments(ctx context.Context, q Queryer) ([]string, error) {
	rows, err := q.Query(ctx, `select id::text from artifacts where kind = 'doc' order by created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a document: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	return ids, nil
}

package docs

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
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
//  3. Pending. No update leaves the fold holding structs it parked: an update stored before one it
//     depends on was stored out of order.
//
// The fold starts from the document's oldest retained update, its base. Compaction folds older
// history into that row (Compact) and a rebuild replaces the whole history with one row there
// (RebuildTx), so the base is a state and its own class is not checked; a version recorded before
// it is skipped. A history rewritten after the base - a prune's checkpoint standing, or a prune
// that finished and was appended past (PruneAfter, recoverPruneTx) - is refused: the versions
// recorded against it name updates the store no longer holds.

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
	// StoredOrderPending is an update after which the fold holds more parked structs than before
	// it: an update it depends on is stored after it, or not at all.
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
	// VersionsChecked counts the versions compared with the fold; VersionsSkipped those recorded
	// before the base or before the since time.
	VersionsChecked, VersionsSkipped int
	// Refused says why the document's history cannot be checked; a refused document never passes.
	Refused    string
	Mismatches []StoredOrderMismatch
}

// Clean reports a document whose stored history replays: something checked, and no row
// disagreeing. A check that compared no update and no version proves nothing about the document.
func (r StoredOrderReport) Clean() bool {
	return r.Checked() && r.Refused == "" && len(r.Mismatches) == 0
}

// Checked reports whether the check compared at least one update or version with the fold.
func (r StoredOrderReport) Checked() bool {
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
	// The versions each update is compared against, by the update they record. A version recorded
	// before the base, or created before since, is skipped.
	at := make(map[int64][]storedVersion)
	for _, version := range versions {
		if count == 0 || version.docUpdateVersion < first || version.createdAt.Before(since) {
			report.VersionsSkipped++
			continue
		}
		at[version.docUpdateVersion] = append(at[version.docUpdateVersion], version)
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
	markdown, _, _, err := renderDocumentForUpdate(doc.GetXmlFragment(fragmentName))
	return storedRender{markdown: markdown, renders: err == nil}
}

// foldStoredOrder folds the document's updates in version order into one document, reading one row
// at a time, and checks each update after the base and each version against the fold.
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
	var parked crdt.PendingStats
	base := true
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
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			report.Refused = fmt.Sprintf("update %d does not apply to the fold before it: %v", version, err)
			return nil
		}
		now := doc.PendingStats()
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
			if now.Items > parked.Items || now.DeleteRanges > parked.DeleteRanges {
				report.Mismatches = append(report.Mismatches, StoredOrderMismatch{
					Kind: StoredOrderPending, Update: version, ParkedItems: now.Items, ParkedDeleteRanges: now.DeleteRanges,
				})
			}
		}
		previous, parked = current, now
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
	return nil
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

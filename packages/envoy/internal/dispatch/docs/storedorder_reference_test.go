package docs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
)

// fullPrefixStoredOrder is the reference replay: every omitted row is judged by replaying its
// entire prefix. Keep it independent of the optimized prefix reuse so that a bad copy cannot
// validate itself. Classification, pending payload comparison, and version checks are unchanged.
func fullPrefixStoredOrder(ctx context.Context, tx pgx.Tx, artifactID string, since time.Time, at map[int64][]storedVersion, report *StoredOrderReport) error {
	rows, err := tx.Query(ctx, `
		select version, update, content_changed, created_at from doc_updates where artifact_id = $1 order by version
	`, artifactID)
	if err != nil {
		return fmt.Errorf("read the document's stored updates: %w", err)
	}
	defer rows.Close()
	doc := newDocumentCopy()
	defer doc.Destroy()
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
	for _, row := range resolved {
		differs, err := compareFullStoredPrefix(ctx, tx, artifactID, row)
		if err != nil {
			return err
		}
		if differs {
			report.Mismatches = append(report.Mismatches, row.mismatch)
		}
	}
	return nil
}

func compareFullStoredPrefix(ctx context.Context, tx pgx.Tx, artifactID string, omitted *parkedRow) (bool, error) {
	rows, err := tx.Query(ctx, `
		select version, update from doc_updates where artifact_id = $1 and version <= $2 order by version
	`, artifactID, omitted.through)
	if err != nil {
		return false, fmt.Errorf("read the document's stored updates again: %w", err)
	}
	defer rows.Close()
	full, without := newDocumentCopy(), newDocumentCopy()
	defer full.Destroy()
	defer without.Destroy()
	applies := true
	for rows.Next() {
		var version int64
		var update []byte
		if err := rows.Scan(&version, &update); err != nil {
			return false, fmt.Errorf("scan a stored document update: %w", err)
		}
		var unsubscribe func()
		if applies && version == omitted.mismatch.Update {
			unsubscribe = full.OnUpdate(func(integrated []byte, _ any) {
				applies = decodeStoredState(without, integrated) == nil
			})
		}
		err := decodeStoredState(full, update)
		if unsubscribe != nil {
			unsubscribe()
		}
		if err != nil {
			return false, fmt.Errorf("replay the fold through update %d: %w", version, err)
		}
		if applies && version != omitted.mismatch.Update {
			applies = decodeStoredState(without, update) == nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read the document's stored updates again: %w", err)
	}
	return !applies || !sameStoredState(without, full), nil
}

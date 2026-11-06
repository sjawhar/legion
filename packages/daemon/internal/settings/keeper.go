package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// Keeper is the daemon's settings arms: the one settings holder, and the store of what the route
// changed. Every arm settles through config.Live.Apply, so a route change meets exactly the
// refusals a file change meets, and the store is kept in step with the holder's overlay — what
// the daemon is running on after a restart is what it was running on before it.
//
// A Keeper with no store keeps nothing: the route still applies, and a restart reads the file
// alone. That is the daemon whose configuration is a value rather than a file (config.Settled),
// and a test's.
type Keeper struct {
	live  *config.Live
	store *Store
	log   *slog.Logger
	now   func() time.Time
	// mu orders the arms against each other: each one reads what the file says for the paths it
	// is about, applies, and writes its rows under it, so two changes of one path can never
	// store the older value over the newer, and the file arm never reads a digest a route change
	// is in the middle of writing. The holder has a lock of its own, which orders the applies;
	// this one covers the reads and the writes around them. unstored is read and written under
	// it too.
	mu sync.Mutex
	// unstored is the file's digest for each path the route applied and stored no row for: a
	// secret, which is never written into a database every project's daemon shares, and every
	// path of a change the store refused (NotRecorded). The holder is running on those values
	// and the store knows nothing of them, so without this the file arm would find no row for
	// the path and never take the key back: an operator's later edit of that key in
	// `legion.yaml` would do nothing until the daemon restarted. It is memory alone, as the
	// overlay it answers for is: a restart reads the file's own value for those paths.
	unstored map[string]string
}

// New is the keeper of live's changes, storing what the route applies in store (nil keeps none).
func New(live *config.Live, store *Store, log *slog.Logger) *Keeper {
	if log == nil {
		log = slog.Default()
	}
	return &Keeper{live: live, store: store, log: log, unstored: map[string]string{},
		now: func() time.Time { return time.Now().UTC() }}
}

// Live is the settings holder this keeper applies through.
func (k *Keeper) Live() *config.Live { return k.live }

// Route applies a caller's change of the named paths and records what it applied, so the change
// outlives the daemon's next restart. caller is the arm that asked — the operator bearer sets
// every path the loader takes, a projected ServiceAccount token only a path whose value is
// plain data (config.Caller) — and by is the person the caller named.
//
// A path mapped to nil withdraws that path from the stored overlay: the file's own value takes
// it back, and the row goes with it. Every other path is laid over the file. A path the class
// table does not hold, one fixed for the process, and one this caller may not set are refused
// by name and nothing is stored for them (applyRoute, config.Live.Apply).
//
// A secret path is applied and never stored: the daemon runs on it, and the value — a private
// key the caller inlined, the path of a credential file, an address that may carry one — is not
// written into a database every project's daemon shares. Its row goes with it, so a restart
// reads the file's own value for that path rather than one a release before this kept.
//
// The store is written after the holder has settled, so a change the loader refused is stored
// nowhere, and a failed write answers NotRecorded: the change is applied, and the caller is told
// it will not outlive the daemon rather than that nothing happened. What the daemon's next
// restart then reads is not one answer for every path of that change: a path this change laid
// over the file is in the store nowhere, so the boot reads the file's own value for it, while a
// path it withdrew — and a secret whose row it dropped — keeps the row the store would not
// drop, and the boot lays that row's value back over the file (Restore). Nothing here can do
// better, since the one act that would remove such a row is the write that failed, so the rows
// left standing are logged by name.
func (k *Keeper) Route(ctx context.Context, caller config.Caller, by string, changes map[string]any) ([]config.Change, []config.Refusal, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	requested := slices.Sorted(maps.Keys(changes))
	values, err := k.fileValues(requested)
	if err != nil {
		return nil, nil, err
	}
	applied, refused, err := k.applyRoute(caller, by, changes)
	if err != nil {
		return nil, refused, err
	}
	at := k.now()
	var rows []Change
	var forget []string
	for _, path := range requested {
		if slices.ContainsFunc(refused, func(r config.Refusal) bool { return r.Path == path }) {
			continue
		}
		row, known := config.ClassOf(path)
		switch {
		case changes[path] == nil:
			// The overlay is gone, so there is nothing left for this keeper to remember either.
			forget = append(forget, path)
			delete(k.unstored, path)
			continue
		case known && row.Secret:
			k.log.Warn("legion settings: a credential changed through the route is applied and not stored; the daemon's next restart reads the file's own value for it",
				"path", path, "by", by)
			forget = append(forget, path)
			k.unstored[path] = Digest(values[path])
			continue
		}
		value, err := json.Marshal(changes[path])
		if err != nil {
			return applied, refused, fmt.Errorf("record the settings change of %s: %w", path, err)
		}
		rows = append(rows, Change{
			Path: path, Value: value, FileDigest: Digest(values[path]),
			Source: config.SourceRoute, By: by, At: at,
		})
		delete(k.unstored, path)
	}
	if k.store == nil {
		k.remember(rows)
		return applied, refused, nil
	}
	if err := k.store.Write(ctx, rows, forget); err != nil {
		// The change is applied and the store holds none of it, so the file's digest of each
		// path it laid is the keeper's to hold instead: the file arm takes the key back when the
		// file's own value for it moves, as it does for a path the store does hold a row for.
		k.remember(rows)
		if len(forget) > 0 {
			// The rows this change asked the store to drop are still there, and the next boot
			// lays each one back over the file. That outlives the daemon where the change itself
			// does not, so it is named rather than left inside the caller's NotRecorded.
			k.log.Error("legion settings: the store kept the rows this change asked it to drop; the daemon runs on the change, and its next restart lays those rows back over the file",
				"paths", forget, "by", by, "error", err)
		}
		return applied, refused, &config.NotRecorded{Paths: requested, Err: err}
	}
	return applied, refused, nil
}

// remember holds the file's digest of each row's path in memory, for the paths this keeper
// applied and stored no row for (unstored). The caller holds mu.
func (k *Keeper) remember(rows []Change) {
	for _, row := range rows {
		k.unstored[row.Path] = row.FileDigest
	}
}

// File settles the configuration file as it now reads. It is the file arm, and the watcher's one
// call: the file is read again under the route changes still standing above it, and every path
// the file has itself changed since a route change of that path takes its key back — the latest
// change of a key wins, and the stored row goes with it (split).
//
// A path the route applied and stored no row for — a secret, and every path of a change the
// store refused — is taken back the same way, from the digest this keeper holds for it in
// memory (unstored): the overlay the daemon is running on is the same overlay whether or not a
// row backs it, so the file's own value takes the key back either way.
//
// Nothing this settle reads is forgotten until the holder has settled: an apply runs the whole
// loader, App key commands included, and the one that fails changes nothing, so the overlay it
// left standing keeps both the stored row that backs it and the mark that backs an unstored one
// — the next settle of the fixed file takes the key back exactly as this one would have.
func (k *Keeper) File(ctx context.Context) ([]config.Change, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, taken, unstored, err := k.split(ctx)
	if err != nil {
		return nil, err
	}
	taken = append(taken, unstored...)
	withdraw := make(map[string]any, len(taken))
	for _, path := range taken {
		withdraw[path] = nil
	}
	applied, _, err := k.live.Apply(config.SourceFile, "", withdraw)
	if err != nil {
		return nil, err
	}
	for _, path := range unstored {
		delete(k.unstored, path)
	}
	if k.store != nil {
		if err := k.store.Write(ctx, nil, taken); err != nil {
			return applied, err
		}
	}
	if len(taken) > 0 {
		k.log.Info("legion settings: the file changed keys a route change held; the file's own values stand again",
			"paths", taken)
	}
	return applied, nil
}

// Restore lays the route changes the store kept back over the file, and answers the rows it
// applied, newest state first for the caller's provenance (api.SettingProvenance.Restore). It is
// the boot's one call, made before anything is built from the configuration, so every holder the
// boot then builds is built from what the daemon is actually running on.
//
// A row the file has taken back since — its own value for that path has moved — is applied to
// nothing and deleted: a route change survives a restart until the file changes that key.
func (k *Keeper) Restore(ctx context.Context) ([]Change, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	// The boot's keeper holds no mark of its own: unstored is memory, and this is the first
	// thing the process does with the store.
	kept, taken, _, err := k.split(ctx)
	if err != nil {
		return nil, err
	}
	if kept == nil {
		// No store, or a store holding no rows: there is nothing to lay back over the file.
		return nil, nil
	}
	overlay := map[string]any{}
	for _, row := range kept {
		var value any
		if err := json.Unmarshal(row.Value, &value); err != nil {
			return nil, fmt.Errorf("read the stored settings change of %s: %w", row.Path, err)
		}
		overlay[row.Path] = value
	}
	if err := k.store.Write(ctx, nil, taken); err != nil {
		return nil, err
	}
	if len(taken) > 0 {
		k.log.Info("legion settings: the file changed keys a route change held while the daemon was down; the file's own values stand",
			"paths", taken)
	}
	if len(overlay) == 0 {
		return nil, nil
	}
	// One apply for every kept row: the boot reads the file once more, not once per row, since
	// each read runs the App private key commands the file names.
	if _, refused, err := k.applyRoute(config.CallerOperator, "", overlay); err != nil {
		return nil, err
	} else if len(refused) > 0 {
		// A stored path the class table no longer holds, or one this release made fixed: the row
		// is over, and the daemon runs on the file's own value for it.
		var paths []string
		for _, refusal := range refused {
			paths = append(paths, refusal.Path)
			kept = slices.DeleteFunc(kept, func(row Change) bool { return row.Path == refusal.Path })
		}
		k.log.Warn("legion settings: a stored route change names a path this daemon will not take; the file's own value stands",
			"paths", paths, "refusals", refused)
		if err := k.store.Write(ctx, nil, paths); err != nil {
			return nil, err
		}
	}
	for _, row := range kept {
		k.log.Info("legion settings: a stored route change is laid over the file again",
			"path", row.Path, "source", row.Source, "by", row.By, "at", row.At)
	}
	return kept, nil
}

// applyRoute is the one call of the settings holder's route arm: the route itself and the boot's
// restore of what the store kept both come through here, so the daemon holds one call site per
// arm and no second resolution path exists (TestEachSettingsArmHasOneApplyCallSite).
//
// It is where a caller meets what that arm may set (config.Caller.Admits): a path this caller
// may not change is refused by name, with the reason the row gives, and never reaches the
// holder. The operator bearer, and the boot's restore of what it changed, may set every path.
func (k *Keeper) applyRoute(caller config.Caller, by string, overlay map[string]any) ([]config.Change, []config.Refusal, error) {
	allowed, refused := overlay, []config.Refusal(nil)
	for _, path := range slices.Sorted(maps.Keys(overlay)) {
		admitted, why := caller.Admits(path)
		if admitted {
			continue
		}
		if len(refused) == 0 {
			allowed = maps.Clone(overlay)
		}
		refused = append(refused, config.Refusal{Path: path, Reason: why})
		delete(allowed, path)
	}
	if len(allowed) == 0 {
		return nil, refused, nil
	}
	applied, loader, err := k.live.Apply(config.SourceRoute, by, allowed)
	return applied, append(refused, loader...), err
}

// split is the comparison every reading of what this keeper applied shares: the store's rows,
// the digests the keeper holds for the paths no row backs (unstored), what the file itself now
// says for all of their paths — read once, so a settle parses the file once however many paths
// it is about — and the one decision both turn on, a file value that has moved since the change
// was applied being the file taking that key back.
//
// kept is the stored rows whose file value has not moved, in the store's own path order; taken
// the paths of the stored rows the file is taking back; and unstored the marked paths it is
// taking back, in path order. A keeper with no store, and a store holding no rows, split into
// no rows at all: kept is nil for both, which is what tells Restore there is nothing to lay
// back over the file. The caller holds mu.
func (k *Keeper) split(ctx context.Context) (kept []Change, taken, unstored []string, err error) {
	var stored []Change
	if k.store != nil {
		if stored, err = k.store.Rows(ctx); err != nil {
			return nil, nil, nil, err
		}
	}
	marked := slices.Sorted(maps.Keys(k.unstored))
	values, err := k.fileValues(append(pathsOf(stored), marked...))
	if err != nil {
		return nil, nil, nil, err
	}
	moved := func(path, digest string) bool { return Digest(values[path]) != digest }
	if len(stored) > 0 {
		kept = make([]Change, 0, len(stored))
		for _, row := range stored {
			if moved(row.Path, row.FileDigest) {
				taken = append(taken, row.Path)
				continue
			}
			kept = append(kept, row)
		}
	}
	for _, path := range marked {
		if moved(path, k.unstored[path]) {
			unstored = append(unstored, path)
		}
	}
	return kept, taken, unstored, nil
}

// fileValues is what the file itself says for each path, by path, read from the document alone.
func (k *Keeper) fileValues(paths []string) (map[string]string, error) {
	if len(paths) == 0 {
		return map[string]string{}, nil
	}
	return k.live.FileValues(paths)
}

func pathsOf(stored []Change) []string {
	paths := make([]string, 0, len(stored))
	for _, row := range stored {
		paths = append(paths, row.Path)
	}
	return paths
}

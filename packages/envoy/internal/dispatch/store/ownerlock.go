package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ownerLockNamespace is the first key of every owner lock ("OWNR"). Postgres keeps the two-key
// advisory key space apart from the single-key one, and within it the other namespaces of this
// module are the copy locks' (docs.copiedAsksLockNamespace, "COPY"), the duty leases'
// (duty.leaseNamespace, "LOOP") and the hashtext of a name (the dependency, repository-mapping
// and delivery session-group locks), none of which is this value, so an owner lock never waits on
// one of those whatever its owner hashes to.
const ownerLockNamespace int32 = 0x4F574E52

// OwnerKind names what owns a collaboration: an issue, a document no issue owns (a project
// document, keyed by the artifact's id), or a project.
type OwnerKind string

// The owner kinds, each the prefix of its owners' lock keys.
const (
	OwnerIssue    OwnerKind = "issue"
	OwnerArtifact OwnerKind = "artifact"
	OwnerProject  OwnerKind = "project"
)

// Owner is one owner whose lock a write transaction takes.
type Owner struct {
	Kind OwnerKind
	Key  string
}

// IssueOwner is the issue key's owner.
func IssueOwner(key string) Owner { return Owner{Kind: OwnerIssue, Key: key} }

// ArtifactOwner is the owner of a document no issue owns: the artifact itself.
func ArtifactOwner(id string) Owner { return Owner{Kind: OwnerArtifact, Key: id} }

// ProjectOwner is the project key's owner.
func ProjectOwner(key string) Owner { return Owner{Kind: OwnerProject, Key: key} }

// OwnerOf is the owner of a row that names the issue it belongs to, the document it belongs to, or
// neither: the issue when it names one, since an issue owns its document and every comment, ask
// and message under it, and otherwise the document, which owns the comments and asks on a document
// no issue owns. A row naming neither has no owner and nil is returned, a direct message among
// them, whose conversation is no owner. The rule is one rule, so every reader and writer of such a
// row takes the same lock (refs.sourceOwner).
func OwnerOf(issueKey, artifactID *string) *Owner {
	switch {
	case issueKey != nil:
		owner := IssueOwner(*issueKey)
		return &owner
	case artifactID != nil:
		owner := ArtifactOwner(*artifactID)
		return &owner
	}
	return nil
}

// lockKey is the text whose hashtext is the lock's second key: `<kind>:<key>`, the same spelling
// the event broker orders owners by.
func (o Owner) lockKey() string { return string(o.Kind) + ":" + o.Key }

// OwnerLocker is what an owner lock runs on: a transaction, or a savepoint of one, whose rollback
// gives back the locks taken under it. The pool satisfies it too, for a try that holds its lock only
// for its own statement.
type OwnerLocker interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
}

// The owner lock statements. Each names its result owner_lock and spells its key's text type, so
// a session waiting on one is told apart in pg_stat_activity from a wait on the copy lock
// (docs.projectCopiesLock), and no scan of this module's SQL mistakes one for the other.
const (
	ownerLock       = `select pg_advisory_xact_lock($1, hashtext($2::text)) as owner_lock`
	ownerLockShared = `select pg_advisory_xact_lock_shared($1, hashtext($2::text)) as owner_lock`
	// ownersLock takes the locks of several owners in one statement, in the order of their second
	// key. The subquery is sorted and deduplicated before the outer select calls the lock function
	// on each row it hands up, and a subquery with ORDER BY or DISTINCT is never flattened into its
	// parent, so the calls run in that order.
	ownersLock = `
		select pg_advisory_xact_lock($1, keyed.hash) as owner_lock
		from (select distinct hashtext(owner) as hash from unnest($2::text[]) as owner order by hash) as keyed`
	// ownersTryLock tries the lock of each owner without waiting and answers the owners it took. A
	// try never waits, so the order it tries them in cannot close a cycle.
	ownersTryLock = `
		select owner from unnest($2::text[]) with ordinality as listed(owner, place)
		where pg_try_advisory_xact_lock($1, hashtext(owner))
		order by place`
)

// LockOwner takes owner's lock: a transaction advisory lock, exclusive, held until the transaction
// ends. It is Dispatch's per-owner serialization. Every transaction that writes an owner's
// collaboration rows, its own row or anything a write to the owner decides on takes it before it
// reads what that decision rests on, so writers of one owner run one at a time, as they did under
// the owner row's `for no key update`; unlike that row lock it assigns the transaction no
// transaction id, so a transaction holds one only from its first write on.
//
// The lock and the read it guards must be separate statements. Under READ COMMITTED a statement's
// snapshot is taken when it starts, before it waits, so a statement that both took this lock and
// read the owner (`select pg_advisory_xact_lock(...), closed_at from issues ...`) would read the
// row as it stood before the wait and miss a close the lock's previous holder committed meanwhile.
// The statement after the lock takes a fresh snapshot, which holds every commit of that holder.
// LockOwnerAndRead sends the two in one round trip.
//
// A transaction that locks several owners takes them all through LockOwners, which orders them by
// lock key. Taking the lock again in the transaction that holds it returns at once.
func LockOwner(ctx context.Context, q OwnerLocker, owner Owner) error {
	return LockOwners(ctx, q, owner)
}

// LockOwners takes the locks of every owner in owners (LockOwner) in one statement, sorted and
// deduplicated by the lock's key rather than by the owner's name: two owners whose names hash to
// the same key share one lock, and ordering by name could take that lock before a third owner's in
// one transaction and after it in another, a cycle Postgres breaks with `deadlock detected`. Sorted
// by key, every transaction takes any two keys in the same order. As with LockOwner, the reads the
// locks guard come in statements after this one.
func LockOwners(ctx context.Context, q OwnerLocker, owners ...Owner) error {
	if len(owners) == 0 {
		return nil
	}
	lock, args := lockOwnersStatement(owners)
	if _, err := q.Exec(ctx, lock, args...); err != nil {
		return lockOwnersFailed(owners, err)
	}
	return nil
}

// LockOwnerAndRead takes owner's lock (LockOwner) and then runs read with args, the statement the
// lock guards, scanning its one row into dest. The two go in one batch, one round trip, and stay
// two statements: the server starts read, taking its snapshot, only once the lock is granted, so
// read sees every commit of the lock's previous holder. A session waiting on the lock shows the
// lock's statement (owner_lock) in pg_stat_activity. The lock's failure is wrapped as LockOwner
// wraps it; read's comes back as Scan returned it, pgx.ErrNoRows included.
func LockOwnerAndRead(ctx context.Context, tx pgx.Tx, owner Owner, read string, args []any, dest ...any) error {
	return LockOwnersAndRead(ctx, tx, []Owner{owner}, read, args, dest...)
}

// LockOwnersAndRead is LockOwnerAndRead for the locks of several owners, taken as LockOwners takes
// them, in one statement ahead of read.
func LockOwnersAndRead(ctx context.Context, tx pgx.Tx, owners []Owner, read string, args []any, dest ...any) error {
	batch := &pgx.Batch{}
	lock, lockArgs := lockOwnersStatement(owners)
	batch.Queue(lock, lockArgs...)
	batch.Queue(read, args...)
	results := tx.SendBatch(ctx, batch)
	if _, err := results.Exec(); err != nil {
		results.Close()
		return lockOwnersFailed(owners, err)
	}
	err := results.QueryRow().Scan(dest...)
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	return err
}

// QueueLockOwner queues owner's lock (LockOwner) on a write phase's batch, for a statement after it
// in the same batch that the lock guards: a write that takes its owner's lock again where the
// transaction took it in its read phase, which returns at once. Its failure is wrapped as
// LockOwner wraps it.
func QueueLockOwner(b *WriteBatch, owner Owner) {
	lock, args := lockOwnersStatement([]Owner{owner})
	b.Queue(fmt.Sprintf("lock %s %s", owner.Kind, owner.Key), lock, args...)
}

// lockOwnersStatement is the statement that takes the locks of owners, at least one, and its
// arguments: ownerLock for one owner, ownersLock for several.
func lockOwnersStatement(owners []Owner) (string, []any) {
	if len(owners) == 1 {
		return ownerLock, []any{ownerLockNamespace, owners[0].lockKey()}
	}
	return ownersLock, []any{ownerLockNamespace, lockKeys(owners)}
}

// lockOwnersFailed is err, the failure of lockOwnersStatement's statement, naming what it locked.
func lockOwnersFailed(owners []Owner, err error) error {
	if len(owners) == 1 {
		return fmt.Errorf("lock %s %s: %w", owners[0].Kind, owners[0].Key, err)
	}
	return fmt.Errorf("lock owners %v: %w", lockKeys(owners), err)
}

// lockKeys is each owner's lock key, in owners' order.
func lockKeys(owners []Owner) []string {
	keys := make([]string, len(owners))
	for index, owner := range owners {
		keys[index] = owner.lockKey()
	}
	return keys
}

// LockOwnerShared takes owner's lock in shared mode. It conflicts with LockOwner's exclusive lock
// exactly as `for share` conflicted with the writers' `for no key update` on the owner row, and with
// no other shared holder: a transaction that writes only rows every owner writer orders itself
// against, and must not interleave with one (putThreadRead in internal/dispatch/api), waits for the
// writer that holds the owner and holds off the next one. The read it guards is a separate
// statement after it, as LockOwner's is.
func LockOwnerShared(ctx context.Context, q OwnerLocker, owner Owner) error {
	if _, err := q.Exec(ctx, ownerLockShared, ownerLockNamespace, owner.lockKey()); err != nil {
		return fmt.Errorf("lock %s %s for share: %w", owner.Kind, owner.Key, err)
	}
	return nil
}

// TryLockOwners tries each owner's exclusive lock without waiting and answers, in owners' order,
// the owners whose lock this transaction now holds; an owner another transaction holds is left out.
// It is the advisory form of `for no key update skip locked`: a background pass takes the owners
// nobody is writing and leaves the rest. As with LockOwner, what the pass reads of an owner it took
// is read in a statement after this one. Run on the pool, outside a transaction, a lock it takes is
// released when its statement ends, which makes it a probe of whether anyone holds the owner.
func TryLockOwners(ctx context.Context, q OwnerLocker, owners ...Owner) ([]Owner, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	keys := make([]string, len(owners))
	byKey := make(map[string]Owner, len(owners))
	for index, owner := range owners {
		keys[index] = owner.lockKey()
		byKey[keys[index]] = owner
	}
	rows, err := q.Query(ctx, ownersTryLock, ownerLockNamespace, keys)
	if err != nil {
		return nil, fmt.Errorf("try owner locks: %w", err)
	}
	taken, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Owner, error) {
		var key string
		if err := row.Scan(&key); err != nil {
			return Owner{}, err
		}
		return byKey[key], nil
	})
	if err != nil {
		return nil, fmt.Errorf("try owner locks: %w", err)
	}
	return taken, nil
}

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
//
// A transaction that locks several owners takes them all through LockOwners, which orders them by
// lock key. Taking the lock again in the transaction that holds it returns at once.
func LockOwner(ctx context.Context, q OwnerLocker, owner Owner) error {
	if _, err := q.Exec(ctx, ownerLock, ownerLockNamespace, owner.lockKey()); err != nil {
		return fmt.Errorf("lock %s %s: %w", owner.Kind, owner.Key, err)
	}
	return nil
}

// QueueLockOwner queues owner's lock (LockOwner) on batch, ahead of the statements that read what
// it guards, so the lock and those reads cost one round trip and stay statements of their own: the
// server binds the next statement, taking its snapshot, only once the lock has been granted.
func QueueLockOwner(batch *pgx.Batch, owner Owner) {
	batch.Queue(ownerLock, ownerLockNamespace, owner.lockKey())
}

// LockOwners takes the locks of every owner in owners (LockOwner) in one statement, sorted and
// deduplicated by the lock's key rather than by the owner's name: two owners whose names hash to
// the same key share one lock, and ordering by name could take that lock before a third owner's in
// one transaction and after it in another, a cycle Postgres breaks with `deadlock detected`. Sorted
// by key, every transaction takes any two keys in the same order. As with LockOwner, the reads the
// locks guard come in statements after this one.
func LockOwners(ctx context.Context, q OwnerLocker, owners ...Owner) error {
	switch len(owners) {
	case 0:
		return nil
	case 1:
		return LockOwner(ctx, q, owners[0])
	}
	keys := make([]string, len(owners))
	for index, owner := range owners {
		keys[index] = owner.lockKey()
	}
	if _, err := q.Exec(ctx, ownersLock, ownerLockNamespace, keys); err != nil {
		return fmt.Errorf("lock owners %v: %w", keys, err)
	}
	return nil
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

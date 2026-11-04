-- 20261103183833_issue_slots.up.sql — what the admission cap and the children-first phase rule
-- read once a slot counts an issue rather than a tree (INFRA-980): the mark that a design gate
-- once opened, the Dispatch parent of every mirrored issue, and a slot for every issue already at
-- work when the daemon that reads them boots.
--
-- design_gates.ever_opened records that a human approved the gate's document version at some
-- point, which nothing else in the record remembers. classify.DesignGateOpen reads the gate as it
-- stands now, and a changes request clears approved_version, so a gate that opened and was then
-- sent back reads exactly like one that never opened at all. Under the children-first rule a root
-- stays at admitted while its children work, so "this root's spec was approved once" is the only
-- thing that tells such a root from one still waiting for its first approval. It lives on the gate
-- row rather than on issues because record.Store.ClearTreeGeneration deletes the gate when a root
-- is re-admitted, and a re-admitted root's architect writes its spec again: the mark goes with the
-- generation it belongs to.
--
-- Every existing gate is read from what the record still shows. An approval that stands is an
-- opening, and covers both the gate open now and one a newer document version closed, since only a
-- changes request nulls the approval. A gate whose approval a changes request cleared shows
-- nothing of itself, so the tree below it says it instead: a member recorded in a phase the
-- workflow only reaches after the gate opened is a gate that opened. The root's own row is not
-- one of those members, though its tree is its own key: before this release a root whose first
-- spec waits on a person stands in awaiting_person, and can be held from it, neither of which is
-- admitted or done, so reading the root would mark every root waiting for its first approval as
-- opened — and then a changes request would answer it, slotFor would give it no slot
-- (record.AtWork is false once a gate has opened), and its architect would rewrite the spec
-- outside the cap with the catch-up's hold passing over it. phase done is not one of them
-- either, and is the one phase past admitted that proves nothing: workflow's leave records any
-- non-root member a person moves to backlog, icebox, triage or done at done whatever the gate
-- was doing, and the children of a live tree are recorded before any release, so a root still
-- writing its first spec can already have a parked child. A gate that shows neither never
-- opened, and keeps the default, which leaves its root at work (record.AtWork) and its slot
-- where it is.
alter table design_gates add column ever_opened boolean not null default false;

update design_gates set ever_opened = true
  where approved_version is not null
     or exists (select 1 from issues m where m.tree = design_gates.issue
                  and m.key <> design_gates.issue
                  and m.phase not in ('admitted', 'done'));

-- dispatch_issues.parent is the key of the issue's Dispatch parent, null for none. Admission and
-- the workflow judge inside a transaction that does no I/O, so neither can ask Dispatch what hangs
-- under a parent at the moment it decides; the mirror is what they read instead, and it holds
-- every issue of the project, recorded or not, which is exactly the set a parent's children are
-- drawn from. Existing rows are filled from the daemon's own records, the only parent it holds
-- until Dispatch is listed again: an issue the daemon records carries the parent its observation
-- came with, and the first boot listing after this migration fills the rest.
alter table dispatch_issues add column parent text;

update dispatch_issues d set parent = i.parent from issues i where i.key = d.key and i.parent is not null;

-- A tree's liveness moves with this release from the slot its root holds to its lifecycle row,
-- which every rule that read "the root holds a slot" as "the tree runs" now reads instead
-- (record.TreeLive). Before this migration only a root holds a slot, so a root holding one with
-- no linger deadline is a tree that runs; after it, a root whose tree has no open lifecycle row
-- belongs to no live tree, and the first admission pass takes it out of the workflow altogether
-- (admit's unslotted and releaseUnslotted) though its architect is at work in it. So the
-- pre-release fact is written in the shape the new rule reads. Every tree an admission opened
-- has its row already — 0033_tree_lifecycle backfilled the roots recorded then, and each
-- promotion since opened one — so this writes nothing where they are all there, and carries the
-- fact where one is missing. Epoch one under the workflow's authority is 0033's own choice, and
-- the claims of such a tree already carry that epoch; a row that already exists is left exactly
-- as it stands, a reserved cleanup and the operator's authority included.
insert into tree_lifecycles (project, tree, epoch, authority)
select distinct regexp_replace(lower(i.project), '[^a-z0-9]', '', 'g'), i.key, 1, 'workflow'
  from issues i
  join slots s on s.issue = i.key
 where i.key = i.tree and i.linger_until is null
on conflict do nothing;

-- One slot row exists per tree root today; after the whole change any issue may hold one while it
-- has a worker with work to do. At the first boot after this migration every issue already in a
-- worker phase must keep working, so each is granted the slot the new cap will ask it to hold,
-- even where that puts the project above its configured cap: the cap loop admits nothing until the
-- surplus drains as those issues finish, which is the behaviour of a cap that has been lowered,
-- not a failure. An issue in merging that still carries a pending READY version is waiting for its
-- gate rather than working, so it is left out, as are the statuses that are no work at all.
--
-- A tree is live and not lingering when its root holds a slot and has no linger deadline, which
-- before this migration is the whole of "a live tree": only a root holds a slot. index continues
-- the existing sequence because the column is unique across every project's slots (0004_record).
-- The insert never deletes, so a root that holds a row and still needs one keeps the row it has.
--
-- This insert and the lifecycle insert above act on every project in the database, not only on
-- the project of the daemon that runs the migration: the issues, the slots and the tree
-- lifecycles are one table each, shared by every daemon of the database (docs/kubernetes.md,
-- "Rolling back this release"). A daemon still on the earlier release that shares it counts the
-- new child slots against its own cap and admits nothing until they drain, so every daemon of
-- one database moves to this release together. The deploy note says so where an operator reads
-- it.
insert into slots (issue, index, admitted_at)
select i.key,
       row_number() over (order by i.key) + coalesce((select max(index) from slots), -1),
       now()
  from issues i
  join issues r on r.key = i.tree
 where (i.phase in ('planning', 'implementing', 'testing', 'reviewing', 'retro')
        or (i.phase = 'merging' and i.ready_pending_version is null))
   and i.status not in ('triage', 'icebox', 'backlog', 'done')
   and r.linger_until is null
   and exists (select 1 from slots held where held.issue = r.key)
   and not exists (select 1 from slots mine where mine.issue = i.key);

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
-- nothing of itself, so the tree says it instead: the gate's opening is the only thing that moves
-- a member of the tree out of admitted, so a member past admitted is a gate that opened. A gate
-- that shows neither never opened, and keeps the default.
alter table design_gates add column ever_opened boolean not null default false;

update design_gates set ever_opened = true
  where approved_version is not null
     or exists (select 1 from issues m where m.tree = design_gates.issue and m.phase <> 'admitted');

-- dispatch_issues.parent is the key of the issue's Dispatch parent, null for none. Admission and
-- the workflow judge inside a transaction that does no I/O, so neither can ask Dispatch what hangs
-- under a parent at the moment it decides; the mirror is what they read instead, and it holds
-- every issue of the project, recorded or not, which is exactly the set a parent's children are
-- drawn from. Existing rows are filled from the daemon's own records, the only parent it holds
-- until Dispatch is listed again: an issue the daemon records carries the parent its observation
-- came with, and the first boot listing after this migration fills the rest.
alter table dispatch_issues add column parent text;

update dispatch_issues d set parent = i.parent from issues i where i.key = d.key and i.parent is not null;

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

-- 0133_comments_written_version_backfill.up.sql
--
-- Fills comments.written_version (0130) on the comments stored before 0130's trigger, with the
-- trigger's predicate: artifact_id set, anchor null. Nothing recorded the version current when such
-- a comment was written, so it takes the highest version of its document created at or before the
-- comment: the only record for those rows. A version's created_at is its transaction's start, so a
-- comment written while that transaction was still open can name that version. A comment the
-- trigger already recorded keeps its value, and one on a document with no version that old keeps
-- null and is not rewritten. Setting only this column fires no trigger: comments' update triggers
-- read an update of the body.
--
-- It takes comments EXCLUSIVE before it writes a row, as 0089 takes issues: the update alone locks
-- the rows it changes in the order they lie on disk, and a write that locks two of them in another
-- order would deadlock with it. The table lock is taken while this holds nothing, so a write it
-- waits for completes first and no write waits for it while it holds a row that write needs.
-- EXCLUSIVE blocks no read; it holds every comment write until this commits, which updates only the
-- unanchored comments of documents. Its own migration, so the transaction holds comments' lock
-- alone; 0134 fills asks.
lock table comments in exclusive mode;

update comments c
set written_version = (
  select max(v.number) from artifact_versions v
  where v.artifact_id = c.artifact_id and v.created_at <= c.created_at
)
where c.artifact_id is not null
  and c.anchor is null
  and c.written_version is null
  and exists (
    select 1 from artifact_versions v
    where v.artifact_id = c.artifact_id and v.created_at <= c.created_at
  );

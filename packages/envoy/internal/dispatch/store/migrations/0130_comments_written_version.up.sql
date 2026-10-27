-- 0130_comments_written_version.up.sql
--
-- A comment written on a document without an anchor belongs to the document's version current when
-- it was written (its graph node's parent_version). comments.written_version records it: a
-- `before insert` trigger sets it on every comment with artifact_id set and anchor null to the
-- highest artifact_versions.number of that document, so the insert of every build records it, the
-- earlier release's during the roll included. An insert that names a written_version keeps it. An
-- anchored comment (its version is anchor.version) and an issue-owned one keep null, as does a
-- comment on a document with no version yet. 0133 fills the comments stored before this one.
--
-- The column is nullable with no default, a catalog change that rewrites and scans no row, so the
-- transaction holds comments' ACCESS EXCLUSIVE lock for milliseconds. Its own migration, so the
-- transaction holds comments' lock alone (0075's precedent); 0131 does the same for asks. The
-- trigger reads artifact_versions only when it fires, which this transaction does not.
alter table comments add column written_version integer;

create function comments_written_version() returns trigger language plpgsql as $$
begin
  select max(number) into new.written_version from artifact_versions where artifact_id = new.artifact_id;
  return new;
end;
$$;

create trigger comments_written_version before insert on comments
  for each row when (new.artifact_id is not null and new.anchor is null and new.written_version is null)
  execute function comments_written_version();

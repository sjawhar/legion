-- 0131_asks_written_version.up.sql
--
-- An ask written on a document without an anchor belongs to the document's version current when it
-- was written (its graph node's parent_version). asks.written_version records it: a `before insert`
-- trigger sets it, so the insert of every build records it, the earlier release's during the roll
-- included, on
--   - an approval ask (kind 'approval'): approval.version, the version the request names when it
--     opens; the row's approval.version moves with later versions, written_version does not;
--   - another ask with artifact_id set, anchor null and block_artifact_id null: the highest
--     artifact_versions.number of that document.
-- An insert that names a written_version keeps it. An anchored ask (its version is anchor.version),
-- a block ask (its block's document), an issue-owned question and an ask on a document with no
-- version yet keep null. An approval.version that is not a version number is left to
-- asks_approval_kind_check, which refuses the row after the trigger runs, so the trigger records
-- nothing for it rather than failing the cast first. 0134 fills the asks stored before this one.
--
-- The column is nullable with no default, a catalog change that rewrites and scans no row, so the
-- transaction holds asks' ACCESS EXCLUSIVE lock for milliseconds. Its own migration, so the
-- transaction holds asks' lock alone (0076's precedent). The trigger reads artifact_versions only
-- when it fires, which this transaction does not.
alter table asks add column written_version integer;

create function asks_written_version() returns trigger language plpgsql as $$
declare
  named bigint;
begin
  if new.kind = 'approval' then
    if jsonb_typeof(new.approval -> 'version') = 'number' and new.approval ->> 'version' ~ '^[1-9][0-9]{0,9}$' then
      named := (new.approval ->> 'version')::bigint;
      if named <= 2147483647 then
        new.written_version := named;
      end if;
    end if;
  else
    select max(number) into new.written_version from artifact_versions where artifact_id = new.artifact_id;
  end if;
  return new;
end;
$$;

create trigger asks_written_version before insert on asks
  for each row when (
    new.written_version is null
    and (new.kind = 'approval' or (new.artifact_id is not null and new.anchor is null and new.block_artifact_id is null))
  )
  execute function asks_written_version();
